package sms_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/sms"
)

// The conformance gate: descriptions are real, snippets render, and every
// declared endpoint is actually mounted.
func TestConformance(t *testing.T) {
	plugintest.Conformance(t, sms.New(sms.WithProviders(fakeProvider{})))
}

// Until Wave 2 lands Twilio, sms.New() has no providers. That is the only thing
// conformance can hold against it, and this test pins that down so the day a
// provider is added nothing else has quietly rotted.
func TestBarePluginOnlyLacksAProvider(t *testing.T) {
	errs := plugintest.CheckPlugin(sms.New())
	if len(errs) != 1 {
		t.Fatalf("CheckPlugin(sms.New()) reported %d problems, want only the missing provider: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "Providers() is empty") {
		t.Errorf("unexpected conformance failure: %v", errs[0])
	}
}

func TestPluginIdentity(t *testing.T) {
	p := sms.New()
	if p.Name() != "sms" {
		t.Errorf("Name() = %q, want sms", p.Name())
	}
	if p.Title() != "SMS" {
		t.Errorf("Title() = %q, want SMS", p.Title())
	}
	if len(p.Description()) < 40 || !strings.Contains(strings.ToLower(p.Description()), "sms") {
		t.Errorf("Description() = %q, want a couple of real sentences", p.Description())
	}
	if got := p.Providers(); got == nil || len(got) != 0 {
		t.Errorf("Providers() = %v, want an empty non-nil slice until Wave 2", got)
	}

	names, err := fs.Glob(p.Templates(), "*.html")
	if err != nil {
		t.Fatalf("Templates(): %v", err)
	}
	if len(names) == 0 {
		t.Error("Templates() returned no templates; the tab cannot render")
	}
}

func TestWithProviders(t *testing.T) {
	p := sms.New(sms.WithProviders(fakeProvider{}))
	if len(p.Providers()) != 1 {
		t.Fatalf("Providers() = %d, want 1", len(p.Providers()))
	}
	if p.Providers()[0].Plugin() != sms.Name {
		t.Errorf("provider claims plugin %q, want %q", p.Providers()[0].Plugin(), sms.Name)
	}
}

// The routes the plugin advertises must all be mounted, and nothing else.
func TestRegisteredRoutes(t *testing.T) {
	tests := []struct {
		name       string
		register   func(mux plugin.Mux, d plugin.Deps)
		wantRoutes []string
	}{{
		name:     "api",
		register: sms.New().RegisterAPI,
		wantRoutes: []string{
			"GET /messages",
			"GET /messages/{id}",
			"GET /messages/{id}/media/{idx}",
			"DELETE /messages",
		},
	}, {
		name:     "ui",
		register: sms.New().RegisterUI,
		wantRoutes: []string{
			"GET /{$}",
			"GET /list",
			"GET /conversations/{key}",
			"DELETE /events",
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingMux{mux: http.NewServeMux()}
			tt.register(rec, plugintest.NewDeps())
			got := strings.Join(rec.patterns, " | ")
			for _, want := range tt.wantRoutes {
				if !strings.Contains(got, want) {
					t.Errorf("route %q was not mounted; got %s", want, got)
				}
			}
			if len(rec.patterns) != len(tt.wantRoutes) {
				t.Errorf("mounted %d routes (%s), want exactly %d", len(rec.patterns), got, len(tt.wantRoutes))
			}
		})
	}
}

type recordingMux struct {
	mux      *http.ServeMux
	patterns []string
}

func (m *recordingMux) Handle(pattern string, h http.Handler) {
	m.patterns = append(m.patterns, pattern)
	m.mux.Handle(pattern, h)
}

func (m *recordingMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.patterns = append(m.patterns, pattern)
	m.mux.HandleFunc(pattern, h)
}

// The plugin has to appear in the core's discovery surfaces, because that is
// where a user finds out how to poke it.
func TestPluginIsDiscoverable(t *testing.T) {
	in := start(t)

	var info []plugin.PluginInfo
	if status := in.GetJSON(in.API("/plugins"), &info); status != http.StatusOK {
		t.Fatalf("GET /plugins: status %d", status)
	}
	var found *plugin.PluginInfo
	for i := range info {
		if info[i].Name == sms.Name {
			found = &info[i]
		}
	}
	if found == nil {
		t.Fatalf("the sms plugin is missing from /api/v1/plugins: %+v", info)
	}
	if len(found.Providers) != 1 {
		t.Fatalf("sms lists %d providers, want the fake one", len(found.Providers))
	}
	prov := found.Providers[0]
	if len(prov.Snippets) == 0 {
		t.Fatal("the provider ships no snippets")
	}
	// Snippets are rendered against the ports this instance actually bound.
	code := prov.Snippets[0].Code
	if strings.Contains(code, "{{") {
		t.Errorf("snippet still contains a template action: %q", code)
	}
	if !strings.Contains(code, in.IngressURL) {
		t.Errorf("snippet = %q, want the live ingress URL %q", code, in.IngressURL)
	}

	// And the snippet actually works, which is the only claim worth making.
	t.Run("the snippet's request lands as an event", func(t *testing.T) {
		body := snippetPayload(t, code)
		status, resp := in.PostJSON(in.Ingress("/fake-sms/v1/messages"), body)
		if status != http.StatusCreated {
			t.Fatalf("status = %d: %s", status, resp)
		}
		if got := listMessages(t, in, ""); len(got) != 1 {
			t.Fatalf("got %d messages, want the one the snippet sent", len(got))
		}
	})
}

// snippetPayload pulls the JSON body out of the documented curl command, so the
// test exercises the snippet rather than a copy of it.
func snippetPayload(t *testing.T, code string) string {
	t.Helper()
	start := strings.Index(code, "-d '")
	if start < 0 {
		t.Fatalf("no -d payload in snippet %q", code)
	}
	rest := code[start+4:]
	end := strings.Index(rest, "'")
	if end < 0 {
		t.Fatalf("unterminated -d payload in snippet %q", code)
	}
	payload := rest[:end]
	if !json.Valid([]byte(payload)) {
		t.Fatalf("snippet payload is not valid JSON: %q", payload)
	}
	return payload
}

// A disabled plugin must vanish from every surface, without the tab 500ing.
func TestPluginCanBeDisabled(t *testing.T) {
	cfg := config.Ephemeral()
	cfg.SetPluginEnabled(sms.Name, false)
	in := testutil.Start(t, cfg, sms.New(sms.WithProviders(fakeProvider{})))

	if status, _ := in.GetBody(in.API("/sms/messages")); status == http.StatusOK {
		t.Errorf("a disabled plugin still answers on its API route (status %d)", status)
	}
	doc := uiDoc(t, in, in.UIURL)
	if strings.Contains(doc.Find("nav.tabs").Text(), "SMS") {
		t.Error("a disabled plugin still has a tab")
	}
}

// The plugin must not hold on to the message it stored: events are immutable
// once appended.
func TestReadBackDoesNotMutateStoredEvents(t *testing.T) {
	in := start(t)
	msg := &sms.Message{From: "+15005550006", To: "+15551234567", Body: "hi"}
	inject(t, in, msg)

	if _, err := in.Store.List(context.Background(), store.Query{Plugin: sms.Name}); err != nil {
		t.Fatal(err)
	}
	got := listMessages(t, in, "")
	if len(got) != 1 {
		t.Fatalf("got %d messages", len(got))
	}
	if msg.Body != "hi" {
		t.Errorf("the stored message was mutated by a read: %q", msg.Body)
	}
}
