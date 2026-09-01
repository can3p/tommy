// Package sms is tommy's SMS/MMS content type.
//
// It owns the canonical Message every SMS provider converts into, the read-back
// API under /api/v1/sms/ and the phone-style conversation tab under /ui/sms/.
// Providers (Twilio and whatever comes later) live in subpackages and never
// import each other.
package sms

import (
	"strings"
	"unicode/utf8"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
)

// Name is the plugin name and the URL segment it is mounted under.
const Name = "sms"

// EventType is the event.Type every captured message carries. Providers that
// grow new resources later add new types rather than overloading this one.
const EventType = "sms.message"

// Direction says who sent the message, from the point of view of the
// application under test: outbound is what it asked the provider to send.
type Direction string

// The directions a message can have.
const (
	Outbound Direction = "outbound"
	Inbound  Direction = "inbound"
)

// Status is the delivery status of a message. The vocabulary is the one the
// real SMS APIs use, so a provider maps its own status through unchanged.
type Status string

// The statuses a message can have.
const (
	StatusQueued      Status = "queued"
	StatusAccepted    Status = "accepted"
	StatusSending     Status = "sending"
	StatusSent        Status = "sent"
	StatusDelivered   Status = "delivered"
	StatusUndelivered Status = "undelivered"
	StatusFailed      Status = "failed"
	StatusReceived    Status = "received"
)

// Tone maps a status onto a components.Badge tone, so every surface colors it
// the same way.
func (s Status) Tone() string {
	switch s {
	case StatusDelivered, StatusSent, StatusReceived:
		return "ok"
	case StatusFailed, StatusUndelivered:
		return "error"
	case StatusSending, StatusQueued, StatusAccepted:
		return "info"
	default:
		return "muted"
	}
}

// Media is one MMS attachment.
//
// Bytes never travel inline in an event, so a provider that received the actual
// content puts it in the blob store and keeps the Ref here. A provider that was
// only handed a URL (Twilio's MediaUrl points at somebody else's server) keeps
// the URL and leaves Blob nil - tommy does not fetch it, because a fake that
// reaches out to the network is a fake that fails in CI.
type Media struct {
	ContentType string    `json:"content_type,omitempty"`
	Filename    string    `json:"filename,omitempty"`
	Blob        *blob.Ref `json:"blob,omitempty"`
	URL         string    `json:"url,omitempty"`
}

// Stored reports whether the bytes are in the blob store and can be streamed.
func (m Media) Stored() bool { return m.Blob != nil && m.Blob.ID != "" }

// Type returns the content type, preferring the one recorded on the media and
// falling back to the blob's.
func (m Media) Type() string {
	if m.ContentType != "" {
		return m.ContentType
	}
	if m.Blob != nil {
		return m.Blob.ContentType
	}
	return ""
}

// IsImage reports whether the media can be shown as a thumbnail.
func (m Media) IsImage() bool { return strings.HasPrefix(m.Type(), "image/") }

// Size is the stored size in bytes, or 0 when only a URL is known.
func (m Media) Size() int64 {
	if m.Blob == nil {
		return 0
	}
	return m.Blob.Size
}

// Name is the best label for the attachment: its filename, else its blob id,
// else the last path element of its URL.
func (m Media) Name() string {
	if m.Filename != "" {
		return m.Filename
	}
	if m.Blob != nil && m.Blob.Filename != "" {
		return m.Blob.Filename
	}
	if m.URL != "" {
		if i := strings.LastIndexByte(m.URL, '/'); i >= 0 && i < len(m.URL)-1 {
			return m.URL[i+1:]
		}
		return m.URL
	}
	if m.Blob != nil && m.Blob.ID != "" {
		return m.Blob.ID
	}
	return "attachment"
}

// Message is the sms plugin's canonical model: what every provider converts its
// wire format into, and what lands in event.Payload.
//
// Provider-specific metadata (a Twilio SID, an account SID, a status callback
// URL, the credentials that were presented) belongs in Event.Meta, not here.
// This struct only carries what any SMS API has.
type Message struct {
	// From is the sending number in E.164 ("+15005550006"), or an alphanumeric
	// sender id where the API allows one. It may be empty when the message was
	// sent through a messaging service instead.
	From string `json:"from,omitempty"`
	// To is the destination number in E.164.
	To string `json:"to"`
	// MessagingService is the provider-side sender pool identifier used instead
	// of an explicit From (Twilio's MessagingServiceSid, and its equivalents).
	MessagingService string `json:"messaging_service,omitempty"`

	// Body is the text of the message, decoded. It is untrusted input: every
	// surface escapes it and none of them renders it as HTML.
	Body string `json:"body"`
	// Media holds the MMS attachments, in the order they were supplied.
	Media []Media `json:"media,omitempty"`

	Status    Status    `json:"status,omitempty"`
	Direction Direction `json:"direction,omitempty"`

	// Segments is how the body is chopped onto the wire. It is derived from
	// Body by Normalize, never supplied by a provider - the point of showing it
	// is that it is computed the same way for everyone.
	Segments Segments `json:"segments"`
}

// Normalize fills in the derived and defaulted fields. Every provider calls it
// once it has finished converting a request, and the plugin calls it again when
// it reads a message back, so a message is never displayed with stale segments.
func (m *Message) Normalize() {
	m.From = strings.TrimSpace(m.From)
	m.To = strings.TrimSpace(m.To)
	m.MessagingService = strings.TrimSpace(m.MessagingService)
	if m.Direction == "" {
		m.Direction = Outbound
	}
	if m.Status == "" {
		if m.Direction == Inbound {
			m.Status = StatusReceived
		} else {
			m.Status = StatusQueued
		}
	}
	m.Segments = CountSegments(m.Body)
}

// IsMMS reports whether the message carries media.
func (m *Message) IsMMS() bool { return len(m.Media) > 0 }

// Sender is the address the message came from: From, or the messaging service
// when the provider was not given an explicit number.
func (m *Message) Sender() string {
	if m.From != "" {
		return m.From
	}
	return m.MessagingService
}

// Peer is the far end of the conversation - the number the application under
// test is talking to.
func (m *Message) Peer() string {
	if m.Direction == Inbound {
		return m.Sender()
	}
	return m.To
}

// Local is the near end of the conversation: the application's own number or
// messaging service.
func (m *Message) Local() string {
	if m.Direction == Inbound {
		return m.To
	}
	return m.Sender()
}

// EventSummary is the provider-agnostic listing data for this message. Every
// provider uses it so the generic event view, the API and the SMS tab all agree
// on what a message is called.
func (m *Message) EventSummary() event.Summary {
	title := m.Body
	if title == "" {
		if m.IsMMS() {
			title = "(media only)"
		} else {
			title = "(empty message)"
		}
	}
	if i := strings.IndexByte(title, '\n'); i >= 0 {
		title = title[:i]
	}
	return event.Summary{
		From:    m.Sender(),
		To:      []string{m.To},
		Title:   truncateRunes(title, 80),
		Snippet: m.Body,
	}
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// IsE164 reports whether s looks like an E.164 number: a leading +, then 1 to
// 15 digits with a non-zero first digit. Providers use it to decide whether to
// return their own "invalid To number" error shape.
func IsE164(s string) bool {
	if len(s) < 2 || len(s) > 16 || s[0] != '+' {
		return false
	}
	if s[1] == '0' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
