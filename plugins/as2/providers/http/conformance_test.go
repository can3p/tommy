package http_test

import (
	"strings"
	"testing"

	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/plugins/as2"
	as2http "github.com/can3p/tommy/plugins/as2/providers/http"
)

// TestConformance is the gate every provider passes: descriptions that say
// something, snippets that parse and render, and - the two that actually bite -
// every declared endpoint mounted and every mounted route declared.
//
// The config directory is redirected first. Conformance registers the ingress
// with an empty ProviderConfig, so nothing pins where a generated certificate
// goes, and without this a plain `make check` would write a key pair into the
// real user's config directory.
func TestConformance(t *testing.T) {
	isolateConfigDir(t)
	plugintest.ConformanceProvider(t, as2http.New())

	// And the plugin as a whole, which only became checkable once it had a
	// provider: a plugin with none is correctly rejected, because without one
	// nothing can reach it.
	plugintest.Conformance(t, as2.New(as2http.New()))
}

// TestDiscoverySurface checks the thing a newcomer actually meets. Endpoints
// and snippets are only useful if they reach /api/v1/plugins, the how-to-test
// panel and `tommy providers` - all three of which render from the same place -
// and if the snippets there carry the ports this instance really bound rather
// than a hardcoded 8822.
func TestDiscoverySurface(t *testing.T) {
	in := start(t, nil)

	status, body := in.GetBody(in.API("/plugins"))
	if status != 200 {
		t.Fatalf("GET /plugins = %d", status)
	}
	for _, want := range []string{
		`"/as2"`,
		`"/as2/certificate"`,
		in.IngressURL + "/as2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the discovery surface does not carry %q", want)
		}
	}
	// A snippet that leaked a template port would be wrong the moment somebody
	// passed --in-port, and would still have rendered without error.
	if strings.Contains(body, "localhost:8822") {
		t.Error("a snippet rendered a hardcoded ingress port")
	}
}
