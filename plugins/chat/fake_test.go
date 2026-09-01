package chat_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/chat"
)

// fakeProvider is a test-only chat provider. It exists because the real Slack
// and Teams providers are separate tasks and the plugin still has to be proven
// end to end: it converts a trivial JSON body into the canonical model exactly
// the way a real provider does, and Inject does the same without going near
// HTTP, so a failure in the plugin's own tests is the plugin's.
type fakeProvider struct{}

func (fakeProvider) Name() string   { return "fake" }
func (fakeProvider) Plugin() string { return chat.PluginName }

func (fakeProvider) Description() string {
	return "A test-only chat API that accepts a canonical chat message as plain JSON, so the chat plugin can be exercised end to end before a real vendor provider exists."
}

func (fakeProvider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{{
		Method:      "POST",
		Path:        "/fake-chat/v1/messages",
		Description: "Accept a chat message as JSON and record it as a chat.message event.",
	}}
}

func (fakeProvider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{{
		Title: "Post a fake chat message",
		Lang:  "bash",
		Code: `curl -s {{.IngressURL}}/fake-chat/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{"channel":{"id":"C0123ABCD","name":"general"},
       "author":{"name":"deploy-bot","bot":true},
       "text":"It works.","ts":"1700000000.000100"}'`,
	}}
}

func (p fakeProvider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()
	mux.HandleFunc("POST /fake-chat/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var m chat.Message
		if err := json.Unmarshal(body, &m); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		ev := chat.NewEvent("fake", &m)
		ev.Raw = event.Raw{
			Transport: "http",
			PeerAddr:  r.RemoteAddr,
			Method:    r.Method,
			Path:      r.URL.Path,
			Headers:   r.Header.Clone(),
			Body:      body,
			Text:      true,
		}
		if err := d.Append(r.Context(), ev); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": string(ev.ID), "ts": m.TS})
	})
}

// event builds the event the fake would record for a message.
func (fakeProvider) event(m *chat.Message, meta map[string]any, at time.Time) *event.Event {
	ev := chat.NewEvent("fake", m)
	ev.Meta = meta
	ev.ReceivedAt = at
	raw, _ := json.Marshal(m)
	ev.Raw = event.Raw{
		Transport: "http",
		Method:    "POST",
		Path:      "/fake-chat/v1/messages",
		Body:      raw,
		Text:      true,
	}
	return ev
}

// Inject records a message straight into the store, which is how the plugin's
// own tests fill it: no wire format in the way.
func (p fakeProvider) Inject(ctx context.Context, d plugin.Deps, m *chat.Message, meta map[string]any, at time.Time) (*event.Event, error) {
	ev := p.event(m, meta, at)
	return ev, d.Append(ctx, ev)
}

// base is a fixed instant every injected message is timed relative to, so the
// derived ordering in a test never depends on the wall clock.
var base = time.Date(2024, 5, 4, 9, 0, 0, 0, time.UTC)

// at returns base plus n seconds.
func at(n int) time.Time { return base.Add(time.Duration(n) * time.Second) }

// injectAt records a message with an explicit arrival time, which is what makes
// the channel and thread ordering assertions deterministic.
func injectAt(t *testing.T, in *testutil.Instance, when time.Time, m *chat.Message) *event.Event {
	t.Helper()
	return injectMeta(t, in, when, m, nil)
}

func injectMeta(t *testing.T, in *testutil.Instance, when time.Time, m *chat.Message, meta map[string]any) *event.Event {
	t.Helper()
	ev, err := fakeProvider{}.Inject(context.Background(), plugin.Deps{Store: in.Store}, m, meta, when)
	if err != nil {
		t.Fatalf("inject message: %v", err)
	}
	return ev
}

// start boots a whole tommy with the chat plugin and the fake provider.
func start(t *testing.T) *testutil.Instance {
	t.Helper()
	return testutil.Start(t, nil, chat.New(fakeProvider{}))
}

// msg is the shorthand for a top-level message from a bot in one channel.
func msg(channel, name, text, ts string) *chat.Message {
	return &chat.Message{
		Channel: chat.ChannelRef{ID: channel},
		Author:  chat.Author{Name: name, Bot: true},
		Text:    text,
		TS:      ts,
	}
}

// reply is msg with a parent.
func reply(channel, name, text, ts, parent string) *chat.Message {
	m := msg(channel, name, text, ts)
	m.ThreadTS = parent
	return m
}
