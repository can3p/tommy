package event_test

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/can3p/tommy/core/event"
)

func TestCloneIsIndependentWhereItMatters(t *testing.T) {
	orig := &event.Event{
		ID:      "a",
		Plugin:  "mail",
		Summary: event.Summary{Title: "hi", To: []string{"a@example.com"}},
		Meta:    map[string]any{"k": "v"},
	}
	c := orig.Clone()

	c.Summary.Title = "changed"
	c.Summary.To[0] = "b@example.com"
	if orig.Summary.Title != "hi" {
		t.Error("clone shares the summary struct")
	}
	if orig.Summary.To[0] != "a@example.com" {
		t.Error("clone shares the To slice; plugins commonly append to it")
	}

	// Meta and Payload are documented as shared and immutable.
	if len(c.Meta) != 1 {
		t.Error("clone lost Meta")
	}
}

func TestCloneNil(t *testing.T) {
	var e *event.Event
	if e.Clone() != nil {
		t.Error("Clone of nil should be nil")
	}
}

func TestWithoutRawBody(t *testing.T) {
	e := &event.Event{
		ID:  "a",
		Raw: event.Raw{Transport: "http", Body: []byte("big payload"), Headers: http.Header{"X": {"1"}}},
	}
	stripped := e.WithoutRawBody()

	if stripped.Raw.Body != nil {
		t.Error("Raw.Body must be dropped: every SSE subscriber would otherwise pay for it")
	}
	if stripped.Raw.Transport != "http" || len(stripped.Raw.Headers) != 1 {
		t.Error("the rest of Raw must survive")
	}
	if string(e.Raw.Body) != "big payload" {
		t.Error("the original must not be modified")
	}
}

func TestJSONShapeIsStable(t *testing.T) {
	// Wave 1 and every API consumer code against these names.
	e := &event.Event{
		ID:         "id-1",
		Plugin:     "mail",
		Provider:   "mailjet",
		Type:       "mail.message",
		ReceivedAt: time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		Summary:    event.Summary{From: "a", To: []string{"b"}, Title: "t", Snippet: "s"},
		Meta:       map[string]any{"custom_id": "x"},
		Payload:    map[string]any{"body": "hi"},
		Raw:        event.Raw{Transport: "http", Method: "POST", Path: "/v3.1/send", Body: []byte("ab"), Text: true},
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	keys := make([]string, 0, len(generic))
	for k := range generic {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"id", "meta", "payload", "plugin", "provider", "raw", "received_at", "summary", "type"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("top-level keys = %v, want %v", keys, want)
	}

	raw, _ := generic["raw"].(map[string]any)
	if raw["body"] != "YWI=" {
		t.Errorf("raw.body = %v, want base64 of the bytes", raw["body"])
	}
	if raw["transport"] != "http" || raw["text"] != true {
		t.Errorf("raw = %v", raw)
	}
}

func TestNewIDIsUniqueAndSortable(t *testing.T) {
	const n = 2000
	ids := make([]string, 0, n)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, 0, n/8)
			for range n / 8 {
				local = append(local, event.NewID())
			}
			mu.Lock()
			ids = append(ids, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		if len(id) != 24 {
			t.Fatalf("id %q has length %d, want a fixed 24 so ids sort lexicographically", id, len(id))
		}
	}

	// Ids minted later must sort after ids minted earlier.
	first := event.NewID()
	time.Sleep(2 * time.Millisecond)
	second := event.NewID()
	if first >= second {
		t.Errorf("ids are not time sortable: %q >= %q", first, second)
	}
}
