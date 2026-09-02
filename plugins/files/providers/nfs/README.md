# `nfs` files provider

A real NFSv3 server (RFC 1813), backed by the shared virtual filesystem
instead of a real disk. An operating system can `mount` it, and a file copied
into the mount point is the same tree `ftp`, `sftp` and `tftp` write into -
listed in the **Files** tab and downloadable over the HTTP API exactly the
same way.

It is built on [`willscott/go-nfs`](https://github.com/willscott/go-nfs),
whose backend contract is a [`go-billy`](https://github.com/go-git/go-billy)
filesystem rather than the `afero.Fs` the FTP provider adapts to. `fs.go` is
the translation layer and does the same job `providers/ftp/fs.go` does: every
method forwards onto a `plugins/files.Session` and none of them interprets a
path, because `VFS.Resolve` is the plugin's one security gate.

## Mounting: the part you have to know

An NFS client normally finds the NFS and `mountd` services by asking
**rpcbind** (the portmapper) on port 111. **tommy runs no portmapper**, and
does not want to: 111 is privileged on every OS tommy runs on, and
registering with the machine's real rpcbind would advertise this fake to
everything on the host. Instead both RPC programs - MOUNT (100005) and NFS
(100003) - are answered on the *one* TCP port this provider binds, dispatched
on the program number in each request header.

So the client must be told both ports explicitly. With neither given it goes
looking for rpcbind and the mount fails. Linux
[`nfs(5)`](https://man7.org/linux/man-pages/man5/nfs.5.html) and macOS
`mount_nfs(8)` spell them the same way:

```bash
# Linux
sudo mkdir -p /mnt/tommy
sudo mount -t nfs -o nfsvers=3,tcp,port=2049,mountport=2049,nolock,noacl 127.0.0.1:/ /mnt/tommy

echo 'it works' | sudo tee /mnt/tommy/hello.txt
sudo umount /mnt/tommy
```

```bash
# macOS
sudo mkdir -p /Volumes/tommy
sudo mount -t nfs -o vers=3,tcp,port=2049,mountport=2049,nolocks 127.0.0.1:/ /Volumes/tommy
```

Mounting needs root on every operating system. For a test that cannot have
it, [libnfs](https://github.com/sahlberg/libnfs)'s tools are a userspace NFS
client and need none:

```bash
echo 'it works' > ./local.txt
nfs-cp ./local.txt 'nfs://127.0.0.1/tommy?version=3&nfsport=2049&mountport=2049'
nfs-ls -l 'nfs://127.0.0.1/tommy?version=3&nfsport=2049&mountport=2049'
```

The export name (`/` above, `tommy` in the URL) is **ignored**: there is one
tree and every MOUNT request is answered with it. What the client asked for
is recorded, not checked - provider rule 1.

```bash
curl -s http://localhost:8811/api/v1/files/tree
curl -s http://localhost:8811/api/v1/files/content/hello.txt
```

All of these are `Snippets()` entries rendered against the live `SnippetCtx`,
so `tommy providers files/nfs` prints them with the port this instance
actually bound - `2049` above is only the fallback for when nothing is
running.

## Why 2049, unlike the other three

`ftp`, `sftp` and `tftp` all had to move off their real ports (21, 22, 69)
because those are privileged. **NFS's real port, 2049, is already above 1024**,
so there is no reason to serve this anywhere else and one good reason not to:
a client told `port=2049` is a client told the default. The only machine where
it collides is one already running a real `nfsd`, and `--nfs-port` moves it.

Note 2049 is the traditional port of the *NFS* program only. `mountd` has no
fixed port anywhere - it is normally discovered through rpcbind - which is why
`mountport=` is passed as well, pointing at the same listener.

## What lands in the event log

NFS is **stateless**: there is no open or close on the wire. A file therefore
arrives as `NFSPROC3_CREATE` followed by one `NFSPROC3_WRITE` per chunk the
client chose to send, and each of those commits to the tree and appends its
own `files.upload` event. A test fixture is one CREATE plus one WRITE; a large
file is one event per chunk.

That is deliberate, and it is the honest translation. Unlike FTP's `STOR` or
an SFTP upload there is no end-of-transfer signal to hang a single event on -
`NFSPROC3_COMMIT` exists but `go-nfs` handles it internally and never reaches
the filesystem backend. `Event.Raw.Body` names the procedure that caused each
one, so the sequence reads correctly in the UI.

`Event.Raw.Transport` is `"tcp"`, `Event.Raw.PeerAddr` and `Op.Peer` are the
address of the connection that **mounted**, and `Meta.uid` / `Meta.gid` carry
the AUTH_UNIX credential. `Op.User` is the client's own hostname from that
credential, which is the closest thing to an identity NFS puts on the wire.

The mount connection is the right caveat to know: a kernel client opens one
TCP connection to `mountd` and another to the NFS program, and only the first
one runs through `Handler.Mount`. billy's methods take neither a context nor a
connection, so the mounting connection's identity is what every event from
that client carries. Same host, possibly a different source port.

Reads record nothing, the way `RETR` and SFTP's `Read` do: the event log is a
record of change and a directory listing is not one.

## Path and handle safety

Two separate questions, because NFS addresses objects by **handle**, not by
path.

- **Names.** Every path a client component contributes is joined by `Join`
  (which only joins and cleans, using `path`, never `filepath`) and then
  resolved through `VFS.Resolve` before any node is touched. There is no host
  filesystem underneath the tree for a traversal to reach, and `..` clamps at
  the root the way it does on any rooted filesystem.
- **Handles.** A handle here is a random UUID minted by `go-nfs`'s caching
  handler and kept in an LRU. It encodes no path at all, so a handle a client
  invents, replays or has held past eviction can only be answered with
  `NFS3ERR_STALE`. A handle this server *did* mint names a path this server
  produced from a successful lookup - and that path is resolved again, through
  the same single gate, on every operation.

`listener_test.go` proves both with raw RPC calls the client library would
never make on its own: it cleans a path before splitting it, so a hostile
single component (`..`, an absolute host path, a name carrying a NUL) has to
be put on the wire by hand.

## Ownership and permissions

The VFS has no owners and fixed `0644`/`0755` bits, but a mounted NFS share is
policed by the **client** kernel against whatever the server reports. Reporting
uid 0 and `0644` would leave an ordinary process on the client unable to write
to a fake that accepts everything. So the caller is reported as the owner and
the modes are world-writable. There is no access control to model here, and a
fake that answers "permission denied" is useless.

Directories report a link count of two, the way a real one does: a count below
two makes `find`'s leaf optimisation skip subdirectories.

## Configuration

```toml
[plugins.files.providers.nfs]
enabled = true
port    = 2049          # 0 binds an ephemeral port
bind    = "127.0.0.1"

# handle_cache = 16384  # live file handles; see below
```

An **absent** `port` means 2049; `port = 0` means "bind an ephemeral port",
which is what the test harness uses. `--nfs-port` is the CLI equivalent and is
the only flag this provider adds: NFS has no login step to pin credentials
for, and `handle_cache` is a tuning knob no test run needs to flip.

`handle_cache` matters in both directions. Too small and a client still
walking a directory gets `NFS3ERR_STALE`, because `go-nfs` also caps one
`READDIR`/`READDIRPLUS` response at half the limit; too large and a
long-running instance holds paths nobody will ask for again. The default is
generous next to the VFS's own 4096-entries-per-directory limit, and anything
below 64 is raised.

## Known edges

- **`O_EXCL` without `O_CREATE`.** `go-nfs` opens a file that way when SETATTR
  carries a new size - evidently meaning "do not create". POSIX leaves that
  combination undefined and the VFS reads it strictly, as "must not exist", so
  the adapter drops the flag. Without that, truncating a file over NFS (what
  `open(O_TRUNC)` becomes on a mount) fails outright. There is a test for it.
- **Symlinks, hard links and special files** are not supported: the VFS has no
  concept of them, and inventing one would mean a second way to name a node,
  which is exactly what the one-resolver rule exists to prevent. `go-nfs`
  advertises symlink support in FSINFO regardless - it infers it from the Go
  interface being present, which billy requires - so a client sees the offer
  and gets `NFS3ERR_NOTSUPP` if it takes it.
- **Nested writes need their parents.** Unlike `ftp`, which pre-creates
  missing directories the way `curl --ftp-create-dirs` expects, an NFS client
  issues its own `MKDIR` per level. Nothing is guessed here.
- **`go-nfs` logs through a package-level global** rather than a per-server
  logger. `log.go` replaces it, once per process, with a bridge onto tommy's
  `slog`. Its severities are mapped down deliberately: the library calls
  `Errorf` for ordinary client outcomes like a lookup that misses.

## Files

- `provider.go` - `plugin.Provider` / `ListenerProvider` /
  `AddressableProvider`: configuration, description, snippets, and the
  listener lifecycle.
- `handler.go` - the `go-nfs` `Handler`: MOUNT, `FSSTAT`, and the AUTH_UNIX
  credential decoder. Wrapped in the library's caching handler, which is what
  mints file handles.
- `fs.go` - the `billy.Filesystem` adapter over a `*files.Session`.
- `log.go` - the `go-nfs` logger bridged onto `slog`.
