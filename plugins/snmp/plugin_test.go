package snmp_test

import (
	"strings"
	"testing"

	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/plugins/snmp"
	"github.com/can3p/tommy/plugins/snmp/providers/trap"
)

// The conformance gate: descriptions are real, snippets render, and the
// listener provider (which mounts no HTTP route at all) is exempt from the
// endpoint checks rather than failing them.
func TestConformance(t *testing.T) {
	plugintest.Conformance(t, snmp.New(trap.New()))
}

func TestPluginIdentity(t *testing.T) {
	p := snmp.New(trap.New())
	if p.Name() != "snmp" {
		t.Errorf("Name() = %q, want snmp", p.Name())
	}
	if p.Title() != "SNMP" {
		t.Errorf("Title() = %q, want SNMP", p.Title())
	}
	if len(p.Description()) < 40 || !strings.Contains(strings.ToLower(p.Description()), "snmp") {
		t.Errorf("Description() = %q, want a couple of real sentences mentioning snmp", p.Description())
	}
	if got := p.Providers(); len(got) != 1 {
		t.Errorf("Providers() = %v, want exactly the trap provider", got)
	}
	// No bespoke UI: Templates() is nil on purpose - see plugin.go.
	if tpl := p.Templates(); tpl != nil {
		t.Errorf("Templates() = %v, want nil - this plugin relies on the generic event view", tpl)
	}
}

func TestBarePluginOnlyLacksAProvider(t *testing.T) {
	errs := plugintest.CheckPlugin(snmp.New())
	if len(errs) != 1 {
		t.Fatalf("CheckPlugin(snmp.New()) reported %d problems, want only the missing provider: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "Providers() is empty") {
		t.Errorf("unexpected conformance failure: %v", errs[0])
	}
}
