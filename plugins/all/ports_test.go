package all_test

import (
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/all"
)

// TestEveryShippedListenerReportsADistinctDefaultPort is the check
// plugintest.Conformance cannot make on its own: it sees one provider at a
// time, and a default port is only interesting next to the others.
//
// Two things have to hold for the port list to be derivable rather than
// hand-maintained - which is what the image's EXPOSE list, the compose file
// and the site's port table all depend on. Every shipped listener provider
// must have a default port (one that picks a random port each run would be
// unpublishable, and would silently drop out of all three), and no two may
// claim the same one, because a default tommy starts all of them at once and
// the second would fail to bind.
//
// Nothing binds here: every provider is asked, none is started.
func TestEveryShippedListenerReportsADistinctDefaultPort(t *testing.T) {
	reg, err := plugin.New(config.Default(), all.Plugins()...)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	refs := reg.ListenerRefs()
	if len(refs) == 0 {
		t.Skip("this build has no listener providers compiled in")
	}

	claimed := map[string]string{} // "2575/tcp" -> "hl7/mllp"
	for _, ref := range refs {
		name := ref.Plugin.Name() + "/" + ref.Provider.Name()
		lp, ok := reg.ListenPort(ref.Plugin.Name(), ref.Provider.Name())
		if !ok {
			t.Errorf("%s does not implement plugin.PortProvider, so nothing can publish its port", name)
			continue
		}
		if lp.Ephemeral() {
			t.Errorf("%s has no default port: a shipped listener must land somewhere predictable, or `docker run -p` has nothing to publish", name)
			continue
		}
		// Every default is unprivileged, so the container can run as a
		// non-root user with no capability grants.
		if lp.Port < 1024 {
			t.Errorf("%s defaults to %s, a privileged port; tommy must run unprivileged", name, lp)
		}
		if prev, dup := claimed[lp.String()]; dup {
			t.Errorf("%s and %s both default to %s; a default tommy starts both and the second cannot bind", prev, name, lp)
		}
		claimed[lp.String()] = name
	}
}
