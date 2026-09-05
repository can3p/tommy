# push

## What it is

Captures the push notifications your backend sends instead of handing them to
Apple or Google, and shows each one the way a phone would: a lock-screen card
with the title, subtitle and body, next to the payload exactly as it was
posted. Two providers ship: `fcm` for Firebase Cloud Messaging's HTTP v1 send
API, and `apns` for Apple's HTTP/2 provider API. Both land in the same
`push.Message` model but are kept apart rather than pretended to be one thing
— a device token and a topic are not the same kind of address, and the tab
says which one a given push used.

## What it's for

Your backend claims it sent a silent background push, and the question that
actually matters is whether it would have shown the user anything — most
silent-push debugging comes down to exactly that, and it is not answerable
from a JSON body alone. Or: you want to see the title and body a phone's lock
screen would render, without a physical device, an Apple developer account,
or a Firebase project in the loop. Or: a CI test places an order and asserts
that exactly one notification went to the right device token — no polling an
emulator, no mocking the SDK, just a request tommy captured and a filter on
its target. The tab answers "does this push display anything at all?" in one
word, styles a silent card so it can't be mistaken for a real notification,
and shows the payload verbatim next to that verdict.

## How to test it for real

Boot it on its own (or add `push` to a `tommy serve` config):

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . push --ui-port 8811 --in-port 8822
```

Send an FCM notification to a topic:

```bash
curl -s -X POST http://localhost:8822/v1/projects/my-project/messages:send \
  -H 'Authorization: Bearer any-oauth-access-token' \
  -H 'Content-Type: application/json' \
  -d '{"message":{"topic":"weather","notification":{"title":"Storm warning","body":"Batten down the hatches"}}}'
```

Send an APNs alert. APNs is HTTP/2 only — Apple retired the binary protocol
and there is no HTTP/1.1 form — so the client has to speak prior-knowledge
h2c, which is what `--http2-prior-knowledge` does; the ingress serves
cleartext HTTP/2 by default:

```bash
curl -s -i --http2-prior-knowledge -X POST \
  http://localhost:8822/3/device/00fc13adff785122b4ad28809a3420982341241421348097878e577c991de8f0 \
  -H 'apns-topic: com.example.MyApp' -H 'apns-push-type: alert' \
  -d '{"aps":{"alert":{"title":"Game Request","body":"Bob wants to play poker"}}}'
```

`--http2` alone is not the same thing: without prior knowledge there is no TLS
to negotiate ALPN over and no `Upgrade: h2c` support (RFC 9113 removed that
handshake), so the request quietly falls back to plain HTTP/1.1. tommy still
captures it — the response carries `Tommy-Warning: this request arrived over
HTTP/1.1; APNs is HTTP/2 only` and the event is tagged accordingly — but a
real Apple client would never do this, so the capture is flagged rather than
treated as a normal push.

Read the captures back either through the plugin's own filtered API or the
generic event feed:

```bash
curl -s http://localhost:8811/api/v1/push/messages | jq '.[].message.target'
curl -s 'http://localhost:8811/api/v1/events?plugin=push' | jq length
```

Or open the tab directly: `http://localhost:8811/ui/push/`.

`plugins/push/providers/fcm/README.md` and `plugins/push/providers/apns/README.md`
cover each provider's own wire format, error shapes, and what real vendor
documentation contradicted — including what each one deliberately refuses to
fake, since neither can be honest about delivery state it does not have.

## Two ecosystems, neither one's vocabulary

Every field of `push.Message` is named in neutral English and documents the wire
field it comes from on each side. Where only one side has a concept, the doc
comment says so, so that a provider author does not reach for the nearest-looking
neighbor on the other side.

```go
type Message struct {
    Kind     Kind            // notification | silent | empty — does the device show anything
    PushType string          // what the sender declared: apns-push-type. FCM has none.
    App      string          // apns-topic (a bundle ID!) or the Firebase project
    Target   Target          // where it went, and which wire location said so
    Alert    *Alert          // what displays; nil is what makes a push silent
    Data     json.RawMessage // the app-directed payload, verbatim
    Delivery Delivery        // priority, expiry, collapse key
    Payloads []Payload       // the request verbatim, tagged by Format
}
```

### Targeting is not "a recipient"

| | APNs | FCM |
|---|---|---|
| where | the request path, `POST /3/device/{token}` | the body |
| what | one device | exactly one of `token`, `fid`, `topic`, `condition` |
| fan-out | never | a topic or condition reaches every subscriber |

`Target.Kind` is `device`, `topic` or `condition`; `Target.Source` is the wire
location — `"path"` for APNs, and the body field's own name for FCM. That is
what keeps a deprecated `token` distinguishable from an `fid`, and what lets the
tab say *where in the request* the address was read from.

**`apns-topic` is not a topic.** It is the app's bundle ID. It goes in
`Message.App`. Mapping it onto `TargetTopic` is the easiest way to get this
model wrong.

### Notification, silent, empty

`Kind` is about the effect, not about what the sender called it:

- `notification` — carries alert text, a badge or a sound. Apple's own line is
  that those three are the keys that "interact with the user".
- `silent` — displays nothing; the app is woken to handle the payload.
- `empty` — displays nothing and carries nothing. Almost always a mistake, and
  named so the tab can say so.

`Normalize` derives it when a provider leaves it blank. **One case a provider
must set itself:** an APNs background push (`content-available: 1`) with no
custom keys carries no alert and no data, so the rule would call it `empty`. An
APNs provider that sees `content-available` — or `apns-push-type: background` —
sets `KindSilent` explicitly.

### Priority is normalized; expiry is not

Priority is mapped onto `high` / `normal` / `low` by `push.PriorityOf`, because
a badge has to mean the same thing whichever provider captured the message and a
list cannot show three vocabularies. `Delivery.PriorityRaw` keeps what the sender
actually wrote, because "did this go out at 5 or at 10" is the question.

Expiry is **not** converted, because the two ecosystems state it in incompatible
terms and converting needs a "since when" that a catcher does not know:

- APNs `apns-expiration` is an absolute UNIX epoch → `Expiry.At`.
- FCM `android.ttl` is a duration (`"3600s"`) → `Expiry.TTLSeconds`.
- Both spell "try once, do not store" as zero → `Expiry.Immediate`, the one
  shared meaning neither vocabulary owns.

Use `push.ExpiresAt` and `push.ExpiresAfter` rather than filling the struct in
by hand; the zero sentinel is why `Expiry` cannot simply be a `*time.Time`.

`Delivery.CollapseKey` **is** normalized — `apns-collapse-id` and
`android.collapse_key` (equivalently `collapseKey` - FCM v1 accepts both
spellings, see below) are the same mechanism. Note that this is unrelated to
`Alert.Category` (APNs `aps.category`, which decides the action buttons), and
unrelated to `android.notification.notification_priority`, which is display
prominence rather than delivery urgency.

### The payload is kept verbatim

`Payloads` holds the request untouched behind a `Format` discriminator, the same
shape the `chat` plugin uses for Block Kit and Adaptive Cards. The first entry is
the vendor's own body; a provider appends the per-platform blocks it found inside
it (`fcm.v1.android`, `fcm.v1.apns`, `fcm.v1.webpush`) so a reader sees them
called out. An unmodeled key is still captured, still shown in the inspector and
still copyable — which is what lets a provider ship before the model understands
every field.

## API

Mounted at `/api/v1/push/`.

| Route | Notes |
|---|---|
| `GET /messages` | the core's `?plugin=…&search=…&since=…&limit=&offset=` plus the filters below |
| `GET /messages/{id}` | one push |
| `GET /messages/{id}/raw` | the request body exactly as it arrived, `text/plain` + `nosniff` |
| `DELETE /messages` | clear |
| `DELETE /messages/{id}` | delete one |

Every message carries a `url`: the link to that event's own page in the UI, so
a client that just posted something can open what it sent.

Push-specific filters: `displays=true|false`, `kind`, `target_kind`, `target`
(substring), `app`, `push_type`, `priority` (level or raw), `data_key`. Paging is
applied after them, so a `limit` never counts messages the filter excluded.

Each envelope carries `displays` and `explain` alongside the model, so a client
asserting "this push should not have displayed" does not have to reimplement the
rule.

## Security

Everything captured is untrusted. Titles, bodies, bundle IDs, device tokens and
arbitrary data keys all reach the page as plain strings through `html/template`;
nothing on this tab is ever rendered as `template.HTML`. `Alert.Image` is a URL
the sender chose, so it is shown **as text** and never as an `<img>` source or an
`href` — tommy does not fetch it and the page must not make a browser fetch it
either. The hostile-input suite asserts against the parsed document rather than
grepping the HTML.

## Getting pushes in during development

See "How to test it for real" above for the two providers' own request
shapes. The plugin's own unit tests drive it through a test-only provider in
`fake_test.go`, which is the worked example of how the model is meant to be
filled in from each of the two request shapes without depending on either
provider package.

## What live vendor documentation contradicted

Checked against the FCM discovery document at
`https://fcm.googleapis.com/$discovery/rest?version=v1` and Apple's *Generating a
remote notification* and *Sending notification requests to APNs*:

- FCM's target union has **four** members, not three. `token` is marked
  deprecated in favor of `fid`, a Firebase Installation ID; both are still
  accepted.
- `apns-push-type` has **eleven** values, including `controls` and `widgets`,
  which is why `PushType` is a free string rather than an enum.
- The plan grouped "category/collapse key" as though they were one concept under
  two names. They are unrelated: `aps.category` decides action buttons,
  `apns-collapse-id` / `collapse_key` supersede an undelivered message.
- APNs has no image key at all; the image in the model is FCM's.
- FCM has no subtitle anywhere; the subtitle in the model is Apple's.

## FCM field spellings

FCM v1 is proto3-backed, so its JSON parser accepts **both** the canonical
lowerCamelCase name and the original snake_case proto field name —
`collapseKey` and `collapse_key` are the same field, likewise `clickAction` /
`click_action` and `validateOnly` / `validate_only`. Google's discovery document
lists only the camelCase form because that is the canonical *output* name; it
does not mean snake_case is rejected, and snake_case here is **not** the
deprecated Legacy API. This document uses whichever spelling reads more clearly.

A provider must accept both, and accepting only one is a silent-data-loss bug
rather than a cosmetic one: the request still returns 200 and the field simply
disappears from the capture. The `fcm` provider normalises the spelling of known
keys before decoding, while leaving caller-owned `data`, `headers` and `payload`
keys untouched — renaming someone's own `user_id` key would corrupt the very
thing they came to inspect.
