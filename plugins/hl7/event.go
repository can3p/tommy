package hl7

import (
	"encoding/json"

	"github.com/can3p/tommy/core/event"
)

// EventSummary is what the generic event log, the SSE stream and the store's
// search index see. Every provider gets it from here rather than building its
// own, so a message is named the same way wherever it is shown and a search for
// a control id or a patient name works regardless of which provider captured it.
func (m *Message) EventSummary() event.Summary {
	s := event.Summary{
		Title:   m.Title(),
		Snippet: m.Preview(),
	}
	if from := m.Header.Sender(); from != "" {
		s.From = from
	}
	if to := m.Header.Receiver(); to != "" {
		s.To = []string{to}
	}
	return s
}

// EventMeta is the message's own header as Event.Meta: the fields somebody
// filters a capture by. A provider adds its transport details (the peer
// address, the framing, the connection) to this rather than replacing it.
func (m *Message) EventMeta() map[string]any {
	meta := map[string]any{
		"segment_count": len(m.Segments),
		"segments":      m.SegmentIDs(),
		"separators": map[string]string{
			"field":        m.Separators.Field,
			"component":    m.Separators.Component,
			"repetition":   m.Separators.Repetition,
			"escape":       m.Separators.Escape,
			"subcomponent": m.Separators.Subcomponent,
		},
	}
	put := func(key, value string) {
		if value != "" {
			meta[key] = value
		}
	}
	put("message_type", m.Header.MessageType())
	put("message_code", m.Header.Code)
	put("trigger_event", m.Header.TriggerEvent)
	put("message_structure", m.Header.Structure)
	put("control_id", m.Header.ControlID)
	put("sending_application", m.Header.SendingApplication)
	put("sending_facility", m.Header.SendingFacility)
	put("receiving_application", m.Header.ReceivingApplication)
	put("receiving_facility", m.Header.ReceivingFacility)
	put("version", m.Header.Version)
	put("processing_id", m.Header.ProcessingID)
	put("message_datetime", m.Header.Timestamp)
	if len(m.Issues) > 0 {
		codes := make([]string, 0, len(m.Issues))
		for _, i := range m.Issues {
			codes = append(codes, i.Code)
		}
		meta["issues"] = codes
	}
	return meta
}

// NewEvent builds the event a provider appends for one captured message.
//
// raw must be the bytes exactly as they arrived, with the message's own line
// endings and without whatever framing carried it: Raw.Body is the copy of
// record, and everything else - the model, the summary, the tab - is derived
// from it and can be re-derived, while the bytes cannot.
//
// The caller fills in Raw.PeerAddr and anything else it knows about the
// transport, and may add its own keys to Meta.
func NewEvent(provider string, m *Message, raw []byte) *event.Event {
	return &event.Event{
		Plugin:   Name,
		Provider: provider,
		Type:     EventType,
		Summary:  m.EventSummary(),
		Meta:     m.EventMeta(),
		Payload:  m,
		Raw: event.Raw{
			Transport: Transport,
			Body:      raw,
			// HL7 v2 is text by definition, so the raw pane shows it as text
			// rather than falling back to the hex viewer.
			Text: true,
		},
	}
}

// Captured pairs an event with the message decoded from it.
type Captured struct {
	Event   *event.Event
	Message *Message
}

// MessageOf extracts the canonical message from an event.
//
// It accepts the in-process payload a provider appended (*Message), a value
// copy, and a payload that has been through JSON, so a store that round-trips
// events later does not break every read surface.
func MessageOf(e *event.Event) (*Message, bool) {
	if e == nil || e.Payload == nil {
		return nil, false
	}
	switch p := e.Payload.(type) {
	case *Message:
		if p == nil {
			return nil, false
		}
		clone := *p
		return &clone, true
	case Message:
		clone := p
		return &clone, true
	default:
		encoded, err := json.Marshal(e.Payload)
		if err != nil {
			return nil, false
		}
		var decoded Message
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return nil, false
		}
		if len(decoded.Segments) == 0 && decoded.Separators.Field == "" {
			return nil, false
		}
		return &decoded, true
	}
}

// Messages extracts every decodable message from a list of events, keeping the
// event alongside it. Events of another type - a provider's future query
// response, say - are skipped rather than guessed at.
func Messages(events []*event.Event) []Captured {
	out := make([]Captured, 0, len(events))
	for _, e := range events {
		if e.Type != EventType {
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
