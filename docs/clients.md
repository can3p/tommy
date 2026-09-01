# Pointing official SDKs at tommy

Every snippet below is written against the ports in the repo's own
`tommy.toml`: `bind = "127.0.0.1"`, `host = "localhost"`, ingress on `8822`.
Run `tommy providers` (or check your own config) if you have moved a port —
every value here is otherwise exactly what a fresh `tommy serve` binds.

Three SDKs, three different amounts of cooperation:

| SDK | Base URL support | What you do |
|---|---|---|
| **mailjet-apiv3-go** (`github.com/mailjet/mailjet-apiv3-go/v4`) | First class | Pass the base URL as the third argument to `NewMailjetClient`, or call `SetBaseURL`/`SetURL` after construction |
| **sendgrid-go** (`github.com/sendgrid/sendgrid-go`) | First class, via a different entry point | Build the request with `sendgrid.GetRequest(key, endpoint, host)` instead of `sendgrid.NewSendClient(key)`, which hardcodes the real host |
| **twilio-go** (`github.com/twilio/twilio-go`) | **None** | There is no field, flag or env var that lets `api.twilio.com` become anything else. Inject a custom `*http.Client` instead, via [`clienthelp`](../clienthelp) |

`clienthelp` is tommy's own package for the twilio-go case and anything like
it: an SDK that accepts a custom `*http.Client` but refuses a custom base URL.
It is pure standard library — importing it never pulls a vendor SDK into your
own `go.mod`, and it never gets pulled into tommy's. See the package doc in
[`clienthelp/clienthelp.go`](../clienthelp/clienthelp.go) for how the rewrite
works. **Do not point a `clienthelp` transport at a real vendor host** — it
redirects every request that passes through it, unconditionally.

## Mailjet — `mailjet-apiv3-go`

`NewMailjetClient` takes the base URL as an optional third argument. The one
sharp edge: the SDK's `SendMailV31` builds its URL as `apiBase + ".1/send"`,
so the base URL must already end in `/v3` for the arithmetic to land on
tommy's mounted route, `/v3.1/send` — passing plain `http://localhost:8822`
here sends the request to `http://localhost:8822.1/send`, which tommy does
not mount.

```go
package main

import (
	"fmt"

	mailjet "github.com/mailjet/mailjet-apiv3-go/v4"
)

func main() {
	// Any public/private key pair is accepted; tommy just records it.
	client := mailjet.NewMailjetClient("any-key", "any-secret", "http://localhost:8822/v3")

	messages := mailjet.MessagesV31{
		Info: []mailjet.InfoMessagesV31{{
			From:     &mailjet.RecipientV31{Email: "alice@example.com", Name: "Alice"},
			To:       &mailjet.RecipientsV31{{Email: "bob@example.com"}},
			Subject:  "Hello from tommy",
			TextPart: "It works.",
		}},
	}

	res, err := client.SendMailV31(&messages)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%+v\n", res)
}
```

A client already built for production also works, unchanged — call
`client.SetBaseURL("http://localhost:8822/v3")` (or the equivalent
`SetURL`) in a test's setup and put it back (or just construct a second
client) for anything that must reach the real Mailjet API.

## SendGrid — `sendgrid-go`

`sendgrid.NewSendClient(key)` hardcodes `https://api.sendgrid.com` and has no
override. The documented way around it is one level down: build the
`rest.Request` yourself with `sendgrid.GetRequest`, which takes the host as
its third argument, and hand that to `sendgrid.API`.

```go
package main

import (
	"fmt"

	"github.com/sendgrid/sendgrid-go"
	"github.com/sendgrid/sendgrid-go/helpers/mail"
)

func main() {
	m := mail.NewV3MailInit(
		mail.NewEmail("Alice", "alice@example.com"),
		"Hello from tommy",
		mail.NewEmail("Bob", "bob@example.com"),
		mail.NewContent("text/plain", "It works."),
	)

	// Any bearer token is accepted; tommy just records it.
	req := sendgrid.GetRequest("SG.fake-key", "/v3/mail/send", "http://localhost:8822")
	req.Method = "POST"
	req.Body = mail.GetRequestBody(m)

	resp, err := sendgrid.API(req)
	if err != nil {
		panic(err)
	}
	// tommy answers with the real contract: 202, empty body, X-Message-Id set.
	fmt.Println(resp.StatusCode, resp.Headers["X-Message-Id"])
}
```

If your code is already built around `sendgrid.NewSendClient`, the smallest
change is to replace it with the `GetRequest`/`API` pair above only in the
test path — `Client` is just `struct{ rest.Request }`, so `&sendgrid.Client{
Request: req}` gets you back a value with the same `.Send(...)` method if you
need to keep that call shape.

## Twilio — `twilio-go`

`RequestHandler.BuildUrl` reparses whatever host you give it and rebuilds it
as `product[.edge][.region].twilio.com`; `TWILIO_EDGE`/`TWILIO_REGION` are the
only knobs, and neither one can produce a non-`twilio.com` host. The supported
extension point is a custom `client.BaseClient` — concretely, a
`*twilio-go/client.Client` with its own `*http.Client` — passed through
`twilio.NewRestClientWithParams`. That `*http.Client` is exactly what
`clienthelp.HTTPClient` builds.

```go
package main

import (
	"fmt"

	"github.com/twilio/twilio-go"
	twclient "github.com/twilio/twilio-go/client"
	openapi "github.com/twilio/twilio-go/rest/api/v2010"

	"github.com/can3p/tommy/clienthelp"
)

func main() {
	accountSid := "ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	authToken := "authtokenxxxxxxxxxxxxxxxxxxxxxxxx" // any value; tommy records it

	tc := &twclient.Client{
		Credentials: twclient.NewCredentials(accountSid, authToken),
		HTTPClient:  clienthelp.HTTPClient("http://localhost:8822"),
	}
	tc.SetAccountSid(accountSid)

	rc := twilio.NewRestClientWithParams(twilio.ClientParams{Client: tc})

	params := &openapi.CreateMessageParams{}
	params.SetTo("+15558675310")
	params.SetFrom("+15557122661")
	params.SetBody("It works.")

	msg, err := rc.Api.CreateMessage(params)
	if err != nil {
		panic(err)
	}
	fmt.Println(*msg.Sid, *msg.Status)
}
```

Six lines actually do the work: build a `*twclient.Client` with its
`HTTPClient` field set to `clienthelp.HTTPClient(tommyURL)`, set its account
SID, and pass it as `ClientParams.Client`. Everything else — `rc.Api`,
`CreateMessage`, the generated params types — is unmodified twilio-go; it has
no idea its requests are landing on tommy instead of `api.twilio.com`.

The same `tc.HTTPClient` swap is the general answer for any other twilio-go
resource (Verify, Lookups, Voice, …) once tommy grows a fake for it — nothing
about the wiring above is specific to Messages.

## Non-Go SDKs

Most vendor SDKs in other languages have twilio-go's problem to some degree —
official Twilio helper libraries in particular tend to hardcode
`api.twilio.com` the same way. Two routes, in order of preference:

1. **The library's own custom-HTTP-client hook, if it has one.** Ruby, Python,
   Node and Java's Twilio SDKs all accept an injectable HTTP client or
   transport at construction, the same shape as `clienthelp` fills for Go —
   check that library's docs for how to point it at `http://localhost:8822`
   before reaching for the next option. Mailjet's and SendGrid's non-Go SDKs
   generally take a base URL directly, mirroring their Go SDKs above.

2. **A hosts-file entry plus TLS**, for a library that truly has no hook. Map
   `api.twilio.com` (or whichever host is hardcoded) to `127.0.0.1` in
   `/etc/hosts`, and run tommy behind TLS with a certificate the test
   environment trusts, so the SDK's HTTPS request lands on tommy without
   noticing. **tommy does not have a TLS mode yet** — this route needs the
   `--tls` flag / `[tls]` config block on the implementation roadmap, not
   something you can use today. Track it before relying on this path.

In practice most non-Go test suites talk to the vendor's raw HTTP API rather
than a generated SDK, and that always works against tommy with nothing
special: point whatever HTTP client the test uses at `http://localhost:8822`
and the request paths are identical to the real API (`/v3.1/send`,
`/v3/mail/send`, `/2010-04-01/Accounts/{sid}/Messages.json`, …).
