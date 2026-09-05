# Docker

## What it is

`can3p/tommy` is tommy as a single-binary container image: the same binary the
release archives carry, on `gcr.io/distroless/static:nonroot`, with every
default listener exposed, the repository's own `tommy.toml` at
`/etc/tommy/tommy.toml`, and a writable `/data` for the things tommy generates
rather than captures. The entrypoint is the binary, so every subcommand — `serve`,
`providers`, `openapi`, the single-plugin shortcuts — works exactly as it does
outside a container.

## What it's for

You reach for the image when a Go toolchain is not where tommy needs to run:

- **In CI.** A job that has to prove your application sent the right email adds
  `can3p/tommy` as a service container and points the SDK's base URL at it. No
  toolchain, no build step, one pinned tag.
- **In a compose stack.** Your application already runs beside a database and a
  queue in `docker compose`; tommy joins them as one more service and your app's
  `MAILJET_API_BASE`, `SMTP_HOST` or `FTP_HOST` points at it by service name.
- **On a machine with no Go.** A colleague wants to see what your integration
  sends to a hospital system, and `docker run` is the whole of the setup.

What tommy captures, and what each plugin and provider stands in for, is in
[`docs/catalogue.md`](catalogue.md); nothing about that changes in a container.
This page is only about running it as one.

## How to test it for real

Every command below has been run against a real container. CI runs them too, by
extracting the blocks marked `# ci: <name>` from this file and executing them —
so the commands a reader copies are literally the commands that are proven on
every pull request. Keep them paste-ready and self-contained.

### Build the image from this repository

Skip this once `can3p/tommy` is published — `docker run` will pull it. From a
checkout, this builds the same `Dockerfile` GoReleaser uses at release time, and
tags it with the published name so every command below runs against it:

```bash
# ci: build
ctx="$(mktemp -d)"
arch="$(go env GOARCH)"
mkdir -p "$ctx/linux/$arch"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$ctx/linux/$arch/tommy" .
cp Dockerfile LICENSE tommy.toml "$ctx/"
docker buildx build --platform "linux/$arch" --load -t can3p/tommy:latest "$ctx"
rm -rf "$ctx"
```

The staged directory is not a quirk of building locally: GoReleaser's
`dockers_v2` hands the Dockerfile a context with each prebuilt binary under a
directory named after its platform, which is why the file says
`COPY $TARGETPLATFORM/tommy`. Mimicking that layout is what lets one Dockerfile
serve both paths, instead of a second one that drifts.

### Run it

```bash
# ci: run
docker run -d --rm --name tommy \
  -p 8811:8811 -p 8822:8822 \
  -p 1025:1025 -p 2121:2121 -p 2222:2222 -p 2049:2049 -p 2575:2575 \
  -p 6969:6969/udp -p 1162:1162/udp \
  -v tommy-data:/data \
  can3p/tommy:latest
```

```
tommy is running
  ui       http://localhost:8811/ui/
  api      http://localhost:8811/api/v1
  ingress  http://localhost:8822
  plugin   mail ([mailjet resend sendgrid smtp])
  plugin   sms ([twilio])
  plugin   files ([ftp sftp tftp nfs])
  plugin   chat ([slack msteams])
  plugin   hl7 ([mllp])
  plugin   snmp ([trap])
  plugin   push ([fcm apns])
  plugin   as2 ([http])
run `tommy providers` for copy-paste examples
```

Every plugin and every provider is on, exactly as `tommy serve` is outside a
container. `docker logs tommy` shows the same lines; `docker run -P` publishes
every exposed port instead of naming them.

### Send it something

```bash
# ci: quickstart
curl -s http://127.0.0.1:8822/v3.1/send \
  -u "any-key:any-secret" -H 'Content-Type: application/json' -d '{
  "Messages":[{"From":{"Email":"a@example.com","Name":"Alice"},"To":[{"Email":"b@example.com"}],
  "Subject":"Hello from tommy","TextPart":"It works."}]}'
```

```
{"Messages":[{"Status":"success","CustomID":"","To":[{"Email":"b@example.com","MessageUUID":"e494ba6b-d5ed-4ecc-bcd5-b147f8bde658","MessageID":100000000000001,"MessageHref":"https://api.mailjet.com/v3/message/100000000000001"}],"Cc":[],"Bcc":[]}]}
```

Then read it back, or open `http://127.0.0.1:8811/ui/`:

```bash
# ci: readback
curl -s "http://127.0.0.1:8811/api/v1/events?plugin=mail"
```

```
[{"id":"01a07385c3a30001f5aebd6a","plugin":"mail","provider":"mailjet","type":"mail.message",
  "received_at":"2026-09-05T21:42:21.347380761Z","summary":{"from":"\"Alice\" <a@example.com>",
  "to":["b@example.com"],"title":"Hello from tommy","snippet":"It works."}, ...}]
```

### Drive a real protocol through a published port

The HTTP ingress is the easy half. This is the half that proves the container
is not quietly listening on its own loopback — SMTP, on its own listener, from
outside the container:

```bash
# ci: smtp
printf 'From: app@example.com\r\nTo: ops@example.com\r\nSubject: Sent over SMTP\r\n\r\nThe published port reached the SMTP listener.\r\n' > /tmp/tommy-mail.txt
curl -s --url smtp://127.0.0.1:1025 \
  --mail-from app@example.com --mail-rcpt ops@example.com \
  --upload-file /tmp/tommy-mail.txt
curl -s "http://127.0.0.1:8811/api/v1/events?plugin=mail&provider=smtp"
```

```
[{"id":"01a0738886ab0001431be100","plugin":"mail","provider":"smtp","type":"mail.message",
  "summary":{"from":"app@example.com","to":["ops@example.com"],"title":"Sent over SMTP",
  "snippet":"The published port reached the SMTP listener."}, ...}]
```

### Fetch the AS2 certificate

AS2 needs a key pair, which tommy mints on first use and keeps on `/data`:

```bash
# ci: as2
curl -s http://127.0.0.1:8822/as2/certificate | openssl x509 -noout -subject -enddate
```

```
subject=O=tommy, CN=tommy AS2
notAfter=Sep  2 20:42:35 2036 GMT
```

The certificate a partner imports survives the container being recreated, as
long as the same `/data` volume is attached — the fingerprint at
`http://127.0.0.1:8811/api/v1/as2/identity` is unchanged after a
`docker rm -f tommy` and a fresh `docker run` with `-v tommy-data:/data`.

```bash
# ci: stop
docker rm -f tommy
```

### Narrow it to one plugin

The image ships the repository's `tommy.toml`, so narrowing is one read-only
mount over that path and no flags to remember:

```bash
# ci: narrow
cat > /tmp/mail-only.toml <<'TOML'
# Only the mail plugin, and only its mailjet provider.
default_enabled = false

[plugins.mail]
enabled = true

[plugins.mail.providers.mailjet]
enabled = true
TOML
docker run -d --rm --name tommy-mail \
  -p 8811:8811 -p 8822:8822 \
  -v /tmp/mail-only.toml:/etc/tommy/tommy.toml:ro \
  can3p/tommy:latest
```

Ask the container what that actually enabled — `providers` reads the same file
without starting anything:

```bash
# ci: narrow-providers
docker run --rm -v /tmp/mail-only.toml:/etc/tommy/tommy.toml:ro \
  can3p/tommy:latest providers --config /etc/tommy/tommy.toml
```

```
Mail (mail)
  Captures the email your application sends instead of delivering it, whether it went out through a vendor's HTTP API or plain SMTP. ...

  mail/mailjet
    Mailjet's transactional Send API v3.1: ...
    POST   /v3.1/send   Accept a Mailjet v3.1 { "Messages": [...] } batch, ...
```

And the disabled providers are really gone — mailjet answers, SendGrid's route
is not mounted at all:

```bash
# ci: narrow-check
curl -s -o /dev/null -w 'mailjet  %{http_code}\n' http://127.0.0.1:8822/v3.1/send \
  -u any:any -H 'Content-Type: application/json' \
  -d '{"Messages":[{"From":{"Email":"a@example.com"},"To":[{"Email":"b@example.com"}],"Subject":"narrowed","TextPart":"hi"}]}'
curl -s -o /dev/null -w 'sendgrid %{http_code}\n' http://127.0.0.1:8822/v3/mail/send \
  -H 'Authorization: Bearer x' -H 'Content-Type: application/json' -d '{}'
docker rm -f tommy-mail
```

```
mailjet  200
sendgrid 404
```

The single-plugin shortcuts work too, and need no file at all — the entrypoint
is the binary, so `docker run … can3p/tommy mail` is `tommy mail`. Remember that
overriding the command loses the image's `--bind 0.0.0.0`, so put it back:

```bash
# ci: shortcut
docker run -d --rm --name tommy-shortcut -p 8811:8811 -p 8822:8822 \
  can3p/tommy:latest mail --bind 0.0.0.0 --enabled-providers mailjet
sleep 2
curl -s "http://127.0.0.1:8811/api/v1/plugins"
docker rm -f tommy-shortcut
```

### The compose stack, and FTP

[`docker-compose.yml`](../docker-compose.yml) publishes every default port,
attaches the `/data` volume and mounts
[`docker/tommy.toml`](../docker/tommy.toml). It is what to paste into a stack of
your own:

```bash
# ci: compose-up
docker compose up -d
```

FTP is the one protocol a container gets wrong by default, so it is the one
worth driving. Upload a file and read it back with `curl`, which speaks FTP:

```bash
# ci: ftp
echo 'invoice 4711' > /tmp/invoice.txt
curl -s -T /tmp/invoice.txt --ftp-create-dirs ftp://tommy:secret@127.0.0.1:2121/orders/
curl -s ftp://tommy:secret@127.0.0.1:2121/orders/invoice.txt
curl -s "http://127.0.0.1:8811/api/v1/events?plugin=files"
```

```
invoice 4711
[{"plugin":"files","provider":"ftp","summary":{"from":"tommy","to":["/orders/invoice.txt"],
  "title":"/orders/invoice.txt","snippet":"uploaded /orders/invoice.txt (13 B)"}, ...}]
```

Any credentials are accepted, as everywhere in tommy.

```bash
# ci: compose-down
docker compose down -v
```

## How it works

### Why `gcr.io/distroless/static:nonroot`

`scratch` is smaller and wrong: it has no CA certificates and no tzdata, both of
which a Go binary that makes outbound TLS calls and stamps events with a
timestamp wants. `alpine`'s only real advantage over distroless is a shell for
`docker exec`, and tommy's whole debugging surface — the UI, the events API, the
SSE stream — is already reachable over the network, so the shell buys little and
brings a package manager and a libc with it. `:nonroot` means the image runs as
uid 65532 with no capability grants, which is possible only because every
default port is ≥ 1024 (a test in `plugins/all` holds that).

### The bind trap

`config.DefaultBind` is `127.0.0.1`. That is right for a binary on a laptop and
useless in a container: a published port never reaches a loopback listener. The
image's default command therefore carries `--bind 0.0.0.0`, and the default
itself is deliberately left alone — a fake that listens on every interface by
default is a worse default for the common case.

Two consequences worth knowing:

- **`--bind` reaches every listener, not only the HTTP ones.** A provider's
  section inherits the top-level bind when it names none of its own, so SMTP,
  FTP, SFTP, TFTP, NFS, MLLP and the SNMP trap receiver all follow the flag.
  (This was not true until the image was built: the flag reached the three core
  listeners and nothing else, and the container published nine ports while
  answering on three.) A section that *does* set its own `bind` still wins, so a
  mounted config with `bind = "127.0.0.1"` under a provider will make that one
  listener unreachable from outside — the one edit to avoid.
- **Overriding `command:` loses it.** `docker run … can3p/tommy mail` replaces
  the whole default command, `--bind 0.0.0.0` included, so pass it again.

### The configuration story

The default command is:

```
serve --bind 0.0.0.0 --config /etc/tommy/tommy.toml --as2-cert-dir /data/as2
```

`--config` names the repository's own `tommy.toml`, shipped in the image. That
file is default-equivalent — `TestRepoConfigIsDefaultEquivalent` in
`core/config` loads it, applies defaults and compares against a bare config, so
the claim in its header is enforced rather than asserted — which is what makes
shipping it a no-op. What it buys is the mount: narrowing tommy is
`-v ./tommy.toml:/etc/tommy/tommy.toml:ro`, with nothing to remember about
flags.

The flags stay on the command line because `cmd/serve.go` loads `--config`
first and applies flags *over* it, and `--bind` additionally clears the three
core listeners' own binds. That is the whole reason the mount is safe: a config
copied from the repository example, `bind = "127.0.0.1"` still in it, cannot
silently make the container unreachable.

**The alternative, rejected:** leave `--config` off the default command and tell
people to add it when they mount. It loses `--bind 0.0.0.0` the moment anyone
overrides the command, which puts the loopback trap back in front of exactly the
users who narrowed their configuration — the ones already doing something
deliberate.

### `/data`, and what lives on it

`/data` is a declared `VOLUME`, owned by uid 65532. It holds what tommy
*generates*, never what it captures:

- the AS2 identity (`--as2-cert-dir /data/as2` in the default command). AS2
  mints a key pair on first use, and without this it would land beside the
  config file — a directory this user cannot write. A partner imports that
  certificate once; the volume is what keeps them from having to do it again
  after every restart.
- the SFTP host key, if you point `host_key_path` at it as
  [`docker/tommy.toml`](../docker/tommy.toml) does. Without that the key is
  regenerated whenever the container is recreated, and an `sftp` client that
  remembers the old one refuses to connect.

Captured events and payload bytes are in memory by design and are not on the
volume: tommy is a catcher, not a store.

### FTP passive mode

FTP is the one protocol here that hands the client an address to dial back, and
a container is exactly where that goes wrong. Two settings have to agree, and
both live in the mounted config
([`docker/tommy.toml`](../docker/tommy.toml)) because they are file-only
settings on `tommy serve`:

- `passive_ports` pins the range the data connection comes from. Unset, the OS
  picks any free port — which is not published, so every transfer hangs.
  `docker-compose.yml` publishes the same range.
- `passive_host` is the address tommy tells the client to dial. `127.0.0.1` is
  right for a client on the docker host itself; a client anywhere else needs the
  host's own reachable address, and it is the one value tommy cannot work out
  for itself.

### The ports

The image `EXPOSE`s the two core listeners (8811 for the UI and API, 8822 for
the ingress) plus every listener provider's default port, `/udp` marked where it
belongs. Rather than a table here that would go stale, ask the binary — it is
the same answer the `Dockerfile` and `docker-compose.yml` are held to by test:

```bash
docker run --rm can3p/tommy:latest providers --json |
  jq -r '.[].providers[] | select(.listener) | "\(.port)/\(.network)  \(.name)"'
```

### Building and publishing

The `Dockerfile` is written for GoReleaser's `dockers_v2` build context, which
stages each prebuilt binary under a directory named after its platform and adds
whatever `extra_files` lists. Two files besides the binaries have to be in that
context: `LICENSE` (MIT requires the notice to travel with every copy) and
`tommy.toml`. The local build above stages a context in the same shape, so there
is one Dockerfile and no second, divergent copy.
