<!-- short_description: A fake for the services your app talks to - mail, SMS, files, chat - shows what you sent -->
<!--
  This file IS the Docker Hub page: the release workflow pushes it verbatim as
  the repository's full description, and the line above as its short one. Docker
  Hub caps them at 25,000 and 100 bytes and truncates silently past either, so
  a test in this repository holds both. Keep the short description on one line,
  in an HTML comment, so it has one source and never renders twice.
-->

# tommy

tommy stands in for the services an application talks to but which are awkward
to run locally — mail providers, SMS gateways, file transfer, chat webhooks,
HL7, SNMP, push and AS2 — and shows you exactly what your code sent. It answers
the fake vendor APIs the way the real ones do, so official SDKs work against it
unmodified, and it gives you a UI, a JSON API and an SSE stream to inspect
every message as it arrives. Nothing is ever delivered anywhere.

*This page is deliberately thin.* Docker Hub cannot render the project's
documentation, so what follows is the smallest thing that gets you running,
and every other question is a link. Keeping a second copy of the real
documentation here would only give it somewhere to drift.

- **Full documentation:** https://can3p.github.io/tommy/
- **Running the image:** https://github.com/can3p/tommy/blob/main/docs/docker.md
- **Source:** https://github.com/can3p/tommy

## Run it

```bash
docker run -d --rm --name tommy \
  -p 8811:8811 -p 8822:8822 \
  -p 1025:1025 -p 2121:2121 -p 2222:2222 -p 2049:2049 -p 2575:2575 \
  -p 6969:6969/udp -p 1162:1162/udp \
  -v tommy-data:/data \
  can3p/tommy:latest
```

Send it something:

```bash
curl -s http://127.0.0.1:8822/v3.1/send \
  -u "any-key:any-secret" -H 'Content-Type: application/json' -d '{
  "Messages":[{"From":{"Email":"a@example.com","Name":"Alice"},"To":[{"Email":"b@example.com"}],
  "Subject":"Hello from tommy","TextPart":"It works."}]}'
```

That is Mailjet's real Send API, answered with Mailjet's real success envelope.
Read it back:

```bash
curl -s "http://127.0.0.1:8811/api/v1/events?plugin=mail"
```

Or open `http://127.0.0.1:8811/ui/` and look at it. Every response tommy sends
also carries an `X-Tommy-Event-URL` header naming the page of what it just
captured, so the link is already in your application's log.

## Ports

| Port | What |
|---|---|
| 8811/tcp | UI and JSON API |
| 8822/tcp | ingress — every fake vendor HTTP API |
| 1025/tcp | SMTP |
| 2121/tcp | FTP |
| 2222/tcp | SFTP |
| 2049/tcp | NFS |
| 2575/tcp | HL7 MLLP |
| 6969/udp | TFTP |
| 1162/udp | SNMP traps |

FTP needs more than a published port: its passive mode tells the client where
to dial back, which a container cannot guess. The compose file in the
repository has that wired up.

## Narrow it down

By default the image runs every plugin. Mount a configuration over
`/etc/tommy/tommy.toml` to run less than that — no flags to remember:

```bash
cat > mail-only.toml <<'TOML'
default_enabled = false

[plugins.mail]
enabled = true

[plugins.mail.providers.mailjet]
enabled = true
TOML

docker run -d --rm --name tommy \
  -p 8811:8811 -p 8822:8822 \
  -v ./mail-only.toml:/etc/tommy/tommy.toml:ro \
  can3p/tommy:latest
```

The entrypoint is the binary, so every subcommand still works —
`docker run --rm can3p/tommy providers` prints every plugin, its endpoints and
a runnable example for each.

## Tags

`latest` follows the newest release. Every release also has its exact version
(`0.1.0`), plus moving major and major.minor tags (`0`, `0.1`). Prereleases get
their exact version only, and never move `latest`.

Also published as `ghcr.io/can3p/tommy`.

MIT licensed.
