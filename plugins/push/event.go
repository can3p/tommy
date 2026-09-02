package push

import (
	"encoding/json"
	"strconv"

	"github.com/can3p/tommy/core/event"
)

// EventSummary is what the generic event log, the SSE stream and the store's
// search index see. Every provider gets it from here rather than building its
// own, so a push is named the same way wherever it is shown and a search for a
// device token, a bundle ID or a body works regardless of which provider
// captured it.
//
// Preview carries the data keys for a silent push, which is what makes one
// findable at all: it has no title and no body to search for.
func (m *Message) EventSummary() event.Summary {
	s := event.Summary{
		Title:   m.Title(),
		Snippet: m.Preview(),
	}
	if m.App != "" {
		s.From = m.App
	}
	if !m.Target.Empty() {
		// The full value, not the shortened display form: this feeds the
		// store's substring search, and somebody pasting a whole device token
		// from their app's log must find it.
		s.To = []string{m.Target.Value}
	}
	return s
}

// EventMeta is the message's own routing and delivery facts as Event.Meta: the
// fields somebody filters a capture by. A provider adds its transport details
// (the request headers, the bearer token it was handed, the JWT claims) to this
// rather than replacing it.
func (m *Message) EventMeta() map[string]any {
	meta := map[string]any{
		"kind":     string(m.Kind),
		"displays": m.Displays(),
	}
	put := func(key string, value string) {
		if value != "" {
			meta[key] = value
		}
	}
	put("push_type", m.PushType)
	put("app", m.App)
	put("target_kind", string(m.Target.Kind))
	put("target", m.Target.Value)
	put("target_source", m.Target.Source)
	if m.Target.Kind.Fanout() {
		meta["fanout"] = true
	}
	put("priority", string(m.Delivery.Priority))
	put("priority_raw", m.Delivery.PriorityRaw)
	put("collapse_key", m.Delivery.CollapseKey)
	if e := m.Delivery.Expiry; e != nil {
		put("expiry", e.Describe())
	}
	if a := m.Alert; a != nil {
		put("title", a.Title)
		put("subtitle", a.Subtitle)
		put("category", a.Category)
		put("sound", a.Sound)
		put("image", a.Image)
		if a.Badge != nil {
			meta["badge"] = *a.Badge
		}
		if !a.Localization.Empty() {
			meta["localized"] = true
		}
	}
	if keys := m.DataKeys(); len(keys) > 0 {
		meta["data_keys"] = keys
	}
	if len(m.Payloads) > 0 {
		formats := make([]string, 0, len(m.Payloads))
		for _, p := range m.Payloads {
			formats = append(formats, string(p.Format))
		}
		meta["payload_formats"] = formats
	}
	return meta
}

// NewEvent builds the event a provider appends for one captured push.
//
// The caller fills in Raw - the untouched request, body included - plus
// Raw.PeerAddr and anything else it knows about the transport, and may add its
// own keys to Meta. One request produces one event per delivered message: FCM's
// send endpoint takes exactly one message, but a provider that ever fans out
// appends one of these each.
func NewEvent(provider string, m *Message) *event.Event {
	m.Normalize()
	return &event.Event{
		Plugin:   Name,
		Provider: provider,
		Type:     EventType,
		Summary:  m.EventSummary(),
		Meta:     m.EventMeta(),
		Payload:  m,
		Raw: event.Raw{
			Transport: Transport,
			// Both ecosystems post JSON, so the raw pane shows text rather
			// than falling back to the hex viewer.
			Text: true,
		},
	}
}

// MessageOf returns the canonical message carried by an event, if it carries
// one.
//
// The in-memory store shares payloads, so the common path is a type assertion;
// the JSON fallback keeps every read surface honest for an event that came back
// through a serializing store.
func MessageOf(e *event.Event) (*Message, bool) {
	if e == nil || e.Payload == nil {
		return nil, false
	}
	var m *Message
	switch p := e.Payload.(type) {
	case *Message:
		if p == nil {
			return nil, false
		}
		clone := *p
		m = &clone
	case Message:
		clone := p
		m = &clone
	default:
		encoded, err := json.Marshal(e.Payload)
		if err != nil {
			return nil, false
		}
		var decoded Message
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return nil, false
		}
		if decoded.Kind == "" && decoded.Target.Value == "" && decoded.Alert == nil &&
			len(decoded.Payloads) == 0 && emptyJSON(decoded.Data) {
			return nil, false
		}
		m = &decoded
	}
	m.Normalize()
	return m, true
}

// Captured pairs an event with the message decoded from it. It is what every
// derived view is built out of, because the arrival time and the provider name
// live on the event while the content lives on the message.
type Captured struct {
	Event   *event.Event
	Message *Message
}

// ID is the event id of the captured push.
func (c Captured) ID() event.ID {
	if c.Event == nil {
		return ""
	}
	return c.Event.ID
}

// Messages extracts every decodable push from a list of events, keeping the
// event alongside it. Events of another type - a provider's future token
// registration, say - are skipped rather than guessed at.
func Messages(events []*event.Event) []Captured {
	out := make([]Captured, 0, len(events))
	for _, e := range events {
		if e == nil || e.Type != EventType {
			continue
		}
		m, ok := MessageOf(e)
		if !ok {
			continue
		}
		out = append(out, Captured{Event: e, Message: m})
	}
	return out
}

// badgeText renders a badge count for a label, keeping "clears the badge"
// distinct from "badge 0" being unset.
func badgeText(n int) string {
	if n == 0 {
		return "clears the badge"
	}
	return "badge " + strconv.Itoa(n)
}
