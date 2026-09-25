# s3

## What it is

Tommy's stand-in for Amazon S3, or any object store that speaks the same
wire protocol — MinIO, Ceph RGW, Cloudflare R2, a self-hosted Garage. It
accepts bucket and object operations over a real S3-compatible HTTP API and
keeps them in a catalog you can browse, download from and assert against —
in memory by default, or on disk so buckets and objects survive a restart
(see [Persistence](#persistence)). Every bucket create/delete and object put/copy/delete is also
recorded as an event, so **the catalog shows what is there now and the log
shows how it got that way** — the same split `files` makes between its tree
and its event log.

The plugin is named for the protocol rather than a vendor, because unlike
`mail` or `sms` there is only one wire format worth faking here: S3's own.
`http` is presently its only provider, but the plugin/provider split still
buys the usual thing — a second S3-compatible surface (MinIO's own admin API,
say) would be a sibling provider over the same shared catalog, never a fork
of this one.

## What it's for

The situations that keep coming up are all "my application writes to S3 (or
something that speaks S3) and I do not want a real bucket in the loop":

- Your app uploads user exports, invoices or generated reports to S3 through
  the AWS SDK, and a CI run needs to assert the right key went up with the
  right bytes and content type — without AWS credentials or a throwaway
  bucket.
- A batch job or ETL pipeline reads from and writes to S3-shaped storage, and
  a local integration test wants the same `PutObject`/`GetObject` calls to
  land somewhere inspectable instead of mocking the SDK's HTTP client by
  hand.
- Something under test is built against MinIO or another S3-compatible
  target rather than AWS itself; tommy's path-style listener is exactly the
  shape those targets already expect a client to be configured for.
- You want to see, not just assert, what a multipart upload or a copy
  actually produced — the object's ETag, checksums, content type and
  metadata, as the vendor's own API would report them back.

## How to test it for real

Start tommy with the s3 plugin, and point the AWS CLI at its dedicated
listener with path-style addressing:

```bash
TOMMY_NO_UPDATE_CHECK=1 TOMMY_S3_BUCKETS=example go run . s3
# then open http://localhost:8811/ui/s3/
```

```bash
export AWS_ACCESS_KEY_ID=tommy AWS_SECRET_ACCESS_KEY=tommy AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url http://localhost:9000 s3api head-bucket --bucket example
printf 'captured by tommy\n' > /tmp/tommy-s3.txt
aws --endpoint-url http://localhost:9000 s3 cp /tmp/tommy-s3.txt s3://example/hello.txt
aws --endpoint-url http://localhost:9000 s3 cp s3://example/hello.txt -
```

`example` exists before the listener accepts requests. Configured buckets do
not emit `s3.bucket.create`, so the event log still describes only what the
application did. See the same object land three ways — the tab, the read-back
API, and the event log:

```bash
open http://localhost:8811/ui/s3/
curl -s http://localhost:8811/api/v1/s3/buckets/example/objects
curl -s 'http://localhost:8811/api/v1/events?plugin=s3'
```

This is verified end to end (bucket create, put, copy, presigned GET,
multipart upload, bulk delete, recursive bucket removal, read-back API, tab,
events) in `plugins/s3/providers/http/README.md`, against a live instance on
non-default ports (18811 UI, 19000 S3) so as not to collide with anything
else already running — the AWS CLI's `s3api`, `s3 cp`, `s3 presign` and
`s3 rm`/`rb --force` subcommands, and a raw `curl` against the read-back and
events APIs.

## Persistence

By default the catalog lives as long as the process. With filesystem storage
buckets, objects, their metadata and unfinished multipart uploads are kept on
disk and come back after a restart, so an application that stores object keys
in its own database does not find them dangling. Captured **events are not
kept**: after a restart the catalog shows what is there and the event log
starts empty.

`tommy s3 --persist ./tommy-data` turns it on (`TOMMY_PERSIST` in a container,
see [`docs/docker.md`](../../docs/docker.md); `[storage]` in `tommy.toml`, where
`[storage.plugins.s3]` overrides this plugin alone). Run from a cold start with
the AWS CLI:

```bash
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1
tommy s3 --persist ./tommy-data &
aws --endpoint-url http://127.0.0.1:9000 s3 mb s3://media
echo hello > hello.txt
aws --endpoint-url http://127.0.0.1:9000 s3 cp hello.txt s3://media/hello.txt
kill %1; tommy s3 --persist ./tommy-data &
aws --endpoint-url http://127.0.0.1:9000 s3 cp s3://media/hello.txt -   # hello
curl -s 'http://127.0.0.1:8811/api/v1/events?plugin=s3'                # []
```

A snapshot that cannot be restored — corrupt, from an unknown version, or
naming bytes that are missing — stops startup with an error rather than
starting with an empty catalog. Startup buckets (`buckets = [...]`) compose
with it: existing ones are left alone. The mechanism is in
[`docs/contracts.md`](../../docs/contracts.md); this plugin's side is
`persistence.go`.

## Event types

| Event | When |
|---|---|
| `s3.bucket.create` | a bucket is created |
| `s3.bucket.delete` | a bucket is deleted |
| `s3.object.put` | an object is written — a plain `PutObject` or a completed multipart upload |
| `s3.object.copy` | an object is created by `CopyObject`, with `Payload.Source` naming the original bucket/key |
| `s3.object.delete` | an object is deleted, singly or as part of a bulk `DeleteObjects`/recursive bucket delete |

`Payload` (`plugins/s3/event.go`) carries the operation, bucket, key, size,
ETag, content type and (for a copy) the source location; bytes never go in
the event, only a `blob.Ref` pointing at the blob store. `Payload.Snippet()`
is what the UI's activity list and the event summary line render — e.g.
`stored s3://example/hello.txt (18 bytes)`.

Reads (`HeadBucket`, `ListObjects`, `HeadObject`, `GetObject`) record
nothing, the same way a directory listing over the `files` HTTP API records
nothing.

## API

Mounted under `/api/v1/s3/`, reading from the shared catalog
rather than the event log — so a client that writes an object and then reads
it back through the API sees its own write immediately, even if the write's
event has since scrolled out of the ring buffer.

These routes have their own OpenAPI description, generated from the server's
route table: `GET /api/v1/s3/openapi.json` from a running tommy, or
[`docs/openapi-s3.json`](../../docs/openapi-s3.json) in the repository. The
generic event routes are described separately, in
[`docs/openapi.json`](../../docs/openapi.json).

| Route | Notes |
|---|---|
| `GET /buckets` | every current bucket plus aggregate catalog stats |
| `DELETE /buckets` | delete every bucket, object and inactive multipart upload; the event history survives |
| `GET /buckets/{bucket}` | one bucket snapshot with an `objects` link |
| `DELETE /buckets/{bucket}?recursive=` | delete an empty bucket, or everything in it first when `recursive=true` |
| `GET /buckets/{bucket}/objects?prefix=&delimiter=&start_after=&continuation=&max_keys=` | one page of a listing, with exact S3 prefix/delimiter/continuation semantics |
| `GET /buckets/{bucket}/objects/{key...}` | metadata for one exact key — the key is never treated as a filesystem path |
| `DELETE /buckets/{bucket}/objects/{key...}` | delete one object and append `s3.object.delete` |
| `GET /buckets/{bucket}/content/{key...}` | download the object's bytes with its ETag, checksums and metadata headers and HTTP range support; always served as an attachment under a sandbox CSP, whatever disposition the uploader stored |

Verified against the live instance above:

```bash
curl -s http://localhost:8811/api/v1/s3/buckets
curl -s http://localhost:8811/api/v1/s3/buckets/example/objects/hello.txt
curl -s http://localhost:8811/api/v1/s3/buckets/example/content/hello.txt
curl -s -X DELETE -o /dev/null -w '%{http_code}\n' http://localhost:8811/api/v1/s3/buckets
```

The last one clears the whole catalog and answers `204`.

## UI

`/ui/s3/` — a bucket list on the left, a prefix-delimited object browser (with
breadcrumbs, so `/`-delimited keys behave like folders even though S3 itself
has no directories) on the right, object metadata (size, ETag, content type,
`x-amz-meta-*`) and a download link per row, and a "recent activity" list fed
from the event log below. It refreshes live over SSE on every `s3.*` event
type. `GET /ui/events/{id}` is left to the core, so any operation also opens
in the generic raw inspector.

Bucket names, keys and metadata values are untrusted input, so they are
interpolated as plain strings through `html/template` and never as
`template.HTML`; the URLs built around them are percent-encoded in Go first
(`escapePathValue` even escapes `.` in a key so a segment like `..` cannot be
mistaken for a path-traversal token by anything downstream).

## Limits

`s3.Limits` (`plugins/s3/store.go`) bounds the catalog and every request:
1000 buckets, 50 000 objects, 1000 active multipart uploads, 10 000 parts,
255-byte bucket names, 1024-byte keys, 16KiB of metadata, and a 64MiB default
per-object size — all overridable per the `http` provider's own README. A
limit exceeded never fails silently or partway; it comes back as an
S3-shaped error: a catalog limit (too many buckets, objects, active uploads
or parts) is `503 ServiceUnavailable`, an object or its metadata that is too
large is `400 EntityTooLarge`/`MetadataTooLarge`, and an invalid bucket name,
key or part number is `400 InvalidBucketName`/`InvalidArgument` — the same
codes and shapes a real S3-compatible client already knows how to parse.

## What is deliberately not implemented

Tommy captures what an application sent and answers with what the protocol
requires so the client proceeds — it does not simulate a full vendor. Left
out on purpose:

- **Virtual-host-style addressing** (`bucket.s3.amazonaws.com/key`). Only
  path-style (`/bucket/key`) is routed; every AWS SDK and the CLI support
  path-style explicitly for exactly this kind of target (`UsePathStyle` /
  `--endpoint-url` with `s3.addressing_style = path`), so this is a
  configuration line in the client, not a missing feature in the protocol.
- **Object versioning.** There is one current object per key; `PutObject`
  replaces it, there is no version id, and `ListObjectVersions` is not
  mounted.
- **Bucket policies, ACLs, and IAM.** Every request is accepted regardless of
  the credentials or signature presented — SigV4 signatures are parsed for
  the access key and region they name (recorded on the event) but never
  cryptographically verified, and there is no way to pin an expected access
  key the way `mailjet`/`sendgrid` let you pin an API key. A fake that
  enforced permissions would be deciding policy, which is out of scope by
  design; see `CLAUDE.md`'s scope boundary.
- **`aws-chunked` streaming payloads** (`Content-Encoding: aws-chunked` /
  `x-amz-content-sha256: STREAMING-*`, the encoding the CLI and some SDKs use
  for large unsigned or trailer-checksummed uploads). Rejected with an
  S3-shaped `501 NotImplemented` rather than being stored as literal chunk
  framing; see the `http` provider's own README for a verified example.
- **Object Lock, replication, lifecycle rules, and server-side encryption
  configuration.** These are AWS storage-management features with no wire
  request an application under test would send and observe a response to —
  there is nothing here for a catcher to capture.

## Automated tests

```bash
go test ./plugins/s3/...
go test -race ./plugins/s3/...
```

They cover the store (bucket/object CRUD, listing pagination and common
prefixes, multipart upload lifecycle, limits), the session/event layer, the
read-back API including range requests, the tab with `httptest`, concurrency
under `-race`, and `plugintest.Conformance`.
