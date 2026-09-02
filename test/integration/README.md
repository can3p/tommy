# test/integration

This is where the official vendor SDKs — `mailjet-apiv3-go/v4`, `sendgrid-go`,
`twilio-go` — get pointed at a live tommy and asked to do real work.

Every other test in this repository drives tommy with a hand-built HTTP
request (the plugin/provider unit tests) or with the raw wire format an SDK
would produce (`test/e2e`). Neither ever imports a vendor SDK, so neither
would catch a response-shape mismatch the SDK's own JSON decoder would choke
on — a quoted integer where the SDK expects a bare one, a field that must be
`null` rather than absent, a status code the client library treats as an
error. This module closes that gap: it is the proof that tommy's fakes are
faithful enough for the real client library, not just for a curl command that
happens to match the docs.

## Why a separate Go module

tommy's headline promise is a single, dependency-free binary. Importing any
of these SDKs from tommy's own `go.mod` would pull all three — and their own
transitive dependencies — into every build and into anyone else's `go.mod`
who imports tommy as a library. So this directory is its own module:

- `go.mod` declares `module github.com/can3p/tommy/test/integration` and
  `require`s `github.com/can3p/tommy` back via
  `replace github.com/can3p/tommy => ../..`, so it always tests the working
  tree rather than a published release.
- A nested module is automatically excluded from the root module's `./...`,
  so `go build ./...`, `go test ./...` and `golangci-lint run ./...` at the
  repo root never see these imports, and the root `go.mod`/`go.sum` never
  gain a line because of anything in here.
- Every test file carries `//go:build integration`, so this module builds
  (and runs zero tests) even without the tag — the SDKs are only compiled in
  when a caller opts in.

## Running it

```sh
cd test/integration
go test -tags integration ./...
```

Each test boots a real tommy in-process (`core/testutil`, `plugins/all`) on
ephemeral ports, wires the real SDK at it exactly as
[`docs/clients.md`](../../docs/clients.md) documents — including
`clienthelp` for twilio-go, which has no base-URL override at all — sends
something real, and asserts both that the SDK is satisfied by tommy's
response and that tommy's event store actually captured it correctly.

## What's covered

- **Mailjet** (`mailjet_test.go`) — `NewMailjetClient` with the base URL
  ending in `/v3` (the SDK's `SendMailV31` builds its own URL as
  `apiBase + ".1/send"`), a multi-recipient send, a multi-message fan-out,
  and an attachment read back byte-for-byte from the blob store.
- **SendGrid** (`sendgrid_test.go`) — `sendgrid.GetRequest` + `sendgrid.API`
  with a payload built entirely from the SDK's own `mail` helpers, checking
  the real 202/empty-body/`X-Message-Id` contract and a personalizations
  fan-out with a shared attachment.
- **Twilio** (`twilio_test.go`) — a `client.Client` carrying
  `clienthelp.HTTPClient(ingressURL)`, passed through
  `twilio.NewRestClientWithParams`. `CreateMessage`'s response is checked
  field by field against the SDK's generated struct, including the two
  easy-to-miss shapes (`num_segments`/`num_media` as quoted strings,
  `error_code`/`error_message`/`price` as JSON `null`). `FetchMessage` and
  `ListMessage` then read the same message back through the SDK on two
  different routes — the strongest fidelity check in the suite.
- **SMTP** (`smtp_test.go`) — stdlib `net/smtp` delivering a real MIME
  multipart message with an attachment.
- **FCM** (`fcm_test.go`) — `google.golang.org/api/fcm/v1`, the discovery-
  document-generated Go client (none of the Firebase Admin SDKs expose a
  custom-endpoint hook, so this is the closest thing to an official Go SDK
  FCM has), pointed at tommy with `option.WithEndpoint` +
  `option.WithoutAuthentication`. Covers a notification-plus-android-override
  send decoded back into the SDK's own `fcm.Message` response type, a
  data-only send confirmed silent, and a request with no target field
  confirmed to surface as the SDK's own `*googleapi.Error` rather than a
  decode panic. Boots the `push` plugin directly (`push.New(fcm.New())`)
  rather than through `startTommy`/`all.Plugins()`, since `push` is not yet
  wired into `plugins/all` (see `plugins/push/README.md`).

## Fidelity findings

None from these tests themselves. Every SDK, unmodified, sent and read back
real data on the first passing run against its provider - no gap in a
provider fake needed a workaround here. (The FCM provider did have a real
fidelity bug during development, caught by fetching and parsing the live
discovery document directly rather than assuming: FCM v1 is a proto3-backed
API, so per the proto3 JSON mapping spec both camelCase - `collapseKey`,
the discovery document's canonical spelling - and the underscore form -
`collapse_key` - are valid input, and the provider's wire structs originally
matched only the former, silently dropping any field a client sent
snake_case. Fixed with a general key-normalizing pass rather than a
per-field patch; `fcm_test.go`'s SDK send here confirms the camelCase side
of that from the wire direction. See
`plugins/push/providers/fcm/README.md`.) See each
test file's doc comment for exactly what it proves and why.
