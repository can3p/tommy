# S3 HTTP provider

## What it is

The `http` provider is a dedicated path-style S3-compatible HTTP server. It accepts bucket, object, bulk-delete, copy, conditional/range download, checksum, presigned URL, and multipart-upload traffic; successful mutations are captured while object bytes and catalogue state remain available to later S3 reads.

## What it's for

Use it when an application normally writes to Amazon S3 through the AWS CLI, an official AWS SDK, or a transfer manager and you want local or CI runs to keep the same request flow without contacting AWS. Credentials and SigV4 signatures are captured for inspection but deliberately are not verified.

## How to test it for real

Boot tommy with this provider enabled, then point the AWS CLI at its dedicated listener:

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . s3
```

```bash
export AWS_ACCESS_KEY_ID=tommy AWS_SECRET_ACCESS_KEY=tommy AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url http://127.0.0.1:9000 s3api create-bucket --bucket example
printf 'captured by tommy\n' > /tmp/tommy-s3.txt
aws --endpoint-url http://127.0.0.1:9000 s3 cp /tmp/tommy-s3.txt s3://example/hello.txt
aws --endpoint-url http://127.0.0.1:9000 s3 cp s3://example/hello.txt -
```

Beyond a plain put/get, the same listener has been driven through `s3 cp`
(copy), `s3 presign` followed by a bare `curl` against the signed URL, a
multipart upload (`s3 cp` of a file above the configured
`multipart_threshold`), `s3api delete-objects` (bulk delete) and
`s3 rb --force` (recursive bucket delete) — all against a live instance on
alternate ports (19000/18811) so as not to collide with anything else
running, and all producing the AWS CLI's normal success output with no
error. A request carrying `Content-Encoding: aws-chunked` gets back a real
S3-shaped error instead of being stored as literal chunk framing:

```bash
curl -s -X PUT "http://127.0.0.1:9000/example/x.txt" \
  -H 'x-amz-content-sha256: STREAMING-AWS4-HMAC-SHA256-PAYLOAD' \
  -H 'Content-Encoding: aws-chunked' \
  -H 'Authorization: AWS4-HMAC-SHA256 Credential=tommy/20260922/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=deadbeef' \
  --data 'irrelevant'
```

```
<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NotImplemented</Code><Message>aws-chunked streaming payloads are not supported; send a normal HTTP request body.</Message>...</Error>
```

SDK clients must use path-style addressing. With AWS SDK for Go v2, set `BaseEndpoint` to the listener URL and `UsePathStyle` to `true` in `s3.Options`.

## Configuration

The provider inherits the top-level `bind` value. It listens on TCP port 9000 by default; `port = 0` asks the operating system for an ephemeral test port.

`buckets = ["media", "exports"]` creates those buckets before the listener accepts requests. Existing buckets are left alone, so the setting is safe across restarts; a bucket deleted at runtime is created again on the next start. Configuration creates state directly and emits no `s3.bucket.create` events. The shortcut flag is `tommy s3 --s3-buckets media,exports`. `TOMMY_S3_BUCKETS=media,exports` provides the same setting to `tommy serve` and container deployments; the environment overrides TOML, while the shortcut flag overrides the environment. An explicitly empty environment value clears a TOML list.

All additional settings are optional and belong under `[plugins.s3.providers.http]`: catalogue and request limits (`max_buckets`, `max_objects`, `max_active_uploads`, `max_parts`, `max_bucket_bytes`, `max_key_bytes`, `max_metadata_bytes`, `max_object_bytes`, `max_xml_bytes`, `max_header_bytes`) and timeout values in seconds (`read_header_timeout`, `read_timeout`, `write_timeout`, `idle_timeout`, `shutdown_timeout`). The default maximum object size is 64 MiB.

The wire server accepts unsigned requests and arbitrary SigV4 credentials. It validates payload checksums when `Content-MD5` or an `x-amz-checksum-*` header is present. AWS's encoded `aws-chunked` streaming framing is rejected with an S3-shaped `NotImplemented` response rather than being stored as object data.
