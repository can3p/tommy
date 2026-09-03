# `sendgrid` provider

## What it is

A stand-in for the [Twilio SendGrid v3 Mail Send
API](https://www.twilio.com/docs/sendgrid/api-reference/mail-send/mail-send).
It mounts `POST /v3/mail/send` on the shared ingress at the same path the real
API uses, accepts the real request shape — `personalizations[]`, `content[]`,
`attachments[]`, categories, custom args — and answers with SendGrid's actual
`202 Accepted` / empty-body / `X-Message-Id` contract, so the official
`sendgrid-go` SDK works against it unmodified.

## What it's for

Reach for this when your application sends mail through SendGrid's SDK or its
v3 Mail Send endpoint and you want to see the message without a real SendGrid
account or API key, and without any chance of it reaching a real inbox:

- Your order-confirmation email goes out through SendGrid — send it in a test
  run and read the rendered HTML back from tommy's UI instead of digging
  through a real SendGrid activity log.
- You're asserting in CI that a request with several `personalizations[]`
  produces the right number of distinct sends, each with the right recipient
  and subject override.
- You want to confirm your code actually sets the `202`/empty-body contract
  correctly on the client side — most hand-rolled fakes return a JSON body
  SendGrid never sends, which hides bugs in code that (wrongly) tries to parse
  a response.

Pick this provider over `mailjet` when your code is written against SendGrid's
SDK or wire format specifically — the request and response shapes here are
SendGrid's own, verified against the live docs. If your code speaks plain SMTP
instead of calling a vendor API, use the `smtp` provider — nothing here accepts
an SMTP connection.

## How to test it for real

Boot the mail plugin with sendgrid enabled (it's on by default) on ports that
won't collide with anything else running:

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . mail --ui-port 18901 --in-port 18902 --smtp-port 11025
```

Send a message the way the real API expects — a bearer token (any value is
accepted unless `api_key` is configured), `personalizations[]`, `content[]`:

```bash
curl -si http://localhost:18902/v3/mail/send \
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

This returns exactly what SendGrid returns on success — no JSON body:

```
HTTP/1.1 202 Accepted
X-Message-Id: 01a06816929c000408d3a6b0.filter-001.pop-sendgrid
Content-Length: 0
```

Read it back — the tab, or the API filtered to this provider:

```bash
open http://localhost:18901/ui/mail/
curl -s "http://localhost:18901/api/v1/mail/messages?provider=sendgrid" | jq '.[0].message.subject'
# -> "Hello from tommy"
```

For the real SDK rather than curl, `test/integration/sendgrid_test.go` — in the
separate `test/integration` Go module, kept apart so the vendor SDK never
enters tommy's own `go.mod` — builds the payload with the SDK's own helpers
(`mail.NewV3MailInit`, `Personalization`, `Attachment`) and sends it with
`sendgrid.GetRequest` + `sendgrid.API`, which is the documented way around
`NewSendClient`'s hardcoded host (see `docs/clients.md`). It is run with
`cd test/integration && go test -tags integration -run TestSendGridSDK ./...`.
That command was verified, but only after fixing a break it exposed: adding the
`as2` plugin's S/MIME dependency to the root module left `test/integration`'s
own `go.sum` stale, and since it is a **separate module** that `./...` never
reaches, `make check` compiled none of it. Every test in the module had stopped
building. Re-tidying that module fixed it; the lesson is that adding a
dependency to the root module is a two-module change.

Every `curl` command above was executed against a running tommy while writing
this document.

## Fan-out

`personalizations[]` is a list, and **each entry becomes one delivered
message** — one `mail.Message`, one event. A request with three
personalizations appends three events, mirroring the mail plugin's rule that a
`Message` is one delivered message, not one API request.

Per personalization, `subject` and `headers` **override** the message-level
values; everything else on a personalization (`to`/`cc`/`bcc`, `custom_args`,
`send_at`) has no message-level equivalent and merges the same direction
(personalization wins on a shared key, message-level fills the rest).
`reply_to` and `reply_to_list` both map onto the canonical `Message.ReplyTo`
slice — the real API rejects setting both, and so does this fake.

## Content and attachments

`content[]` carries the body parts: `text/plain` becomes `Message.Text`,
`text/html` becomes `Message.HTML`. `attachments[]` are base64-decoded and
stored through `Message.AttachBytes`, so their bytes live in the blob store,
never inline in the event; `disposition: "inline"` plus `content_id` is how an
HTML body's `cid:` reference resolves back to the right attachment. Attaching
past the blob store's capacity surfaces as a `413` rather than panicking.

## Auth

Any `Authorization` header is accepted by default, and whatever was presented
is recorded on `Event.Meta.authorization` — nothing is validated unless the
provider's config section pins an `api_key`, in which case a missing or
mismatched bearer token is rejected with SendGrid's real
`{"errors":[{"message","field"}]}` shape and a `401`.

```toml
[plugins.mail.providers.sendgrid]
api_key = "SG.your-fake-key"
```

## The 202 contract

A successful send answers **`202 Accepted` with an empty body and an
`X-Message-Id` header** — no JSON payload, which is the part most fakes get
wrong. `categories`, `custom_args`, `send_at`, `batch_id`, `asm`,
`ip_pool_name`, the presented credentials and the generated message id all go
in `Event.Meta`, never on the canonical `Message`.

## Package tests

`go test ./plugins/mail/providers/sendgrid/...` covers fan-out, override
precedence, attachment round-tripping through the blob store, the
202/empty-body/`X-Message-Id` contract, auth capture and pinning, and
SendGrid's error shapes.
