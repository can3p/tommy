package snmp_test

import (
	"encoding/json"
	"testing"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/plugins/snmp"
)

func sampleTrap() *snmp.Trap {
	return &snmp.Trap{
		Version:   snmp.VersionV2c,
		Community: "public",
		RequestID: 7,
		Varbinds: []snmp.Varbind{
			{OID: ".1.3.6.1.2.1.1.3.0", Type: "TimeTicks", Value: "12345"},
			{OID: ".1.3.6.1.6.3.1.1.4.1.0", Type: "ObjectIdentifier", Value: ".1.3.6.1.6.3.1.1.5.3"},
		},
		V2: &snmp.V2Info{SysUpTime: 12345, TrapOID: ".1.3.6.1.6.3.1.1.5.3"},
	}
}

func TestNewEvent(t *testing.T) {
	tr := sampleTrap()
	raw := []byte{0x30, 0x01, 0x02}
	ev := snmp.NewEvent("trap", tr, raw, "203.0.113.9:54321")

	if ev.Plugin != snmp.Name {
		t.Errorf("Plugin = %q", ev.Plugin)
	}
	if ev.Provider != "trap" {
		t.Errorf("Provider = %q", ev.Provider)
	}
	if ev.Type != snmp.EventType {
		t.Errorf("Type = %q", ev.Type)
	}
	if ev.Raw.Transport != "udp" {
		t.Errorf("Raw.Transport = %q, want udp", ev.Raw.Transport)
	}
	if ev.Raw.Text {
		t.Error("Raw.Text = true, want false: SNMP is BER-encoded binary")
	}
	if string(ev.Raw.Body) != string(raw) {
		t.Errorf("Raw.Body = %v, want %v", ev.Raw.Body, raw)
	}
	if ev.Raw.PeerAddr != "203.0.113.9:54321" {
		t.Errorf("Raw.PeerAddr = %q", ev.Raw.PeerAddr)
	}
	if ev.Meta["community"] != "public" {
		t.Errorf("Meta[community] = %v", ev.Meta["community"])
	}
	if ev.Meta["trap_oid"] != ".1.3.6.1.6.3.1.1.5.3" {
		t.Errorf("Meta[trap_oid] = %v", ev.Meta["trap_oid"])
	}
	if ev.Summary.Title == "" {
		t.Error("Summary.Title is empty")
	}
}

func TestTrapOfDirect(t *testing.T) {
	tr := sampleTrap()
	ev := &event.Event{Type: snmp.EventType, Payload: tr}
	got, ok := snmp.TrapOf(ev)
	if !ok {
		t.Fatal("TrapOf() = false")
	}
	if got.Community != "public" || len(got.Varbinds) != 2 {
		t.Errorf("got = %+v", got)
	}
}

func TestTrapOfJSONRoundTrip(t *testing.T) {
	tr := sampleTrap()
	encoded, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var payload any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("unmarshal into any: %v", err)
	}
	ev := &event.Event{Type: snmp.EventType, Payload: payload}
	got, ok := snmp.TrapOf(ev)
	if !ok {
		t.Fatal("TrapOf() = false after a JSON round trip")
	}
	if got.Version != snmp.VersionV2c || got.V2.TrapOID != ".1.3.6.1.6.3.1.1.5.3" {
		t.Errorf("got = %+v", got)
	}
}

func TestTrapOfRejectsOtherPayloads(t *testing.T) {
	ev := &event.Event{Type: snmp.EventType, Payload: map[string]any{"unrelated": true}}
	if _, ok := snmp.TrapOf(ev); ok {
		t.Error("TrapOf() = true for an unrelated payload")
	}
	if _, ok := snmp.TrapOf(nil); ok {
		t.Error("TrapOf(nil) = true")
	}
	if _, ok := snmp.TrapOf(&event.Event{}); ok {
		t.Error("TrapOf() = true for an event with no payload")
	}
}

func TestTraps(t *testing.T) {
	tr := sampleTrap()
	events := []*event.Event{
		{ID: "a", Type: snmp.EventType, Payload: tr},
		{ID: "b", Type: "other.type", Payload: tr},
	}
	got := snmp.Traps(events)
	if len(got) != 1 || got[0].Event.ID != "a" {
		t.Errorf("Traps() = %+v, want exactly the snmp.trap event", got)
	}
}
