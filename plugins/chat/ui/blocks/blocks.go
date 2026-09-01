// Package blocks is the card-rendering seam for the chat plugin: it turns a
// Slack Block Kit array, a Slack legacy attachments array, a Teams
// MessageCard or a Teams Adaptive Card into HTML that looks like the real
// thing, so a card does not fall back to only its plain-text preview plus a
// raw JSON inspector.
//
// It deliberately does not import plugins/chat: Render has the primitive
// signature chat.RichRenderer expects (a format string and the verbatim
// JSON), which is what lets this package live under plugins/chat/ui/blocks
// without creating an import cycle. Wiring it in is one call the caller
// makes: chat.New(...).WithRichRenderer(blocks.Render).
//
// Every byte this package writes into its returned template.HTML can
// originate from the application under test - a message author, a bot name,
// a URL in a link or a button, a fact value - and lands in the page
// unescaped, so it is a stored-XSS surface if handled carelessly. The rule
// followed throughout is: never concatenate a payload string into markup
// without going through escapeHTML first (or renderMrkdwn, which escapes
// internally), and never let a payload string choose a URL scheme without it
// passing sanitizeURL's allowlist. See escape.go.
//
// Coverage is deliberately partial: a handful of element types per schema,
// picked for how often they actually show up, rather than the whole spec
// shallowly. Anything unrecognized inside a known payload is simply skipped;
// anything unrecognized at the top level (unknown format, malformed JSON,
// wrong top-level type) makes Render return false so the caller's
// text-plus-JSON-inspector fallback takes over instead of showing nothing or
// something wrong.
package blocks

import (
	"encoding/json"
	"html/template"
)

// The format discriminators Render dispatches on. These mirror the
// chat.Format constants exactly (see plugins/chat/message.go) but are
// declared here, as plain strings, because this package must not import
// plugins/chat.
const (
	formatSlackBlocks       = "slack.blocks"
	formatSlackAttachments  = "slack.attachments"
	formatTeamsMessageCard  = "msteams.messagecard"
	formatTeamsAdaptiveCard = "msteams.adaptivecard"
)

// Render turns one piece of structured chat content into HTML, implementing
// chat.RichRenderer. It returns false - never a panic, never a hang - for an
// unknown format, empty or malformed JSON, or a payload whose top level does
// not match the schema's expected shape; the caller's fallback renders the
// text preview and JSON inspector in that case.
func Render(format string, data json.RawMessage) (template.HTML, bool) {
	switch format {
	case formatSlackBlocks:
		return renderSlackBlocks(data)
	case formatSlackAttachments:
		return renderSlackAttachments(data)
	case formatTeamsMessageCard:
		return renderMessageCard(data)
	case formatTeamsAdaptiveCard:
		return renderAdaptiveCard(data)
	default:
		return "", false
	}
}
