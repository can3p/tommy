// Package slack imitates two Slack posting surfaces well enough that a real
// Slack SDK, or any HTTP client pointed at tommy's ingress, can post a message
// and have it show up in the chat tab exactly the way it would land in Slack:
//
//   - POST /services/{team}/{bot}/{token} - an incoming webhook.
//   - POST /api/chat.postMessage          - the Web API's chat.postMessage.
//
// Both convert into the one canonical chat.Message. Everything Slack-specific
// - the team/bot/token path segments, the presented bearer, icon_emoji,
// mrkdwn, unfurl_links/unfurl_media, the minted ts - lives in Event.Meta, never
// in the canonical model. This package never imports another provider's
// package.
//
// chat.update and chat.delete are deliberately not implemented: core/store
// events are immutable once appended and the chat plugin has no edit/delete
// resource type, so faking either endpoint would mean returning a
// success envelope that does not actually change what the store or the chat
// tab show - exactly the "shaky" result the provider checklist warns against.
// Two correct routes beat four half-real ones.
package slack

import (
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/chat"
)

// Name is the provider's URL segment and the value it reports for Name().
const Name = "slack"

// Route paths. net/http's ServeMux wildcards each occupy exactly one path
// segment, which matches Slack's own three-segment webhook shape
// (team/bot/token) exactly.
const (
	pathWebhook     = "/services/{team}/{bot}/{token}"
	pathPostMessage = "/api/chat.postMessage"
)

// maxBody caps how much of a request this provider will read.
const maxBody = 1 << 20

// Provider imitates Slack's incoming webhooks and the chat.postMessage Web API
// method.
type Provider struct{}

// New returns the Slack provider.
func New() *Provider { return &Provider{} }

// Name returns the provider's URL segment.
func (p *Provider) Name() string { return Name }

// Plugin says this provider belongs to the chat plugin.
func (p *Provider) Plugin() string { return chat.PluginName }

// Description says what this fakes and why.
func (p *Provider) Description() string {
	return "Imitates Slack's incoming webhooks and the chat.postMessage Web API method: accepts Block Kit " +
		"blocks and legacy attachments verbatim, replies with the literal \"ok\" a real webhook returns, and " +
		"mints the {ok, channel, ts, message} envelope chat.postMessage returns, straight out of tommy's event store."
}

// Endpoints declares the two routes RegisterIngress mounts.
func (p *Provider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{
		{
			Method: "POST",
			Path:   pathWebhook,
			Description: "Accept a Slack incoming webhook payload (text, blocks, attachments, channel/username/icon " +
				"overrides, thread_ts) and record it as a chat.message event. Responds with the literal text " +
				"\"ok\" as text/plain, matching Slack's own incoming webhook contract.",
		},
		{
			Method: "POST",
			Path:   pathPostMessage,
			Description: "Imitate Slack's Web API chat.postMessage: accepts a Bearer token and either a JSON or " +
				"form-encoded body, mints a message ts, records a chat.message event, and returns Slack's own " +
				"{ok, channel, ts, message} success envelope or {ok:false, error} on failure.",
		},
	}
}

// Snippets returns copy-paste manual tests, rendered against the live ingress.
func (p *Provider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{
		{
			Title: "Post via an incoming webhook",
			Lang:  "bash",
			Code: `curl -s {{.IngressURL}}/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX \
  -H 'Content-Type: application/json' \
  -d '{"text":"It works.","channel":"#general","username":"deploy-bot"}'`,
		},
		{
			Title: "Post Block Kit blocks via an incoming webhook",
			Lang:  "bash",
			Code: `curl -s {{.IngressURL}}/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX \
  -H 'Content-Type: application/json' \
  -d '{"channel":"#general","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"*It works.*"}}]}'`,
		},
		{
			Title: "Post via chat.postMessage",
			Lang:  "bash",
			Code: `curl -s {{.IngressURL}}/api/chat.postMessage \
  -H 'Authorization: Bearer xoxb-fake-token' \
  -H 'Content-Type: application/json' \
  -d '{"channel":"C0123ABCD","text":"It works."}'`,
		},
	}
}

// RegisterIngress mounts the two routes this provider owns.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()
	mux.HandleFunc("POST "+pathWebhook, p.webhook(d))
	mux.HandleFunc("POST "+pathPostMessage, p.postMessage(d))
}
