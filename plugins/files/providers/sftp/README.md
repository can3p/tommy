# `sftp` files provider

A real SFTP server — an SSH transport plus the SFTP subsystem — that accepts
uploads from any client on its own port and keeps them in the shared virtual
filesystem instead of anywhere on disk. Every upload, `mkdir`, rename and delete
becomes a `files.*` event tagged with whatever credentials the client presented,
so the **Files** tab shows the tree as it is now and the log shows how it got
that way.

SFTP is an SSH subsystem, not FTP with TLS bolted on, so this provider is two
layers: `x/crypto/ssh` runs the handshake and answers a `subsystem` request for
`sftp`, and `pkg/sftp`'s `RequestServer` serves the file operations through a
`files.Session`. Nothing reaches the host filesystem — the one file this
provider owns on disk is its SSH host key.

## Try it

```bash
echo 'it works' > ./local.txt
sftp -P 2222 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -b - any@localhost <<'EOF'
mkdir /upload
put ./local.txt /upload/local.txt
ls -l /upload
EOF
```

```bash
scp -P 2222 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR ./local.txt any@localhost:/local.txt
```

```bash
curl -s http://localhost:8811/api/v1/files/tree
curl -s http://localhost:8811/api/v1/files/content/upload/local.txt
```

All three are `Snippets()` entries rendered against the live `SnippetCtx`, so
`tommy providers files/sftp` prints them with the port this instance actually
bound — the `2222` above is only the fallback for when nothing is running. The
tests execute the first two with the real OpenSSH client.

No password is needed: the server accepts the SSH `none` authentication method,
which is what keeps a copy-pasted command free of a prompt. Host-key checking is
turned off in the snippets on purpose — the key is a throwaway identity for a
local fake — but it is a *stable* throwaway, see below.

## The host key persists

**The one trap in this provider.** An SSH client remembers the key of every host
it has talked to. A server that generates a fresh one on each boot makes every
second connection fail with `REMOTE HOST IDENTIFICATION HAS CHANGED`, and the
person debugging it blames tommy rather than their `known_hosts` file.

So on first run an ed25519 key is generated and written to `host_key_path` with
mode `0600` (creating the directory `0700` if needed, and using `O_EXCL` so two
tommys racing on one path agree on a single key), and every later run reads it
back. The fingerprint is logged at startup, generated or not:

```
INFO sftp listening addr=127.0.0.1:2222 host_key=/home/you/.config/tommy/sftp_host_ed25519
     host_key_generated=false fingerprint=SHA256:0Yq…
```

A key file that exists but cannot be parsed is a **startup error naming the
file**, never a silent regeneration: replacing a key the clients already trust is
exactly the outcome that must not happen by accident.

`TestHostKeyIsStableAcrossRestarts` boots the provider twice against one path and
asserts the client sees the identical key both times.

## Configuration

```toml
[plugins.files.providers.sftp]
enabled       = true
port          = 2222        # 0 binds an ephemeral port
bind          = "127.0.0.1"
host_key_path = "~/.config/tommy/sftp_host_ed25519"   # generated on first run

# username = "app"          # optional: pin the credentials, making auth required
# password = "s3cret"
# authorized_keys = "~/.ssh/authorized_keys"          # optional public-key allowlist

server_version    = "SSH-2.0-tommy"
handshake_timeout = 30      # seconds from accept to authenticated
idle_timeout      = 600     # seconds with no traffic; a live transfer refreshes it
max_connections   = 64
max_auth_tries    = 6
```

An **absent** `port` means 2222; `port = 0` means "bind an ephemeral port", which
is what the test harness uses. `host_key_path` defaults to
`<user config dir>/tommy/sftp_host_ed25519` and understands a leading `~`.

### Authentication

Anyone gets in by default — with a password, with a key, or with nothing at all —
and what they presented lands in `Event.Meta.auth`, recorded rather than judged
(provider rule 1). A client that offers nothing is authenticated with the `none`
method, which is what a fake wants: an application pointed at tommy for the first
time should not fail on credentials it has not been given yet.

The consequence worth knowing: a client only sends credentials when the server
asks for them, and by default this one does not ask. Pin `username` or `password`
and it does — authentication becomes mandatory and checked, and the presented
password is recorded next to the upload, which is how you exercise your
application's error path *and* see what it actually sent.

Set `authorized_keys` and public-key authentication is checked against that file
instead; a password will not walk past the allowlist unless credentials are
pinned as well.

## How the handlers map onto the VFS

| `pkg/sftp` | SFTP methods | `files.Session` |
|---|---|---|
| `Fileread` | `Get` | `Open` → `*files.File`, an `io.ReaderAt` streaming out of the blob store |
| `Filewrite` / `OpenFile` | `Put`, `Open` | `OpenFile` → an `io.WriterAt`; committed atomically on close |
| `Filecmd` | `Mkdir` `Rmdir` `Remove` `Rename` `Setstat` | `Mkdir`, `Remove`, `Rename`, `Chtimes`/`Truncate` |
| `Filelist` | `List` `Stat` `Lstat` | `List`, `Stat` → `Node.FileInfo()` |

**No path is interpreted here.** Every path goes to the session, which resolves
it through `VFS.Resolve` — the single security gate — and there is no host
filesystem underneath to escape to. `../../../etc/passwd` is clamped at the root
the way a chroot does it and lands *inside* the tree; a NUL byte or a control
character is refused outright.

Writes buffer and commit in one step, so a listing never sees a half-written
file, and a transfer that dies mid-flight is aborted rather than committed —
neither a truncated file nor an event. The commit runs with a context that
outlives the request, because `pkg/sftp` cancels a request's context as it closes
the handle, which is exactly when the upload lands.

Deliberately answered with `SSH_FX_OP_UNSUPPORTED`:

- **`Symlink`, `Link`, `Readlink`** — the VFS does not model links, and inventing
  one would be worse than saying so. `Lstat` still works: with no symlinks it is
  the same thing as `Stat`.

Deliberately accepted and dropped:

- **`chmod` and `chown`** — there is no mode and no uid in the VFS, and failing
  here would break every client that preserves a mode after uploading.

`Setstat` never appends an event: a timestamp or mode fixup is metadata, not a
change to what the filesystem holds, and one event per setstat would drown the
uploads in the log. A size *is* applied (the VFS can represent it), and an
`fsetstat` that arrives before an upload is closed — which is how `sftp put -p`
preserves timestamps — is applied to the open handle instead of failing on a file
that does not exist yet.

## Shell and exec are refused, cleanly

This is an SFTP endpoint, not a shell. A `session` channel is accepted and only a
`subsystem` request naming `sftp` is honored; `exec`, `shell` and any other
subsystem get an explanation on stderr, a refusal, and an exit status, so a
client prints an error instead of hanging. A non-`session` channel is rejected
with `UnknownChannelType`. None of it takes the connection or the listener down,
which `TestShellAndExecAreRefusedCleanly` asserts by running SFTP over the same
connection afterwards.

## What lands where

| | |
|---|---|
| the file | the shared `files.VFS`, bytes in `core/blob` |
| `Event.Type` | `files.upload`, `files.mkdir`, `files.rename`, `files.delete` |
| `Event.Provider` | `sftp` |
| `Event.Raw.Transport` | `ssh`, with the peer address and the operation in the body |
| `Event.Meta.auth` | method, user, password or key fingerprint, and `accepted` |
| `Event.Meta.client_version` | the client's SSH identification string |

## Failing safely

Every bound is the VFS's — path depth, name and path length, entries per
directory, total nodes and file size — plus this provider's own:
`max_connections` (a connection over the limit is closed immediately rather than
queued), `handshake_timeout` (a client that connects and says nothing cannot hold
a slot), `idle_timeout` (refreshed by every read and write, so a slow upload is
never cut off) and `max_auth_tries`.

## Files

- `provider.go` — the `plugin.ListenerProvider`: configuration, snippets, the
  listener lifecycle.
- `ssh.go` — the SSH transport: authentication callbacks, channels, the
  subsystem dispatch, the idle-deadline connection.
- `handlers.go` — the four `pkg/sftp` handlers over a `files.Session`.
- `hostkey.go` — generating, persisting and loading the host key.

## How to test

```bash
go test ./plugins/files/providers/sftp/...
go test -race ./plugins/files/providers/sftp/...
```

The tests drive a real `pkg/sftp` client over a real socket against a listener
booted through `core/testutil` on an ephemeral port: byte-exact upload and
download, nested `mkdir`, listings, `Rename`, `Remove`, `Stat`, a tree walk,
append and `O_EXCL` opens, the hostile-path table, default and pinned
credentials, the `authorized_keys` allowlist, refused `exec`/`shell`/subsystem
requests, concurrent sessions under `-race`, host-key persistence across a
restart, and the two OpenSSH snippets executed as written.
