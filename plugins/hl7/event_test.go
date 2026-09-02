package hl7_test

import (
	"encoding/json"
	"testing"

	"github.com/can3p/tommy/plugins/hl7"
)

// The event a provider appends is what the generic log, the SSE stream and the
// store's search index see, so the header has to be lifted into it. A capture
// that can only be found by scrolling is barely a capture.
func TestNewEvent(t *testing.T) {
	raw := fixture(t, "adt_a01.hl7")
	m, err := hl7.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ev := hl7.NewEvent("mllp", m, raw)

	if ev.Plugin != hl7.Name || ev.Provider != "mllp" || ev.Type != hl7.EventType {
		t.Errorf("identity = %s/%s/%s", ev.Plugin, ev.Provider, ev.Type)
	}
	if ev.Raw.Transport != "tcp" || !ev.Raw.Text {
		t.Errorf("raw = %+v, want a text tcp transport", ev.Raw)
	}
	if string(ev.Raw.Body) != string(raw) {
		t.Error("Raw.Body is not byte-exact")
	}

	if ev.Summary.Title != "ADT^A01 · MSG00001" {
		t.Errorf("summary title = %q", ev.Summary.Title)
	}
	if ev.Summary.From != "EPICADT / EPIC_FAC" {
		t.Errorf("summary from = %q", ev.Summary.From)
	}
	if len(ev.Summary.To) != 1 || ev.Summary.To[0] != "SMSADT / SMS_FAC" {
		t.Errorf("summary to = %v", ev.Summary.To)
	}

	for key, want := range map[string]any{
		"message_type":        "ADT^A01",
		"trigger_event":       "A01",
		"message_structure":   "ADT_A01",
		"control_id":          "MSG00001",
		"sending_application": "EPICADT",
		"version":             "2.5",
		"segment_count":       5,
	} {
		if got := ev.Meta[key]; got != want {
			t.Errorf("meta[%q] = %v, want %v", key, got, want)
		}
	}
	if _, ok := ev.Meta["issues"]; ok {
		t.Errorf("a clean message reported issues: %v", ev.Meta["issues"])
	}
}

// A headerless fragment still produces a usable event rather than an empty one.
func TestNewEventForAFragment(t *testing.T) {
	raw := fixture(t, "no_msh.hl7")
	m, err := hl7.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	ev := hl7.NewEvent("mllp", m, raw)
	if ev.Summary.Title != "HL7 message" {
		t.Errorf("title = %q", ev.Summary.Title)
	}
	if ev.Summary.Snippet != "FRAGMENT^ONLY · PID PV1" {
		t.Errorf("snippet = %q", ev.Summary.Snippet)
	}
	codes, _ := ev.Meta["issues"].([]string)
	if len(codes) != 1 || codes[0] != hl7.IssueNoHeader {
		t.Errorf("meta issues = %v", ev.Meta["issues"])
	}
}

// Every read surface goes through MessageOf, so it has to survive a store that
// round-trips its payload through JSON as well as an in-process pointer.
func TestMessageOfRoundTrips(t *testing.T) {
	raw := fixture(t, "adt_a01.hl7")
	m, err := hl7.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	ev := hl7.NewEvent("mllp", m, raw)
	got, ok := hl7.MessageOf(ev)
	if !ok {
		t.Fatal("MessageOf rejected the pointer payload")
	}
	if got.Value("PID-5.1") != "DOE" {
		t.Errorf("pointer payload: PID-5.1 = %q", got.Value("PID-5.1"))
	}

	// A value copy, as a provider might build one.
	ev.Payload = *m
	if got, ok = hl7.MessageOf(ev); !ok || got.Value("PID-5.1") != "DOE" {
		t.Errorf("value payload: ok=%v PID-5.1=%q", ok, got.Value("PID-5.1"))
	}

	// And through JSON, as a store that persisted the event would hand it back.
	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ev.Payload = generic
	got, ok = hl7.MessageOf(ev)
	if !ok {
		t.Fatal("MessageOf rejected a payload that had been through JSON")
	}
	if got.Value("PID-3[2].1") != "999887777" {
		t.Errorf("json payload lost a repetition: %q", got.Value("PID-3[2].1"))
	}
	if got.Separators != m.Separators {
		t.Errorf("json payload lost the separators: %+v", got.Separators)
	}

	// Anything that is not an HL7 message is refused rather than guessed at.
	ev.Payload = map[string]any{"unrelated": true}
	if _, ok := hl7.MessageOf(ev); ok {
		t.Error("MessageOf accepted a payload that is not an HL7 message")
	}
	ev.Payload = nil
	if _, ok := hl7.MessageOf(ev); ok {
		t.Error("MessageOf accepted a nil payload")
	}
}
