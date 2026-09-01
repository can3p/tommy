# `sendgrid` provider

Imitates the [Twilio SendGrid v3 Mail Send
API](https://www.twilio.com/docs/sendgrid/api-reference/mail-send/mail-send):
`POST /v3/mail/send`, mounted on the shared ingress exactly as the real API
paths it.

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

## How to test

```bash
curl -si http://localhost:8822/v3/mail/send \
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

Read it back through the mail plugin's own API:

```bash
curl -s http://localhost:8811/api/v1/mail/messages | jq '.[0].message.subject'
```

Run the package tests, which cover fan-out, override precedence, attachment
round-tripping through the blob store, the 202/empty-body/`X-Message-Id`
contract, auth capture and pinning, and SendGrid's error shapes:

```bash
go test ./plugins/mail/providers/sendgrid/...
```
