// Package event defines the transport-agnostic envelope every tommy plugin
// produces. An Event is what the store keeps, what the API serves and what the
// UI renders; plugins put their canonical model in Payload and the untouched
// wire data in Raw.
package event

import (
	"net/http"
	"time"
)

// ID identifies a single event inside a Store.
type ID string

// Event is the envelope around everything tommy captures.
//
// Events are treated as immutable once appended to a store: the store hands out
// copies of the envelope, but Meta, Payload and Raw.Body are shared, so nothing
// downstream may mutate them.
type Event struct {
	ID         ID             `json:"id"`
	Plugin     string         `json:"plugin"`   // "mail", "sms", "files"
	Provider   string         `json:"provider"` // "mailjet", "sendgrid", "smtp", "twilio"
	Type       string         `json:"type"`     // "<plugin>.<resource>", free-form, never an enum
	ReceivedAt time.Time      `json:"received_at"`
	Summary    Summary        `json:"summary"`           // provider-agnostic listing data
	Meta       map[string]any `json:"meta,omitempty"`    // provider metadata
	Payload    any            `json:"payload,omitempty"` // *mail.Message, *sms.Message, ...
	Raw        Raw            `json:"raw"`               // the untouched request
}

// Summary carries the provider-agnostic fields the generic list views render.
// Every plugin fills it in, whatever its Payload looks like.
type Summary struct {
	From    string   `json:"from,omitempty"`
	To      []string `json:"to,omitempty"`
	Title   string   `json:"title,omitempty"`   // subject / first line / file path / channel
	Snippet string   `json:"snippet,omitempty"` // short preview of the body
}

// Raw is transport-agnostic on purpose: HL7 over MLLP, ISO 8583 over TCP and
// SNMP over UDP have no method, path or headers, and their bodies are not text.
type Raw struct {
	Transport string      `json:"transport"`           // "http" | "tcp" | "udp" | "smtp" | "ftp" | "ssh"
	PeerAddr  string      `json:"peer_addr,omitempty"` // who sent it
	Method    string      `json:"method,omitempty"`    // http only
	Path      string      `json:"path,omitempty"`      // http only
	Headers   http.Header `json:"headers,omitempty"`   // http/smtp; nil elsewhere
	Body      []byte      `json:"body,omitempty"`      // bytes, not string - may be binary
	Text      bool        `json:"text"`                // hint: render as text, else hex viewer
}

// Clone returns a shallow copy of the envelope. The To slice is copied because
// plugins commonly append to it; Meta, Payload and Raw.Body are shared and must
// be treated as read-only.
func (e *Event) Clone() *Event {
	if e == nil {
		return nil
	}
	c := *e
	if e.Summary.To != nil {
		c.Summary.To = append([]string(nil), e.Summary.To...)
	}
	return &c
}

// WithoutRawBody returns a copy with Raw.Body stripped and its length recorded
// in Raw. It is what the SSE stream sends: raw bodies can be megabytes and every
// subscriber would pay for them. Fetch the event by ID for the full body.
func (e *Event) WithoutRawBody() *Event {
	c := e.Clone()
	if c == nil {
		return nil
	}
	c.Raw.Body = nil
	return c
}
