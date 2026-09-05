# `mail` plugin

## What it is

A stand-in for wherever your application's email would otherwise go: a
transactional-email vendor's HTTP API, or a real SMTP server. Tommy accepts the
message — over Mailjet's or SendGrid's API shape, or a genuine SMTP
conversation — parses it into one canonical `mail.Message` regardless of which
route it came in on, and never delivers it anywhere. Every message is stored as
an event and served back over `/api/v1/mail/…` and the **Mail** tab.

## What it's for

The situations this plugin exists for are all "I need to see the email, not
send it":

- **Checking what a password-reset or order-confirmation email actually renders
  as**, including the HTML, without sending it to a real inbox or a colleague.
- **Asserting in CI that signing up sends exactly one message, to the right
  address, with the right subject** — `GET /api/v1/mail/messages` after the
  request under test, no mailbox to poll.
- **Confirming an attachment survived** — an invoice PDF, a CSV export — byte
  for byte, by fetching it back from the blob store.
- **Pointing a whole staging environment at something harmless**, so a bug in
  an environment check can never leak a test order confirmation to a real
  customer's inbox.

Which of the three providers to reach for depends on how your application talks
to mail: **mailjet** and **sendgrid** are HTTP APIs, for when your code calls
those vendors' SDKs or a compatible client library. **smtp** is a real mail
server on its own port, for anything that speaks SMTP directly — most
languages' standard mail libraries, mail-sending frameworks that default to
SMTP, or infrastructure (a mail relay, a `sendmail`-compatible tool) that has no
concept of a vendor API at all. If your application is already coded against a
specific vendor's SDK, use that vendor's provider so the wire format matches
exactly; if it just wants "an SMTP server", the smtp provider is the more
faithful stand-in and needs no code changes at all — just a different host and
port.

## How to test it for real

Boot mail on its own, using ports that will not collide with anything else on
your machine:

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . mail --ui-port 18901 --in-port 18902 --smtp-port 11025
```

Send one message through each route. Mailjet and SendGrid are HTTP:

```bash
curl -s http://localhost:18902/v3.1/send \
  -u "any-key:any-secret" -H 'Content-Type: application/json' -d '{
  "Messages":[{"From":{"Email":"a@example.com","Name":"Alice"},"To":[{"Email":"b@example.com"}],
  "Subject":"Hello from mailjet","TextPart":"It works."}]}'

curl -si http://localhost:18902/v3/mail/send \
  -H 'Authorization: Bearer SG.fake-key' -H 'Content-Type: application/json' -d '{
  "personalizations": [{"to": [{"email": "bob@example.com"}], "subject": "Hello from sendgrid"}],
  "from": {"email": "alice@example.com"},
  "content": [{"type": "text/plain", "value": "It works."}]}'
```

SMTP is a real conversation — `curl`'s `smtp://` scheme drives one:

```bash
curl -s smtp://localhost:11025 \
  --mail-from alice@example.com --mail-rcpt bob@example.com -T - <<'EOF'
From: Alice <alice@example.com>
To: Bob <bob@example.com>
Subject: Hello from smtp

It works.
EOF
```

All three show up the same way, because they all become the same canonical
model — open the tab or ask the API, newest first:

```bash
open http://localhost:18901/ui/mail/
curl -s "http://localhost:18901/api/v1/events?plugin=mail" | jq '[.[].summary]'
```

Every command above was run against a live instance while writing this
document; each provider's own README goes deeper on that provider's wire
format, and `test/integration/` drives the real `mailjet-apiv3-go`,
`sendgrid-go` and stdlib `net/smtp` clients against tommy end to end — read
`mailjet_test.go`, `sendgrid_test.go` and `smtp_test.go` there for the
SDK-level version of what curl does above. Running that suite is
also what revealed a stale `go.sum` in it, since it is a separate module the
main gate never compiles — see the mailjet provider's README.

## Internals

- `message.go` — the canonical model every provider converts into.
- `api.go` — the read-back API.
- `ui.go` + `ui/inbox.html` — the three-pane inbox tab.
- `mailtest/` — a **test-only** fake provider. It is a fixture; it is never
  registered in `plugins/all/all.go`.
- `providers/` — the real providers (Mailjet, SendGrid, SMTP), added in Wave 2.

## The canonical message

One `Message` is **one delivered message, not one API request**: a Mailjet
`Messages[]` entry or a SendGrid `personalizations[]` entry each become one
`Message`, so a request that fans out to three recipients appends three events.

```go
type Message struct {
    From    Address   // Address{Name, Email}
    To, Cc, Bcc, ReplyTo []Address
    Subject string
    Text    string    // the text/plain part
    HTML    string    // the text/html part
    Headers Headers   // map[string][]string, case-insensitive lookups
    Attachments []Attachment
}

type Attachment struct {
    Filename, ContentType string
    Size      int64
    Inline    bool      // Content-Disposition: inline
    ContentID string     // the cid the HTML body references, no angle brackets
    Blob      blob.Ref   // the bytes live in the blob store, never in the event
}
```

Provider-specific metadata (Mailjet `CustomID`, SendGrid `categories`, the
credentials that were presented, …) goes in `Event.Meta`, never in `Message`.

A provider builds one like this:

```go
msg := &mail.Message{From: from, To: to, Subject: subject, Text: text, HTML: html}
msg.Headers.Set("X-Campaign", "billing")
if _, err := msg.AttachBytes(ctx, d.Blobs, mail.Attachment{
    Filename: "invoice.csv", ContentType: "text/csv",
}, decoded); err != nil { /* ... */ }

ev := mail.NewEvent("mailjet", msg) // plugin, type, summary and payload filled in
ev.Meta = map[string]any{"custom_id": customID}
ev.Raw = event.Raw{Transport: "http", Method: r.Method, Path: r.URL.Path,
    Headers: r.Header.Clone(), Body: body, Text: true}
err := d.Append(ctx, ev)
```

## API

Mounted under `/api/v1/mail`; every route reads from the store, so a client that
sends and immediately fetches sees its own write.

These routes, their filters and their response schemas are also in the machine-readable
description at `GET /api/v1/openapi.json` (checked in as `docs/openapi.json`).

| Route | Notes |
|---|---|
| `GET /messages` | newest first. `?provider=&search=&since=&limit=&offset=` plus the mail-specific `?to=&from=&subject=&has_attachments=` |
| `GET /messages/{id}` | one message with `links` to its bodies and attachments |
| `GET /messages/{id}/html` | the HTML part, `text/html`, with a no-script CSP — **untrusted content** |
| `GET /messages/{id}/text` | the text part, `text/plain` |
| `GET /messages/{id}/raw` | the untouched request that produced it; `?download=1` for a `.eml` |
| `GET /messages/{id}/attachments/{idx}` | streams the blob with the right `Content-Type` and `Content-Disposition`; range requests supported; `?inline=1` / `?download=1` |
| `DELETE /messages` | clears every captured message; attachment blobs deliberately survive |

Every message carries a `url`: the link to that mail's own page. It is the
answer to the question this plugin exists for — the application sends a password
reset in local development, prints the link from the response header or from
`GET /api/v1/mail/messages`, and the developer opens the mail itself.

## UI

`/ui/mail/` is a three-pane inbox: the message list, a header table, and the
body with **HTML / Text / Raw** toggles. The HTML body is written by the
application under test, so it is *never* injected into the page — it is loaded
from `GET /api/v1/mail/messages/{id}/html` into a fully restricted
`<iframe sandbox="">`. The list refreshes live off the shell's SSE connection on
`mail.message`.

## Package tests and the fixture provider

Run the package tests, which boot a whole tommy on ephemeral ports:

```bash
go test ./plugins/mail/...
```

Manually, with a real provider enabled, use that provider's own snippet
(`tommy providers mail` prints them). With the test-only fake provider mounted,
one message goes in with:

```bash
curl -s http://localhost:8822/mailtest/v1/send \
  -H 'Content-Type: application/json' -d '{
  "from": "Alice <alice@example.com>",
  "to": ["bob@example.com"],
  "subject": "Hello from tommy",
  "text": "It works.",
  "html": "<p>It <b>works</b>.</p>"
}'
```

and comes back out with:

```bash
curl -s http://localhost:8811/api/v1/mail/messages | jq '.[0].message.subject'
```
