# `resend` provider

## What it is

A stand-in for the [Resend Email API](https://resend.com/docs/api-reference/emails/send-email).
It mounts the three routes an application actually sends mail through, at the
paths the real `api.resend.com` uses:

| | |
|---|---|
| `POST /emails` | send one email; answers `200` with `{"id": "<uuid>"}` |
| `POST /emails/batch` | send up to 100; answers `200` with `{"data":[{"id":…}]}`, index-aligned |
| `GET /emails/{id}` | read one back, served from tommy's event store |

Each message becomes one event: the addresses, subject, HTML and text bodies,
custom headers and attachments as a canonical message you can read in the UI,
and everything Resend-specific — tags, `topic_id`, `template`, `scheduled_at`,
the `Idempotency-Key`, the credential that was presented — recorded as event
metadata. Attachment bytes go to the blob store, never inline in the event.

## What it's for

Reach for this when your application sends mail through Resend — the
`resend-go` or `resend` (Node) SDK, or a plain `POST` to `api.resend.com` — and
you want to see what it sent without a Resend account, a verified domain, or
any chance of a message reaching a real inbox:

- Your signup flow sends a welcome email through Resend. Point
  `RESEND_BASE_URL` at tommy in your test environment and read the rendered
  HTML in the UI, instead of hunting for it in a real Resend activity log.
- You are asserting in CI that a `POST /emails/batch` with three entries
  really produces three distinct sends with the right recipients — tommy
  records one event per message, so a count is a count.
- You want the read-back path exercised: your code sends and then calls
  `emails.get(id)` to check the message went out. tommy serves that fetch from
  the store, so the SDK sees its own write, with the same `object`/`id`/
  `last_event` fields the real response carries.
- Your Resend domain is not verified yet (or the account is on the free
  `resend.dev` sandbox, which only lets you mail yourself) and you cannot run
  the flow end to end against production at all.

Pick this over `sendgrid` or `mailjet` when your code is written against
Resend's wire format specifically — the union-typed recipient fields, the
`{"id": …}` response and the UUID ids are Resend's own. If your code speaks
plain SMTP rather than a vendor HTTP API, use the `smtp` provider; nothing here
accepts an SMTP connection.

## How to test it for real

Boot the mail plugin with only this provider, on ports that will not collide
with anything else you are running:

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . mail --enabled-providers resend --ui-port 18901 --in-port 18902
```

Send an email the way the real API expects it. Any bearer token is accepted
unless one is pinned (see **Auth** below):

```bash
curl -si http://localhost:18902/emails \
  -H 'Authorization: Bearer re_fake_key' \
  -H 'Content-Type: application/json' -d '{
  "from": "Acme <alice@example.com>",
  "to": ["bob@example.com"],
  "subject": "Hello from tommy",
  "html": "<p>It <b>works</b>.</p>",
  "text": "It works."
}'
```

which answers exactly what Resend answers — `200`, and a body of nothing but
the id:

```
HTTP/1.1 200 OK
Content-Type: application/json
X-Tommy-Event-Url: http://127.0.0.1:18901/ui/events/01a072750393000101b37f29

{"id":"01a07275-0393-4000-a1b3-7f29b9facade"}
```

(`X-Tommy-Event-Url` is tommy's own, not Resend's — it links to the captured
message, and SDKs ignore headers they do not know.)

Read it back by that id. This is served out of the event store, so an SDK that
sends and then fetches sees its own write:

```bash
id=$(curl -s http://localhost:18902/emails \
  -H 'Authorization: Bearer re_fake_key' -H 'Content-Type: application/json' \
  -d '{"from":"Acme <alice@example.com>","to":"bob@example.com","cc":["carol@example.com"],"reply_to":"support@example.com","subject":"Round trip","text":"hi","tags":[{"name":"category","value":"confirm_email"}]}' \
  | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
curl -s http://localhost:18902/emails/$id
```

```json
{"object":"email","id":"01a07275-039e-4000-a230-987342facade","message_id":"<01a07275-039e-4000-a230-987342facade@example.com>","to":["bob@example.com"],"from":"Acme <alice@example.com>","created_at":"2026-09-05 16:44:26.398835+00","subject":"Round trip","html":null,"text":"hi","bcc":[],"cc":["carol@example.com"],"reply_to":["support@example.com"],"last_event":"delivered","scheduled_at":null,"tags":[{"name":"category","value":"confirm_email"}]}
```

Note that this one request used **both** spellings of the recipient union —
`"to"` as a bare string, `"cc"` as an array — and that they come back
normalised to arrays, which is what the real API does too.

Send a batch. Each entry is one delivered message, so this appends two events
and returns two ids in request order:

```bash
curl -si http://localhost:18902/emails/batch \
  -H 'Authorization: Bearer re_fake_key' \
  -H 'Idempotency-Key: order-9001' \
  -H 'Content-Type: application/json' -d '[
  {"from": "Acme <alice@example.com>", "to": "bob@example.com",   "subject": "First",  "text": "one"},
  {"from": "Acme <alice@example.com>", "to": ["carol@example.com"], "subject": "Second", "text": "two"}
]'
# {"data":[{"id":"01a07273-6b19-4000-a59a-2a2852facade"},{"id":"01a07273-6b19-4000-a6c5-566392facade"}]}
```

Attach something — base64, exactly as the REST reference documents:

```bash
curl -s http://localhost:18902/emails -H 'Content-Type: application/json' -d '{
  "from": "alice@example.com",
  "to": "bob@example.com",
  "subject": "With an attachment",
  "text": "see attached",
  "attachments": [{"content": "SGVsbG8sIHRvbW15Lg==", "filename": "hello.txt", "content_type": "text/plain"}]
}'
```

Errors come back in Resend's own `{name, message, statusCode}` shape, which is
what both official SDKs decode:

```bash
curl -s http://localhost:18902/emails -H 'Content-Type: application/json' -d '{"to":"bob@example.com","subject":"x"}'
# {"name":"missing_required_field","message":"Missing `from` field.","statusCode":422}

curl -s http://localhost:18902/emails/49a3999c-0ce1-4ea6-ab68-afcd6dc2e794
# {"name":"not_found","message":"Email not found","statusCode":404}
```

Then look at what was captured, in the tab or through the mail plugin's API:

```bash
open http://localhost:18901/ui/mail/
curl -s "http://localhost:18901/api/v1/mail/messages?provider=resend" \
  | python3 -c 'import json,sys; print([m["message"]["subject"] for m in json.load(sys.stdin)])'
# ['With an attachment', 'Round trip', 'Hello from tommy']
```

Every command in this section was executed against a running tommy while
writing this document, and the outputs above are what came back — including
the `go run . mail …` line that started it.

For the official Go SDK instead of `curl`, `resend-go` reads its base URL from
`RESEND_BASE_URL` (`resend.go`, `getEnv("RESEND_BASE_URL", "https://api.resend.com/")`)
and resolves every path against it relatively, so **the trailing slash
matters**:

```bash
RESEND_BASE_URL=http://localhost:18902/ your-app
```

That line is read from the SDK's source rather than executed here — the SDK
lives in the separate `test/integration` module, so that it never enters
tommy's own `go.mod`, and that is where it is driven against a running tommy.

## Auth

Any `Authorization` header is accepted by default — a fake that rejects
credentials is useless — and whatever was presented is recorded on
`Event.Meta.authorization`. Pinning a key turns it into a real check with
Resend's own 401s:

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . mail --enabled-providers resend \
  --ui-port 18901 --in-port 18902 --resend-api-key re_expected
```

```
# no header at all
{"name":"missing_api_key","message":"Missing API key in the authorization header.","statusCode":401}
# a key that does not match
{"name":"validation_error","message":"API key is invalid","statusCode":401}
```

The same setting from a config file:

```toml
[plugins.mail.providers.resend]
api_key = "re_your_fake_key"
last_event = "delivered"
```

## The recipient union

`to`, `cc`, `bcc` and `reply_to` are each **a string or an array of strings**,
and both spellings turn up in the same request from the same client:
`resend-go` marshals `to`/`cc`/`bcc` as arrays but `reply_to` as a bare string.
All four accept either form here. Each element is an RFC 5322 address, parsed
with the mail plugin's own parser, so `Name <email@example.com>` keeps its
display name; an address that does not parse gets Resend's real
`invalid_parameter` 422 rather than being silently swallowed.

On the way back out, the retrieve response renders them the way Resend does:
bare addresses when there is no display name, `Name <addr>` when there is —
without the quotes Go's `net/mail` formatter would add and without RFC 2047
encoding a non-ASCII name, neither of which the vendor puts in a JSON body.

## Attachments

`content` has three spellings in the wild and all three are accepted:

- **base64 string** — what the REST reference documents, what `curl` and the
  Node SDK send;
- **a JSON array of byte values** — what `resend-go` sends. Its
  `Attachment.MarshalJSON` runs the bytes through `BytesToIntArray` "in the way
  Resend supports", so an implementation that only handles base64 silently
  fails against the official Go SDK;
- **`{"type":"Buffer","data":[…]}`** — what a Node `Buffer` serializes to if it
  reaches `JSON.stringify` unconverted.

Whichever arrives, the bytes go through `Message.AttachBytes` into the blob
store, never inline in the event. `content_id` (or `resend-go`'s deprecated
`inline_content_id`) marks the part inline, which is how an HTML body's `cid:`
reference resolves.

An attachment with neither `content` nor `path` gets Resend's documented
`invalid_attachment` 422.

## What this deliberately does not do

Each of these would need state or an outbound request, which is the line
`docs/implementation-plan.md` §2 draws around tommy.

- **`path` attachments are never fetched.** Resend fetches the URL you give it;
  tommy makes no outbound requests at all, so the URL is recorded under
  `Event.Meta.remote_attachments` and no blob is stored. A test that needs the
  bytes captured should send them as `content`.
- **`Idempotency-Key` never deduplicates.** It is recorded on every event and
  acted on by none. Deduplicating means remembering which keys were seen and
  for 24 hours — that is state, and state is scenario machinery. The only rule
  enforced is the documented length limit, which answers
  `invalid_idempotency_key`.
- **`scheduled_at` does not schedule.** The message is captured immediately and
  the requested time is recorded and echoed back on retrieve. Nothing waits,
  and there is no `PATCH /emails/{id}` or `POST /emails/{id}/cancel` to change
  or cancel a send that was never pending.
- **`template` renders nothing.** The template id and its variables are
  recorded; the body is whatever was in the payload. Rendering a published
  template would mean storing templates, which is a resource tommy does not
  keep. The one template rule that *is* enforced is the documented conflict:
  `template` together with `html` or `text` is a validation error.
- **`x-batch-validation: permissive` is not implemented.** The header is
  recorded, and a batch is always validated strictly: one bad entry and none of
  it is stored, which is the API's own default. Permissive mode reports
  per-entry failures in an `errors[]` array, and the failures it would report
  are the ones tommy does not have — an unverified domain, a suppressed
  address, a quota. Inventing them would be inventing behaviour, so the
  `errors` field the SDKs decode is simply never present.
- **`last_event` is a fixed answer, not a lifecycle.** Resend walks an email
  through `sent` → `delivered` → `opened`; tommy delivers nothing, so it
  reports `delivered` and stays there, which is what lets a client that polls
  for delivery proceed instead of spinning. Set `--resend-last-event bounced`
  (or the `last_event` config key) when a test wants to read some other state
  back.
- **No inbound webhooks**, for the reason every provider gives: they need
  outbound HTTP and a way to define scenarios.

## The email id

Resend addresses an email by a UUID; tommy's event ids are 24 lowercase hex
characters. `GET /emails/{id}` bridges the two with a **reversible encoding**,
not a second index — the same approach the `twilio` provider uses to get from a
`Sid` back to an event id. The 24 hex characters are laid into the free
positions of a v4 UUID, with the six spare ones carrying a fixed marker:

```
eeeeeeee-eeee-4eee-aeee-eeeeeefacade
         ^ event id            ^ marker
```

So every id this provider mints is a syntactically valid v4 UUID (the .NET SDK
parses one into a `Guid`) and ends in `facade`. An id it never minted does not
decode, which is what separates "that email is not here" (`404 not_found`) from
"that is not an email id at all" (`422 invalid_parameter`).

## Where the wire details came from

Every response shape here was checked against live documentation, per
`CLAUDE.md` rule 2:

- <https://resend.com/docs/api-reference/emails/send-email>
- <https://resend.com/docs/api-reference/emails/send-batch-emails>
- <https://resend.com/docs/api-reference/emails/retrieve-email>
- <https://resend.com/docs/api-reference/errors>

The reference does not spell out the JSON body of an error, only the codes; the
`{name, message, statusCode}` shape, and the exact wording of the
missing-field message, the `invalid_parameter` address message and
`Email not found`, come from the official Node SDK's own response fixtures
(`resend/resend-node`, `src/emails/emails.spec.ts`), with `statusCode`
confirmed by `resend/resend-node#286`. The attachment-as-integer-array
encoding and the exact field spellings a Go client puts on the wire come from
`resend/resend-go`.

Four things had no primary source and are composed rather than quoted; each is
marked `UNVERIFIED` at its definition in the code: the message for a *wrong*
(as opposed to missing) API key, the batch size-limit error, the
`template`-with-`html` conflict message, and the oversized-body 413 (Resend
documents no code for one).

## Package tests

`go test ./plugins/mail/providers/resend/...` covers the response contract,
both spellings of the recipient union, all three attachment content encodings,
the `path` refusal, batch fan-out and all-or-nothing validation, every error
body byte for byte, auth capture and pinning, the retrieve response, and the id
encoding's round trip — including a full-stack pass through `testutil.Start`
with real event ids, since the unit tests pin ids to a counter.
