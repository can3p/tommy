# tommy

tommy stands in for the services an application talks to but which are
awkward to run locally — mail providers, SMS gateways, file transfer, chat
webhooks, and more to come — and shows you exactly what your code sent. It
answers the fake vendor APIs the way the real ones do, so official SDKs work
against it unmodified, and it gives you a UI, a JSON API and an SSE stream to
inspect every message as it arrives.

Point your application's mail, SMS, file-transfer or chat client at tommy
instead of Mailjet, SendGrid, Twilio, a real SMTP/FTP/SFTP server or a Slack/
Teams webhook; nothing is ever delivered anywhere, and every message it
captured is one API call or one browser tab away.

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
  plugin   files ([ftp sftp])
  plugin   chat ([slack msteams])
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

You do not have to go looking for it, though. **Every response tommy sends
names what it captured**, so the link is already in your application's log:

```bash
curl -si http://127.0.0.1:8822/v3.1/send -u any:any \
  -H 'Content-Type: application/json' -d '{"Messages":[{"From":{"Email":"a@example.com"},
  "To":[{"Email":"b@example.com"}],"Subject":"Open me","TextPart":"It works."}]}' | grep -i x-tommy
# X-Tommy-Event-Url: http://127.0.0.1:8811/ui/events/01a07138320d00019f31cb1b
```

That URL is the mail on a page of its own — the rendered body, the headers, the
raw request. Every event the API returns carries the same link in its `url`
field, on `/api/v1/events`, on the SSE stream and on each plugin's own
read-back API, so a test that just sent something can print where to look at it.

`tommy providers` prints every enabled plugin and provider, its endpoints and
a ready-to-run snippet for each one, rendered against whatever ports your
configuration actually bound — useful before you've sent anything at all.

## What ships today

| Plugin | Providers | What it fakes |
|---|---|---|
| `mail`  | `mailjet`, `sendgrid`, `smtp` | The vendor HTTP send APIs, plus a real SMTP listener |
| `sms`   | `twilio` | The Programmable Messaging REST API (create, list, fetch) |
| `files` | `ftp`, `sftp`, `tftp`, `nfs` | Real FTP, SFTP, TFTP and NFSv3 servers, backed by one shared virtual filesystem |
| `push`  | `fcm`, `apns` | Firebase Cloud Messaging's HTTP v1 send API and Apple's HTTP/2 provider API, shown as lock-screen cards |
| `hl7`   | `mllp` | A real MLLP listener that parses HL7 v2 and answers with a mechanical ACK |
| `chat`  | `slack`, `msteams` | Slack incoming webhooks + `chat.postMessage`, and both generations of Teams incoming webhook |
| `snmp`  | `trap` | A real UDP trap receiver: v1/v2c traps and informs, every varbind decoded by its wire type |
| `as2`   | `http` | RFC 4130 EDIINT over HTTP: unwraps signed/encrypted/compressed messages and answers with a real MDN receipt |

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

### trap — a real SNMP trap receiver on `:1162`

```bash
snmptrap -v 2c -c public 127.0.0.1:1162 '' 1.3.6.1.6.3.1.1.5.3 \
  1.3.6.1.2.1.1.5.0 s "host01"
```

Accepts v1 traps, v2c traps and v2c informs, decoding every varbind by its
real wire type (integers, OIDs, counters, gauges, timeticks, IP addresses,
octet strings — hex-dumped when not printable text) rather than flattening
everything to one string. An inform gets a `GetResponse` back, echoing its
request id and varbinds; a trap, v1 or v2c, gets none — SNMP defines it as
unconfirmed. Any community string is accepted and recorded, never checked.
There is no bespoke tab: the generic event view's JSON payload panel already
shows every varbind, deliberately — see `plugins/snmp/README.md`.

### http (as2) — `POST /as2`, `GET /as2/certificate`

```bash
curl -s -o tommy.pem http://127.0.0.1:8822/as2/certificate

printf 'ISA*00*          *00*          *ZZ*PARTNER        *ZZ*TOMMY          *260903*1200*U*00401*000000001*0*P*>~SE*1*0001~IEA*1*000000001~' |
curl -s -D - --data-binary @- \
  -H 'AS2-From: PARTNER' -H 'AS2-To: TOMMY' -H 'AS2-Version: 1.1' \
  -H 'Message-ID: <1@partner.example>' \
  -H 'Content-Type: application/edi-x12' \
  -H 'Disposition-Notification-To: as2@partner.example' \
  http://127.0.0.1:8822/as2
```

Signed, encrypted and compressed messages are unwrapped layer by layer and
answered synchronously with a real MDN; anything that cannot be opened is
still captured and reported honestly in the MDN's disposition rather than
refused. See `plugins/as2/README.md` for a full sign-and-encrypt walkthrough
with OpenSSL.

## Configuration: CLI flags or TOML, never two code paths

`tommy serve` runs every plugin and provider compiled into the binary,
filtered by config:

```bash
tommy serve --config tommy.toml
tommy serve --ui-port 8811 --api-port 8811 --ingress-port 8822 --bind 127.0.0.1 --host localhost
tommy serve --h2c=false   # the ingress serves cleartext HTTP/2 alongside HTTP/1.1 by default
```

The repo root ships [`tommy.toml`](./tommy.toml), a complete example with
every section commented and every value equal to the built-in default —
copy it and change the two lines you care about. TOML and CLI flags build the
exact same `config.Config` struct and hand it to the exact same bootstrap
(`core/server`); nothing about the runtime behaves differently depending on
which one you used.

When you only care about one content type, skip the config file entirely:

```bash
tommy mail  --ui-port 8811 --in-port 8822 --enabled-providers mailjet,sendgrid
tommy sms   --ui-port 8811 --in-port 8822 --enabled-providers twilio
tommy files --ui-port 8811 --in-port 8822 --ftp-port 2121 --sftp-port 2222
tommy chat  --ui-port 8811 --in-port 8822 --enabled-providers slack
tommy hl7   --ui-port 8811 --in-port 8822 --mllp-port 2575
tommy push  --ui-port 8811 --in-port 8822 --enabled-providers fcm
tommy snmp  --ui-port 8811 --in-port 8822 --trap-port 1162
tommy as2   --ui-port 8811 --in-port 8822
```

`tommy mail`, `tommy sms`, `tommy files`, `tommy chat`, `tommy hl7`,
`tommy snmp` and `tommy as2` are shortcuts that build a `Config` with every other plugin
switched off in memory, then run through that identical bootstrap — there is
no second, lighter-weight server. `--enabled-providers` narrows which of that
plugin's providers run; leave it off and every provider the plugin ships is
enabled. An unknown provider name is rejected up front, naming the valid
ones:

```
$ tommy mail --enabled-providers bogus
Error: unknown mail provider "bogus": valid providers are mailjet, sendgrid, smtp
```

Every subcommand mirrors `serve`'s flags where they apply: `--ui-port`,
`--api-port`, `--in-port` (the shared ingress port), `--bind`, `--host` and
`--log-level`.

Every provider also gets its own flags for whatever credentials it takes,
named `--<provider>-<option>` so two providers of one plugin never collide.
Pinning a vendor credential — Mailjet's `api_key`, Twilio's `auth_token`,
SMTP's AUTH password — is the same error-path test either way: unset it and
anything is accepted, set it and a mismatch gets that vendor's real error
response:

| Provider | Flags |
|---|---|
| `mailjet` (`tommy mail`)  | `--mailjet-api-key`, `--mailjet-secret-key` |
| `sendgrid` (`tommy mail`) | `--sendgrid-api-key` |
| `smtp` (`tommy mail`)     | `--smtp-port`, `--smtp-username`, `--smtp-password` |
| `twilio` (`tommy sms`)    | `--twilio-account-sid`, `--twilio-auth-token` |
| `tftp` (`tommy files`)    | `--tftp-port` |
| `nfs` (`tommy files`)     | `--nfs-port` |
| `mllp` (`tommy hl7`)      | `--mllp-port` |
| `fcm` (`tommy push`)      | `--fcm-bearer-token` |
| `apns` (`tommy push`)     | `--apns-topic`, `--apns-key-id` |
| `trap` (`tommy snmp`)     | `--trap-port` |
| `ftp` (`tommy files`)     | `--ftp-port`, `--ftp-passive-host`, `--ftp-passive-ports`, `--ftp-username`, `--ftp-password` |
| `sftp` (`tommy files`)    | `--sftp-port`, `--sftp-host-key`, `--sftp-authorized-keys`, `--sftp-username`, `--sftp-password` |
| `http` (`tommy as2`)      | `--as2-cert-file`, `--as2-key-file`, `--as2-partner-cert-file`, `--as2-cert-dir`, `--as2-common-name`, `--as2-in-memory`, `--as2-to`, `--as2-max-body` |

`slack` and `msteams` take no provider-specific flags at all: neither reads
any option beyond `enabled`.

Only the real protocol servers get a `--<provider>-port`, because only they
have a listener of their own: smtp, ftp, sftp, tftp, nfs, mllp and trap.
Every HTTP provider — mailjet, sendgrid, twilio, slack, msteams, fcm, apns and
as2's http — shares the one `--in-port` / `[ingress]` listener and is told
apart by path. There is no per-provider listener for an HTTP provider (that
would be real core work, not this shortcut), so none of them gets a
`--<provider>-port` flag.

An unset flag never overrides a provider's own default, and setting one for a
provider `--enabled-providers` excludes is a clear error rather than a flag
that silently does nothing:

```
$ tommy files --enabled-providers ftp --sftp-port 2200
Error: files: flags were given for provider "sftp", but --enabled-providers only enables ftp
```

Everything else settable in a provider's `tommy.toml` section — smtp's,
ftp's and sftp's own `bind`, and tuning knobs like timeouts, message/recipient
limits and connection caps — is deliberately config-file-only; see the
comments next to each key in [`tommy.toml`](./tommy.toml) for why.

Every subcommand rejects a stray positional argument rather than silently
starting a server with it ignored — `tommy mail serve` and `tommy files oops`
are both errors, not a running server you have to notice and kill by hand.

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

Every event in those responses carries a `url` naming its own page,
`GET /ui/events/{id}` — one event, rendered by the plugin that captured it.

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
