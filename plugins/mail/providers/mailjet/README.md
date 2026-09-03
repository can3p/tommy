# mailjet

## What it is

A stand-in for [Mailjet's transactional Send API
v3.1](https://dev.mailjet.com/email/guides/send-api-v31/). It mounts
`POST /v3.1/send` on the shared ingress, accepts the real `{"Messages":[...]}`
batch shape — attachments, inlined images, custom headers, sandbox mode — and
answers with Mailjet's real success and error envelopes, so the official
`mailjet-apiv3-go` SDK (or any client built against Mailjet's documented API)
works against it unmodified.

## What it's for

Reach for this provider when your application already calls Mailjet's SDK or
posts to Mailjet's v3.1 endpoint directly, and you want to see what it's
actually sending without a real Mailjet account, real API keys, or a risk of a
test run landing in someone's real inbox. Concretely:

- Your signup flow calls Mailjet to send a welcome email, and you want a CI
  test that asserts the right recipient and subject went out, without hitting
  Mailjet's real API or paying for a sandbox account.
- You're debugging why a batch of `Messages[]` produced the wrong number of
  sends — point the SDK at tommy and read back exactly how many events landed
  and what each one contained.
- You want to confirm an attachment (an invoice, a report) is being built and
  encoded correctly before it ever reaches a real Mailjet key.

If your code is written against Mailjet's SDK or wire format specifically, use
this provider rather than `sendgrid` or `smtp` — the request and response
shapes are Mailjet's own, verified against the live docs, so nothing about
your integration code has to change to point at tommy instead of Mailjet.

## How to test it for real

Boot the mail plugin with mailjet enabled (it's on by default) on ports that
won't collide with anything else running:

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . mail --ui-port 18901 --in-port 18902 --smtp-port 11025
```

Send a message the way the real API expects — Basic auth with any
credentials, a `Messages[]` batch:

```bash
curl -s http://localhost:18902/v3.1/send \
  -u "any-key:any-secret" -H 'Content-Type: application/json' -d '{
  "Messages":[{"From":{"Email":"a@example.com","Name":"Alice"},"To":[{"Email":"b@example.com"}],
  "Subject":"Hello from tommy","TextPart":"It works."}]}'
```

This returns Mailjet's real per-recipient success envelope:

```json
{"Messages":[{"Status":"success","CustomID":"","To":[{"Email":"b@example.com","MessageUUID":"...","MessageID":100000000000001,"MessageHref":"https://api.mailjet.com/v3/message/100000000000001"}],"Cc":[],"Bcc":[]}]}
```

Send one with an attachment:

```bash
curl -s http://localhost:18902/v3.1/send \
  -u "any-key:any-secret" -H 'Content-Type: application/json' -d '{
  "Messages":[{"From":{"Email":"a@example.com"},"To":[{"Email":"b@example.com"}],
  "Subject":"Receipt","TextPart":"See attached.",
  "Attachments":[{"ContentType":"text/plain","Filename":"note.txt","Base64Content":"aGVsbG8="}]}]}'
```

Read both back — the tab, or the API filtered to this provider:

```bash
open http://localhost:18901/ui/mail/
curl -s "http://localhost:18901/api/v1/mail/messages?provider=mailjet" | jq '[.[].message.subject]'
# -> ["Receipt", "Hello from tommy"]
```

For the real SDK rather than curl, `test/integration/mailjet_test.go` — a
separate Go module so the vendor SDK never enters tommy's own `go.mod` — points
the actual `mailjet-apiv3-go/v4` client at a live tommy and checks both that the
SDK parses tommy's response and that tommy captured what was sent, including
the fan-out and attachment cases above. It is run with
`cd test/integration && go test -tags integration -run TestMailjet ./...`.
That command was verified, but only after fixing a break it exposed: adding the
`as2` plugin's S/MIME dependency to the root module left `test/integration`'s
own `go.sum` stale, and since it is a **separate module** that `./...` never
reaches, `make check` compiled none of it. Every test in the module had stopped
building. Re-tidying that module fixed it; the lesson is that adding a
dependency to the root module is a two-module change.

One sharp edge documented there and in `docs/clients.md`: `SendMailV31` builds
its URL as `apiBase + ".1/send"`, so the base URL you construct the SDK client
with must already end in `/v3` for the arithmetic to land on `/v3.1/send`.

Every `curl` command above was executed against a running tommy while writing
this document.

## Endpoints

Mounts `POST /v3.1/send` on the shared ingress. Accepts the real
`{"Messages":[...]}` batch shape, including `Attachments` /
`InlinedAttachments` (`Base64Content` decoded into the blob store),
`ReplyTo`, `Headers`, `CustomID`, `EventPayload`, `CustomCampaign` and
`SandboxMode`. Basic-auth credentials are accepted by default and recorded on
the event; a provider config with `api_key` (and optionally `secret_key`) set
pins an expected pair and rejects a mismatch with Mailjet's real 401 shape.

Every entry of `Messages[]` is one delivered message: a request that fans out
to three logical messages appends three events, each one recipient's
`MessageID`/`MessageUUID` minted per-address, matching the real API's own
multi-recipient example.

## Notes on the wire shapes

Verified against the live docs while implementing this provider:

- `ReplyTo` is a **single** `{"Email","Name"}` object, unlike `To`/`Cc`/`Bcc`.
- A per-message validation failure (e.g. missing `TextPart`/`HTMLPart`/`TemplateID`)
  rides inside a **200** response as `{"Status":"error","Errors":[...]}` at that
  message's position in `Messages[]`; only a malformed request as a whole (bad
  JSON, empty `Messages[]`, bad auth) is a top-level 4xx with the flat
  `{"ErrorIdentifier","ErrorCode","StatusCode","ErrorMessage"}` shape.
- In `SandboxMode`, the response omits `MessageID`/`MessageUUID` (`0`/`""`).
- `blob.ErrCapacityExceeded` is reported as a per-message error too (so a
  batch can't be aborted by one oversized attachment), but with a tommy-only
  `ErrorCode` - Mailjet itself has no such limit.
