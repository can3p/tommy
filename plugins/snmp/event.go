package snmp

import (
	"encoding/json"

	"github.com/can3p/tommy/core/event"
)

// EventSummary is what the generic event log, the SSE stream and the store's
// search index see.
func (t *Trap) EventSummary() event.Summary {
	return event.Summary{
		Title:   t.Title(),
		Snippet: t.Preview(),
	}
}

// EventMeta is what a filter and the generic event detail's metadata panel
// see - the fields somebody hunts a capture by, flattened out of whichever of
// V1/V2 this trap actually carries.
func (t *Trap) EventMeta() map[string]any {
	meta := map[string]any{
		"version":   string(t.Version),
		"inform":    t.Inform,
		"community": t.Community,
		"varbinds":  len(t.Varbinds),
	}
	if t.RequestID != 0 {
		meta["request_id"] = t.RequestID
	}
	if t.V1 != nil {
		meta["enterprise_oid"] = t.V1.EnterpriseOID
		meta["agent_address"] = t.V1.AgentAddress
		meta["generic_trap"] = t.V1.GenericTrap
		if t.V1.GenericTrapName != "" {
			meta["generic_trap_name"] = t.V1.GenericTrapName
		}
		meta["specific_trap"] = t.V1.SpecificTrap
	}
	if t.V2 != nil {
		meta["sys_uptime"] = t.V2.SysUpTime
		meta["trap_oid"] = t.V2.TrapOID
	}
	if t.DecodeError != "" {
		meta["decode_error"] = t.DecodeError
	}
	return meta
}

// NewEvent builds the event a provider appends for one captured datagram.
//
// raw must be the untouched UDP payload and peerAddr the sender's address -
// the caller (the trap provider) is the only thing that knows either.
func NewEvent(provider string, t *Trap, raw []byte, peerAddr string) *event.Event {
	return &event.Event{
		Plugin:   Name,
		Provider: provider,
		Type:     EventType,
		Summary:  t.EventSummary(),
		Meta:     t.EventMeta(),
		Payload:  t,
		Raw: event.Raw{
			Transport: Transport,
			PeerAddr:  peerAddr,
			Body:      raw,
			// SNMP is BER-encoded binary, never text: the generic view's raw
			// pane should hex-dump it, not try to print it.
			Text: false,
		},
	}
}

// Captured pairs an event with the trap decoded from it.
type Captured struct {
	Event *event.Event
	Trap  *Trap
}

// TrapOf extracts the canonical trap from an event.
//
// It accepts the in-process payload a provider appended (*Trap), a value
// copy, and a payload that has been through JSON, so a store that round-trips
// events later does not break every read surface.
func TrapOf(e *event.Event) (*Trap, bool) {
	if e == nil || e.Payload == nil {
		return nil, false
	}
	switch p := e.Payload.(type) {
	case *Trap:
		if p == nil {
			return nil, false
		}
		clone := *p
		return &clone, true
	case Trap:
		clone := p
		return &clone, true
	default:
		encoded, err := json.Marshal(e.Payload)
		if err != nil {
			return nil, false
		}
		var decoded Trap
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return nil, false
		}
		if decoded.Version == "" && len(decoded.Varbinds) == 0 && decoded.DecodeError == "" {
			return nil, false
		}
		return &decoded, true
	}
}

// Traps extracts every decodable trap from a list of events, keeping the
// event alongside it. Events of another type are skipped rather than guessed
// at.
func Traps(events []*event.Event) []Captured {
	out := make([]Captured, 0, len(events))
	for _, e := range events {
		if e.Type != EventType {
			continue
		}
		t, ok := TrapOf(e)
		if !ok {
			continue
		}
		out = append(out, Captured{Event: e, Trap: t})
	}
	return out
}
