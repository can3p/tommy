# `mail` plugin

Captures the email an application sends instead of delivering it, whether it
went out through a vendor's HTTP API or plain SMTP. Every message is parsed into
one canonical `mail.Message`, stored as an event, and served back over
`/api/v1/mail/…` and the **Mail** tab.

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

| Route | Notes |
|---|---|
| `GET /messages` | newest first. `?provider=&search=&since=&limit=&offset=` plus the mail-specific `?to=&from=&subject=&has_attachments=` |
| `GET /messages/{id}` | one message with `links` to its bodies and attachments |
| `GET /messages/{id}/html` | the HTML part, `text/html`, with a no-script CSP — **untrusted content** |
| `GET /messages/{id}/text` | the text part, `text/plain` |
| `GET /messages/{id}/raw` | the untouched request that produced it; `?download=1` for a `.eml` |
| `GET /messages/{id}/attachments/{idx}` | streams the blob with the right `Content-Type` and `Content-Disposition`; range requests supported; `?inline=1` / `?download=1` |
| `DELETE /messages` | clears every captured message; attachment blobs deliberately survive |

## UI

`/ui/mail/` is a three-pane inbox: the message list, a header table, and the
body with **HTML / Text / Raw** toggles. The HTML body is written by the
application under test, so it is *never* injected into the page — it is loaded
from `GET /api/v1/mail/messages/{id}/html` into a fully restricted
`<iframe sandbox="">`. The list refreshes live off the shell's SSE connection on
`mail.message`.

## How to test

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
