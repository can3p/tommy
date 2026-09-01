package msteams

import "encoding/json"

// adaptiveCardContentType is the contentType every Bot-Framework-shaped
// attachment carries when it wraps an Adaptive Card, verified against the
// live "Create an Incoming Webhook" sample.
const adaptiveCardContentType = "application/vnd.microsoft.card.adaptive"

// wireSniff is decoded once from every request body to work out which
// generation of webhook the payload targets, before either format is decoded
// in full. Both formats parse into this struct without error - the fields
// that do not apply to one simply stay zero - so a successful Unmarshal here
// also doubles as "the body is valid JSON".
type wireSniff struct {
	// AtType is "MessageCard" on an O365/M365 connector card. Its presence is
	// the one reliable signal that distinguishes a connector payload from a
	// workflow payload, since a workflow trigger accepts either card format
	// and a bare {"text":"..."} besides.
	AtType string `json:"@type"`
	// Attachments is populated on a Bot-Framework-shaped envelope
	// ({"type":"message","attachments":[...]}), which is how a workflow
	// trigger carries an Adaptive Card.
	Attachments []wireAttachment `json:"attachments"`
	// Text and Summary cover the bare {"text":"..."} shape a workflow trigger
	// also accepts directly (the "Send a request to the webhook" example in
	// Microsoft's own docs), and double as the MessageCard's own top-level
	// text/summary fields.
	Text    string `json:"text"`
	Summary string `json:"summary"`
}

// wireAttachment is one entry of the envelope's attachments[] array.
type wireAttachment struct {
	ContentType string          `json:"contentType"`
	Content     json.RawMessage `json:"content"`
}

// messageCard decodes the fields of a MessageCard this provider has a use
// for, beyond the byte-for-byte copy already kept as the message's
// structured content. Everything else - the full sections list, custom
// fields a connector card may carry - is left unparsed and reaches a reader
// through the verbatim chat.Content instead: the contract is that this
// provider owns its namespace, not that every MessageCard field needs a Go
// field on day one.
type messageCard struct {
	AtContext       string               `json:"@context,omitempty"`
	ThemeColor      string               `json:"themeColor,omitempty"`
	Text            string               `json:"text,omitempty"`
	Summary         string               `json:"summary,omitempty"`
	Sections        []messageCardSection `json:"sections,omitempty"`
	PotentialAction json.RawMessage      `json:"potentialAction,omitempty"`
}

// messageCardSection is one entry of a MessageCard's sections[] array. Only
// activityTitle and activityImage are pulled out - as the author name and
// icon of the message that posted the card, the closest thing a MessageCard
// has to an author - everything else stays in the verbatim content.
type messageCardSection struct {
	ActivityTitle string `json:"activityTitle,omitempty"`
	ActivityImage string `json:"activityImage,omitempty"`
}

// isEmptyRaw reports whether a json.RawMessage carries nothing worth keeping:
// absent, or explicitly null.
func isEmptyRaw(r json.RawMessage) bool {
	if len(r) == 0 {
		return true
	}
	s := string(r)
	return s == "null"
}
