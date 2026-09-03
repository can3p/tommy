# `apns` provider

## What it is

Imitates Apple's push provider API: `POST /3/device/{deviceToken}`, mounted
on the shared ingress exactly as Apple's own path, and served over the
ingress's cleartext HTTP/2 rather than TLS. It records every `apns-*` header
and the unverified claims of the ES256 provider token — a wrong key id or a
token generated an hour ago and never refreshed is exactly the kind of
mistake this exists to surface — and answers with the real empty-bodied
`200` plus `apns-id`, or Apple's own `{"reason":"..."}` error for a request
that is malformed on its face.

## What it's for

Your backend sends a push through the APNs provider API and you want to see
the title, subtitle and body a lock screen would actually render, without an
Apple developer account, a signing key, or a physical device anywhere in the
loop. Or you send a background push — `content-available: 1`, no alert — and
need to confirm it really is silent rather than accidentally carrying visible
text. Or a client library builds its own provider token and you want to
check, without APNs's opaque `403`, whether the `kid` it signed with is the
one you configured, or whether the token's `iat` is stale. tommy never
verifies the ES256 signature — it has no way to, and no key to check it
against — but it decodes and shows the header and claims regardless, because
a wrong key id or an hour-old token is precisely the kind of client bug that
a real `403 InvalidProviderToken` gives you no detail on.

## How to test it for real

Boot it:

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . push --ui-port 8811 --in-port 8822
```

APNs is HTTP/2 only — Apple retired the binary protocol in 2021 and never
offered an HTTP/1.1 form of this API — so the client has to speak
prior-knowledge h2c. `curl --http2-prior-knowledge` does; plain `--http2`
does not, because without TLS there is no ALPN negotiation to carry the
upgrade, and the `Upgrade: h2c` handshake was removed from HTTP/2 entirely by
RFC 9113. The ingress serves cleartext HTTP/2 by default (`[ingress] h2c` /
`--h2c`, on unless turned off), so this should work out of the box — check
your own `curl --version` for HTTP/2 support if it does not:

```bash
curl -s -i --http2-prior-knowledge -X POST \
  http://localhost:8822/3/device/00fc13adff785122b4ad28809a3420982341241421348097878e577c991de8f0 \
  -H 'apns-topic: com.example.MyApp' \
  -H 'apns-push-type: alert' \
  -H 'apns-priority: 10' \
  -H 'authorization: bearer eyJhbGciOiJFUzI1NiIsImtpZCI6IjhZTDNHM1JSWDcifQ.eyJpc3MiOiJDODZOVjlKWDNEIiwiaWF0IjoxNzU2ODAwMDAwfQ.c2lnbmF0dXJlLW5ldmVyLXZlcmlmaWVk' \
  -d '{"aps":{"alert":{"title":"Game Request","subtitle":"Five Card Draw","body":"Bob wants to play poker"},"badge":1,"sound":"default","category":"GAME_INVITATION"},"gameID":"12345678"}'
```

This answers `HTTP/2 200` with an `apns-id` and an `apns-unique-id` header
and no body — the real success shape.

A silent background push:

```bash
curl -s -i --http2-prior-knowledge -X POST \
  http://localhost:8822/3/device/00fc13adff785122b4ad28809a3420982341241421348097878e577c991de8f0 \
  -H 'apns-topic: com.example.MyApp' \
  -H 'apns-push-type: background' \
  -H 'apns-priority: 5' \
  -d '{"aps":{"content-available":1}}'
```

Read the captures back:

```bash
curl -s http://localhost:8811/api/v1/push/messages | jq '[.[] | {kind: .message.kind, displays, app: .message.app}]'
```

`http2-prior-knowledge` really is required. `curl --http2` (no
prior-knowledge) against this same cleartext endpoint silently falls back to
plain HTTP/1.1 instead of failing — there is no ALPN to negotiate over, so
curl has nothing to upgrade from. tommy still captures the request, but flags
it: the response carries `Tommy-Warning: this request arrived over HTTP/1.1;
APNs is HTTP/2 only`, and the stored event's `Meta.warning` says the same.
That is deliberate — a capture tool that dropped the request over a
transport detail would be worse than one that warns — but no real Apple
client ever falls into this path, so treat the warning as a client
misconfiguration, not a passing test.

Two client-mistake responses, both decided from the request alone:

```bash
curl -s -i --http2-prior-knowledge -X POST http://localhost:8822/3/device/abc -d '{}'
# 400 {"reason":"MissingTopic"}

curl -s -i --http2-prior-knowledge -X POST http://localhost:8822/3/device/ -H 'apns-topic: x' -d '{}'
# 400 {"reason":"MissingDeviceToken"}
```

There is no official Apple SDK for the provider API in any language — every
vendor client (`sideshow/apns2` for Go included) is community-maintained and
talks this same HTTP/2 JSON wire format, so there is nothing more "official"
to point at than the two `curl` commands above.

## The one thing `--h2c=false` changes

Turning h2c off (`[ingress] h2c = false` / `--h2c=false`) makes the ingress
HTTP/1.1-only. An HTTP/2 client then cannot connect at all — there is no
fallback negotiation on a plain TCP+HTTP/1.1 listener — while a client that
happens to speak HTTP/1.1 (like the `curl --http2` case above) still gets a
`200` with the downgrade warning. The provider does not detect the config
setting itself; it only ever sees the protocol the request actually arrived
over. `TestH2CDisabledRefusesHTTP2` pins this: an h2c-speaking client fails
outright, an HTTP/1.1 one still succeeds and is still flagged.

## What live vendor documentation contradicted

Apple's documentation pages are rendered by JavaScript, so fetching the HTML
yields a title and nothing else. The tables below were read from the JSON
each page is built from, fetched directly under
`https://developer.apple.com/tutorials/data/documentation/usernotifications/`:

- `sending-notification-requests-to-apns.json` — the request header table,
  and the **eleven** documented `apns-push-type` values: `alert`,
  `background`, `complication`, `controls`, `fileprovider`, `liveactivity`,
  `location`, `mdm`, `pushtotalk`, `voip`, `widgets`. `sideshow/apns2`, the Go
  client this provider is tested against, knows only nine — it has no
  constant for `controls` or `widgets`. Taking the list from the client
  rather than from Apple would have made this provider reject two push types
  the real service accepts, which is why `push.Message.PushType` is a free
  string and `apns-push-type` validation is checked against this file's own
  list rather than any client library's enum.
- `handling-notification-responses-from-apns.json` — the response header
  table, the status codes, and the full `reason` list. Only the reasons this
  provider can honestly produce from the request alone are implemented; see
  below for what is left out and why.
- `generating-a-remote-notification.json` — Tables 1, 2 and 3 (the `aps`,
  `alert` and `sound` dictionaries) and the payload size limits: 4 KB for a
  normal push, 5 KB for VoIP.
- `establishing-a-token-based-connection-to-apns.json` — the four JWT
  key/value pairs (`alg`, `kid`, `iss`, `iat`) and the one-hour `iat` rule.

Two things worth calling out specifically:

- **`apns-topic` is not a topic.** It is the app's bundle ID, and it lands in
  `push.Message.App`, never in `push.Target` — see the push plugin core's own
  warning on this. A provider author reaching for the nearest-looking field
  gets this wrong on the first try.
- **Apple's own worked example encodes `iat` as a JSON *string***
  (`"iat": "1459143580650"`), not a number, and that value is in
  milliseconds — despite the field being documented in seconds. `jwt.go`'s
  `issuedAt` accepts both a JSON number and a string, and treats anything
  implausibly large as milliseconds rather than seconds. A parser that
  assumed a bare JSON number in seconds would silently fail to read Apple's
  own documented example.

## What is deliberately not implemented

This provider only ever answers a fixed, small set of `reason` values, all of
them things it can determine **from the request alone**:
`MissingDeviceToken`, `BadPath`, `MethodNotAllowed`, `MissingTopic`,
`TopicDisallowed` (only when a topic is pinned), `InvalidPushType`,
`BadPriority`, `BadExpirationDate`, `BadMessageId`, `BadCollapseId`,
`DuplicateHeaders`, `PayloadEmpty`, `PayloadTooLarge`, `InvalidProviderToken`
(only when a key id is pinned), and `InternalServerError`.

It declines every reason on Apple's list that describes **delivery state
tommy does not have**: `BadDeviceToken` and `Unregistered` (`410`) need a
token registry to say whether a token is well-formed or still valid;
`DeviceTokenNotForTopic` needs to know which app a token belongs to;
`ExpiredProviderToken` (`403`) and `Forbidden`/`MissingProviderToken` need an
actual signature check against a real signing key, which tommy has no
business holding; `TooManyProviderTokenUpdates` and `TooManyRequests` (`429`)
need a request history and a rate-limit policy; `BadCertificate` and
`BadCertificateEnvironment` belong to the older certificate-based connection
method, which this provider does not implement at all (only token-based
auth); `ServiceUnavailable` and `Shutdown` describe Apple's own operational
state. Inventing any of these would be tommy deciding which requests fail —
exactly the scenario simulation `CLAUDE.md`'s charter rules out. A stale
`iat` and a mismatched `kid` are recorded on the event (`jwt.Stale`,
`jwt.Kid`) rather than rejected, for the same reason.

`410 Unregistered` also carries a documented `timestamp` field that only
ever appears on that one status code. Since this provider never answers
`410`, no error response it builds ever has a `timestamp` field — checked
directly by `TestErrorShapes`.

## Auth: recorded, never verified

Every provider token is `base64url` two segments read (header, claims) and
the third — the signature — is left untouched. There is no signing key here
to check it against, and a fake that rejected a well-formed-looking but
unverifiable token would be worse than one that shows it as-is (`CLAUDE.md`
rule 1). What is captured:

- `alg`, `kid` from the JWT header; `iss`, `iat` from the claims — the four
  key/value pairs Apple documents and no others.
- `iat` resolved to a timestamp when it parses, and a `stale` flag when it is
  more than an hour old (Apple: "APNs rejects any notifications containing
  the token, returning an ExpiredProviderToken (403) error" — this provider
  never does that rejection itself).
- A malformed token — not three dot-separated segments, not valid base64url,
  not a JSON object — is recorded with an `Error` string rather than
  discarded or rejected. `TestMalformedJWTIsAcceptedAndRecorded` covers this.

Both `authorization: bearer <token>` (Apple's own lowercase spelling, and
what `sideshow/apns2` sends) and RFC 6750's `Bearer` are accepted, along with
a bare token with no scheme at all.

Two config pins exist, both off by default, both rejecting rather than
recording when set:

```toml
[plugins.push.providers.apns]
topic  = "com.example.MyApp"   # 400 TopicDisallowed for anything else
key_id = "8YL3G3RRX7"          # 403 InvalidProviderToken for a mismatched kid
```

Neither checks a signature — `key_id` only compares the `kid` claim's
plaintext value against the pinned string.

## The payload

The `aps` dictionary is read according to Apple's documented shapes:
`alert` as either a bare string (the body text) or the title/subtitle/body/
localization dictionary; `sound` as either a bare string or the
critical-alert dictionary; `badge` as a pointer, because `0` is a real,
documented value ("Specify 0 to remove the current badge") and must not look
like an absent badge. Everything Table 1 documents that the shared
`push.Message` model has no field for — `thread-id`, `mutable-content`,
`target-content-id`, `interruption-level`, `relevance-score`,
`filter-criteria`, and the whole Live Activity key set — stays in the
verbatim payload rather than being dropped, and the handful worth filtering a
capture by are additionally lifted into `Event.Meta` (`aps_content_available`,
`aps_mutable_content`, `aps_thread_id`, `aps_interruption_level`,
`aps_critical_sound`).

**A background push needs a provider-side decision the shared model cannot
make on its own.** `content-available: 1` with no alert and no custom data
carries nothing a generic "does it have an alert or data" rule can see, so
that rule alone would call it `empty` rather than `silent` — and those mean
different things to the tab. This provider checks `content-available`
directly and sets `push.KindSilent` itself; see `push.Message.Kind`'s own
documentation for the general rule this is the one documented exception to.

A body that isn't JSON at all, or whose `aps` key holds something other than
a dictionary, is still captured whole — `TestUnparseablePayloadIsStillCaptured`
covers both. Apple documents no `reason` string for a malformed payload, so
none is invented; the request still gets a normal `200`, since a client
sending malformed JSON at APNs would (per Apple's error table) get one too
only if a *header* was also wrong, not for payload shape alone.

## Running the package tests

```bash
go test ./plugins/push/providers/apns/...
```

Covers header validation and all its error `reason`s, the success shape (an
empty body with `apns-id`/`apns-unique-id` and nothing else), an `apns-id`
supplied by the client being echoed back rather than replaced, the full
payload conversion (alert forms, sound forms, badge zero-vs-absent,
localization, custom data, the background/silent special case), JWT claim
capture without verification, a malformed JWT still being recorded, the
`topic`/`key_id` pins, and an unparseable body still being captured. A
separate suite in `h2_test.go` drives a **real HTTP/2 connection** over the
real ingress — prior-knowledge h2c, exactly as `sideshow/apns2` or `curl
--http2-prior-knowledge` would — rather than calling the handler in-process,
because a test that only proves the handler function works has not proven
this provider is reachable at all over the one protocol Apple actually
speaks. It also proves the HTTP/1.1 downgrade path is captured and flagged,
and that turning `[ingress] h2c` off actually refuses an h2c connection
rather than only claiming to.
