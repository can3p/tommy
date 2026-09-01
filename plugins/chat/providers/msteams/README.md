# `msteams` provider

Imitates a Microsoft Teams incoming webhook: the retired Office 365 / M365
connector's `MessageCard` format and the current Power-Automate-backed
[workflow trigger](https://learn.microsoft.com/en-us/microsoftteams/platform/webhooks-and-connectors/how-to/add-incoming-webhook)'s
Adaptive Card format, both on the real webhook URL shape:

```
POST /webhookb2/{guid}@{tenant}/IncomingWebhook/{id}/{key}
```

Bot Framework (`POST /v3/conversations/{id}/activities`) needs an OAuth token
exchange and is deliberately out of scope; see
`docs/implementation-plan.md` §12.

## Route

`{guid}@{tenant}` is one URL path segment - the two identifiers are joined by
an `@`, not a `/` - so it is mounted as a single wildcard, `{GuidTenant}`,
which the handler splits on `@` itself. This is the same rule the `twilio`
provider hit from the other side (a wildcard cannot share a segment with a
literal suffix like `.json`): `net/http`'s `ServeMux` requires a wildcard to
occupy a whole path segment, and a single wildcard over `guid@tenant` is
exactly what that segment needs.

## Two payload generations

- **MessageCard** (the retired O365/M365 connector format): a JSON object
  whose top level carries `"@type": "MessageCard"`, along with `@context`,
  `summary`, `themeColor`, `title`, `text`, a `sections[]` array
  (`activityTitle`, `activitySubtitle`, `activityImage`, `facts`, `text`) and
  `potentialAction[]`.
- **Adaptive Card**, current generation: `{"type":"message","attachments":
  [{"contentType":"application/vnd.microsoft.card.adaptive","content":{…}}]}`.
  A workflow trigger also accepts a bare `{"text":"..."}` body directly, per
  Microsoft's own "Send a request to the webhook" example.

## Deciding the response

**This is the easy thing to get wrong**, so here is the rule: a body carrying
top-level `"@type":"MessageCard"` is a connector webhook, and gets that
generation's real success response — the literal digit `1`, as `text/plain`,
`200 OK`. Everything else — an Adaptive Card envelope, or the bare
`{"text":"..."}` shape — is a workflow trigger, and gets `202 Accepted` with
an empty body.

This is a **payload-shape** rule rather than a path or header rule, because
both generations share the identical `webhookb2` URL shape in the wild (the
URL is the only "credential" either one presents) — there is nothing else to
key off. A workflow trigger can itself accept a `MessageCard` (Teams
"Workflows support both Adaptive Cards and Message Card format"), but the
reverse is never true, so keying off `@type` is the one signal that never
misclassifies a genuine connector card.

## Converting into the canonical message

- `Channel.ID` is the request's own path (`/webhookb2/{guid}@{tenant}/
  IncomingWebhook/{id}/{key}`) — stable for as long as the webhook URL is,
  which is exactly the scope of "the webhook target" the canonical model asks
  for. There is no channel name in either payload, so `Channel.Name` stays
  empty.
- A `MessageCard`'s `sections[0].activityTitle` / `activityImage` become
  `Author.Name` / `Author.IconURL` — the closest thing a MessageCard has to
  "who posted this". `Author.Bot` is always `true`.
- `Text` prefers the payload's own top-level `text`, then `summary`; when
  neither is present (a bare card with only section text, or an Adaptive Card
  with nothing at the top level), `chat.Message.Normalize` derives it from the
  structured content via `chat.FallbackText`, which already knows to harvest
  `activityTitle`/`activitySubtitle`/`text` — exactly the fields these two
  schemas use for prose.
- The **inner card object** is stored as `chat.FormatTeamsAdaptiveCard`,
  never the `{"type":"message","attachments":[…]}` envelope around it — a
  `json.RawMessage` field decodes to the exact input bytes for that value, so
  this is byte-for-byte with no re-marshaling. A `MessageCard` payload has no
  envelope to begin with, so the whole (trimmed) request body is stored as
  `chat.FormatTeamsMessageCard`. Several attachments produce several
  `Content` entries, in the order they arrived.
- `ThreadTS` is always empty: Teams webhooks have no thread semantics.
- `themeColor`, `potentialAction`, `@context` and the four webhook path
  components (`guid`, `tenant`, `id`, `key`, plus the full `path` and a
  `generation` tag of `connector` or `workflow`) go on `Event.Meta`, never on
  the canonical `Message`.

## Auth

None is presented beyond the URL itself, which is the secret — nothing is
ever rejected for it. The path components are recorded on `Event.Meta` for
inspection, never validated.

## Errors

- Invalid JSON, or a body over Teams' own documented 28KB message limit
  (`msteams.MaxBody`), or valid JSON with no card and no top-level
  `text`/`summary` — that last one is the same "Bad payload received by
  generic incoming webhook" condition the live endpoint reports, so the fake
  answers with its own real message, `Summary or Text is required.`, as
  `text/plain` (there is no documented JSON error envelope here, unlike
  SendGrid or Twilio).

## How to test

```bash
curl -si http://localhost:8822/webhookb2/11111111-1111-1111-1111-111111111111@22222222-2222-2222-2222-222222222222/IncomingWebhook/33333333333333333333333333333333/44444444-4444-4444-4444-444444444444 \
  -H 'Content-Type: application/json' -d '{
  "@type": "MessageCard",
  "@context": "https://schema.org/extensions",
  "summary": "Build failed",
  "themeColor": "FF0000",
  "title": "Build #482 failed",
  "text": "It works.",
  "sections": [{
    "activityTitle": "deploy-bot",
    "activitySubtitle": "2 minutes ago",
    "facts": [{"name": "Branch", "value": "main"}]
  }],
  "potentialAction": [{
    "@type": "OpenUri",
    "name": "View build",
    "targets": [{"os": "default", "uri": "https://example.com/build/482"}]
  }]
}'
```

```bash
curl -si http://localhost:8822/webhookb2/11111111-1111-1111-1111-111111111111@22222222-2222-2222-2222-222222222222/IncomingWebhook/33333333333333333333333333333333/44444444-4444-4444-4444-444444444444 \
  -H 'Content-Type: application/json' -d '{
  "type": "message",
  "attachments": [{
    "contentType": "application/vnd.microsoft.card.adaptive",
    "content": {
      "$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
      "type": "AdaptiveCard",
      "version": "1.4",
      "body": [{"type": "TextBlock", "text": "It works.", "weight": "bolder"}]
    }
  }]
}'
```

Read it back through the chat plugin's own API:

```bash
curl -s http://localhost:8811/api/v1/chat/messages | jq '.[0].message.text'
```

Run the package tests, which cover both payload generations, both response
shapes, the Adaptive Card being stored unwrapped (byte for byte, no
re-marshaling), multiple attachments in order, fallback text from a card with
no top-level text, the webhook's path components landing in `Event.Meta`, and
malformed/oversized/missing-content payloads:

```bash
go test ./plugins/chat/providers/msteams/...
go test -race ./plugins/chat/providers/msteams/...
```
