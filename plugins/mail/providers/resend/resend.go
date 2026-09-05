// Package resend imitates the Resend Email API (https://api.resend.com), the
// three endpoints an application actually sends mail through:
//
//	POST /emails        - send one message
//	POST /emails/batch  - send up to 100, one event each
//	GET  /emails/{id}   - read one back, served from the event store
//
// Every wire detail here was checked against the live reference
// (https://resend.com/docs/api-reference/emails/send-email, .../send-batch-emails,
// .../retrieve-email and .../errors) and against the two official SDKs, whose
// source is the only place some spellings are written down at all - notably
// resend-go, which marshals an attachment's bytes as a JSON array of integers
// rather than the base64 string the REST reference documents.
//
// Everything Resend-specific - tags, topic_id, template, scheduled_at, the
// idempotency key, the presented credentials, the id that was minted - is
// recorded on Event.Meta. The canonical mail.Message carries only what every
// mail provider agrees on.
package resend

import (
	"net/http"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/mail"
)

// ProviderName is this provider's name: the URL segment under which it is
// listed and the value stamped on every event it produces.
const ProviderName = "resend"

// The three routes this provider mounts, at the paths the real API uses.
const (
	SendPath  = "/emails"
	BatchPath = "/emails/batch"
	GetPath   = "/emails/{id}"
)

// MaxBody caps the request body. Resend documents 40MB of attachments per
// email after base64 encoding, and a batch may carry 100 emails - but a fake
// exists to show you a message, not to move a gigabyte, so this is a
// deliberate ceiling rather than the vendor's. Note that resend-go inflates
// attachment bytes about 4x (each byte becomes up to four characters of a
// JSON integer array), not the 4/3 base64 costs, so a very large attachment
// sent through that SDK hits this sooner than its size suggests.
const MaxBody = 64 << 20

// MaxBatch is the number of emails one /emails/batch request may carry, per
// the live reference: "up to 100 batch emails at once".
const MaxBatch = 100

// MaxRecipients is the documented per-field recipient cap ("Max 50") on to,
// and is applied to cc and bcc as well.
const MaxRecipients = 50

// MaxIdempotencyKey is the documented maximum length of an Idempotency-Key.
const MaxIdempotencyKey = 256

// DefaultLastEvent is what GET /emails/{id} reports in last_event. Resend
// walks an email through sent -> delivered -> opened; tommy delivers nothing
// and simulates no lifecycle, so it reports the terminal state that lets a
// client which polls for delivery proceed instead of spinning forever. Pin a
// different value with the "last_event" config key when a test wants to read
// some other state back.
const DefaultLastEvent = "delivered"

// Provider is the Resend Email API fake.
type Provider struct{}

// New returns the Resend provider.
func New() *Provider { return &Provider{} }

// Name implements plugin.Provider.
func (p *Provider) Name() string { return ProviderName }

// Plugin implements plugin.Provider.
func (p *Provider) Plugin() string { return mail.PluginName }

// Description implements plugin.Provider.
func (p *Provider) Description() string {
	return "Imitates the Resend Email API: POST /emails and POST /emails/batch capture one event per delivered " +
		"message, attachments go to the blob store, and GET /emails/{id} reads a message back out of the event " +
		"store under a Resend-shaped UUID."
}

// Endpoints implements plugin.Provider.
func (p *Provider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{
		{
			Method: "POST",
			Path:   SendPath,
			Description: "Send one email. Answers 200 with {\"id\": \"<uuid>\"} exactly as the real API does, " +
				"and records one mail.message event.",
		},
		{
			Method: "POST",
			Path:   BatchPath,
			Description: "Send up to 100 emails in one call. Answers 200 with {\"data\":[{\"id\":…}]} index-aligned " +
				"with the request, and records one event per message.",
		},
		{
			Method: "GET",
			Path:   GetPath,
			Description: "Retrieve one sent email by the id the send call returned, served from the event store so " +
				"an SDK that writes and then reads sees its own write.",
		},
	}
}

// Snippets implements plugin.Provider.
func (p *Provider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{
		{
			Title: "Send an email through the Resend API",
			Lang:  "bash",
			Code: `curl -si {{.IngressURL}}` + SendPath + ` \
  -H 'Authorization: Bearer re_fake_key' \
  -H 'Content-Type: application/json' -d '{
  "from": "Acme <alice@example.com>",
  "to": ["bob@example.com"],
  "subject": "Hello from tommy",
  "html": "<p>It <b>works</b>.</p>",
  "text": "It works."
}'`,
		},
		{
			Title: "Send a batch, one event per message",
			Lang:  "bash",
			Code: `curl -si {{.IngressURL}}` + BatchPath + ` \
  -H 'Authorization: Bearer re_fake_key' \
  -H 'Content-Type: application/json' -d '[
  {"from": "Acme <alice@example.com>", "to": "bob@example.com",   "subject": "First",  "text": "one"},
  {"from": "Acme <alice@example.com>", "to": ["carol@example.com"], "subject": "Second", "text": "two"}
]'`,
		},
		{
			Title: "Read a sent email back by its id",
			Lang:  "bash",
			Code: `id=$(curl -s {{.IngressURL}}` + SendPath + ` \
  -H 'Authorization: Bearer re_fake_key' \
  -H 'Content-Type: application/json' \
  -d '{"from":"alice@example.com","to":"bob@example.com","subject":"Round trip","text":"hi"}' \
  | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
curl -s {{.IngressURL}}/emails/$id`,
		},
	}
}

// RegisterIngress implements plugin.Provider. It only mounts routes: nothing
// is created or generated here (CLAUDE.md rule 11).
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()

	mux.HandleFunc("POST "+SendPath, func(w http.ResponseWriter, r *http.Request) {
		handleSend(w, r, d)
	})
	mux.HandleFunc("POST "+BatchPath, func(w http.ResponseWriter, r *http.Request) {
		handleBatch(w, r, d)
	})
	mux.HandleFunc("GET "+GetPath, func(w http.ResponseWriter, r *http.Request) {
		handleGet(w, r, d)
	})
}
