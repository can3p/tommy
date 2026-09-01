# sms

Captures the SMS and MMS your code sends through a provider API instead of
delivering them, and shows each one as a phone-style conversation with the
**segment count and encoding** it would really cost on the wire.

That last part is the reason this tab exists rather than the generic event view:
a body that looks like 158 characters in your editor is three billed segments
the moment somebody pastes a curly quote into it, and nothing else in a test
environment tells you.

## The canonical model

Every provider converts its wire format into `sms.Message` and puts it in
`Event.Payload`. Provider-specific metadata (SIDs, callback URLs, the
credentials that were presented) goes in `Event.Meta`, never in the message.

```go
type Message struct {
    From             string    // E.164, or an alphanumeric sender id; may be empty
    To               string    // E.164
    MessagingService string    // sender pool used instead of an explicit From

    Body  string  // decoded text; untrusted, always escaped when rendered
    Media []Media // MMS attachments

    Status    Status    // queued | accepted | sending | sent | delivered | undelivered | failed | received
    Direction Direction // outbound | inbound
    Segments  Segments  // derived from Body; never supplied by a provider
}

type Media struct {
    ContentType string
    Filename    string
    Blob        *blob.Ref // the bytes, when the provider was given bytes
    URL         string    // the provider's own link, when that is all it was given
}

type Segments struct {
    Count     int      // segments the body needs; an empty body still costs one
    Encoding  Encoding // GSM-7 | UCS-2
    Units     int      // septets, or UTF-16 code units
    Capacity  int      // units per segment at this Count (160/153, or 70/67)
    Remaining int      // units still free in the last segment
}
```

Bytes never travel inline in an event: MMS content lives in the `BlobStore` and
the message keeps a `blob.Ref`. tommy never fetches a remote `MediaUrl` — a fake
that reaches out to the network is a fake that fails in CI — so media supplied
as a link keeps the link and `Blob` stays nil.

Call `(*Message).Normalize()` once you have finished converting a request. It
trims the numbers, defaults `Status` and `Direction`, and recomputes `Segments`.
`(*Message).EventSummary()` gives you the `event.Summary` every surface lists.

### Segment arithmetic

GSM-7 packs seven bits per character, so one segment holds 160 characters — 153
once a concatenation header is needed. Nine characters (`^ { } \ [ ~ ] |` and
`€`, plus form feed) live in the escape table and cost **two** septets each. One
character outside GSM-7 forces the whole message to UCS-2: 70 code units per
segment, 67 concatenated, and a non-BMP character such as an emoji costs two.

Escape pairs and surrogate pairs may not straddle a segment boundary, so packing
is greedy rather than a division: 153 `€` characters are 306 septets but need
**three** segments, not two.

## API

Mounted under `/api/v1/sms/`.

| Route | Notes |
|---|---|
| `GET /messages` | newest first; `search`, `provider`, `type`, `since`, `limit`, `offset`, plus `to`, `from`, `status`, `direction`, `encoding`, `mms` |
| `GET /messages/{id}` | one message; 404 for an unknown id or an event of another plugin |
| `GET /messages/{id}/media/{idx}` | streams an MMS attachment out of the blob store with its recorded `Content-Type` and range support |
| `DELETE /messages` | clears every captured SMS; 204 |

Each entry is a `MessageEnvelope`: the event id, timestamp, provider, type and
`Meta`, plus the message and a `media[]` array whose `url` either streams from
tommy or is the provider's own link (`stored` says which).

## UI

`/ui/sms/` — conversations on the left, the open thread as bubbles on the right,
a segment + encoding badge under every message, thumbnails for image media, and
live updates over SSE on `sms.message`. `GET /ui/sms/events/{id}` is left to the
core, so any message also opens in the generic raw inspector.

## How to test

No provider ships yet — the Twilio provider is Wave 2 — so the runnable snippets
appear once one is enabled. From a cold start:

```bash
tommy serve                 # then open http://localhost:8811/ui/sms/
tommy providers sms         # descriptions, endpoints and snippets for this build
curl -s http://localhost:8811/api/v1/sms/messages
```

The package's own tests drive a test-only fake provider that both accepts JSON
over the ingress and injects messages straight into the store:

```bash
go test ./plugins/sms/...
go test -race ./plugins/sms/...
```

They cover the canonical model, the segment table (both alphabets, the escape
table, the 160/161 and 70/71 boundaries, the 153/67 concatenation thresholds and
emoji forcing UCS-2), every API route including media streaming and range
requests, the conversation grouping, and the tab itself with `httptest` +
`goquery` — including that a message body is never rendered as HTML.
