# `tftp` files provider

## What it is

A real TFTP server (RFC 1350), backed by the shared virtual filesystem instead
of a real disk. RRQ (download) and WRQ (upload) both work over genuine UDP
against any TFTP client - `curl`, the BSD `tftp` command, a PXE boot ROM,
whatever an application under test already speaks - so a file put by TFTP is
the same tree `ftp` and `sftp` write into, listed and downloadable exactly the
same way.

It is built on [`pin/tftp/v3`](https://github.com/pin/tftp), whose server
contract is two plain functions - a read handler and a write handler, each
handed the filename and an `io.ReaderFrom` / `io.WriterTo` to move bytes
through. `handlers.go` is the thin adapter: it opens or creates the name on a
`plugins/files.Session` and never interprets the path itself - `VFS.Resolve`
is the plugin's one security gate, and going around it would defeat the
point.

## What it's for

Reach for this one for network-device and PXE-style flows: firmware pushers,
provisioning tools and boot ROMs that speak TFTP and nothing else, where you
want to confirm the exact bytes and filename sent before ever touching real
hardware. It is also the simplest provider in this plugin to script against,
since `curl` speaks TFTP natively.

The protocol itself has no login step at all - there is nothing to accept or
pin the way `ftp`'s `USER`/`PASS` or `sftp`'s key/password auth do, by design
of RFC 1350, not a gap in this provider. The only identity a TFTP client ever
offers is its UDP peer address, and that is what every event records.

## How to test it for real

Booted with `TOMMY_NO_UPDATE_CHECK=1 go run . files --tftp-port 6969` (or
`tommy serve` with the provider enabled). One caveat worth stating plainly:
**address the server by `127.0.0.1`, not `localhost`.** On a machine where
`localhost` resolves to `::1` first, `curl`'s TFTP client tries UDP over IPv6
to a port nothing listens on and hangs until it times out - this is a client
DNS-resolution behavior, not a bug in the provider, but it is exactly the kind
of thing that wastes an afternoon if undocumented.

```bash
echo 'it works' > ./local.txt
curl -sS -T ./local.txt tftp://127.0.0.1:6969/upload/local.txt
curl -sS tftp://127.0.0.1:6969/upload/local.txt -o ./downloaded.txt
diff ./local.txt ./downloaded.txt && echo "round-trip ok"
```

And the same file read back over HTTP:

```bash
curl -s http://127.0.0.1:8811/api/v1/files/tree
curl -s http://127.0.0.1:8811/api/v1/files/content/upload/local.txt
```

Verified against a live instance on ports 16969 (tftp) / 18921 (ui) so as not
to collide with anything else running at the time: both `curl` commands above
round-tripped byte-for-byte only once addressed by `127.0.0.1`, and the
resulting event carried `Meta.tsize_declared`, `Raw.Transport = "udp"` and the
client's UDP peer address, exactly as this document already claimed.

## Try it

```bash
echo 'it works' > ./local.txt
curl -sS -T ./local.txt tftp://127.0.0.1:6969/upload/local.txt
curl -sS tftp://127.0.0.1:6969/upload/local.txt -o ./downloaded.txt
diff ./local.txt ./downloaded.txt && echo "round-trip ok"
```

```bash
curl -s http://127.0.0.1:8811/api/v1/files/tree
curl -s http://127.0.0.1:8811/api/v1/files/content/upload/local.txt
```

The address is deliberately `127.0.0.1`, not `localhost`: on a machine where
`localhost` resolves to `::1` first, `curl`'s TFTP client tries UDP over IPv6
to a port nothing listens on and hangs. Both snippets above are `Snippets()`
entries rendered against the live `SnippetCtx`, so
`tommy providers files/tftp` prints them with the port this instance actually
bound - `6969` above is only the fallback for when nothing is running.

## Why 6969, not 69

TFTP's real port is 69, which is privileged on every OS tommy runs on:
binding it would mean running the whole binary as root or granting it
`CAP_NET_BIND_SERVICE`, and tommy is a local testing tool, not a PXE server.
6969 is the unprivileged stand-in, the same pattern `ftp` uses with 2121
instead of 21 and `sftp` uses with 2222 instead of 22. Point a client at
`:6969` explicitly, or forward UDP/69 to it if something (a boot ROM, most
often) cannot be told a different port.

## Configuration

```toml
[plugins.files.providers.tftp]
enabled = true
port    = 6969          # 0 binds an ephemeral port
bind    = "127.0.0.1"

# timeout_seconds = 5   # per-retransmission wait before pin/tftp/v3 retries
# retries         = 5   # retransmission attempts before a transfer is abandoned
```

An **absent** `port` means 6969; `port = 0` means "bind an ephemeral port",
which is what the test harness uses.

TFTP (RFC 1350) has no login step at all - there is nothing to accept or pin
the way `ftp`'s `USER`/`PASS` or `sftp`'s key/password auth do. Every event
this provider appends still records the client's UDP peer address
(`Event.Raw.PeerAddr`), which is the only thing a TFTP client ever identifies
itself by.

## Options supported

- **`octet`** mode (binary) and **`netascii`** mode both work - `pin/tftp/v3`
  wraps the reader/writer for `netascii` automatically based on the mode the
  client requested, so there is no special-casing here at all.
- **`blksize`** (RFC 2348) and **`tsize`** (RFC 2349) are negotiated by
  `pin/tftp/v3` itself. `tsize` on a download is automatic with no extra code:
  the file handle this provider hands the library satisfies `io.Seeker`, which
  is exactly what the library checks for to learn a file's size before it
  starts sending. `tsize` on an upload, when a client declares one, is
  recorded on the event as `Meta.tsize_declared` for inspection value; the
  actual size the tree ends up with is always what was really written, not
  what was declared.
- **`timeout`** (RFC 2349) is parsed by the library but not currently echoed
  back in an `OACK` - a client that asks for a specific retransmission
  interval falls back to its own default, which is spec-compliant behavior for
  an option that was not granted, but it means this provider cannot honor a
  per-request timeout. `Config.Timeout` sets one server-wide value instead
  (`pin/tftp/v3`'s own default: 5 seconds).

## What lands where

Every WRQ is a `Session.OpenFile` call, so it both changes the shared tree and
appends a `files.upload` event exactly the way `ftp`'s `STOR` and `sftp`'s
uploads do - a file uploaded over TFTP is immediately visible over FTP, SFTP
and in the **Files** tab. `Event.Raw.Body` carries the command
(`WRQ <name>`), and `Event.Raw.Transport` is `"udp"`. A client naming a nested
path (`sub/dir/file.bin`) gets its missing parent directories created first,
the same server-side convenience `ftp` gives `--ftp-create-dirs` clients.

RRQ is a read: like `RETR` and SFTP's `Read`, it records nothing.

## Path safety

The filename a client sends in RRQ or WRQ is handed unmodified to a
`Session` method, which resolves it through `VFS.Resolve` before touching the
tree - the same clamping the HTTP API, `ftp` and `sftp` all get for free.
There is no host filesystem underneath any of it for a traversal to reach.

## Files

- `provider.go` - `plugin.Provider` / `plugin.ListenerProvider` /
  `plugin.AddressableProvider`: configuration, descriptions, snippets, the
  listener lifecycle (`net.ListenPacket` on UDP rather than `net.Listen`,
  since this is tommy's first UDP provider).
- `handlers.go` - the RRQ/WRQ handlers `pin/tftp/v3` calls, translated onto a
  `*files.Session`.
