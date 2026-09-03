# files

## What it is

Tommy's stand-in for the far end of any file-transfer protocol: the FTP drop a
partner gives you, the SFTP server a deployment script pushes to, the TFTP
server a device firmware pusher talks to, the NFS export a backup job mounts.
Instead of standing up the real thing, `files` accepts what is sent over
`ftp`, `sftp`, `tftp` or `nfs` and keeps it in a virtual filesystem you can
browse, download from and assert against. Every upload, mkdir, delete and
rename is also recorded as an event, so **the tree shows what is there now and
the log shows how it got that way**.

The plugin is named for what it holds rather than for one protocol — `ftp`,
`sftp`, `tftp` and `nfs` are sibling providers over one shared filesystem,
exactly the way Mailjet and SendGrid are siblings inside `mail`.

## What it's for

The situations that keep coming up are all "my application writes files
somewhere over a protocol I do not want to stand up for real":

- Your app exports a nightly CSV to a partner's FTP drop, and you want to see
  what it actually wrote without getting a partner test account.
- A bank-statement importer uploads a file over SFTP as part of a batch job,
  and a CI test needs to assert the right filename went up and the bytes come
  back byte-for-byte.
- A firmware pusher or provisioning tool talks TFTP to a device, and you want
  to confirm it sent the right image before touching real hardware.
- Something under test expects a **mounted filesystem**, not a client library —
  the specific misery of spinning up a Dockerized Samba or NFS server just to
  exercise one upload path. Point it at tommy's NFS export instead.

Which provider to reach for is mostly decided by what your client already
speaks, not by picking the "best" protocol:

- **`ftp`** — legacy partner drops, anything scripted with `curl -T` or `lftp`,
  or an SDK that only knows FTP.
- **`sftp`** — anything that already carries an SSH client: real OpenSSH
  `sftp`/`scp`, or a language library built on `libssh`/`golang.org/x/crypto/ssh`.
- **`tftp`** — network-device and PXE-style flows. It is UDP with no
  authentication at all, by design of the protocol, not a tommy shortcut.
- **`nfs`** — when the thing under test mounts a filesystem rather than
  speaking a transfer protocol to a client library.

## How to test it for real

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . files
# then open http://localhost:8811/ui/files/
TOMMY_NO_UPDATE_CHECK=1 go run . providers files   # descriptions, endpoints and snippets for this build
```

A full round trip through one provider (`ftp` here — see each provider's own
README for `sftp`, `tftp` and `nfs`):

```bash
echo 'it works' > ./local.txt
curl -T ./local.txt ftp://localhost:2121/upload/local.txt --ftp-create-dirs -u any:any
curl -s http://localhost:8811/api/v1/files/content/upload/local.txt -o ./downloaded.txt
diff ./local.txt ./downloaded.txt && echo "round-trip ok"
```

See the result the same file lands three ways — the tab, the tree API, and the
event log:

```bash
open http://localhost:8811/ui/files/
curl -s http://localhost:8811/api/v1/files/tree
curl -s 'http://localhost:8811/api/v1/events?plugin=files'
```

This is verified end to end (upload, download, diff, tree, events) in the
`ftp`, `sftp` and `tftp` provider READMEs, executed against a live instance on
non-default ports so as not to collide with anything else already running.

## Two stores, two lifetimes

This is the first stateful plugin, and the design turns on one distinction:

| | What it is | Where it lives | When it goes away |
|---|---|---|---|
| **VFS** | the tree as it is right now | `vfs.go`, in memory | when something deletes it, or `DELETE /tree` |
| **Blobs** | the bytes of every file in the tree | `core/blob` | when the file is deleted or overwritten |
| **Events** | the history of what happened | `core/store` ring buffer | when the buffer wraps |

So a file stays listed and downloadable long after the `files.upload` event that
announced it has been evicted. That is the whole reason bytes live in the blob
store rather than in the event, and there is a test that proves it
(`TestFileOutlivesItsEvent`) — if it ever fails, uploads start disappearing
under load.

## The VFS

```go
v := files.NewVFS()                      // or plugin.VFS(), the one every provider shares
n, err := v.PutBytes(ctx, "/upload/report.csv", data, files.WriteOptions{Provider: "ftp"})
entries, err := v.List("/upload")
f, err := v.Open(ctx, "/upload/report.csv")   // streams out of the blob store
```

A `Node` is a snapshot value — name, path, `Dir`, size, mtime, the provider that
wrote it, the content type, and a `blob.Ref`. `Node.FileInfo()` adapts it to the
`fs.FileInfo` that both `ftpserverlib` and `pkg/sftp` want back from a stat.

Errors are `*fs.PathError` wrapping sentinels that reuse the `io/fs` ones where
they exist, so `errors.Is(err, files.ErrNotExist)` and
`errors.Is(err, fs.ErrNotExist)` both work.

### Path resolution is the security boundary

Every method funnels through `VFS.Resolve`, and nothing else — in this package or
in any provider — is allowed to interpret a path. It:

- rejects NUL bytes, control characters and invalid UTF-8;
- normalizes Windows separators, so `..\..` cannot slip past a check that only
  knew about `../`;
- cleans the path with `..` **clamped at the root**, the way a chroot is, so
  `../../etc/passwd` resolves to `/etc/passwd` *inside the tree*;
- enforces the depth, name-length and path-length limits.

There is no host filesystem underneath any of it: the tree is a map in memory
and the VFS never opens a real file, so even a bug here cannot read or write
anything of the machine's. `path_test.go` runs a table of hostile paths through
`Resolve` and then through every operation.

### Bounds

`Limits` caps path depth (32), name length (255), path length (4096), entries
per directory (4096), total nodes (50 000) and single-file size (64 MiB). A
hostile client looping on `MKD` gets an error rather than an out-of-memory.

### Locking

One `sync.RWMutex` guards the tree; reads take the read lock, mutations the
write lock. **No blob I/O ever runs while the tree is locked**: an upload streams
into the blob store first and is installed into the tree in one short locked
step, so a slow transfer never stalls a listing and a listing never sees a
half-written file. Freeing a replaced blob happens after the new node is
visible, and the blob store hands out snapshots, so an in-flight download is
never torn. `concurrency_test.go` runs the lot under `-race`.

## Writing from a provider

Providers use a `Session` rather than the VFS directly: it is the VFS bound to
one provider's identity and to the store, so every mutation lands in the tree
**and** in the log without the provider having to remember the second half.

```go
s := files.NewSession(v, deps,
    files.WithProvider("ftp"), files.WithTransport("ftp"),
    files.WithPeer(conn.RemoteAddr().String()), files.WithUser(user))

s.MkdirAll(ctx, "/upload")                                     // files.mkdir
s.Put(ctx, "/upload/a.txt", r, files.WithCommand("STOR /upload/a.txt"))  // files.upload
s.Rename(ctx, "/upload/a.txt", "/upload/b.txt")                // files.rename
s.RemoveAll(ctx, "/upload")                                    // files.delete
```

Reads (`Stat`, `List`, `Open`) record nothing — a directory listing is not a
change. A write handle from `Session.Create` records its event when it is
closed, so an abandoned transfer leaves neither a file nor an event.

The plugin hands each provider the shared VFS through `BindVFS`:

```go
type Provider struct{ vfs *files.VFS }
func (p *Provider) BindVFS(v *files.VFS) { p.vfs = v }
```

## API

Mounted under `/api/v1/files/`.

| Route | Notes |
|---|---|
| `GET /tree?path=&recursive=` | one directory listing plus breadcrumb, parent and whole-tree counts; `recursive=1` walks the subtree |
| `GET /stat/{path...}` | one entry, file or directory |
| `GET /content/{path...}` | streams the bytes with the recorded `Content-Type`, a correct `Content-Length` and range support; `?inline=1` for an inline disposition |
| `DELETE /content/{path...}` | 204; a non-empty directory needs `?recursive=1` |
| `DELETE /tree` | empties the filesystem; the event log deliberately survives |

Listings come from the VFS, not from the event log. Every entry carries `links`
so a client can walk the tree without building URLs itself.

## UI

`/ui/files/` — breadcrumb navigation, a directory table with size, modification
time and the protocol that wrote each file, download links, and a "recent
activity" list underneath fed from the event log. It refreshes live over SSE on
every `files.*` type. `GET /ui/files/events/{id}` is left to the core, so any
operation also opens in the generic raw inspector.

Filenames are untrusted input, so they are interpolated as plain strings through
`html/template` and never as `template.HTML`; the URLs built around them are
percent-encoded in Go first. `TestUIEscapesHostileFilenames` uploads a file
called `<img src=x onerror=alert(1)>.txt` and asserts it never reaches the page
as markup.

## Automated tests

The package's own tests drive a test-only fake provider that both accepts an
upload over the ingress and writes into the VFS directly:

```bash
go test ./plugins/files/...
go test -race ./plugins/files/...
```

They cover the VFS surface (overwrite, rename, recursive delete,
mkdir-with-parents, the write-handle modes), the hostile-path table, the
concurrency behavior under `-race`, blob lifetime across ring-buffer eviction,
every API route including a byte-exact download and a range request, the tab
with `httptest` + `goquery` including filename escaping, and
`plugintest.Conformance`.
