# What tommy can stand in for

Every plugin and provider, what it is for, and where its documentation lives.
This page is an **index only** — one line each and a link. The authoritative
text is each component's own `README.md`, next to the code it describes, so
there is one copy of every claim rather than two that drift apart.

The runtime equivalent of this page is `tommy providers`, which prints every
provider's description, endpoints and runnable snippets rendered against the
ports the current configuration would actually bind. Use that when you want a
command to paste; use this when you want to know whether tommy covers the
thing you are trying to test.

Every component's README opens with the same three sections, per `CLAUDE.md`
rule 12: **what it is**, **what it's for**, and **how to test it for real** —
the last one being commands that have been run rather than commands that look
plausible.

## Plugins

A *plugin* owns a content type: a canonical model, API routes and a UI tab.

| Plugin | Stands in for | Docs |
|---|---|---|
| `mail` | Email you need to *see* rather than send — a rendered password reset, an attachment that must survive, a staging environment that must not reach real customers. | [`plugins/mail`](../plugins/mail/README.md) |
| `sms` | Text messages, with the segment arithmetic that decides whether your one-time passcode costs one message or two. | [`plugins/sms`](../plugins/sms/README.md) |
| `files` | Anything your application writes over a file-transfer protocol you would rather not stand up for real. | [`plugins/files`](../plugins/files/README.md) |
| `chat` | Webhook notifications to Slack or Teams, rendered as the card a channel would show, without spamming a real channel. | [`plugins/chat`](../plugins/chat/README.md) |
| `hl7` | HL7 v2 clinical messages sent to a hospital interface engine you cannot get a test instance of. | [`plugins/hl7`](../plugins/hl7/README.md) |
| `snmp` | Your own agent's or device's *outbound* traps — whether the alert fires, and what varbinds it really carries. | [`plugins/snmp`](../plugins/snmp/README.md) |
| `push` | Mobile push, answering the question most silent-push debugging comes down to: would this have displayed anything at all? | [`plugins/push`](../plugins/push/README.md) |
| `as2` | An EDI trading partner's AS2 endpoint, including the certificate exchange and the MDN receipt your integration blocks on. | [`plugins/as2`](../plugins/as2/README.md) |

## Providers

A *provider* translates one vendor's or one protocol's wire format into its
plugin's model. Providers never import each other.

| Provider | Reach for it when | Docs |
|---|---|---|
| `mail/mailjet` | Your code calls Mailjet's SDK or posts to its v3.1 endpoint. | [`mailjet`](../plugins/mail/providers/mailjet/README.md) |
| `mail/resend` | Your code sends through Resend's SDK or posts to `api.resend.com`. Send, batch and read-back. | [`resend`](../plugins/mail/providers/resend/README.md) |
| `mail/sendgrid` | Your code sends through SendGrid's SDK or v3 Mail Send. | [`sendgrid`](../plugins/mail/providers/sendgrid/README.md) |
| `mail/smtp` | Your code speaks SMTP directly rather than a vendor HTTP API. A real mail server on its own port. | [`smtp`](../plugins/mail/providers/smtp/README.md) |
| `sms/twilio` | Whatever normally calls `api.twilio.com` can be pointed elsewhere. | [`twilio`](../plugins/sms/providers/twilio/README.md) |
| `files/ftp` | The thing under test already speaks FTP — a legacy partner drop, a nightly export. | [`ftp`](../plugins/files/providers/ftp/README.md) |
| `files/sftp` | It carries an SSH client: `scp`, `sftp`, a batch job that logs in. | [`sftp`](../plugins/files/providers/sftp/README.md) |
| `files/tftp` | Network-device and PXE-style flows — firmware pushers, boot ROMs. UDP, no auth. | [`tftp`](../plugins/files/providers/tftp/README.md) |
| `files/nfs` | It **mounts a filesystem** instead of speaking a transfer protocol. | [`nfs`](../plugins/files/providers/nfs/README.md) |
| `chat/slack` | Anything that talks to Slack — the SDK, a webhook URL, an alerting library. | [`slack`](../plugins/chat/providers/slack/README.md) |
| `chat/msteams` | The Teams webhook URL your pipeline or monitoring already posts to. | [`msteams`](../plugins/chat/providers/msteams/README.md) |
| `hl7/mllp` | HL7 over MLLP — framed TCP, so no HTTP client can drive it and `curl` is useless. | [`mllp`](../plugins/hl7/providers/mllp/README.md) |
| `snmp/trap` | Checking a trap fires and says what you think it says. v1, v2c and informs. | [`trap`](../plugins/snmp/providers/trap/README.md) |
| `push/fcm` | Your backend pushes through Firebase, and you want the targeting and payload shown without a device or project. | [`fcm`](../plugins/push/providers/fcm/README.md) |
| `push/apns` | Your backend pushes through Apple. HTTP/2 only — there is no HTTP/1.1 form. | [`apns`](../plugins/push/providers/apns/README.md) |
| `as2/http` | AS2 over HTTP (RFC 4130), the transport binding a partner points a URL at. | [`as2/http`](../plugins/as2/providers/http/README.md) |

## What tommy deliberately will not do

Worth knowing before you look for a feature that is not here. Tommy captures
what an application *sent* and answers with whatever the protocol requires so
the client proceeds. It does not simulate scenarios, drive inbound traffic, or
make policy decisions.

So a reply that is **mechanical** — derivable from the request — fits: an HL7
`ACK`, an AS2 MDN, Slack's `ok`, SMTP's `250`. A reply that encodes a
**decision** somebody has to configure does not: approve or decline this card
payment, accept or reject this login. That is scenario machinery, and it is a
different tool.

Inbound traffic is out of scope in every form — webhooks and callbacks, Stripe
events, Twilio `StatusCallback`, SendGrid event webhooks, Slack interactivity,
asynchronous AS2 MDNs — because each needs outbound HTTP and a way to define
scenarios.

Individual providers refuse individual things for the same reason, and each
says so in its own README: FCM will not fake `404 UNREGISTERED`, APNs will not
fake the errors that need delivery state, Slack's `chat.update`/`chat.delete`
are absent because events are immutable. Every one of those would require
inventing state tommy does not keep.

See `docs/implementation-plan.md` §2 for the full scoping rule and what is
planned next.
