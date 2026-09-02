package trap

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gosnmp/gosnmp"

	"github.com/can3p/tommy/plugins/snmp"
)

// oidSysUpTime and oidSnmpTrapOID are the two varbinds RFC 3416 §4.2.6-4.2.7
// requires to lead every SNMPv2-Trap-PDU and InformRequest-PDU's variable
// bindings, in that order (verified against the live RFC text, September
// 2026: "The first two variable bindings in the variable binding list of an
// SNMPv2-Trap-PDU are sysUpTime.0 and snmpTrapOID.0 respectively"). They are
// spelled without a leading dot here; gosnmp's own decoder always renders a
// parsed OID *with* one (see parseObjectIdentifier in its helper.go), so
// v2InfoFrom strips it off an incoming varbind's name before comparing.
const (
	oidSysUpTime   = "1.3.6.1.2.1.1.3.0"
	oidSnmpTrapOID = "1.3.6.1.6.3.1.1.4.1.0"
)

// fromPacket converts a decoded gosnmp packet into the plugin's canonical,
// gosnmp-free model. It is deliberately lenient: a v2c trap missing its
// conventional leading varbinds still gets recorded, just with V2 left at its
// zero value rather than rejected - CLAUDE.md rule 1's "accept any credentials
// by default" applied to malformed input in general, not just credentials.
func fromPacket(pkt *gosnmp.SnmpPacket) *snmp.Trap {
	t := &snmp.Trap{
		Community: pkt.Community,
		RequestID: pkt.RequestID,
		Inform:    pkt.PDUType == gosnmp.InformRequest,
		Varbinds:  varbindsFrom(pkt.Variables),
	}

	switch pkt.PDUType {
	case gosnmp.Trap:
		t.Version = snmp.VersionV1
		t.V1 = &snmp.V1Info{
			EnterpriseOID:   pkt.Enterprise,
			AgentAddress:    pkt.AgentAddress,
			GenericTrap:     pkt.GenericTrap,
			GenericTrapName: snmp.GenericTrapName(pkt.GenericTrap),
			SpecificTrap:    pkt.SpecificTrap,
			Timestamp:       uint32(pkt.Timestamp), //nolint:gosec // sysUpTime ticks fit uint32 on the wire already
		}
	case gosnmp.SNMPv2Trap, gosnmp.InformRequest:
		t.Version = snmp.VersionV2c
		t.V2 = v2InfoFrom(pkt.Variables)
	}
	return t
}

// v2InfoFrom pulls sysUpTime.0 and snmpTrapOID.0 out of a v2c/inform
// varbind list, wherever they actually are - not assumed to be positions 0
// and 1, since a malformed or hand-built sender is exactly the kind of thing
// worth still capturing. Returns nil only when neither is present at all, so
// a well-formed trap always gets a non-nil V2.
func v2InfoFrom(vars []gosnmp.SnmpPDU) *snmp.V2Info {
	var info snmp.V2Info
	found := false
	for _, v := range vars {
		switch stripLeadingDot(v.Name) {
		case oidSysUpTime:
			if tt, ok := v.Value.(uint32); ok {
				info.SysUpTime = tt
				found = true
			}
		case oidSnmpTrapOID:
			if oid, ok := v.Value.(string); ok {
				info.TrapOID = oid
				found = true
			}
		}
	}
	if !found {
		return nil
	}
	return &info
}

func stripLeadingDot(oid string) string { return strings.TrimPrefix(oid, ".") }

func varbindsFrom(vars []gosnmp.SnmpPDU) []snmp.Varbind {
	if len(vars) == 0 {
		return nil
	}
	out := make([]snmp.Varbind, 0, len(vars))
	for _, v := range vars {
		out = append(out, varbindFrom(v))
	}
	return out
}

// varbindFrom renders one decoded gosnmp.SnmpPDU as the plugin's Varbind.
//
// gosnmp's decoder already did the real work of telling an Integer from a
// Counter32 from an OID; what is left here is choosing how each of the Go
// types it produces (see decodeValue in gosnmp's own helper.go) is displayed
// - and, for OctetString/Opaque, deciding whether the bytes are printable
// text or need a hex dump instead.
func varbindFrom(v gosnmp.SnmpPDU) snmp.Varbind {
	vb := snmp.Varbind{OID: v.Name, Type: v.Type.String()}

	switch val := v.Value.(type) {
	case nil:
		// Null, NoSuchObject, NoSuchInstance, EndOfMibView, and a missing
		// IPAddress all decode to a nil value; Type already says which.
	case int:
		vb.Value = strconv.Itoa(val)
	case uint:
		vb.Value = strconv.FormatUint(uint64(val), 10)
	case uint32:
		vb.Value = strconv.FormatUint(uint64(val), 10)
	case uint64:
		vb.Value = strconv.FormatUint(val, 10)
	case string:
		// ObjectIdentifier and IPAddress are already formatted strings by
		// the time gosnmp hands them back.
		vb.Value = val
	case []byte:
		if isPrintableText(val) {
			vb.Value = string(val)
		} else {
			vb.Value = hex.EncodeToString(val)
			vb.Binary = true
		}
	case float32:
		vb.Value = strconv.FormatFloat(float64(val), 'g', -1, 32)
	case float64:
		vb.Value = strconv.FormatFloat(val, 'g', -1, 64)
	default:
		vb.Value = fmt.Sprintf("%v", val)
	}
	return vb
}

// isPrintableText decides how an OctetString or Opaque's raw bytes are
// presented: as text when they plausibly are some, as a hex dump otherwise.
// Empty is printable trivially - the varbind is simply empty text.
func isPrintableText(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	if !utf8.Valid(b) {
		return false
	}
	for _, r := range string(b) {
		if r == utf8.RuneError {
			return false
		}
		if r == 0x7f { // DEL
			return false
		}
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}
