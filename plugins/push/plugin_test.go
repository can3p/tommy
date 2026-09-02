package push_test

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/plugins/push"
)

// The conformance gate: descriptions are real, snippets render, and every
// declared endpoint is actually mounted.
func TestConformance(t *testing.T) {
	plugintest.Conformance(t, push.New(fakeProvider{}))
	plugintest.ConformanceProvider(t, fakeProvider{})
}

// Until the FCM provider lands, push.New() has no providers. That is the only
// thing conformance can hold against it, and it is why this plugin is not in
// plugins/all/all.go yet. This test pins that down so the day a provider is
// added nothing else has quietly rotted.
func TestBarePluginOnlyLacksAProvider(t *testing.T) {
	errs := plugintest.CheckPlugin(push.New())
	if len(errs) != 1 {
		t.Fatalf("CheckPlugin(push.New()) reported %d problems, want only the missing provider: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "Providers() is empty") {
		t.Errorf("unexpected conformance failure: %v", errs[0])
	}
}

func TestPluginIdentity(t *testing.T) {
	p := push.New()
	if p.Name() != "push" {
		t.Errorf("Name() = %q, want push", p.Name())
	}
	if p.Title() != "Push" {
		t.Errorf("Title() = %q, want Push", p.Title())
	}
	d := strings.ToLower(p.Description())
	if len(d) < 40 || !strings.Contains(d, "push") {
		t.Errorf("Description() = %q, want a couple of real sentences", p.Description())
	}
	// With no provider there are no endpoints to advertise, so the description
	// has to say where they come from instead.
	if !strings.Contains(d, "provider") {
		t.Errorf("Description() must say the endpoints arrive with the providers: %q", p.Description())
	}
	if got := p.Providers(); got == nil || len(got) != 0 {
		t.Errorf("Providers() = %v, want an empty non-nil slice until a provider lands", got)
	}

	names, err := fs.Glob(p.Templates(), "*.html")
	if err != nil {
		t.Fatalf("Templates(): %v", err)
	}
	if len(names) == 0 {
		t.Error("Templates() returned no templates; the tab cannot render")
	}
}

// The whole path, over the real server: a push posted to a provider is
// converted, stored, and readable back through the plugin's own API.
func TestEndToEnd(t *testing.T) {
	in := start(t)

	resp, err := in.Client.Post(
		in.Ingress("/fake-push/fcm/my-project"), "application/json",
		strings.NewReader(string(fixture(t, "fcm_topic_data.json"))))
	if err != nil {
		t.Fatalf("post message: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID       string `json:"id"`
		Kind     string `json:"kind"`
		Displays bool   `json:"displays"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.Kind != "silent" || created.Displays {
		t.Errorf("created = %+v, want a silent push", created)
	}

	var envelope struct {
		Title    string `json:"title"`
		Displays bool   `json:"displays"`
		Explain  string `json:"explain"`
		Message  struct {
			Target struct {
				Kind   string `json:"kind"`
				Value  string `json:"value"`
				Source string `json:"source"`
			} `json:"target"`
		} `json:"message"`
	}
	if status := in.GetJSON(in.API("/push/messages/"+created.ID), &envelope); status != http.StatusOK {
		t.Fatalf("read back status = %d", status)
	}
	if envelope.Message.Target.Kind != "topic" || envelope.Message.Target.Value != "weather" {
		t.Errorf("read back target = %+v", envelope.Message.Target)
	}
	if envelope.Displays || envelope.Explain == "" {
		t.Errorf("the API must say plainly that this displayed nothing: %+v", envelope)
	}

	// And an APNs-shaped request over the same server, so the two request
	// shapes are both proven end to end.
	req, err := http.NewRequest(http.MethodPost,
		in.Ingress("/fake-push/apns/00fc13adff785122b4ad28809a3420982341241421348097878e577c991de8f0"),
		strings.NewReader(string(fixture(t, "apns_alert.json"))))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range apnsAlertHeaders {
		req.Header.Set(k, v)
	}
	apnsResp := in.Do(req)
	defer func() { _ = apnsResp.Body.Close() }()
	if apnsResp.StatusCode != http.StatusCreated {
		t.Fatalf("apns status = %d, want 201", apnsResp.StatusCode)
	}

	events := in.Events(store.Query{Plugin: push.Name})
	if len(events) != 2 {
		t.Fatalf("captured %d events, want 2", len(events))
	}
}
