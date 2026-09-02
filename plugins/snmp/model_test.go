package snmp_test

import (
	"strings"
	"testing"

	"github.com/can3p/tommy/plugins/snmp"
)

func TestGenericTrapName(t *testing.T) {
	cases := map[int]string{
		0: "coldStart", 1: "warmStart", 2: "linkDown", 3: "linkUp",
		4: "authenticationFailure", 5: "egpNeighborLoss", 6: "enterpriseSpecific",
		7: "", -1: "", 100: "",
	}
	for n, want := range cases {
		if got := snmp.GenericTrapName(n); got != want {
			t.Errorf("GenericTrapName(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTrapTitleV1(t *testing.T) {
	tr := &snmp.Trap{
		Version: snmp.VersionV1,
		V1: &snmp.V1Info{
			GenericTrap:     2,
			GenericTrapName: snmp.GenericTrapName(2),
		},
	}
	if got := tr.Title(); got != "v1 linkDown" {
		t.Errorf("Title() = %q, want %q", got, "v1 linkDown")
	}
}

func TestTrapTitleV1EnterpriseSpecific(t *testing.T) {
	tr := &snmp.Trap{
		Version: snmp.VersionV1,
		V1: &snmp.V1Info{
			GenericTrap:     6,
			GenericTrapName: snmp.GenericTrapName(6),
			SpecificTrap:    42,
		},
	}
	if got := tr.Title(); got != "v1 enterpriseSpecific #42" {
		t.Errorf("Title() = %q, want %q", got, "v1 enterpriseSpecific #42")
	}
}

func TestTrapTitleV2c(t *testing.T) {
	tr := &snmp.Trap{
		Version: snmp.VersionV2c,
		V2:      &snmp.V2Info{TrapOID: ".1.3.6.1.6.3.1.1.5.3"},
	}
	if got := tr.Title(); got != "v2c trap .1.3.6.1.6.3.1.1.5.3" {
		t.Errorf("Title() = %q", got)
	}
}

func TestTrapTitleInform(t *testing.T) {
	tr := &snmp.Trap{
		Version: snmp.VersionV2c,
		Inform:  true,
		V2:      &snmp.V2Info{TrapOID: ".1.3.6.1.6.3.1.1.5.3"},
	}
	if got := tr.Title(); got != "v2c inform .1.3.6.1.6.3.1.1.5.3" {
		t.Errorf("Title() = %q", got)
	}
}

func TestTrapTitleDecodeError(t *testing.T) {
	tr := &snmp.Trap{DecodeError: "short buffer"}
	if got := tr.Title(); got != "undecodable SNMP datagram" {
		t.Errorf("Title() = %q", got)
	}
}

func TestTrapPreview(t *testing.T) {
	tr := &snmp.Trap{Varbinds: []snmp.Varbind{
		{OID: ".1.3.6.1.2.1.1.3.0", Type: "TimeTicks", Value: "12345"},
		{OID: ".1.3.6.1.6.3.1.1.4.1.0", Type: "ObjectIdentifier", Value: ".1.3.6.1.6.3.1.1.5.3"},
	}}
	got := tr.Preview()
	if !strings.HasPrefix(got, "2 varbinds, starting .1.3.6.1.2.1.1.3.0") {
		t.Errorf("Preview() = %q", got)
	}
}

func TestTrapPreviewSingular(t *testing.T) {
	tr := &snmp.Trap{Varbinds: []snmp.Varbind{{OID: ".1.2.3"}}}
	if got := tr.Preview(); got != "1 varbind, starting .1.2.3" {
		t.Errorf("Preview() = %q", got)
	}
}

func TestTrapPreviewDecodeError(t *testing.T) {
	tr := &snmp.Trap{DecodeError: "boom"}
	if got := tr.Preview(); got != "boom" {
		t.Errorf("Preview() = %q, want the decode error", got)
	}
}

func TestNilTrapIsSafe(t *testing.T) {
	var tr *snmp.Trap
	if got := tr.Title(); got != "SNMP trap" {
		t.Errorf("nil Title() = %q", got)
	}
	if got := tr.Preview(); got != "" {
		t.Errorf("nil Preview() = %q", got)
	}
}
