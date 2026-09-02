package hl7_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/hl7"
)

// fakeProvider is a test-only HL7 provider. It exists because the MLLP provider
// is the next wave and the plugin core still has to be proven end to end: it
// takes a v2 message as a plain body and converts it exactly the way a real
// provider will, and Inject does the same without going near a transport.
//
// It is deliberately HTTP rather than a socket. What the plugin core needs
// proving about is the model, the API and the tab; MLLP framing is the other
// task's problem and mixing the two would make a failure here ambiguous.
type fakeProvider struct{}

func (fakeProvider) Name() string   { return "fake" }
func (fakeProvider) Plugin() string { return hl7.Name }

func (fakeProvider) Description() string {
	return "A test-only endpoint that accepts an HL7 v2 message as a plain text body, so the hl7 plugin " +
		"can be exercised end to end before the MLLP provider exists."
}

func (fakeProvider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{{
		Method:      "POST",
		Path:        "/fake-hl7/messages",
		Description: "Accept an HL7 v2 message as a text body and record it as an hl7.message event.",
	}}
}

func (fakeProvider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{{
		Title: "Send a fake HL7 message",
		Lang:  "bash",
		Code: `printf 'MSH|^~\&|APP|FAC|DEST|DFAC|20240101120000||ADT^A01|MSG1|P|2.5\rPID|1||MRN1||DOE^JOHN\r' \
  | curl -s --data-binary @- {{.IngressURL}}/fake-hl7/messages`,
	}}
}

func (p fakeProvider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()
	mux.HandleFunc("POST /fake-hl7/messages", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ev, err := p.eventFor(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ev.Raw.PeerAddr = r.RemoteAddr
		if err := d.Append(r.Context(), ev); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		m, _ := hl7.MessageOf(ev)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         string(ev.ID),
			"control_id": m.Header.ControlID,
			"segments":   len(m.Segments),
		})
	})
}

func (fakeProvider) eventFor(raw []byte) (*event.Event, error) {
	m, err := hl7.Parse(raw)
	if err != nil {
		return nil, err
	}
	return hl7.NewEvent("fake", m, raw), nil
}

// Inject records a message straight into the store, which is how the plugin's
// own tests fill it: no transport in the way, so a failure is the plugin's.
func (p fakeProvider) Inject(ctx context.Context, d plugin.Deps, raw []byte) (*event.Event, error) {
	ev, err := p.eventFor(raw)
	if err != nil {
		return nil, err
	}
	return ev, d.Append(ctx, ev)
}

// inject is the test-side shorthand.
func inject(t *testing.T, in *testutil.Instance, raw []byte) *event.Event {
	t.Helper()
	ev, err := fakeProvider{}.Inject(context.Background(), plugin.Deps{Store: in.Store}, raw)
	if err != nil {
		t.Fatalf("inject message: %v", err)
	}
	return ev
}

func injectFixture(t *testing.T, in *testutil.Instance, name string) *event.Event {
	t.Helper()
	return inject(t, in, fixture(t, name))
}

// start boots a whole tommy with the hl7 plugin and the fake provider.
func start(t *testing.T) *testutil.Instance {
	t.Helper()
	return testutil.Start(t, nil, hl7.New(fakeProvider{}))
}
