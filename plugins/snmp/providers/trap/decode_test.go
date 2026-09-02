package trap

import (
	"testing"

	"github.com/gosnmp/gosnmp"
)

func TestVarbindFromTypes(t *testing.T) {
	cases := []struct {
		name   string
		pdu    gosnmp.SnmpPDU
		want   string
		binary bool
	}{
		{"Integer", gosnmp.SnmpPDU{Type: gosnmp.Integer, Value: -42}, "-42", false},
		{"OctetString printable", gosnmp.SnmpPDU{Type: gosnmp.OctetString, Value: []byte("hello world")}, "hello world", false},
		{"OctetString empty", gosnmp.SnmpPDU{Type: gosnmp.OctetString, Value: []byte{}}, "", false},
		{"ObjectIdentifier", gosnmp.SnmpPDU{Type: gosnmp.ObjectIdentifier, Value: ".1.3.6.1.2.1.1.5.0"}, ".1.3.6.1.2.1.1.5.0", false},
		{"IPAddress", gosnmp.SnmpPDU{Type: gosnmp.IPAddress, Value: "192.0.2.1"}, "192.0.2.1", false},
		{"Counter32", gosnmp.SnmpPDU{Type: gosnmp.Counter32, Value: uint(4294967295)}, "4294967295", false},
		{"Gauge32", gosnmp.SnmpPDU{Type: gosnmp.Gauge32, Value: uint(100)}, "100", false},
		{"TimeTicks", gosnmp.SnmpPDU{Type: gosnmp.TimeTicks, Value: uint32(12345)}, "12345", false},
		{"Counter64", gosnmp.SnmpPDU{Type: gosnmp.Counter64, Value: uint64(18446744073709551615)}, "18446744073709551615", false},
		{"Null", gosnmp.SnmpPDU{Type: gosnmp.Null, Value: nil}, "", false},
		{"NoSuchObject", gosnmp.SnmpPDU{Type: gosnmp.NoSuchObject, Value: nil}, "", false},
		{"NoSuchInstance", gosnmp.SnmpPDU{Type: gosnmp.NoSuchInstance, Value: nil}, "", false},
		{"EndOfMibView", gosnmp.SnmpPDU{Type: gosnmp.EndOfMibView, Value: nil}, "", false},
		{"OpaqueFloat", gosnmp.SnmpPDU{Type: gosnmp.OpaqueFloat, Value: float32(3.5)}, "3.5", false},
		{"OpaqueDouble", gosnmp.SnmpPDU{Type: gosnmp.OpaqueDouble, Value: float64(3.5)}, "3.5", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.pdu.Name = ".1.2.3"
			vb := varbindFrom(c.pdu)
			if vb.OID != ".1.2.3" {
				t.Errorf("OID = %q", vb.OID)
			}
			if vb.Type != c.pdu.Type.String() {
				t.Errorf("Type = %q, want %q", vb.Type, c.pdu.Type.String())
			}
			if vb.Value != c.want {
				t.Errorf("Value = %q, want %q", vb.Value, c.want)
			}
			if vb.Binary != c.binary {
				t.Errorf("Binary = %v, want %v", vb.Binary, c.binary)
			}
		})
	}
}

// TestVarbindFromBinaryOctetString is the case the plan calls out explicitly:
// an OctetString may hold arbitrary binary, and it must render as a hex dump
// with Binary set rather than mangled or silently truncated as text.
func TestVarbindFromBinaryOctetString(t *testing.T) {
	raw := []byte{0x00, 0x01, 0xff, 0xfe, 0x80, 'h', 'i'}
	vb := varbindFrom(gosnmp.SnmpPDU{Name: ".1.2.3", Type: gosnmp.OctetString, Value: raw})
	if !vb.Binary {
		t.Fatal("Binary = false, want true for non-printable bytes")
	}
	if vb.Value != "0001fffe806869" {
		t.Errorf("Value = %q, want the lowercase hex dump", vb.Value)
	}
}

// TestVarbindFromMACAddress proves a classic binary OctetString - a 6-byte
// hardware address - renders as hex, since it is very unlikely to look like
// text but is exactly the kind of value SNMP varbinds carry constantly
// (ifPhysAddress and friends).
func TestVarbindFromMACAddress(t *testing.T) {
	mac := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01}
	vb := varbindFrom(gosnmp.SnmpPDU{Name: ".1.3.6.1.2.1.2.2.1.6.1", Type: gosnmp.OctetString, Value: mac})
	if !vb.Binary || vb.Value != "deadbeef0001" {
		t.Errorf("vb = %+v, want a hex dump with Binary set", vb)
	}
}

func TestIsPrintableText(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"empty", nil, true},
		{"ascii", []byte("hello, world"), true},
		{"tab newline cr", []byte("a\tb\nc\rd"), true},
		{"utf8", []byte("café"), true},
		{"null byte", []byte{'a', 0x00, 'b'}, false},
		{"del", []byte{0x7f}, false},
		{"invalid utf8", []byte{0xff, 0xfe, 0xfd}, false},
		{"mac address", []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPrintableText(c.in); got != c.want {
				t.Errorf("isPrintableText(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestFromPacketV1Trap(t *testing.T) {
	pkt := &gosnmp.SnmpPacket{
		Version:   gosnmp.Version1,
		Community: "public",
		PDUType:   gosnmp.Trap,
		SnmpTrap: gosnmp.SnmpTrap{
			Enterprise:   ".1.3.6.1.4.1.8072.3.2.10",
			AgentAddress: "192.0.2.1",
			GenericTrap:  3,
			SpecificTrap: 0,
			Timestamp:    123456,
		},
		Variables: []gosnmp.SnmpPDU{
			{Name: ".1.3.6.1.2.1.2.2.1.1.1", Type: gosnmp.Integer, Value: 1},
		},
	}
	tr := fromPacket(pkt)

	if tr.Version != "1" {
		t.Errorf("Version = %q, want 1", tr.Version)
	}
	if tr.Inform {
		t.Error("Inform = true for a v1 trap")
	}
	if tr.V2 != nil {
		t.Errorf("V2 = %+v, want nil for a v1 trap - fields must not be synthesized", tr.V2)
	}
	if tr.V1 == nil {
		t.Fatal("V1 = nil")
	}
	if tr.V1.EnterpriseOID != ".1.3.6.1.4.1.8072.3.2.10" {
		t.Errorf("EnterpriseOID = %q", tr.V1.EnterpriseOID)
	}
	if tr.V1.AgentAddress != "192.0.2.1" {
		t.Errorf("AgentAddress = %q", tr.V1.AgentAddress)
	}
	if tr.V1.GenericTrap != 3 || tr.V1.GenericTrapName != "linkUp" {
		t.Errorf("GenericTrap = %d %q", tr.V1.GenericTrap, tr.V1.GenericTrapName)
	}
	if tr.V1.Timestamp != 123456 {
		t.Errorf("Timestamp = %d", tr.V1.Timestamp)
	}
	if len(tr.Varbinds) != 1 || tr.Varbinds[0].Value != "1" {
		t.Errorf("Varbinds = %+v", tr.Varbinds)
	}
}

func TestFromPacketV2cTrap(t *testing.T) {
	pkt := &gosnmp.SnmpPacket{
		Version:   gosnmp.Version2c,
		Community: "public",
		PDUType:   gosnmp.SNMPv2Trap,
		RequestID: 99,
		Variables: []gosnmp.SnmpPDU{
			{Name: ".1.3.6.1.2.1.1.3.0", Type: gosnmp.TimeTicks, Value: uint32(4200)},
			{Name: ".1.3.6.1.6.3.1.1.4.1.0", Type: gosnmp.ObjectIdentifier, Value: ".1.3.6.1.6.3.1.1.5.3"},
			{Name: ".1.3.6.1.2.1.1.5.0", Type: gosnmp.OctetString, Value: []byte("host01")},
		},
	}
	tr := fromPacket(pkt)

	if tr.Version != "2c" {
		t.Errorf("Version = %q, want 2c", tr.Version)
	}
	if tr.Inform {
		t.Error("Inform = true for an SNMPv2Trap PDU")
	}
	if tr.V1 != nil {
		t.Errorf("V1 = %+v, want nil for a v2c trap", tr.V1)
	}
	if tr.V2 == nil {
		t.Fatal("V2 = nil")
	}
	if tr.V2.SysUpTime != 4200 {
		t.Errorf("SysUpTime = %d", tr.V2.SysUpTime)
	}
	if tr.V2.TrapOID != ".1.3.6.1.6.3.1.1.5.3" {
		t.Errorf("TrapOID = %q", tr.V2.TrapOID)
	}
	if len(tr.Varbinds) != 3 {
		t.Fatalf("Varbinds = %+v, want 3", tr.Varbinds)
	}
	if tr.RequestID != 99 {
		t.Errorf("RequestID = %d", tr.RequestID)
	}
}

func TestFromPacketInform(t *testing.T) {
	pkt := &gosnmp.SnmpPacket{
		Version:   gosnmp.Version2c,
		Community: "public",
		PDUType:   gosnmp.InformRequest,
		Variables: []gosnmp.SnmpPDU{
			{Name: ".1.3.6.1.2.1.1.3.0", Type: gosnmp.TimeTicks, Value: uint32(1)},
			{Name: ".1.3.6.1.6.3.1.1.4.1.0", Type: gosnmp.ObjectIdentifier, Value: ".1.3.6.1.6.3.1.1.5.1"},
		},
	}
	tr := fromPacket(pkt)
	if !tr.Inform {
		t.Error("Inform = false for an InformRequest PDU")
	}
	if tr.Version != "2c" {
		t.Errorf("Version = %q, want 2c", tr.Version)
	}
}

// TestV2InfoFromMissingConventionalVarbinds proves a v2c trap that does not
// lead with sysUpTime.0/snmpTrapOID.0 is still captured - malformed input is
// not rejected - just without a synthesized V2.
func TestV2InfoFromMissingConventionalVarbinds(t *testing.T) {
	pkt := &gosnmp.SnmpPacket{
		Version:   gosnmp.Version2c,
		PDUType:   gosnmp.SNMPv2Trap,
		Variables: []gosnmp.SnmpPDU{{Name: ".1.2.3", Type: gosnmp.Integer, Value: 1}},
	}
	tr := fromPacket(pkt)
	if tr.V2 != nil {
		t.Errorf("V2 = %+v, want nil when neither conventional varbind is present", tr.V2)
	}
	if len(tr.Varbinds) != 1 {
		t.Errorf("Varbinds = %+v, want the one varbind still recorded", tr.Varbinds)
	}
}

func TestStripLeadingDot(t *testing.T) {
	if got := stripLeadingDot(".1.2.3"); got != "1.2.3" {
		t.Errorf("got %q", got)
	}
	if got := stripLeadingDot("1.2.3"); got != "1.2.3" {
		t.Errorf("got %q", got)
	}
}
