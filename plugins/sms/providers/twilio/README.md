# twilio

## What it is

Stands in for Twilio's [Programmable Messaging REST API](https://www.twilio.com/docs/messaging/api/message-resource):
the `Messages` resource's create, list and fetch calls. It captures the
message your code posts — recipient, sender, body, media — and answers with
the same JSON shape `api.twilio.com` would, so `twilio-go`, or any HTTP client
pointed at tommy's ingress, sends and reads back exactly as it would in
production.

## What it's for

Point whatever normally calls `api.twilio.com` at tommy instead — an
environment variable or a base-URL override, see `docs/clients.md` — and you
get, without touching a real phone number or a Twilio account:

- CI assertions that a signup flow, a password reset, or an alert texts the
  right number exactly once, with the right body.
- Seeing the real segment/encoding cost of a template before it ships — this
  provider's `num_segments` is computed by the same code the `sms` tab's badge
  uses, so the two can never disagree.
- Exercising the MMS path (repeated `MediaUrl`, `MessagingServiceSid` instead
  of a bare `From`) without hosting real media anywhere.

## How to test it for real

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . sms --ui-port 18911 --in-port 18912
```

Send a message:

```bash
curl -s -u ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx:authtokenxxxxxxxxxxxxxxxxxxxxxxxx \
  http://127.0.0.1:18912/2010-04-01/Accounts/ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx/Messages.json \
  --data-urlencode 'To=+15558675310' \
  --data-urlencode 'From=+15557122661' \
  --data-urlencode 'Body=It works.'
```

which returns Twilio's own resource shape:

```json
{"account_sid":"ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx","body":"It works.","date_created":"Thu, 03 Sep 2026 16:25:06 +0000","direction":"outbound-api","error_code":null,"from":"+15557122661","num_media":"0","num_segments":"1","price":null,"sid":"SM01a06816996600017cbf20c8","status":"queued","to":"+15558675310", "...": "..."}
```

Read it back through the list and fetch routes — same account, same auth:

```bash
curl -s -u ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx:authtokenxxxxxxxxxxxxxxxxxxxxxxxx \
  http://127.0.0.1:18912/2010-04-01/Accounts/ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx/Messages.json
```

and confirm the capture landed, either through this plugin's own API or the
cross-plugin one:

```bash
curl -s "http://127.0.0.1:18911/api/v1/sms/messages"
curl -s "http://127.0.0.1:18911/api/v1/events?plugin=sms"
```

then open `http://127.0.0.1:18911/ui/sms/` to see the same message as a
conversation bubble.

**For the real vendor SDK**, `test/integration` (a separate Go module, kept
apart so `twilio-go` never enters tommy's own `go.mod`) drives `twilio-go`
itself through `clienthelp.HTTPClient` — see `twilio_test.go`,
`TestTwilioSDKCreateMessageIsParsedFaithfully`. It sends via `CreateMessage`,
then reads the same message back with `FetchMessage` and `ListMessage`, all
through the SDK's own generated structs — the strongest check that the wire
shape is right, since a mismatched type (`num_segments` as a bare number
instead of a quoted string, say) fails the SDK's own JSON decode rather than
just looking different in a raw diff. Run it with:

```bash
cd test/integration && go test -tags integration -run TestTwilioSDK ./...
```

Kill the server when done (`go run` forks a child `tommy` binary — kill that
child, not just the shell job, or the ports stay held).

## Routes

All three live under the real API's path namespace, `/2010-04-01/Accounts/...`.

| Route | Notes |
|---|---|
| `POST /2010-04-01/Accounts/{AccountSid}/Messages.json` | Create. Body is `application/x-www-form-urlencoded`, **not JSON**. `To` is required; either `From` or `MessagingServiceSid` is required; either `Body` or a repeated `MediaUrl` is required. Responds `201` with the full message resource. |
| `GET /2010-04-01/Accounts/{AccountSid}/Messages.json` | List, newest first, scoped to the `AccountSid` in the path. Served straight from tommy's event store, so a client that just created a message sees it immediately. |
| `GET /2010-04-01/Accounts/{AccountSid}/Messages/{Sid}.json` | Fetch one message by its `Sid`. Also served from the store. |

`net/http`'s router requires a wildcard to occupy a whole path segment, so the
fetch route is actually mounted as `.../Messages/{Sid}` (see `Endpoints()`).
The `{Sid}` wildcard still captures the entire segment — `SMxxxx.json`
included — and the handler strips a trailing `.json` itself, so a real
client's request to the literal Twilio URL is served exactly as it expects.

## Wire shapes, verified against the live docs

- **Create request** — form fields `To`, `From`, `MessagingServiceSid`,
  `Body`, `MediaUrl` (repeatable — one message may carry several images),
  `StatusCallback`.
- **Resource response** — `sid` (`SM…` for SMS, `MM…` once media is
  attached), `account_sid`, `api_version`, `body`, `date_created` /
  `date_sent` / `date_updated` in RFC 1123 form
  (`"Thu, 30 Jul 2015 20:12:31 +0000"`), `direction: "outbound-api"`,
  `error_code` / `error_message` / `price` / `price_unit` as JSON `null`
  (never an empty string) when absent, `from` / `messaging_service_sid` as
  `null` when the message did not use that sender kind, `num_media` /
  `num_segments` as **quoted strings** (Twilio's own API returns them as
  strings, not numbers), `status`, `subresource_uris` (`media`, `feedback`),
  `to`, `uri`.
- **List envelope** — `messages`, `page`, `page_size`, `start`, `end`,
  `uri`, `first_page_uri`, `next_page_uri`, `previous_page_uri`.
- **Errors** — Twilio's own shape and codes: `{"code":21211,"message":"…",
  "more_info":"https://www.twilio.com/docs/errors/21211","status":400}`.
  A missing `To` is `21604`, an invalid one is `21211`, a missing sender is
  `21603`, a missing body-and-media is `21602`, a bad credential pin is
  `20003` (`401`), and an unknown `Sid` is `20404` (`404`).

`num_segments` always reflects `sms.Message.Segments.Count` computed by
`Normalize()` **after** the canonical message is built — this response and
the SMS tab's badge can never disagree.

## Auth

HTTP Basic. By default any credentials (or none at all) are accepted, and
whatever was presented is recorded in `Event.Meta.basic_auth` for the audit
trail. Set `account_sid` and `auth_token` in this provider's config section to
pin real-looking credentials and start rejecting a mismatch with Twilio's own
`20003` error:

```toml
[plugins.sms.providers.twilio]
account_sid = "ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
auth_token  = "your-auth-token"
```

## Package tests

```bash
go test ./plugins/sms/providers/twilio/...
go test -race ./plugins/sms/providers/twilio/...
```

The package tests are table-driven against golden request fixtures in
`testdata/` (form-encoded bodies), and assert both the exact HTTP response
(status, headers, body) and the canonical `sms.Message` the provider actually
stored — form decoding, repeated `MediaUrl`, `MessagingServiceSid` in place of
`From`, GSM-7 vs UCS-2 segment counting, the date format, every error shape,
auth capture (default and pinned), and that list/fetch read back exactly what
create wrote without leaking another plugin's or another sms provider's
events. `plugintest.Conformance` and a full `testutil.Start` end-to-end pass
are included.
