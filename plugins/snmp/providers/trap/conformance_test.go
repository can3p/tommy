package trap_test

import (
	"testing"
	"time"

	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/snmp"
	"github.com/can3p/tommy/plugins/snmp/providers/trap"
)

// TestConformance proves the provider on its own satisfies the
// discoverability contract: a real description, at least one snippet that
// renders, and no dangling endpoint declaration - a ListenerProvider is
// exempt from mounting any HTTP route at all.
func TestConformance(t *testing.T) {
	plugintest.ConformanceProvider(t, trap.New())
}

// TestPluginConformance proves the same thing through the snmp plugin, the
// way it is actually wired up.
func TestPluginConformance(t *testing.T) {
	plugintest.Conformance(t, snmp.New(trap.New()))
}

// TestBootsViaTestutil is the shape every provider must work from: snmp.New
// wrapping the real provider, started through the shared test harness on an
// ephemeral port.
func TestBootsViaTestutil(t *testing.T) {
	prov := trap.New()
	inst := testutil.Start(t, nil, snmp.New(prov))

	addr, err := prov.Addr(5 * time.Second)
	if err != nil {
		t.Fatalf("listener never bound: %v", err)
	}
	if addr == "" {
		t.Fatal("Addr() returned an empty address")
	}
	if _, ok := inst.Registry.Plugin(snmp.Name); !ok {
		t.Fatal("snmp plugin not registered")
	}
}
