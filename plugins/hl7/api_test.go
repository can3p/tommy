package hl7_test

import (
	"net/http"
	"testing"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/hl7"
)

// envelope mirrors the API's MessageEnvelope closely enough to assert on, and
// deliberately goes through JSON so a change that breaks a client shows up here
// rather than only in the Go types.
type envelope struct {
	ID      event.ID `json:"id"`
	Title   string   `json:"title"`
	Preview string   `json:"preview"`
	RawURL  string   `json:"raw_url"`
	Meta    map[string]any
	Message hl7.Message `json:"message"`
}

func listMessages(t *testing.T, in *testutil.Instance, query string) []envelope {
	t.Helper()
	var out []envelope
	if status := in.GetJSON(in.API("/hl7/messages"+query), &out); status != http.StatusOK {
		t.Fatalf("GET /hl7/messages%s: status %d", query, status)
	}
	return out
}

func TestAPIList(t *testing.T) {
	in := start(t)
	adt := injectFixture(t, in, "adt_a01.hl7")
	oru := injectFixture(t, in, "oru_r01.hl7")

	got := listMessages(t, in, "")
	if len(got) != 2 {
		t.Fatalf("listed %d messages, want 2", len(got))
	}
	// Newest first, like every other listing in tommy.
	if got[0].ID != oru.ID || got[1].ID != adt.ID {
		t.Errorf("order = %v, want the ORU first", []event.ID{got[0].ID, got[1].ID})
	}
	if got[1].Title != "ADT^A01 · MSG00001" {
		t.Errorf("title = %q", got[1].Title)
	}
	if got[1].Message.Header.SendingApplication != "EPICADT" {
		t.Errorf("header did not survive JSON: %+v", got[1].Message.Header)
	}
	// The tree survives the round trip, repetitions and all.
	pid := got[1].Message.Segment("PID", 1)
	if pid == nil || !pid.Field(3).Repeats() {
		t.Fatalf("PID-3 lost its repetitions through JSON: %+v", pid)
	}
	if got[1].RawURL != "/api/v1/hl7/messages/"+string(adt.ID)+"/raw" {
		t.Errorf("raw url = %q", got[1].RawURL)
	}
}

func TestAPIFilters(t *testing.T) {
	in := start(t)
	adt := injectFixture(t, in, "adt_a01.hl7")
	oru := injectFixture(t, in, "oru_r01.hl7")

	tests := []struct {
		query string
		want  []event.ID
	}{
		{"?message_type=ADT^A01", []event.ID{adt.ID}},
		{"?message_type=adt", []event.ID{adt.ID}},
		{"?message_type=R01", []event.ID{oru.ID}},
		{"?control_id=MSG00002", []event.ID{oru.ID}},
		{"?sending_application=EPICADT", []event.ID{adt.ID}},
		{"?receiving_application=HOSPITAL", []event.ID{}},
		{"?receiving_application=EHR", []event.ID{oru.ID}},
		{"?segment=OBX", []event.ID{oru.ID}},
		{"?segment=EVN", []event.ID{adt.ID}},
		{"?segment=ZZZ", []event.ID{}},
		// The core's own search runs first, over the summary the plugin built.
		{"?search=MSG00001", []event.ID{adt.ID}},
		{"?search=ALICE", []event.ID{oru.ID}},
		{"?limit=1", []event.ID{oru.ID}},
		{"?limit=1&offset=1", []event.ID{adt.ID}},
	}
	for _, tc := range tests {
		got := listMessages(t, in, tc.query)
		var ids []event.ID
		for _, e := range got {
			ids = append(ids, e.ID)
		}
		if len(ids) != len(tc.want) {
			t.Errorf("%s returned %v, want %v", tc.query, ids, tc.want)
			continue
		}
		for i := range ids {
			if ids[i] != tc.want[i] {
				t.Errorf("%s returned %v, want %v", tc.query, ids, tc.want)
				break
			}
		}
	}
}

func TestAPIGet(t *testing.T) {
	in := start(t)
	ev := injectFixture(t, in, "adt_a01.hl7")

	var got envelope
	if status := in.GetJSON(in.API("/hl7/messages/"+string(ev.ID)), &got); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got.Message.Value("PID-5.1") != "DOE" {
		t.Errorf("PID-5.1 = %q after the round trip", got.Message.Value("PID-5.1"))
	}
	if got.Meta["control_id"] != "MSG00001" {
		t.Errorf("meta = %v, want the header lifted into it", got.Meta)
	}

	if status, _ := in.GetBody(in.API("/hl7/messages/nope")); status != http.StatusNotFound {
		t.Errorf("unknown id status = %d, want 404", status)
	}
}

// An event belonging to another plugin must not be readable through this
// plugin's routes, however it is addressed.
func TestAPIRefusesAnotherPluginsEvent(t *testing.T) {
	in := start(t)
	other := &event.Event{Plugin: "sms", Provider: "fake", Type: "sms.message"}
	if err := in.Store.Append(t.Context(), other); err != nil {
		t.Fatalf("append: %v", err)
	}
	for _, path := range []string{"/hl7/messages/" + string(other.ID), "/hl7/messages/" + string(other.ID) + "/raw"} {
		if status, _ := in.GetBody(in.API(path)); status != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", path, status)
		}
	}
}

// The raw endpoint is the copy of record: byte for byte what arrived, \r line
// endings and all, served as inert text.
func TestAPIRaw(t *testing.T) {
	in := start(t)
	raw := fixture(t, "adt_a01.hl7")
	ev := inject(t, in, raw)

	resp := in.Get(in.API("/hl7/messages/" + string(ev.ID) + "/raw"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("content type = %q", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff header = %q; a browser could sniff a captured message into HTML", got)
	}
	body := make([]byte, len(raw)+16)
	n, _ := resp.Body.Read(body)
	if string(body[:n]) != string(raw) {
		t.Errorf("raw body was not byte-exact:\n got %q\nwant %q", body[:n], raw)
	}
}

func TestAPIDelete(t *testing.T) {
	in := start(t)
	one := injectFixture(t, in, "adt_a01.hl7")
	injectFixture(t, in, "oru_r01.hl7")

	req, err := http.NewRequest(http.MethodDelete, in.API("/hl7/messages/"+string(one.ID)), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := in.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete one: status %d", resp.StatusCode)
	}
	if got := listMessages(t, in, ""); len(got) != 1 {
		t.Fatalf("after deleting one, %d messages remain", len(got))
	}

	req, err = http.NewRequest(http.MethodDelete, in.API("/hl7/messages"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp = in.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear: status %d", resp.StatusCode)
	}
	if got := listMessages(t, in, ""); len(got) != 0 {
		t.Errorf("after clearing, %d messages remain", len(got))
	}
	if got := in.Events(store.Query{Plugin: hl7.Name}); len(got) != 0 {
		t.Errorf("clear left %d events in the store", len(got))
	}
}
