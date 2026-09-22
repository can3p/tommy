package s3_test

import (
	"net/http"
	"testing"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/plugins/s3"
)

type fakeProvider struct{ store *s3.Store }

func (p *fakeProvider) BindStore(store *s3.Store) { p.store = store }
func (*fakeProvider) Name() string                { return "fake" }
func (*fakeProvider) Plugin() string              { return s3.PluginName }
func (*fakeProvider) Description() string {
	return "A test-only S3-compatible HTTP endpoint used to verify the plugin and shared-store provider contract."
}
func (*fakeProvider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{{Method: http.MethodPost, Path: "/fake-s3", Description: "Accept a test-only S3 operation for plugin conformance."}}
}
func (*fakeProvider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{{Title: "Call the fake S3 endpoint", Lang: "sh", Code: `curl -X POST {{.IngressURL}}/fake-s3`}}
}
func (*fakeProvider) RegisterIngress(mux plugin.Mux, _ plugin.Deps) {
	mux.HandleFunc("POST /fake-s3", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
}

func TestConformance(t *testing.T) {
	provider := &fakeProvider{}
	plugintest.ConformanceProvider(t, provider)
	plugintest.Conformance(t, s3.New(provider))
}

func TestPluginIdentityAndSharedStore(t *testing.T) {
	a, b := &fakeProvider{}, &fakeProvider{}
	p := s3.New(a, b)
	if p.Name() != "s3" || p.Title() != "S3" {
		t.Fatalf("identity = %q/%q", p.Name(), p.Title())
	}
	if p.Store() == nil || a.store != p.Store() || b.store != p.Store() {
		t.Fatal("providers did not receive the plugin's one shared store")
	}
	if got := s3.New().Providers(); got == nil || len(got) != 0 {
		t.Fatalf("Providers() = %#v, want a non-nil empty slice", got)
	}
	if p.Templates() == nil {
		t.Fatal("Templates() must expose the embedded S3 tab templates")
	}
}
