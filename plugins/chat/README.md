# `chat` plugin

Captures the Slack and Microsoft Teams messages an application posts instead of
delivering them, keeping each one's Block Kit blocks or Adaptive Card exactly as
it was sent. Every message is converted into one canonical `chat.Message`,
stored as an event, and served back over `/api/v1/chat/…` and the **Chat** tab.

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

| Route | Notes |
|---|---|
| `GET /messages` | newest first. `?provider=&search=&since=&limit=&offset=` plus the chat-specific `?channel=&author=&thread=&format=&bot=&replies=` |
| `GET /messages/{id}` | one message, with its derived `channel_key`, `thread_key` and `root_id` |
| `GET /channels` | the derived channel index: message, thread, reply and orphan counts, and the last message |
| `DELETE /messages` | clears every captured chat message |

`?channel=` accepts the provider's channel id, its display name or the derived
key, because all three appear in API responses.

## UI

`/ui/chat/` is a channel sidebar plus a message stream: avatars (or initials),
bot names, replies nested under the message they answer, and a timestamp on
every message. Message text, author names and channel names are written by the
application under test, so they are interpolated as plain strings through
`html/template` and never as `template.HTML`. The tab refreshes off the shell's
SSE connection on `chat.message`.

## How to test

Run the package tests, which boot a whole tommy on ephemeral ports:

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
