# `chat` plugin

## What it is

Tommy's chat content type. It stands in for the Slack and Microsoft Teams
endpoints an application posts messages to, capturing what was sent instead
of delivering it — keeping each message's Block Kit blocks or Adaptive Card
exactly as it arrived. Every message is converted into one canonical
`chat.Message`, stored as an event, and served back over `/api/v1/chat/…`
and the **Chat** tab.

## What it's for

Three concrete situations:

- Your service posts a deploy notification or an alert to a Slack incoming
  webhook, and you want to see the rendered card without spamming a real
  channel every time you run it.
- A CI job asserts that a failing build posts exactly one message with the
  right text — against tommy, that assertion is a `curl` against
  `/api/v1/chat/messages`, not a Slack workspace and a bot token in CI
  secrets.
- You're developing a Block Kit layout or an Adaptive Card and want to
  iterate on it without a workspace or a Teams tenant at all — post it here,
  see the JSON tommy captured, adjust, repeat.

Tommy renders Block Kit and Adaptive Cards where a renderer is wired up
(`chat.New(...).WithRichRenderer(blocks.Render)`) and falls back to plain
text plus a collapsible JSON inspector otherwise — capture never waits on
rendering fidelity. It also deliberately **accepts** `channel`/`username`
overrides on the Slack webhook surface that Slack's current-generation apps
reject: a fake that mirrors production's refusals is less useful than one
that shows you what your code actually sent.

## How to test it for real

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . chat --ui-port 18931 --in-port 18932
```

Post a plain-text Slack webhook message:

```bash
curl -s http://localhost:18932/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX \
  -H 'Content-Type: application/json' \
  -d '{"text":"Deploy of api v2.14.0 succeeded.","channel":"#deploys","username":"deploy-bot"}'
```

returns the literal text `ok`. Post Block Kit blocks — the interesting case,
since rendering is the point:

```bash
curl -s http://localhost:18932/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX \
  -H 'Content-Type: application/json' \
  -d '{"channel":"#deploys","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"*Build 482 failed*"}}]}'
```

Post through Slack's Web API instead:

```bash
curl -s http://localhost:18932/api/chat.postMessage \
  -H 'Authorization: Bearer xoxb-fake-token' \
  -H 'Content-Type: application/json' \
  -d '{"channel":"C0123ABCD","text":"It works."}'
```

returns `{"ok":true,"channel":"C0123ABCD","ts":"…","message":{...}}`. Post a
Teams Adaptive Card through a workflow-trigger webhook:

```bash
curl -si http://localhost:18932/webhookb2/11111111-1111-1111-1111-111111111111@22222222-2222-2222-2222-222222222222/IncomingWebhook/33333333333333333333333333333333/44444444-4444-4444-4444-444444444444 \
  -H 'Content-Type: application/json' -d '{
  "type": "message",
  "attachments": [{
    "contentType": "application/vnd.microsoft.card.adaptive",
    "content": {"type": "AdaptiveCard", "version": "1.4",
      "body": [{"type": "TextBlock", "text": "It works.", "weight": "bolder"}]}
  }]
}'
```

returns `202 Accepted`. See everything that landed, across both providers:

```bash
curl -s http://localhost:18931/api/v1/chat/channels | jq
curl -s http://localhost:18931/api/v1/chat/messages | jq '.[0].message.text'
curl -s 'http://localhost:18931/api/v1/events?plugin=chat' | jq '.[0].summary'
```

or open the tab at `http://localhost:18931/ui/chat/`. Each provider's own
README (`providers/slack`, `providers/msteams`) has the full wire-format
detail and more payload shapes to try.

- `message.go` — the canonical model every provider converts into.
- `thread.go` — the channel and thread index, **derived from the flat event list
  at render time**. Nothing is stored as a relation.
- `api.go` — the read-back API.
- `ui.go` + `ui/chat.html` — the channel sidebar and message stream.
- `providers/` — the real providers (Slack, Microsoft Teams).

## The canonical message

One `Message` is **one posted message, not one API request**.

```go
type Message struct {
    Channel  ChannelRef // {ID, Name} - where it was posted
    Author   Author     // {ID, Name, IconURL, Bot} - who it was posted as
    Text     string     // always populated, even for a card-only payload
    Contents []Content  // structured payloads, verbatim
    TS       string     // the message's own id in the provider's terms
    ThreadTS string     // the parent's TS; empty for a top-level message
}

type Content struct {
    Format Format          // the schema discriminator
    Data   json.RawMessage // the original JSON, byte for byte
}
```

`Format` is one of `slack.blocks`, `slack.attachments`, `msteams.messagecard` or
`msteams.adaptivecard`. The three schemas are **not** normalized into one — that
would throw away exactly the fidelity a card renderer needs — so a renderer
switches on `Format` and decodes `Data` with the vendor's own shape.

`Text` is always populated: `Normalize` derives a readable fallback from
`Contents` when the payload carried no text of its own, so a message is useful
before any renderer exists. Provider-specific metadata (the webhook path, the
team and bot ids, the tenant guid, the bearer token that was presented) goes in
`Event.Meta`, never in `Message`.

A provider builds one like this:

```go
msg := &chat.Message{
    Channel: chat.ChannelRef{ID: "C0123ABCD", Name: "general"},
    Author:  chat.Author{Name: "deploy-bot", Bot: true, IconURL: icon},
    Text:    payload.Text,
    TS:      ts,
    ThreadTS: payload.ThreadTS,
}
if len(payload.Blocks) > 0 {
    msg.Contents = append(msg.Contents, chat.Content{
        Format: chat.FormatSlackBlocks, Data: payload.Blocks,
    })
}

ev := chat.NewEvent("slack", msg) // plugin, type, summary and payload filled in
ev.Meta = map[string]any{"team": team, "webhook_token": token}
ev.Raw = event.Raw{Transport: "http", Method: r.Method, Path: r.URL.Path,
    Headers: r.Header.Clone(), Body: body, Text: true}
err := d.Append(ctx, ev)
```

## Channels and threads

Threads are a relation and `core/store` deliberately has none, so the index is
derived on every render by `chat.Channels(events)`:

1. messages are grouped by `Channel.ID`;
2. inside a channel each message is filed under its **root key** — its parent's
   `ThreadTS` when it is a reply, its own identity otherwise (`TS`, falling back
   to the event id for a webhook post that has none);
3. a thread's root is the non-reply message with that identity, and **a thread
   whose root never arrived is kept anyway**, marked `Orphan`. That happens
   whenever the parent was posted before tommy started or has been evicted from
   the ring buffer, and it must never lose the replies.

Channels are listed by most recent activity; threads sit at their root's
timestamp, so a late reply does not drag a thread to the bottom of the stream.

## Rendering structured content

The tab ships the **plain-text fallback plus a collapsible JSON inspector** for
every schema, so message capture never waits on rendering fidelity. A real
Block Kit or Adaptive Card renderer slots in without touching capture, the model
or the API:

```go
chat.New(slack.New(), msteams.New()).WithRichRenderer(blocks.Render)
// func Render(format string, data json.RawMessage) (template.HTML, bool)
```

Returning `false` falls back. The HTML a renderer returns is injected unescaped,
so it must escape every string it takes out of the payload: card content is
written by the application under test and is untrusted.

## API

Mounted under `/api/v1/chat`; every route reads from the store, so a client that
posts and immediately fetches sees its own write.

These routes, their filters and their response schemas are also in the machine-readable
description at `GET /api/v1/openapi.json` (checked in as `docs/openapi.json`).

| Route | Notes |
|---|---|
| `GET /messages` | newest first. `?provider=&search=&since=&limit=&offset=` plus the chat-specific `?channel=&author=&thread=&format=&bot=&replies=` |
| `GET /messages/{id}` | one message, with its derived `channel_key`, `thread_key` and `root_id` |
| `GET /channels` | the derived channel index: message, thread, reply and orphan counts, and the last message |
| `DELETE /messages` | clears every captured chat message |

Every message carries a `url`: the link to that event's own page in the UI, so
a client that just posted something can open what it sent.

`?channel=` accepts the provider's channel id, its display name or the derived
key, because all three appear in API responses.

## UI

`/ui/chat/` is a channel sidebar plus a message stream: avatars (or initials),
bot names, replies nested under the message they answer, and a timestamp on
every message. Message text, author names and channel names are written by the
application under test, so they are interpolated as plain strings through
`html/template` and never as `template.HTML`. The tab refreshes off the shell's
SSE connection on `chat.message`.

## Testing this package

The section above drives a real provider end to end; this is the internal
path, useful when working on the plugin itself rather than a provider. Run
the package tests, which boot a whole tommy on ephemeral ports:

```bash
go test ./plugins/chat/...
```

Manually, with a real provider enabled, use that provider's own snippet
(`tommy providers chat` prints them, rendered against the ports in use). With
the test-only fake provider mounted, one message goes in with:

```bash
curl -s http://localhost:8822/fake-chat/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{"channel":{"id":"C0123ABCD","name":"general"},
       "author":{"name":"deploy-bot","bot":true},
       "text":"It works.","ts":"1700000000.000100"}'
```

and comes back out with:

```bash
curl -s http://localhost:8811/api/v1/chat/channels | jq '.[0]'
curl -s http://localhost:8811/api/v1/chat/messages | jq '.[0].message.text'
```
