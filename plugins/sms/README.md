# sms

## What it is

Stands in for an SMS/MMS provider's send API — currently Twilio's Programmable
Messaging REST API. It captures the message your code posts and shows it as a
phone-style conversation, with the **segment count and encoding** it would
really cost on the wire, instead of actually delivering anything.

## What it's for

The concrete situations this tab is for:

- Checking a one-time-passcode or alert message actually fits in one segment
  before it ships — a body that looks like 158 characters in your editor is
  three billed segments the moment somebody pastes a curly quote or an emoji
  into the template, and nothing else in a test environment tells you that.
- Seeing exactly what a shortened link, an emoji, or an MMS attachment looks
  like in the delivered body, without a real phone.
- Asserting in CI that a signup flow texts the right number exactly once —
  point the vendor SDK at tommy's ingress, send, then read the capture back
  over `/api/v1/sms/messages` or the generic `/api/v1/events?plugin=sms`.

## How to test it for real

From a cold start, with no provider configured yet you still get the tab and
the API, just no ingress route to post to:

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . sms --ui-port 18911 --in-port 18912
```

```
tommy is running (sms only)
  ui       http://127.0.0.1:18911/ui/
  api      http://127.0.0.1:18911/api/v1
  ingress  http://127.0.0.1:18912
  plugin   sms ([twilio])
```

Send a message through the Twilio-shaped ingress and read it back — see
`providers/twilio/README.md` for the exact commands and what the Twilio
wire response looks like. The short version, run against the instance above:

```bash
curl -s -u ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx:authtokenxxxxxxxxxxxxxxxxxxxxxxxx \
  http://127.0.0.1:18912/2010-04-01/Accounts/ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx/Messages.json \
  --data-urlencode 'To=+15558675310' \
  --data-urlencode 'From=+15557122661' \
  --data-urlencode 'Body=Your OTP is 483920. Reply STOP to opt out.'
```

returns Twilio's own resource shape, `num_segments` and all:

```json
{"sid":"SM01a06816996600017cbf20c8","status":"queued","num_segments":"1", "...": "..."}
```

Then open `http://127.0.0.1:18911/ui/sms/` to see it as a conversation bubble
with its segment badge, or pull it back over the API:

```bash
curl -s "http://127.0.0.1:18911/api/v1/sms/messages"
curl -s "http://127.0.0.1:18911/api/v1/events?plugin=sms"
```

A body with a character outside GSM-7 — a curly quote is the classic one —
flips `encoding` to `UCS-2` and shrinks the per-segment capacity from 160 to
70, all still visible in the same read-back:

```bash
curl -s -u ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx:authtokenxxxxxxxxxxxxxxxxxxxxxxxx \
  http://127.0.0.1:18912/2010-04-01/Accounts/ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx/Messages.json \
  --data-urlencode 'To=+15558675310' \
  --data-urlencode 'From=+15557122661' \
  --data-urlencode "Body=We're confirming your order." # use a real U+2019 apostrophe to trigger it
```

which comes back with `"segments":{"count":1,"encoding":"UCS-2","units":28,"capacity":70,"remaining":42}`.

For driving the real vendor SDK rather than curl, `test/integration`'s
`twilio_test.go` (a separate Go module, so the SDK never enters tommy's own
`go.mod`) points `twilio-go` at tommy through `clienthelp.HTTPClient` and
asserts the SDK decodes `CreateMessage`/`FetchMessage`/`ListMessage` without
error — the strongest check that the response shape is real. Run it with:

```bash
cd test/integration && go test -tags integration -run TestTwilioSDK ./...
```

Kill your server(s) when done — `kill` the `go run . sms` process (it forks a
child `tommy` binary; killing the shell job alone leaves the child holding the
ports).

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
tommy or is the provider's own link (`stored` says which). The envelope's own
top-level `url` is different again: the link to that message's page in the UI.

## UI

`/ui/sms/` — conversations on the left, the open thread as bubbles on the right,
a segment + encoding badge under every message, thumbnails for image media, and
live updates over SSE on `sms.message`. `GET /ui/sms/events/{id}` is left to the
core, so any message also opens in the generic raw inspector, and the *raw* link
under a bubble goes to `/ui/events/{id}` — that message on a page of its own.

## Package tests

```bash
go test ./plugins/sms/...
go test -race ./plugins/sms/...
```

They cover the canonical model, the segment table (both alphabets, the escape
table, the 160/161 and 70/71 boundaries, the 153/67 concatenation thresholds and
emoji forcing UCS-2), every API route including media streaming and range
requests, the conversation grouping, and the tab itself with `httptest` +
`goquery` — including that a message body is never rendered as HTML.
