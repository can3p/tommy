package sms_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/sms"
)

// fakeProvider is a test-only SMS provider. It exists because the real Twilio
// provider is a later wave and the plugin still has to be proven end to end: it
// converts a trivial JSON body into the canonical model exactly the way a real
// provider does, and Inject does the same without going near HTTP.
type fakeProvider struct{}

func (fakeProvider) Name() string   { return "fake" }
func (fakeProvider) Plugin() string { return sms.Name }

func (fakeProvider) Description() string {
	return "A test-only SMS API that accepts a message as plain JSON, so the sms plugin can be exercised end to end before a real vendor provider exists."
}

func (fakeProvider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{{
		Method:      "POST",
		Path:        "/fake-sms/v1/messages",
		Description: "Accept an SMS or MMS as JSON and record it as an sms.message event.",
	}}
}

func (fakeProvider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{{
		Title: "Send a fake SMS",
		Lang:  "bash",
		Code: `curl -s {{.IngressURL}}/fake-sms/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{"from":"+15005550006","to":"+15551234567","body":"It works."}'`,
	}}
}

func (p fakeProvider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()
	mux.HandleFunc("POST /fake-sms/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var m sms.Message
		if err := json.Unmarshal(body, &m); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		ev := p.event(&m, nil)
		ev.Raw.PeerAddr = r.RemoteAddr
		ev.Raw.Headers = r.Header.Clone()
		ev.Raw.Body = body
		if err := d.Append(r.Context(), ev); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           string(ev.ID),
			"status":       string(m.Status),
			"num_segments": m.Segments.Count,
		})
	})
}

// event builds the event the fake would record for a message.
func (fakeProvider) event(m *sms.Message, meta map[string]any) *event.Event {
	m.Normalize()
	raw, _ := json.Marshal(m)
	return &event.Event{
		Plugin:   sms.Name,
		Provider: "fake",
		Type:     sms.EventType,
		Summary:  m.EventSummary(),
		Meta:     meta,
		Payload:  m,
		Raw: event.Raw{
			Transport: "http",
			Method:    "POST",
			Path:      "/fake-sms/v1/messages",
			Body:      raw,
			Text:      true,
		},
	}
}

// Inject records a message straight into the store, which is how the plugin's
// own tests fill it: no wire format in the way, so a failure is the plugin's.
func (p fakeProvider) Inject(ctx context.Context, d plugin.Deps, m *sms.Message, meta map[string]any) (*event.Event, error) {
	ev := p.event(m, meta)
	return ev, d.Append(ctx, ev)
}

// inject is the test-side shorthand.
func inject(t *testing.T, in *testutil.Instance, m *sms.Message) *event.Event {
	t.Helper()
	return injectMeta(t, in, m, nil)
}

func injectMeta(t *testing.T, in *testutil.Instance, m *sms.Message, meta map[string]any) *event.Event {
	t.Helper()
	ev, err := fakeProvider{}.Inject(context.Background(), plugin.Deps{Store: in.Store}, m, meta)
	if err != nil {
		t.Fatalf("inject message: %v", err)
	}
	return ev
}

// start boots a whole tommy with the sms plugin and the fake provider.
func start(t *testing.T) *testutil.Instance {
	t.Helper()
	return testutil.Start(t, nil, sms.New(sms.WithProviders(fakeProvider{})))
}
