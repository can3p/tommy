# tommy

tommy stands in for the services an application talks to but which are
awkward to run locally — mail providers, SMS gateways, and more to come — and
shows you exactly what your code sent. It answers the fake vendor APIs the
way the real ones do, so official SDKs work against it unmodified, and it
gives you a UI, a JSON API and an SSE stream to inspect every message as it
arrives.

Point your application's mail or SMS client at tommy instead of Mailjet,
SendGrid, Twilio or a real SMTP relay; nothing is ever delivered anywhere,
and every message it captured is one API call or one browser tab away.

## 30-second quickstart

```bash
tommy serve
```

```
tommy is running
  ui       http://127.0.0.1:8811/ui/
  api      http://127.0.0.1:8811/api/v1
  ingress  http://127.0.0.1:8822
  plugin   mail ([mailjet sendgrid smtp])
  plugin   sms ([twilio])
run `tommy providers` for copy-paste examples
```

In another terminal, send it something:

```bash
curl -s http://127.0.0.1:8822/v3.1/send \
  -u "any-key:any-secret" -H 'Content-Type: application/json' -d '{
  "Messages":[{"From":{"Email":"a@example.com","Name":"Alice"},"To":[{"Email":"b@example.com"}],
  "Subject":"Hello from tommy","TextPart":"It works."}]}'
```

Then open `http://127.0.0.1:8811/ui/` to see it in the inbox, or:

```bash
curl -s "http://127.0.0.1:8811/api/v1/events?plugin=mail"
```

`tommy providers` prints every enabled plugin and provider, its endpoints and
a ready-to-run snippet for each one, rendered against whatever ports your
configuration actually bound — useful before you've sent anything at all.

## What ships today

| Plugin | Providers | What it fakes |
|---|---|---|
| `mail` | `mailjet`, `sendgrid`, `smtp` | The vendor HTTP send APIs, plus a real SMTP listener |
| `sms`  | `twilio` | The Programmable Messaging REST API (create, list, fetch) |

Every plugin and provider describes itself: `Description()`, the endpoints it
mounts, and at least one runnable snippet, surfaced identically in the UI's
"How to test" panel, `GET /api/v1/plugins` and `tommy providers`. The
snippets below are exactly those, and every one has been run against a live
`tommy serve` while writing this document.

### mailjet — `POST /v3.1/send`

```bash
curl -s http://127.0.0.1:8822/v3.1/send \
  -u "any-key:any-secret" -H 'Content-Type: application/json' -d '{
  "Messages":[{"From":{"Email":"a@example.com","Name":"Alice"},"To":[{"Email":"b@example.com"}],
  "Subject":"Hello from tommy","TextPart":"It works."}]}'
```

Any Basic-auth credentials are accepted and recorded by default; pin
`api_key`/`secret_key` in `[plugins.mail.providers.mailjet]` to make a
mismatch return Mailjet's real 401.

### sendgrid — `POST /v3/mail/send`

```bash
curl -si http://127.0.0.1:8822/v3/mail/send \
  -H 'Authorization: Bearer SG.fake-key' \
  -H 'Content-Type: application/json' -d '{
  "personalizations": [{"to": [{"email": "bob@example.com", "name": "Bob"}], "subject": "Hello from tommy"}],
  "from": {"email": "alice@example.com", "name": "Alice"},
  "content": [
    {"type": "text/plain", "value": "It works."},
    {"type": "text/html", "value": "<p>It <b>works</b>.</p>"}
  ]
}'
```

Answers `202` with an empty body and an `X-Message-Id` header, exactly like
the real API.

### smtp — a real SMTP listener on `:1025`

```bash
curl -s smtp://127.0.0.1:1025 \
  --mail-from alice@example.com --mail-rcpt bob@example.com -T - <<'MSGEOF'
From: Alice <alice@example.com>
To: Bob <bob@example.com>
Subject: Hello from tommy

It works.
MSGEOF
```

No AUTH is required; if a client offers one it is recorded, never checked,
unless `username`/`password` are pinned in config.

### twilio — `POST /2010-04-01/Accounts/{sid}/Messages.json`

```bash
curl -s -u ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx:authtokenxxxxxxxxxxxxxxxxxxxxxxxx \
  http://127.0.0.1:8822/2010-04-01/Accounts/ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx/Messages.json \
  --data-urlencode 'To=+15558675310' \
  --data-urlencode 'From=+15557122661' \
  --data-urlencode 'Body=It works.'
```

Returns the full Twilio message resource (`sid`, `status: "queued"`,
`num_segments`, ...), with matching `GET` list and fetch endpoints served
from the same store — an SDK that creates and then lists a message sees its
own write.

Pointing an official SDK at tommy is its own topic: see `clienthelp/` and
`docs/clients.md` for the per-SDK story, including Twilio's, whose client
libraries need a custom `*http.Client` rather than a base-URL override.

## Configuration: CLI flags or TOML, never two code paths

`tommy serve` runs every plugin and provider compiled into the binary,
filtered by config:

```bash
tommy serve --config tommy.toml
tommy serve --ui-port 8811 --api-port 8811 --ingress-port 8822 --bind 127.0.0.1 --host localhost
```

The repo root ships [`tommy.toml`](./tommy.toml), a complete example with
every section commented and every value equal to the built-in default —
copy it and change the two lines you care about. TOML and CLI flags build the
exact same `config.Config` struct and hand it to the exact same bootstrap
(`core/server`); nothing about the runtime behaves differently depending on
which one you used.

When you only care about one content type, skip the config file entirely:

```bash
tommy mail --ui-port 8811 --in-port 8822 --enabled-providers mailjet,sendgrid
tommy sms  --ui-port 8811 --in-port 8822 --enabled-providers twilio
```

`tommy mail` and `tommy sms` are shortcuts that build a `Config` with every
other plugin switched off in memory, then run through that identical
bootstrap — there is no second, lighter-weight server. `--enabled-providers`
narrows which of that plugin's providers run; leave it off and every provider
the plugin ships is enabled. An unknown provider name is rejected up front,
naming the valid ones:

```
$ tommy mail --enabled-providers bogus
Error: unknown mail provider "bogus": valid providers are mailjet, sendgrid, smtp
```

Both subcommands mirror `serve`'s flags where they apply: `--ui-port`,
`--api-port`, `--in-port` (the shared ingress port), `--bind`, `--host` and
`--log-level`.

## API surface

Everything lives under `/api/v1`, shared by every plugin:

| Route | Notes |
|---|---|
| `GET /health` | status, uptime, enabled plugins, event count, version |
| `GET /plugins` | descriptions, endpoints and snippets, rendered against the live ports |
| `GET /events` | `?plugin=&provider=&type=&search=&since=&limit=&offset=` |
| `GET /events/{id}` | the full event, including the raw request |
| `GET /events/stream` | Server-Sent Events, same filters — what the UI itself consumes |
| `DELETE /events` | `?plugin=` to scope the clear, otherwise clears everything |
| `DELETE /events/{id}` | remove one event |
| `GET /blobs/{id}` | stream an attachment or uploaded file, with range support |
| `GET /mail/messages`, `/mail/messages/{id}`, `.../html`, `.../text`, `.../raw`, `.../attachments/{idx}` | mail read-back |
| `GET /sms/messages`, `/sms/messages/{id}`, `.../media/{idx}` | sms read-back |

`tommy providers [plugin[/provider]] [--json]` prints the same information
`GET /api/v1/plugins` returns, for scripting or a quick look before you've
sent anything.

## Learn more

- [`docs/plan.md`](./docs/plan.md) — the original design brief.
- [`docs/implementation-plan.md`](./docs/implementation-plan.md) — the
  interfaces and the wave-by-wave build plan.
- [`docs/contracts.md`](./docs/contracts.md) — the contracts as actually
  implemented, including the small deviations from the plan.
- Every plugin and provider directory carries its own `README.md` with the
  same description and snippets `tommy providers` prints.

## Installation

### Install Script

Download `tommy` and install into a local bin directory.

#### MacOS, Linux, WSL

Latest version:

```bash
curl -L https://raw.githubusercontent.com/can3p/tommy/main/generated/install.sh | sh
```

Specific version:

```bash
curl -L https://raw.githubusercontent.com/can3p/tommy/main/generated/install.sh | sh -s 0.0.4
```

The script will install the binary into `$HOME/bin` folder by default, you can override this by setting
`$CUSTOM_INSTALL` environment variable

### Manual download

Get the archive that fits your system from the [Releases](https://github.com/can3p/tommy/releases) page and
extract the binary into a folder that is mentioned in your `$PATH` variable.

## Notes

The project has been scaffolded with the help of [kleiner](https://github.com/can3p/kleiner)
