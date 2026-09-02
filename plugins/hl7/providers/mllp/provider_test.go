package mllp_test

import (
	"testing"
	"time"

	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/hl7"
	"github.com/can3p/tommy/plugins/hl7/providers/mllp"
)

// TestConformance proves the provider on its own satisfies the
// discoverability contract: a real description, at least one snippet that
// renders, and no dangling endpoint declaration - a ListenerProvider is
// exempt from mounting any HTTP route at all.
func TestConformance(t *testing.T) {
	plugintest.ConformanceProvider(t, mllp.New())
}

// TestPluginConformance proves the same thing through the hl7 plugin, the
// way it is actually wired up - and, since hl7's core was built and shipped
// with no provider at all, this is also the proof that a plugin with a real
// provider now passes plugintest.Conformance, which a providerless one could
// not.
func TestPluginConformance(t *testing.T) {
	plugintest.Conformance(t, hl7.New(mllp.New()))
}

// TestBootsViaTestutil is the shape every provider must work from: hl7.New
// wrapping the real provider, started through the shared test harness on an
// ephemeral port.
func TestBootsViaTestutil(t *testing.T) {
	prov := mllp.New()
	inst := testutil.Start(t, nil, hl7.New(prov))

	addr, err := prov.Addr(5 * time.Second)
	if err != nil {
		t.Fatalf("listener never bound: %v", err)
	}
	if addr == "" {
		t.Fatal("Addr() returned an empty address")
	}
	if _, ok := inst.Registry.Plugin(hl7.Name); !ok {
		t.Fatal("hl7 plugin not registered")
	}
}
