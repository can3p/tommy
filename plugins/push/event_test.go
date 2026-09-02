package push_test

import (
	"encoding/json"
	"testing"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/plugins/push"
)

// The summary is what the generic event log, the SSE stream and the store's
// search index see. A silent push has no title and no body, so its data keys
// have to reach the snippet or it is unfindable.
func TestEventSummary(t *testing.T) {
	alert := apns(t, "apns_alert.json", apnsAlertHeaders)
	s := alert.EventSummary()
	if s.Title != "Game Request" || s.From != "com.example.MyApp" {
		t.Errorf("summary = %+v", s)
	}
	if len(s.To) != 1 || s.To[0] != "00fc13adff785122b4ad28809a3420982341241421348097878e577c991de8f0" {
		// The full token, not the shortened display form: this feeds a
		// substring search and somebody pastes the whole thing.
		t.Errorf("summary.To = %v, want the whole device token", s.To)
	}

	silent := fcm(t, "fcm_topic_data.json")
	ss := silent.EventSummary()
	if ss.Title != "(silent push)" {
		t.Errorf("silent title = %q", ss.Title)
	}
	if ss.Snippet != "data: kind, region" {
		t.Errorf("silent snippet = %q, want the data keys", ss.Snippet)
	}
	if len(ss.To) != 1 || ss.To[0] != "weather" {
		t.Errorf("silent To = %v", ss.To)
	}
}

func TestEventMeta(t *testing.T) {
	m := apns(t, "apns_alert.json", apnsAlertHeaders)
	meta := m.EventMeta()
	for k, want := range map[string]any{
		"kind":          "notification",
		"displays":      true,
		"push_type":     "alert",
		"app":           "com.example.MyApp",
		"target_kind":   "device",
		"target_source": "path",
		"priority":      "high",
		"priority_raw":  "10",
		"collapse_key":  "poker",
		"category":      "GAME_INVITATION",
		"badge":         3,
	} {
		if meta[k] != want {
			t.Errorf("meta[%q] = %v, want %v", k, meta[k], want)
		}
	}
	if _, ok := meta["fanout"]; ok {
		t.Error("a device push must not be marked as fanning out")
	}
	if got, _ := meta["data_keys"].([]string); len(got) != 2 {
		t.Errorf("meta[data_keys] = %v", meta["data_keys"])
	}

	topic := fcm(t, "fcm_topic_data.json")
	if topic.EventMeta()["fanout"] != true {
		t.Error("a topic push must be marked as fanning out")
	}
}

func TestNewEvent(t *testing.T) {
	m := apns(t, "apns_alert.json", apnsAlertHeaders)
	ev := push.NewEvent("apns", m)
	if ev.Plugin != push.Name || ev.Provider != "apns" || ev.Type != push.EventType {
		t.Errorf("event = %+v", ev)
	}
	if ev.Raw.Transport != "http" || !ev.Raw.Text {
		t.Errorf("raw = %+v, want an http transport rendered as text", ev.Raw)
	}
	if ev.Summary.Title == "" || len(ev.Meta) == 0 {
		t.Error("NewEvent must fill in the summary and meta so no provider builds its own")
	}
}

// Every read surface has to work on an event that came back through a
// serializing store, not just on the shared in-memory payload.
func TestMessageOf(t *testing.T) {
	m := apns(t, "apns_alert.json", apnsAlertHeaders)

	got, ok := push.MessageOf(&event.Event{Payload: m})
	if !ok || got.Title() != "Game Request" {
		t.Fatalf("pointer payload: %v %+v", ok, got)
	}
	if got == m {
		t.Error("MessageOf must hand back a copy; events are immutable once appended")
	}

	if got, ok = push.MessageOf(&event.Event{Payload: *m}); !ok || got.Title() != "Game Request" {
		t.Errorf("value payload: %v %+v", ok, got)
	}

	encoded, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var generic any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		t.Fatal(err)
	}
	got, ok = push.MessageOf(&event.Event{Payload: generic})
	if !ok {
		t.Fatal("a payload that has been through JSON was not recognized")
	}
	if got.Title() != "Game Request" || got.Target.Source != "path" || got.Kind != push.KindNotification {
		t.Errorf("json round trip = %+v", got)
	}

	for _, e := range []*event.Event{nil, {}, {Payload: map[string]any{"unrelated": 1}}} {
		if _, ok := push.MessageOf(e); ok {
			t.Errorf("MessageOf(%v) claimed a push", e)
		}
	}
}

// Events of another type are skipped rather than guessed at.
func TestMessagesSkipsOtherTypes(t *testing.T) {
	m := apns(t, "apns_alert.json", apnsAlertHeaders)
	events := []*event.Event{
		push.NewEvent("apns", m),
		{Type: "push.token", Payload: map[string]any{"token": "x"}},
		nil,
	}
	if got := push.Messages(events); len(got) != 1 {
		t.Errorf("Messages returned %d, want 1", len(got))
	}
}
