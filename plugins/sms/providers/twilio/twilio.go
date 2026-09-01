// Package twilio imitates Twilio's Programmable Messaging REST API well enough
// that the twilio-go SDK, or any HTTP client pointed at tommy's ingress, can
// send a message and read it back exactly the way it would against the real
// api.twilio.com.
//
// It owns one path namespace, "/2010-04-01/Accounts/...", and never imports
// another provider's package. Everything Twilio-specific - the Sid, the
// account Sid, the api_version, the resource URIs, the presented credentials -
// lives in Event.Meta; the canonical sms.Message it builds carries only what
// any SMS API has.
package twilio

import (
	"strings"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/sms"
)

// Name is the provider's URL segment and the value it reports for Name().
const Name = "twilio"

// apiVersion is what every resource reports in its api_version field, and the
// path prefix every route lives under.
const apiVersion = "2010-04-01"

// Route paths. pathFetch deliberately does not spell out the ".json" suffix
// real Twilio URLs end a Sid in: net/http's ServeMux requires a wildcard to
// occupy a whole path segment, so "{Sid}.json" cannot be registered as a
// pattern. {Sid} still captures the whole segment - "SMxxxx.json" included -
// and the handler strips a trailing ".json" itself, so a real SDK's request
// to ".../Messages/SMxxxx.json" is served exactly as it expects; only the
// self-documentation in Endpoints() renders without the suffix.
const (
	pathMessages = "/" + apiVersion + "/Accounts/{AccountSid}/Messages.json"
	pathFetch    = "/" + apiVersion + "/Accounts/{AccountSid}/Messages/{Sid}"
)

// Provider imitates the Twilio Messages resource: create, list and fetch, all
// under /2010-04-01/Accounts/{AccountSid}/Messages(.json|/{Sid}.json).
type Provider struct{}

// New returns the Twilio provider.
func New() *Provider { return &Provider{} }

// Name returns the provider's URL segment.
func (p *Provider) Name() string { return Name }

// Plugin says this provider belongs to the sms plugin.
func (p *Provider) Plugin() string { return sms.Name }

// Description says what this fakes and why.
func (p *Provider) Description() string {
	return "Imitates Twilio's Programmable Messaging REST API: accepts the form-encoded Message resource " +
		"create call, computes real GSM-7/UCS-2 segment counts, and serves the same list and fetch " +
		"endpoints Twilio does, straight out of tommy's event store."
}

// Endpoints declares the three routes RegisterIngress mounts.
func (p *Provider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{
		{
			Method: "POST",
			Path:   pathMessages,
			Description: "Accept a form-encoded Message create request (To, From or MessagingServiceSid, " +
				"Body, repeated MediaUrl, StatusCallback, ...) and record it as an sms.message event.",
		},
		{
			Method:      "GET",
			Path:        pathMessages,
			Description: "List messages this provider has recorded for the account, newest first, served from the event store.",
		},
		{
			Method: "GET",
			Path:   pathFetch,
			Description: "Fetch a single message resource by its Sid, served from the event store. A real " +
				"client's trailing \".json\" (Twilio's own URL shape) is accepted and stripped.",
		},
	}
}

// Snippets returns copy-paste manual tests, rendered against the live ingress.
func (p *Provider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{
		{
			Title: "Send an SMS",
			Lang:  "bash",
			Code: `curl -s -u ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx:authtokenxxxxxxxxxxxxxxxxxxxxxxxx \
  {{.IngressURL}}/2010-04-01/Accounts/ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx/Messages.json \
  --data-urlencode 'To=+15558675310' \
  --data-urlencode 'From=+15557122661' \
  --data-urlencode 'Body=It works.'`,
		},
		{
			Title: "Send an MMS with two images",
			Lang:  "bash",
			Code: `curl -s -u ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx:authtokenxxxxxxxxxxxxxxxxxxxxxxxx \
  {{.IngressURL}}/2010-04-01/Accounts/ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx/Messages.json \
  --data-urlencode 'To=+15558675310' \
  --data-urlencode 'MessagingServiceSid=MGxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx' \
  --data-urlencode 'Body=Two pictures.' \
  --data-urlencode 'MediaUrl=https://example.com/cat.png' \
  --data-urlencode 'MediaUrl=https://example.com/dog.png'`,
		},
		{
			Title: "List and fetch it back",
			Lang:  "bash",
			Code: `curl -s -u ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx:authtokenxxxxxxxxxxxxxxxxxxxxxxxx \
  {{.IngressURL}}/2010-04-01/Accounts/ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx/Messages.json`,
		},
	}
}

// RegisterIngress mounts the three routes this provider owns.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()
	mux.HandleFunc("POST "+pathMessages, p.create(d))
	mux.HandleFunc("GET "+pathMessages, p.list(d))
	mux.HandleFunc("GET "+pathFetch, p.fetch(d))
}

// accountBase is the path prefix every resource and subresource URI for an
// account is built from.
func accountBase(accountSid string) string {
	return "/" + apiVersion + "/Accounts/" + accountSid
}

// idFromSid recovers the underlying event.ID from a Sid this provider minted:
// a two-letter prefix (SM for SMS, MM for MMS) followed by the event id
// itself, e.g. "SMtest-id-001". A trailing ".json" - what a real client
// actually sends, and what the {Sid} wildcard captures along with it - is
// stripped first. A Sid this provider did not mint - a typo, a probe from
// another vendor's test - never round-trips through this, so a fetch on it
// correctly reports 404 rather than panicking on a slice bound.
func idFromSid(sid string) (string, bool) {
	sid = strings.TrimSuffix(sid, ".json")
	if len(sid) <= 2 {
		return "", false
	}
	prefix := sid[:2]
	if prefix != "SM" && prefix != "MM" {
		return "", false
	}
	return sid[2:], true
}

// sidFor mints the Sid for a message: MM for MMS, SM for a plain SMS, matching
// the real API's own prefixing.
func sidFor(id string, mms bool) string {
	if mms {
		return "MM" + id
	}
	return "SM" + id
}
