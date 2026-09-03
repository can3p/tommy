# `smtp` mail provider

## What it is

A real SMTP server — not an HTTP API imitation — that accepts mail from any
client on its own port and never delivers it anywhere. It parses MIME — nested
multiparts, attachments, inline images and encoded-word headers — into the
canonical `mail.Message`, records the envelope and any `AUTH` that was offered
in `Event.Meta`, and keeps the untouched wire bytes in `Event.Raw`.

## What it's for

Reach for this provider when your application (or the infrastructure in front
of it) speaks SMTP directly, rather than calling a vendor's HTTP API — which is
most languages' standard mail libraries, most mail-sending frameworks in their
default configuration, and any relay or `sendmail`-compatible tool with no
concept of Mailjet's or SendGrid's wire format at all:

- Your app is configured with an SMTP host/port/credentials (the classic
  `SMTP_HOST`/`SMTP_PORT` environment variables), and switching it to tommy
  during development or in CI is a config change, not a code change.
- You want to see exactly what a library like Python's `email` package, or a
  framework's mailer, actually produced on the wire — headers, MIME structure,
  encoding — including bugs that only show up in the raw bytes.
- You need a mail server for a whole staging environment that will never
  actually deliver anything, no matter what address someone types in a test
  order.

Pick `smtp` over `mailjet`/`sendgrid` whenever your code has no vendor SDK in
it at all — just point it at `localhost:1025` (or wherever this provider is
bound) with no credentials, and everything sent shows up in the **Mail** tab.
If your code does call a specific vendor's SDK, use that vendor's provider
instead so the wire format matches exactly.

## How to test it for real

Boot the mail plugin with smtp enabled (it's on by default) on ports that
won't collide with anything else running:

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . mail --ui-port 18901 --in-port 18902 --smtp-port 11025
```

`curl`'s `smtp://` scheme drives a real SMTP conversation without any client
library:

```bash
curl -s smtp://localhost:11025 \
  --mail-from alice@example.com --mail-rcpt bob@example.com -T - <<'EOF'
From: Alice <alice@example.com>
To: Bob <bob@example.com>
Subject: Hello from tommy

It works.
EOF
```

Python's standard library sends a MIME multipart message with an HTML
alternative and an attachment — the shape a real application mailer produces:

```python
import smtplib
from email.message import EmailMessage

msg = EmailMessage()
msg["From"] = "Alice <alice@example.com>"
msg["To"] = "Bob <bob@example.com>"
msg["Subject"] = "Hello from tommy"
msg.set_content("It works.")
msg.add_alternative("<p>It <b>works</b>.</p>", subtype="html")
msg.add_attachment(b"id,total\n1,42\n", maintype="text", subtype="csv",
                   filename="invoice.csv")

with smtplib.SMTP("localhost", 11025) as s:
    s.send_message(msg)
```

Read either back — the tab, or the API filtered to this provider:

```bash
open http://localhost:18901/ui/mail/
curl -s "http://localhost:18901/api/v1/mail/messages?provider=smtp" | jq '.[0].message | {subject, attachments}'
```

which (for the Python message) came back as:

```json
{
  "subject": "Hello from tommy",
  "attachments": [
    {"filename": "invoice.csv", "content_type": "text/csv", "size": 14, "blob": {"id": "...", "size": 14}}
  ]
}
```

For the standard library treated as the "official client" for a protocol
rather than a vendor HTTP API, `test/integration/smtp_test.go` — in the
separate `test/integration` Go module — builds a real MIME multipart message
with `mime/multipart` and delivers it with `net/smtp.SendMail`, then checks the
text part and the attachment's bytes round-tripped through the blob store
unchanged. It is run with
`cd test/integration && go test -tags integration -run TestSMTP ./...`.
That command was verified, but only after fixing a break it exposed: adding the
`as2` plugin's S/MIME dependency to the root module left `test/integration`'s
own `go.sum` stale, and since it is a **separate module** that `./...` never
reaches, `make check` compiled none of it. Every test in the module had stopped
building. Re-tidying that module fixed it; the lesson is that adding a
dependency to the root module is a two-module change.

`swaks --server localhost:11025 --to bob@example.com --from alice@example.com`
should also work, going by its documented interface, but `swaks` was not
available in the environment this document was written in, so that specific
invocation was not executed.

Both the `curl` command and the Python script above were executed against a
running tommy while writing this document, including the readback shown.
`Snippets()` in `provider.go` renders both against the live `SnippetCtx`, so
`tommy providers mail/smtp` prints them with the port this instance actually
bound, and the package tests execute both against a running listener.

## Configuration

```toml
[plugins.mail.providers.smtp]
enabled = true
port    = 1025          # 0 binds an ephemeral port
bind    = "127.0.0.1"
domain  = "tommy"       # the greeting banner hostname

max_message_bytes = 26214400   # one DATA transaction
max_recipients    = 100        # RCPT TO per transaction
max_line_length   = 65536      # generous: real senders emit long HTML lines
read_timeout      = 60         # seconds
write_timeout     = 60

# username = "apikey"          # optional: pin the credentials AUTH must present
# password = "s3cret"
```

An **absent** `port` means 1025; `port = 0` means "bind an ephemeral port", which
is what the test harness uses. Nothing else in tommy has to know the difference.

`AUTH PLAIN` and `AUTH LOGIN` are advertised and accepted from any client, with
whatever was presented recorded in `Event.Meta.auth` — a fake that rejected
credentials would fail every application that has not been configured yet. Set
`username` or `password` and the opposite becomes true: `AUTH` is then required
and checked, so you can exercise your own error path.

## What lands where

| | |
|---|---|
| `Message.From/To/Cc/Bcc/ReplyTo/Subject` | the **headers**, RFC 2047 decoded |
| `Message.Text` / `Message.HTML` | the body parts, transfer- and charset-decoded, CRLF normalized |
| `Message.Attachments` | every non-body part, bytes in the blob store |
| `Message.Headers` | every header, decoded, original casing kept |
| `Event.Meta.envelope` | `MAIL FROM` and every `RCPT TO`, which legitimately differ from the headers |
| `Event.Meta.auth` | the mechanism, username and password a client presented |
| `Event.Meta.parse_warnings` | everything the parser had to guess at |
| `Event.Raw` | `Transport: "smtp"`, the peer address, the wire header block and the untouched bytes |

One SMTP transaction is **one event**, even with several `RCPT TO` addresses:
that is one delivered message, and all of its envelope recipients are in
`Meta.envelope.rcpt_to`. `GET /api/v1/mail/messages/{id}/raw` serves the wire
bytes back as `message/rfc822`.

## MIME shapes handled

- `multipart/alternative` — text and HTML; a later alternative wins over an
  earlier one, but only inside its own container.
- `multipart/mixed` — bodies plus attachments.
- **Nested multiparts** — `mixed` wrapping `alternative` is the common real
  shape, and it nests to any depth up to the recursion guard.
- `multipart/related` — inline images keep their `Content-ID`, so a `cid:` URL
  in the HTML body resolves through `Message.AttachmentByContentID`.
- Transfer encodings `base64` (tolerating missing padding, embedded whitespace
  and trailing junk), `quoted-printable`, `7bit`, `8bit`, `binary`.
- RFC 2047 encoded words in subjects and display names: several in a row,
  several charsets in one header, split across a fold.
- Charsets beyond UTF-8 through `x/text`'s IANA index, plus the aliases mail
  clients invent. An unknown charset degrades to a reading that keeps every byte
  rather than producing replacement characters, and says so in a warning.
- No MIME structure at all — a bare `text/plain` body, or a message with no
  header/body separator whatsoever.
- `Content-Disposition: attachment` vs `inline`, with filenames from RFC 2231
  continuations (`filename*0*=UTF-8''…`) or the legacy `name` parameter.

**Deliberately not handled.** `message/rfc822` parts are stored as attachments
rather than parsed as nested messages: the raw bytes are right there, and a mail
catcher that recursed into forwarded mail would show a tree nobody sent.
S/MIME and PGP parts are stored as attachments, not verified or decrypted.
`STARTTLS` is not offered, because credentials are not secrets here and every
client falls back to plaintext for `localhost`.

## Failing safely

Everything a client sends is untrusted, so every loop is bounded: nesting depth,
part count, total decoded bytes relative to the message size, recipients, line
length and message size all have caps, and the number of warnings does too. A
message that violates the standard is never dropped — it is parsed as far as it
goes, the rest is reported in `Meta.parse_warnings`, and the wire bytes are in
`Raw` either way. A blob store that has filled up costs the attachment, with a
warning, never the message.

## Files

- `provider.go` — the `plugin.ListenerProvider`: configuration, snippets, the
  listener lifecycle.
- `session.go` — the SMTP conversation: envelope, `AUTH`, `DATA`, the event.
- `mime.go` — header unfolding, the MIME tree, transfer decoding, addresses.
- `builder.go` — the tree to `mail.Message` conversion, including attachments.
- `charset.go` — charset decoding, with the graceful degradation.
- `testdata/*.eml` — one real message per MIME shape above.
