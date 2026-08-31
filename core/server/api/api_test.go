package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/core/testutil/fakeplugin"
)

func start(t *testing.T) *testutil.Instance {
	t.Helper()
	return testutil.Start(t, nil, fakeplugin.New())
}

func send(t *testing.T, in *testutil.Instance, from, to, text string) string {
	t.Helper()
	status, body := in.PostJSON(in.Ingress("/fake/v1/send"), map[string]string{
		"from": from, "to": to, "text": text,
	})
	if status != http.StatusCreated {
		t.Fatalf("send: status %d body %s", status, body)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("send: decode %q: %v", body, err)
	}
	return out.ID
}

func TestHealth(t *testing.T) {
	in := start(t)

	var health struct {
		Status  string   `json:"status"`
		Plugins []string `json:"plugins"`
		Events  int      `json:"events"`
		Version string   `json:"version"`
		Uptime  string   `json:"uptime"`
	}
	if status := in.GetJSON(in.API("/health"), &health); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if health.Status != "ok" {
		t.Errorf("status = %q", health.Status)
	}
	if len(health.Plugins) != 1 || health.Plugins[0] != "fake" {
		t.Errorf("plugins = %v", health.Plugins)
	}
	if health.Events != 0 {
		t.Errorf("events = %d", health.Events)
	}
	if health.Uptime == "" || health.Version != "test" {
		t.Errorf("uptime = %q version = %q", health.Uptime, health.Version)
	}
}

func TestPluginsDiscovery(t *testing.T) {
	in := start(t)

	var plugins []plugin.PluginInfo
	if status := in.GetJSON(in.API("/plugins"), &plugins); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(plugins) != 1 {
		t.Fatalf("plugins = %+v", plugins)
	}
	p := plugins[0]
	if p.Name != "fake" || p.Title == "" || p.Description == "" {
		t.Errorf("plugin info = %+v", p)
	}
	if len(p.Providers) != 2 {
		t.Fatalf("providers = %+v", p.Providers)
	}

	byName := map[string]plugin.ProviderInfo{}
	for _, prov := range p.Providers {
		byName[prov.Name] = prov
	}

	echo := byName["echo"]
	if len(echo.Endpoints) != 2 {
		t.Errorf("echo endpoints = %+v", echo.Endpoints)
	}
	if len(echo.Snippets) == 0 {
		t.Fatal("echo must ship snippets")
	}
	// The whole point: the snippet carries the port this instance really bound.
	code := echo.Snippets[0].Code
	if !strings.Contains(code, in.IngressURL) {
		t.Errorf("snippet %q must be rendered against the live ingress URL %q", code, in.IngressURL)
	}
	if strings.Contains(code, "{{") {
		t.Errorf("snippet still contains a template action: %q", code)
	}

	line := byName["line"]
	if !line.Listener {
		t.Error("the TCP provider must be reported as owning a listener")
	}
}

func TestEventsEmpty(t *testing.T) {
	in := start(t)
	status, body := in.GetBody(in.API("/events"))
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if strings.TrimSpace(body) != "[]" {
		t.Errorf("body = %q, want an empty JSON array", body)
	}
}

func TestEventsRoundTrip(t *testing.T) {
	in := start(t)
	id := send(t, in, "a@example.com", "b@example.com", "It works.")

	var events []*event.Event
	if status := in.GetJSON(in.API("/events"), &events); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	e := events[0]
	if string(e.ID) != id {
		t.Errorf("id = %q, want %q", e.ID, id)
	}
	if e.Plugin != "fake" || e.Provider != "echo" || e.Type != "fake.message" {
		t.Errorf("event = %+v", e)
	}
	if e.Summary.From != "a@example.com" || e.Summary.Title != "It works." {
		t.Errorf("summary = %+v", e.Summary)
	}
	// Listings drop raw bodies; they can be megabytes each.
	if len(e.Raw.Body) != 0 {
		t.Errorf("listings must not carry raw bodies, got %d bytes", len(e.Raw.Body))
	}
	if e.Raw.Method != "POST" || e.Raw.Transport != "http" {
		t.Errorf("raw metadata must survive: %+v", e.Raw)
	}

	// ...unless asked for.
	var withRaw []*event.Event
	in.GetJSON(in.API("/events?include_raw=1"), &withRaw)
	if len(withRaw[0].Raw.Body) == 0 {
		t.Error("include_raw=1 must return the body")
	}

	// The single-event route always carries the full raw request.
	var single event.Event
	if status := in.GetJSON(in.API("/events/"+id), &single); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(string(single.Raw.Body), "It works.") {
		t.Errorf("raw body = %q", single.Raw.Body)
	}
	if len(single.Raw.Headers) == 0 {
		t.Error("raw headers must be captured")
	}

	if status, _ := in.GetBody(in.API("/events/nope")); status != http.StatusNotFound {
		t.Errorf("unknown event = %d, want 404", status)
	}
}

func TestEventsFilters(t *testing.T) {
	in := start(t)
	send(t, in, "alice@example.com", "bob@example.com", "Invoice 42")
	send(t, in, "carol@example.com", "dave@example.com", "Welcome aboard")

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"by plugin", "?plugin=fake", 2},
		{"by unknown plugin", "?plugin=sms", 0},
		{"by provider", "?provider=echo", 2},
		{"by type", "?type=fake.message", 2},
		{"by unknown type", "?type=fake.nope", 0},
		{"search", "?search=invoice", 1},
		{"search by recipient", "?search=dave@", 1},
		{"limit", "?limit=1", 1},
		{"offset", "?offset=1", 1},
		{"since a duration ago", "?since=1h", 2},
		{"since the future", "?since=" + time.Now().Add(time.Hour).UTC().Format(time.RFC3339), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var events []*event.Event
			if status := in.GetJSON(in.API("/events"+tc.query), &events); status != http.StatusOK {
				t.Fatalf("status = %d", status)
			}
			if len(events) != tc.want {
				t.Errorf("got %d events, want %d", len(events), tc.want)
			}
		})
	}
}

func TestEventsBadQuery(t *testing.T) {
	in := start(t)
	for _, query := range []string{"?limit=abc", "?offset=-1", "?since=yesterday"} {
		status, body := in.GetBody(in.API("/events" + query))
		if status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", query, status)
		}
		if !strings.Contains(body, "error") {
			t.Errorf("%s: body = %s", query, body)
		}
	}
}

func TestDeleteEvents(t *testing.T) {
	in := start(t)
	id := send(t, in, "a@example.com", "b@example.com", "one")
	send(t, in, "a@example.com", "b@example.com", "two")

	req, _ := http.NewRequest(http.MethodDelete, in.API("/events/"+id), nil)
	resp := in.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete one: status %d", resp.StatusCode)
	}
	if got := in.Events(store.Query{}); len(got) != 1 {
		t.Errorf("after deleting one, %d events left", len(got))
	}

	req, _ = http.NewRequest(http.MethodDelete, in.API("/events/"+id), nil)
	resp = in.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("deleting twice = %d, want 404", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodDelete, in.API("/events?plugin=fake"), nil)
	resp = in.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear: status %d", resp.StatusCode)
	}
	if got := in.Events(store.Query{}); len(got) != 0 {
		t.Errorf("after clearing, %d events left", len(got))
	}
}

func TestEventStream(t *testing.T) {
	in := start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.API("/events/stream"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	// The stream opens with a comment frame, which also proves it flushed.
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read preamble: %v", err)
	}

	id := send(t, in, "a@example.com", "b@example.com", "streamed")

	var dataLine, eventLine string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (dataLine == "" || eventLine == "") {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "data: {") && dataLine == "":
			dataLine = strings.TrimPrefix(line, "data: ")
		case strings.HasPrefix(line, "event: "):
			eventLine = strings.TrimPrefix(line, "event: ")
		}
	}

	if dataLine == "" {
		t.Fatal("no JSON frame arrived on the stream")
	}
	var streamed event.Event
	if err := json.Unmarshal([]byte(dataLine), &streamed); err != nil {
		t.Fatalf("decode frame %q: %v", dataLine, err)
	}
	if string(streamed.ID) != id {
		t.Errorf("streamed id = %q, want %q", streamed.ID, id)
	}
	if len(streamed.Raw.Body) != 0 {
		t.Error("the stream must not carry raw bodies")
	}
	if eventLine != "fake.message" {
		t.Errorf("named frame = %q, want the event type so htmx can trigger on it", eventLine)
	}
}

func TestStreamFilter(t *testing.T) {
	in := start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, in.API("/events/stream?plugin=other"), nil)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("read preamble: %v", err)
	}
	send(t, in, "a@example.com", "b@example.com", "filtered out")

	// Nothing should arrive; give the server a moment, then cancel and confirm.
	done := make(chan string, 1)
	go func() {
		line, err := reader.ReadString('\n')
		if err != nil {
			done <- ""
			return
		}
		done <- line
	}()
	select {
	case line := <-done:
		if strings.HasPrefix(line, "data:") {
			t.Errorf("a filtered stream delivered %q", line)
		}
	case <-time.After(300 * time.Millisecond):
	}
}

func TestPluginAPIRoutesAreMounted(t *testing.T) {
	in := start(t)
	send(t, in, "a@example.com", "b@example.com", "hello")

	var messages []fakeplugin.Message
	if status := in.GetJSON(in.API("/fake/messages"), &messages); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(messages) != 1 || messages[0].Text != "hello" {
		t.Errorf("messages = %+v", messages)
	}
}

func TestBlobDownload(t *testing.T) {
	in := start(t)
	ref, err := in.Blobs.Put(context.Background(), strings.NewReader("attachment bytes"), blob.Ref{
		ContentType: "text/plain",
		Filename:    "note.txt",
	})
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}

	resp := in.Get(in.API("/blobs/" + ref.ID))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("content type = %q", got)
	}
	if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "note.txt") {
		t.Errorf("disposition = %q", got)
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("range support is needed for large attachments, Accept-Ranges = %q", got)
	}

	if status, _ := in.GetBody(in.API("/blobs/nope")); status != http.StatusNotFound {
		t.Errorf("unknown blob = %d, want 404", status)
	}
}
