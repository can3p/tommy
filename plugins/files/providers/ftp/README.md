# `ftp` files provider

A real FTP server, backed by the shared virtual filesystem instead of a real
disk. `STOR`, `RETR`, directory listings, rename and delete all work over a
genuine control connection with real passive-mode data transfers, so any FTP
client or library can be pointed at it - `curl`, `lftp`, a language SDK, a
CI script that has always shipped files this way.

It is built on [`fclairamb/ftpserverlib`](https://github.com/fclairamb/ftpserverlib),
whose driver contract (`ClientDriver`) is exactly an `afero.Fs`. `fs.go` is the
thin adapter: it forwards every call straight onto a `plugins/files.Session`
and never interprets a path itself - `VFS.Resolve` is the plugin's one
security gate, and going around it would defeat the point.

## Try it

```bash
curl -T ./local.txt ftp://localhost:2121/upload/local.txt --ftp-create-dirs -u any:any
curl -s http://localhost:8811/api/v1/files/content/upload/local.txt
```

The first line is `Snippets()`, rendered against the live `SnippetCtx`, so
`tommy providers files/ftp` prints it with the port this instance actually
bound. `--ftp-create-dirs` works even though `STOR` itself requires an
existing parent directory (what a real FTP server answers to `STOR` into
nowhere): a write that would create a new file also creates every missing
parent directory first, so the upload above works from a totally empty tree
in one command.

## Configuration

```toml
[plugins.files.providers.ftp]
enabled = true
port    = 2121          # 0 binds an ephemeral port
bind    = "127.0.0.1"

passive_host  = "127.0.0.1"    # must be a literal IPv4 address
# passive_ports = "30000-30009"  # optional: restrict passive data ports

idle_timeout       = 900   # seconds with no command before disconnecting
connection_timeout = 30    # seconds to establish a data connection

# username = "any"             # optional: pin the credentials USER/PASS must present
# password = "any"
```

An **absent** `port` means 2121; `port = 0` means "bind an ephemeral port",
which is what the test harness uses. `passive_host` is what the server tells a
client to dial back to for `PASV`/`EPSV` - it has to be a real dotted-quad
IPv4 address, which is why the default is `127.0.0.1` rather than `localhost`.
`passive_ports` restricts the data-connection port range the way a real FTP
server behind a firewall has to; leaving it unset lets the OS pick a free port
per transfer, which is both simpler and friendlier to parallel test runs.

Any username and password are accepted by default, and whichever was
presented is recorded as `Event.Meta.user` / `Op.User` on every mutation that
connection goes on to make. Set `username` or `password` and the opposite
becomes true: login is then checked, and the wrong credentials are refused at
`PASS` with a `530`, before any session (and so any event) exists - the same
shape as the SMTP provider's pinned-credential behavior.

## What lands where

Every `STOR`, `MKD`, `DELE`/`RMD` and `RNFR`/`RNTO` is a `Session` call, so it
both changes the shared tree and appends `files.upload` / `files.mkdir` /
`files.delete` / `files.rename` exactly the way the SFTP provider's writes do
- one file uploaded over FTP is immediately visible over SFTP and in the
**Files** tab. `Event.Raw.Body` carries the FTP command that caused it
(`STOR /upload/a.txt`, `RNFR /a; RNTO /b`, ...), and `Event.Raw.Transport` is
`"ftp"`.

`LIST`, `NLST`, `CWD`, `PWD`, `CDUP`, `SIZE` and `MDTM` are reads: they record
nothing, the same way a directory listing over the HTTP API records nothing.

## Commands supported

`USER`/`PASS`, `STOR`, `RETR`, `APPE`, `MKD`, `RMD`, `DELE`, `LIST`, `NLST`,
`CWD`/`PWD`/`XCWD`/`XPWD`, `CDUP`, `RNFR`/`RNTO`, `SIZE`, `MDTM`, `MFMT`,
`TYPE`, `PASV`/`EPSV`, `QUIT`, `NOOP`, `FEAT`, `SYST`. Everything but the
first two comes free from `ftpserverlib` once the driver is right; `Chmod` and
`Chown` are accepted and are no-ops, since the VFS has no permission bits or
owners for them to change.

## Path safety

Every path a client sends - in `CWD`, `STOR`, `RNTO`, anywhere - is handed
unmodified to a `Session` method, which resolves it through `VFS.Resolve`
before touching the tree. That is the same clamping the HTTP API and the SFTP
provider get for free: `..` is clamped at the root the way a chroot is, so
`CWD ..` at the root stays at the root, and `RETR ../../../etc/passwd` reads
whatever the *virtual* path `/etc/passwd` holds inside the tree - there is no
host filesystem underneath any of it for a traversal to reach.

## Files

- `provider.go` - configuration: defaults, `port = 0` ephemeral binding,
  `passive_ports` parsing.
- `listener.go` - the `plugin.ListenerProvider` / `plugin.AddressableProvider`:
  descriptions, snippets, the listener lifecycle.
- `driver.go` - `ftpserver.MainDriver`: settings, the welcome banner,
  `AuthUser` turning a login into a `*files.Session`.
- `fs.go` - `fsAdapter`, the `afero.Fs` / `ftpserver.ClientDriver` translation
  layer over the `Session`.
