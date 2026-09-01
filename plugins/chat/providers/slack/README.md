# slack

Imitates the two surfaces a Slack SDK, or any HTTP client, uses to post a
message: [incoming webhooks](https://docs.slack.dev/messaging/sending-messages-using-incoming-webhooks)
and the Web API's [`chat.postMessage`](https://docs.slack.dev/reference/methods/chat.postMessage).
Both convert into tommy's canonical `chat.Message`; Block Kit `blocks` and
legacy `attachments` are stored verbatim so a card renderer never has to
re-derive them.

## Routes

| Route | Notes |
|---|---|
| `POST /services/{team}/{bot}/{token}` | Incoming webhook. Body is JSON: `text`, `blocks`, `attachments`, `channel`, `username`, `icon_url`, `icon_emoji`, `thread_ts`, `unfurl_links`, `unfurl_media`, `mrkdwn`. Responds with the **literal text `ok`** as `text/plain` — not JSON. On error, a plain-text error code (`invalid_payload`, `no_text`) with a real HTTP status, matching Slack's own "webhooks give more expressive errors than the Web API" behavior. |
| `POST /api/chat.postMessage` | Web API. Accepts **either** `application/json` **or** `application/x-www-form-urlencoded` (Slack documents both). `channel` is required; `text`, `blocks`, `attachments`, `thread_ts`, `username`, `icon_url`, `icon_emoji`, `mrkdwn`, `unfurl_links`, `unfurl_media`, `as_user`, `reply_broadcast` are optional. Bearer token via the `Authorization` header (preferred) or a `token` field in the body. Always responds **HTTP 200**, `{"ok":true,"channel":"C…","ts":"…","message":{…}}` on success or `{"ok":false,"error":"…"}` on failure — Slack's Web API does not use HTTP status to signal an application-level error. |

`/api/v1/` is reserved for tommy's own API, but `/api/chat.postMessage` does
not fall under that prefix, so it is legal for this provider to mount.

### Not implemented: `chat.update` / `chat.delete`

`core/store` events are immutable once appended and the chat plugin has no
edit/delete resource type or relation for "this message was later changed" —
by design, since threads are already derived rather than stored (see
`plugins/chat/thread.go`). Faking either endpoint would mean returning a
`{"ok":true}` envelope that does not actually change what the store or the
chat tab show: exactly the "shaky" result the provider checklist warns
against. Two correct routes beat four half-real ones.

## Wire shapes, verified against the live docs

- **Incoming webhook success** — HTTP 200, `Content-Type: text/plain`, body is
  exactly `ok`. This is the single easiest thing to get wrong about this
  surface.
- **Incoming webhook payload** — `text`, `blocks` (Block Kit, verbatim),
  `attachments` (legacy, verbatim), `thread_ts`. Slack's current-generation
  apps reject `channel`/`username`/`icon_url`/`icon_emoji` overrides on this
  surface, but the classic "custom integration" webhooks Slack ran for years
  accepted them, and plenty of real-world payloads still send them — this
  provider accepts and records all of them rather than silently dropping
  fields a test fixture might depend on.
- **`chat.postMessage` success** — `{"ok":true,"channel":"C123ABC456","ts":"1503435956.000247","message":{"type":"message","text":"…","ts":"…","bot_id":"B…", ...}}`.
  `blocks`/`attachments` on the form-encoded path arrive as a JSON-encoded
  string (Slack's own documented shape for a structured argument over a
  form); this provider decodes the percent-encoding and stores the resulting
  bytes verbatim, exactly as it would a JSON body's own `blocks` array.
- **`chat.postMessage` errors** — `{"ok":false,"error":"<code>"}`, **HTTP 200**
  even for `not_authed`, `invalid_auth` and `channel_not_found`. Slack's own
  method reference never documents a different status for `ok:false`; `429`
  only shows up for actual rate limiting, which this fake does not implement.

## Converting into the canonical model

- **`Channel.ID`** is the `C…` id when the caller gave one, the `#name`
  override when it gave that instead, and — only for the webhook, which never
  carries a channel id at all since Slack binds the destination channel to
  the webhook URL at install time — `webhook:{team}/{bot}`, so every post
  through the same webhook lands in the same derived channel.
- **`Author.Bot`** is always `true`: both an incoming webhook and a
  `chat.postMessage` call post as a bot, never as a human.
- **`TS`** is left empty for a webhook post (Slack never hands one back to the
  poster, so message identity falls back to the event id, exactly as
  `message.go` documents) and minted for `chat.postMessage` in Slack's own
  `seconds.ffffff` shape, deterministically from the event id and the clock so
  the same fake `Deps` always mints the same ts.
- **`ThreadTS`** is `thread_ts` passed through untouched on both surfaces.
- Everything Slack-specific — `team`/`bot`/`webhook_token` from the path, the
  presented bearer token, `icon_emoji`, `mrkdwn`, `unfurl_links`/
  `unfurl_media`, `as_user`, `reply_broadcast`, the minted ts — lives in
  `Event.Meta`, never in the canonical `Message`.

## Auth

`chat.postMessage` accepts any bearer token (or none — see below) by default
and records exactly what was presented in `Event.Meta`, matching this repo's
"accept anything, record what was presented" convention (see the `twilio`
provider). Pin a `bot_token` in this provider's config section to start
rejecting a mismatch with Slack's own `invalid_auth` error; a request with no
token at all always gets `not_authed`:

```toml
[plugins.chat.providers.slack]
bot_token = "xoxb-fake-token"
```

## How to test

```bash
tommy serve   # then open http://localhost:8811/ui/chat/
```

```bash
curl -s http://localhost:8822/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX \
  -H 'Content-Type: application/json' \
  -d '{"text":"It works.","channel":"#general","username":"deploy-bot"}'
```

```bash
curl -s http://localhost:8822/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX \
  -H 'Content-Type: application/json' \
  -d '{"channel":"#alerts","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"*It works.*"}}]}'
```

```bash
curl -s http://localhost:8822/api/chat.postMessage \
  -H 'Authorization: Bearer xoxb-fake-token' \
  -H 'Content-Type: application/json' \
  -d '{"channel":"C0123ABCD","text":"It works."}'
```

Read it back:

```bash
curl -s http://localhost:8811/api/v1/chat/channels | jq
curl -s http://localhost:8811/api/v1/chat/messages | jq '.[0].message'
```

```bash
go test ./plugins/chat/providers/slack/...
go test -race ./plugins/chat/providers/slack/...
```

The package tests are table-driven against golden request fixtures in
`testdata/` and assert both the exact HTTP response (status, `Content-Type`,
body) and the canonical `chat.Message` the provider actually stored: the
literal `ok` text/plain webhook response, JSON and form bodies on
`chat.postMessage`, blocks and attachments stored verbatim on both surfaces,
a threaded reply's `thread_ts` passthrough, the webhook's channel-id
fallback, auth capture (default-accept and pinned rejection), and every error
shape. `plugintest.Conformance` and a full `testutil.Start` end-to-end pass
(post through the ingress, read back through `/api/v1/chat/`) are included.
