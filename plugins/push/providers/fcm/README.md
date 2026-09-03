# `fcm` provider

## What it is

Imitates [Firebase Cloud Messaging's HTTP v1 send
API](https://firebase.google.com/docs/cloud-messaging/send-message):
`POST /v1/projects/{project}/messages:send`, mounted on the shared ingress
exactly as the real API paths it. Plain HTTP/1.1-compatible JSON with an
OAuth2 bearer token - no HTTP/2 needed, unlike the `apns` provider that
follows it. It records which of the four addressing fields was actually used,
lifts the `android`/`apns`/`webpush` override blocks out as their own
inspectable payloads, and answers with the real success or error shape so the
generated Google API client works unmodified.

## What it's for

Your backend sends push through Firebase and you want to see, without a real
device or a Firebase project, whether a given send targets the device token
you expect rather than a stale one, whether an `android` override actually
replaces the platform-independent title the way you think it does, or whether
a `data`-only message - the kind meant to wake the app silently - really
carries no visible notification. It is also where you catch the field-name
mistake this README exists to document: FCM v1 accepts both `collapseKey` and
`collapse_key` on the wire, and a client (or a hand-built test fixture) that
sends the "wrong" one still gets a `200 OK` from the real service - tommy has
to accept it too, or a passing test here would fail against Firebase itself.

## How to test it for real

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . push --ui-port 8811 --in-port 8822
```

```bash
curl -s -X POST http://localhost:8822/v1/projects/my-project/messages:send \
  -H 'Authorization: Bearer any-oauth-access-token' \
  -H 'Content-Type: application/json' \
  -d '{"message":{"topic":"weather","notification":{"title":"Storm warning","body":"Batten down the hatches"}}}'
```

```bash
curl -s http://localhost:8811/api/v1/push/messages | jq '.[0].message.target'
```

For a real client rather than `curl`, see "Driving a real SDK" below -
`google.golang.org/api/fcm/v1`, exercised end to end in
`test/integration/fcm_test.go`.

## A wire-format finding, corrected: both spellings are valid v1 input

**The discovery document's canonical field spelling is lowerCamelCase**
(`collapseKey`, `clickAction`, `channelId`, `titleLocKey`, `bodyLocKey`,
`notificationCount`, `notificationPriority`, `validateOnly`, `fcmOptions`,
...). This was verified directly: the discovery document at
`https://fcm.googleapis.com/$discovery/rest?version=v1` was fetched with
`curl` and parsed as JSON, and its `schemas.*.properties` keys are camelCase
throughout, with no exception found in `Message`, `AndroidConfig`,
`AndroidNotification`, `ApnsConfig`, `WebpushConfig` or `SendMessageRequest`.

An earlier version of this README stopped there and concluded the push
plugin core - whose `message.go` doc comments, `README.md` and
`fake_test.go` all use **snake_case** for these same fields (`collapse_key`,
`click_action`, `channel_id`, `title_loc_key`, `body_loc_key`,
`notification_count`) - was describing FCM's older, deprecated Legacy HTTP
API rather than v1. **That conclusion was wrong.** FCM v1 is a proto3-backed
Google API, and the [proto3 JSON mapping
spec](https://protobuf.dev/programming-guides/json/) is explicit: "parsers
accept both the lowerCamelCase name (or the one specified by the `json_name`
option) and the original proto field name." The discovery document shows
camelCase because that is the *canonical* JSON name it advertises for
output, not because the underscore form is rejected on input. The push
core's snake_case spelling is not the Legacy API leaking in - it is the
*other* valid spelling of the same v1 endpoint.

That distinction was not academic. This provider's wire structs originally
carried camelCase JSON tags only, and Go's `encoding/json` matches field
names case-insensitively but does **not** ignore underscores - so
`collapse_key` never matched the `collapseKey` tag. The result was the worst
failure mode a capture tool can have: a request using the equally-valid
underscore spelling was accepted with `200 OK` and the field then silently
vanished from the captured `push.Message`, with nothing in the response or
the event to say so. That directly violates `CLAUDE.md` rule 1's spirit -
never reject or silently drop something a real client may legitimately send
- and the project's standing lesson to be lenient wherever the real service
is lenient.

**Fixed with `normalizeKeys` in `wire.go`**: every JSON object is walked
recursively before being decoded into a typed wire struct, and any
snake_case key is rewritten to the camelCase spelling the structs declare -
so a struct only ever needs one tag and still matches both spellings. It is
one general function, not a per-field lookup table, so a field added to any
wire struct later is covered automatically. Two things it is careful about:

- **It never touches an opaque value.** `message.data` /`android.data` /
  `webpush.data` (the caller's own arbitrary string map), `apns.headers` /
  `webpush.headers` (real HTTP header names) and `apns.payload` (the `aps`
  dictionary plus arbitrary custom keys) are never renamed inside - those
  keys are the caller's own data, not FCM proto field names, and rewriting
  one (a caller's own `my_custom_key`) would corrupt exactly the payload
  this project exists to show verbatim. `TestDataKeysAreNeverRenamed` covers
  this.
- **It never reformats a value it does not rename.** It decodes only into
  `map[string]json.RawMessage` / `[]json.RawMessage` at each level, so an
  untouched value is spliced back byte-for-byte rather than round-tripped
  through `interface{}`. That is what lets the provider normalize a
  *separate copy* of a request for typed decoding while the bytes that
  become each `push.Payload` - and the top-level `Event.Raw.Body` - stay
  exactly what the client sent (rule 4).
- **When both spellings appear in one object, camelCase wins** - the
  discovery document's canonical spelling takes precedence over the
  snake_case entry, which is dropped. `TestBothSpellingsConflictCamelCaseWins`
  covers this.

`TestDualSpellingProducesIdenticalMessage` sends the same fixture twice, once
in each spelling (`collapseKey`/`collapse_key`, `titleLocKey`/`title_loc_key`,
`titleLocArgs`/`title_loc_args`, `bodyLocKey`/`body_loc_key`,
`bodyLocArgs`/`body_loc_args`, `notificationCount`/`notification_count`,
`validateOnly`/`validate_only`), and asserts the two decode to the same
`push.Message`.

This was reported to, and the wording above corrected by, the coordinator
tracking this wave; the push plugin core's own doc comments are being
updated separately to mention both spellings rather than only snake_case.

## Targeting

Exactly one of `token`, `fid`, `topic`, `condition` must be set; zero or more
than one both answer `400 INVALID_ARGUMENT`. `Target.Source` records which
field was actually used - `"token"`, `"fid"`, `"topic"` or `"condition"` -
matching the push core's four-member union (`token` is documented
"Deprecated: Use `fid` instead" and both are still accepted).

## Override blocks

`android`, `apns` and `webpush` are each appended as their own
`push.Payload` (`fcm.v1.android`, `fcm.v1.apns`, `fcm.v1.webpush`), verbatim,
alongside the whole message body (`fcm.v1.message`) - so a reader sees every
platform-specific block called out rather than having to dig through one
undifferentiated JSON blob.

`android.notification` **overrides** the platform-independent `notification`
field by field: only the keys it actually sets replace the corresponding
`Alert` field, matching FCM's own documented override semantics
("If present, it will override `google.firebase.fcm.v1.Notification.title`.").
`android.priority`, `android.ttl` and `android.collapseKey` feed
`Delivery.Priority`/`Delivery.Expiry`/`Delivery.CollapseKey`.
`android.notification.notificationPriority` (`PRIORITY_MIN`...`PRIORITY_MAX`,
a string enum) is deliberately **not** read into `Delivery.Priority` - it is
display prominence, not delivery urgency, per the push core's own warning on
`push.Delivery.Priority`.

`apns.payload` and `webpush.notification` are **not** merged into `Alert` at
all - they stay in their verbatim payloads only, the same choice the push
core's own `fake_test.go` makes for `apns`. A push that displays on Android
but not on iOS is usually explained by what's inside the `fcm.v1.apns`
payload, which is exactly why it is kept intact rather than flattened away.

`Message.Data` is `message.data`, overridden by `android.data` when set, else
by `webpush.data` when set - the push core's documented rule. When both an
`android` and a `webpush` override set `data`, `android` wins.

## Auth

Any `Authorization` header is accepted by default, and whatever was
presented is recorded on `Event.Meta.authorization`. Nothing is validated
unless the provider's config section pins a `bearer_token`, in which case a
missing or mismatched bearer is rejected with the standard Google API 401
body and status `UNAUTHENTICATED`:

```toml
[plugins.push.providers.fcm]
bearer_token = "fake-oauth-access-token"
```

## Success and error shapes

Success is `200 OK` with `{"name":"projects/{project}/messages/{id}"}` - the
`{id}` is tommy's own event id, prefixed `0:` to read closer to a real
message id's shape (`0:1692853925636648%...` in the wild), letting a reader
correlate the response with the event captured. Verified via real-world
example responses (Firebase's own HTTP v1 guide and several independent
worked examples); the discovery document names the response type `Message`
with the `name` field documented `projects/*/messages/{message_id}` but does
not itself show a concrete status code.

Errors use the standard Google API JSON envelope,
`{"error":{"code","message","status","details"}}` - the same shape used
across Google APIs, not something FCM invented - verified against
[Firebase's error-codes reference](https://firebase.google.com/docs/cloud-messaging/error-codes).
This provider only ever answers:

- **`400 INVALID_ARGUMENT`** - malformed JSON, a missing `message`, or
  ambiguous/absent targeting. Carries a `google.rpc.BadRequest` detail with
  `fieldViolations`, matching the documented shape for a request-shape
  problem.
- **`401 UNAUTHENTICATED`** - a pinned `bearer_token` that was not presented
  or presented wrong. Body text matches the standard Google API
  authentication-failure message, documented identically across several
  Google API products.

It deliberately **never** answers `404 UNREGISTERED`,
`403 SENDER_ID_MISMATCH`, `429 QUOTA_EXCEEDED` or the other delivery-time
codes on that same reference page. Those describe what happens when FCM
actually tries to reach a device - whether a token is still registered,
whether the sender owns it, whether the project is over quota - and tommy has
no token registry, no sender identity and no delivery pipeline to be honest
about. Inventing "this specific token is unregistered" would be tommy
deciding which requests fail, and CLAUDE.md's charter is explicit that tommy
"captures and displays what was sent... it does not simulate scenarios,
drive inbound traffic, or make policy decisions."

## `validateOnly`

`validateOnly: true` means "do not actually deliver this." tommy never
delivers anything regardless of the flag - **this provider still records the
event**, tagging `Event.Meta.validate_only = true`, rather than silently
skipping it. The value of a catcher is total capture: a test suite that sets
`validateOnly` by accident and cannot see why nothing shows up in tommy needs
that fact visible in the capture, not a request that vanished without a
trace. This mirrors the mail plugin's `mailjet` provider, which records a
`SandboxMode` send the same way (`Event.Meta.sandbox_mode`) rather than
skipping it - see `plugins/mail/providers/mailjet`.

## Running the package tests

Run the package tests, which cover a notification, a data-only (silent)
message, each targeting form including `fid`, the android/apns/webpush
override blocks landing as separate payloads, the android.data/webpush.data
override precedence, `notificationPriority` staying out of
`Delivery.Priority`, a malformed body, `validateOnly` still being recorded,
auth capture and pinning, and the real success and error shapes:

```bash
go test ./plugins/push/providers/fcm/...
```

## Driving a real SDK

Done, in `test/integration/fcm_test.go`, using `google.golang.org/api/fcm/v1`
- not one of the Firebase Admin SDKs (none of `firebase-admin` for Go, Node,
Python or Java expose a way to point their FCM client at a custom host), but
the same discovery-document-generated Go client family as `gmail/v1`,
`drive/v3` and every other `google-api-go-client` package, generated from the
exact discovery document this provider was verified against. Every client in
that family supports `option.WithEndpoint` and `option.WithoutAuthentication`
out of the box, which is what makes it pointable at tommy at all:

```go
svc, _ := fcm.NewService(ctx,
    option.WithEndpoint(inst.IngressURL),
    option.WithHTTPClient(inst.Client),
    option.WithoutAuthentication(),
)
svc.Projects.Messages.Send("projects/my-project", req).Do()
```

Three tests: a notification-plus-android-override send, decoded back into
the SDK's own generated `fcm.Message` response type with no workaround; a
data-only send, confirmed silent; and a send with no target field set, which
the SDK cannot prevent client-side (its `Message` struct's target fields are
just plain strings), confirmed to surface as the SDK's own `*googleapi.Error`
carrying the `google.rpc.BadRequest` detail - not a decode panic. The
generated client marshals `fcm.AndroidConfig{Priority: "HIGH", Ttl: "3600s",
CollapseKey: "weather"}` onto the wire as `{"priority":"HIGH","ttl":"3600s",
"collapseKey":"weather"}` (protojson's canonical output direction is always
lowerCamelCase), and tommy's own captured event shows the `collapseKey`
value landed correctly. That confirms the camelCase reading from the wire
direction, but says nothing about snake_case input on its own - no SDK in
this suite sends that spelling, so the snake_case side of the "both
spellings" finding above is covered only by this provider's own
`TestDualSpellingProducesIdenticalMessage`, not by an SDK.
