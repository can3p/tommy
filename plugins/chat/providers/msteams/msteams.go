// Package msteams imitates the two generations of Microsoft Teams incoming
// webhook that are actually in use: the (retired but still widely deployed)
// Office 365 / Microsoft 365 connector webhook, whose payload is a
// MessageCard, and the current Power-Automate-backed "Post to a channel when
// a webhook request is received" workflow trigger, whose payload is an
// Adaptive Card wrapped in a Bot-Framework-shaped envelope. Both post to a
// URL shaped like:
//
//	https://<tenant>.webhook.office.com/webhookb2/{guid}@{tenant}/IncomingWebhook/{id}/{key}
//
// tommy mounts the path portion of that URL on the shared ingress. The URL
// itself is the only credential either generation presents, so every
// component is recorded on Event.Meta and nothing is ever rejected for it.
//
// The two generations answer differently on success - a connector webhook
// replies with the literal text "1", a workflow trigger replies "202
// Accepted" - which is exactly the kind of detail a hand-rolled fake gets
// wrong. This provider tells them apart by payload shape: a body carrying
// "@type":"MessageCard" is a connector card, anything else (an Adaptive Card
// envelope or the bare {"text":"..."} a workflow also accepts) is a workflow
// trigger. See handler.go for the full decision.
//
// Bot Framework (POST /v3/conversations/{id}/activities) needs an OAuth token
// exchange and is deliberately out of scope; see docs/implementation-plan.md
// §12.
package msteams

import (
	"net/http"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/chat"
)

// ProviderName is this provider's name: the URL segment it is listed under
// and the value stamped on every event it produces.
const ProviderName = "msteams"

// MaxBody caps the request body at Microsoft's own documented limit for a
// Teams message, 28KB - exceeding it is a real error condition on the live
// endpoint, not just an internal safety net.
const MaxBody = 28 * 1024

// WebhookPath is the route this provider mounts.
//
// The real URL's first segment after webhookb2 is "{guid}@{tenant}" - two
// identifiers separated by an "@", not a "/" - so it is one path segment, not
// two. net/http's ServeMux requires a wildcard to occupy a whole segment (the
// twilio provider hit the same rule from the other direction: a wildcard
// cannot share a segment with a literal suffix such as ".json"), and a single
// wildcard is exactly what this segment needs: {GuidTenant} captures
// "guid@tenant" whole, and the handler splits it on "@" itself.
const WebhookPath = "/webhookb2/{GuidTenant}/IncomingWebhook/{ID}/{Key}"

// Provider is the Microsoft Teams incoming-webhook fake.
type Provider struct{}

// New returns the Microsoft Teams provider.
func New() *Provider { return &Provider{} }

// Name implements plugin.Provider.
func (p *Provider) Name() string { return ProviderName }

// Plugin implements plugin.Provider.
func (p *Provider) Plugin() string { return chat.PluginName }

// Description implements plugin.Provider.
func (p *Provider) Description() string {
	return "Imitates a Microsoft Teams incoming webhook: the retired Office 365 connector MessageCard format " +
		"and the current Power Automate Adaptive Card workflow trigger, both on the real webhookb2 URL shape, " +
		"each answered with its own generation's success contract."
}

// Endpoints implements plugin.Provider.
func (p *Provider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{{
		Method: "POST",
		Path:   WebhookPath,
		Description: "Accept a Teams incoming-webhook POST - a MessageCard or an Adaptive Card wrapped in a " +
			"message envelope - convert it into one chat.message event, and answer '1' as text/plain for a " +
			"connector card or 202 Accepted for a workflow trigger.",
	}}
}

// Snippets implements plugin.Provider.
func (p *Provider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{
		{
			Title: "Post a MessageCard (Office 365 connector webhook)",
			Lang:  "bash",
			Code: `curl -si {{.IngressURL}}/webhookb2/11111111-1111-1111-1111-111111111111@22222222-2222-2222-2222-222222222222/IncomingWebhook/33333333333333333333333333333333/44444444-4444-4444-4444-444444444444 \
  -H 'Content-Type: application/json' -d '{
  "@type": "MessageCard",
  "@context": "https://schema.org/extensions",
  "summary": "Build failed",
  "themeColor": "FF0000",
  "title": "Build #482 failed",
  "text": "It works.",
  "sections": [{
    "activityTitle": "deploy-bot",
    "activitySubtitle": "2 minutes ago",
    "facts": [{"name": "Branch", "value": "main"}]
  }],
  "potentialAction": [{
    "@type": "OpenUri",
    "name": "View build",
    "targets": [{"os": "default", "uri": "https://example.com/build/482"}]
  }]
}'`,
		},
		{
			Title: "Post an Adaptive Card (Power Automate workflow trigger)",
			Lang:  "bash",
			Code: `curl -si {{.IngressURL}}/webhookb2/11111111-1111-1111-1111-111111111111@22222222-2222-2222-2222-222222222222/IncomingWebhook/33333333333333333333333333333333/44444444-4444-4444-4444-444444444444 \
  -H 'Content-Type: application/json' -d '{
  "type": "message",
  "attachments": [{
    "contentType": "application/vnd.microsoft.card.adaptive",
    "content": {
      "$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
      "type": "AdaptiveCard",
      "version": "1.4",
      "body": [{"type": "TextBlock", "text": "It works.", "weight": "bolder"}]
    }
  }]
}'`,
		},
	}
}

// RegisterIngress implements plugin.Provider.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()
	mux.HandleFunc("POST "+WebhookPath, func(w http.ResponseWriter, r *http.Request) {
		handleWebhook(w, r, d)
	})
}
