// Package snmp is tommy's SNMP trap content type: the canonical Trap every
// provider converts into, and the plugin wiring around it.
//
// This is deliberately pure capture. SNMP traps and informs are fire-and-
// forget notifications - there is no query/response protocol for a client to
// read anything back over, unlike mail or sms - so the plugin has no
// bespoke API and no bespoke UI tab; see plugin.go's package comment for why
// that is a deliberate choice rather than an oversight.
//
// Two design points are load bearing:
//
//   - v1 and v2c traps are never flattened into one shape. A v1 trap carries
//     an enterprise OID, an agent address and a generic/specific trap-type
//     pair as PDU header fields; a v2c trap (or inform - the wire shape is
//     identical) carries none of that and instead leads its varbind list with
//     sysUpTime.0 and snmpTrapOID.0 by convention (RFC 3416 §4.2.6-4.2.7).
//     Trap.V1 and Trap.V2 are two separate, mutually exclusive structs rather
//     than one struct with fields that mean nothing for the other version.
//   - Every varbind keeps its wire type name and a human-rendered value
//     rather than being stringified. An OctetString may hold arbitrary
//     binary - hardware addresses, packed counters, worse - so a varbind
//     that is not printable text renders as a hex dump and says so, while the
//     exact bytes stay recoverable from the event's untouched raw datagram.
package snmp

import "strconv"

// Name is the plugin name and the URL segment it is mounted under.
const Name = "snmp"

// EventType is the event.Type every captured trap carries, whatever SNMP
// version or PDU type it arrived as. The plugin has exactly one canonical
// shape - see Trap - so one type is enough; a read surface switches on
// Trap.Version and Trap.Inform rather than on Event.Type.
const EventType = "snmp.trap"

// Transport is the Raw.Transport this plugin's provider records: SNMP traps
// and informs travel over UDP, never TCP.
const Transport = "udp"

// Version is the SNMP protocol version a trap arrived as.
//
// Only v1 and v2c are recognized. A v3 packet cannot be decoded without
// implementing its authentication/privacy security model, which this plugin
// does not do; the provider records it as a DecodeError instead of guessing.
type Version string

const (
	VersionV1  Version = "1"
	VersionV2c Version = "2c"
)

// Varbind is one decoded value from a trap's variable-binding list.
//
// Type is the wire type's own name - Integer, OctetString, ObjectIdentifier,
// Counter32, Counter64, Gauge32, TimeTicks, IPAddress, Opaque, Null,
// NoSuchObject, NoSuchInstance, EndOfMibView - rather than a tommy-invented
// label, so it reads the same as any MIB browser or net-snmp's own tools.
//
// Value is always a display string. When Type is OctetString or Opaque and
// the bytes are not printable text, Value holds a lowercase hex dump instead
// of the raw bytes and Binary is true - the exact bytes are still
// recoverable from the event's Raw.Body, the untouched UDP datagram, which
// this plugin always populates.
type Varbind struct {
	OID    string `json:"oid"`
	Type   string `json:"type"`
	Value  string `json:"value"`
	Binary bool   `json:"binary,omitempty"`
}

// V1Info is what a v1 trap (RFC 1157 §4.1.6) carries that a v2c one does
// not: header fields of the Trap-PDU itself, not varbinds. A v2c trap or
// inform leaves this nil rather than synthesizing zero values for fields it
// never had.
type V1Info struct {
	// EnterpriseOID identifies the type of managed object generating the
	// trap (SNMPv2-SMI sysObjectID of the sending agent, conventionally).
	EnterpriseOID string `json:"enterprise_oid"`
	// AgentAddress is the sending agent's own address, as it put it on the
	// wire - not necessarily the UDP source address a NAT or proxy agent
	// forwarded it from.
	AgentAddress string `json:"agent_address"`
	// GenericTrap is 0-6: coldStart, warmStart, linkDown, linkUp,
	// authenticationFailure, egpNeighborLoss, enterpriseSpecific.
	GenericTrap int `json:"generic_trap"`
	// GenericTrapName is GenericTrap's conventional name, or "" if it falls
	// outside the standard 0-6 range - see GenericTrapName.
	GenericTrapName string `json:"generic_trap_name,omitempty"`
	// SpecificTrap is only meaningful when GenericTrap is 6
	// (enterpriseSpecific); it names a trap defined by EnterpriseOID's own
	// MIB.
	SpecificTrap int `json:"specific_trap"`
	// Timestamp is the agent's sysUpTime when the trap was generated, in
	// hundredths of a second, exactly as the PDU carried it.
	Timestamp uint32 `json:"timestamp"`
}

// V2Info is what a v2c trap or inform (RFC 3416 §4.2.6-4.2.7) carries
// instead: no PDU header fields at all - the whole notification is a
// varbind list, whose first two entries are mandatory by convention.
//
// SysUpTime and TrapOID are pulled out here purely for convenience; both
// still appear in their original position in Trap.Varbinds too; this is not
// the only place they are visible.
type V2Info struct {
	SysUpTime uint32 `json:"sys_uptime"`
	TrapOID   string `json:"trap_oid"`
}

// Trap is the canonical model every provider converts its wire format into,
// and what lands in Event.Payload.
//
// Version decides which of V1 or V2 is populated; the other is always nil.
// Community is recorded but never checked - this plugin, like every provider
// in tommy, accepts any credential by default (CLAUDE.md rule 1).
type Trap struct {
	Version Version `json:"version"`
	// Inform reports whether this PDU was an InformRequest rather than an
	// (unconfirmed) Trap or SNMPv2-Trap. Only an inform gets a reply.
	Inform    bool      `json:"inform"`
	Community string    `json:"community"`
	RequestID uint32    `json:"request_id"`
	Varbinds  []Varbind `json:"varbinds,omitempty"`

	V1 *V1Info `json:"v1,omitempty"`
	V2 *V2Info `json:"v2,omitempty"`

	// DecodeError is set when the datagram could not be parsed as an SNMP
	// v1/v2c trap or inform at all - too short, malformed BER, or an SNMPv3
	// packet this plugin's security model does not support. The event is
	// still recorded: seeing that something arrived and could not be fully
	// understood is exactly what a capture tool is for. Version, Varbinds,
	// V1 and V2 are all left empty in this case; the untouched bytes are on
	// Event.Raw.Body regardless.
	DecodeError string `json:"decode_error,omitempty"`
}

// GenericTrapName maps a v1 generic-trap number (RFC 1157 §4.1.6, carried
// forward unchanged by SNMPv2-MIB) to its conventional name. Anything
// outside 0-6 returns "".
func GenericTrapName(n int) string {
	switch n {
	case 0:
		return "coldStart"
	case 1:
		return "warmStart"
	case 2:
		return "linkDown"
	case 3:
		return "linkUp"
	case 4:
		return "authenticationFailure"
	case 5:
		return "egpNeighborLoss"
	case 6:
		return "enterpriseSpecific"
	default:
		return ""
	}
}

// Title names the trap for a list row or an event summary.
func (t *Trap) Title() string {
	if t == nil {
		return "SNMP trap"
	}
	if t.DecodeError != "" && t.Version == "" {
		return "undecodable SNMP datagram"
	}
	switch {
	case t.Version == VersionV1 && t.V1 != nil:
		name := t.V1.GenericTrapName
		if name == "" {
			name = "trap"
		}
		if t.V1.GenericTrap == 6 {
			return "v1 " + name + " #" + strconv.Itoa(t.V1.SpecificTrap)
		}
		return "v1 " + name
	case t.Inform:
		if t.V2 != nil && t.V2.TrapOID != "" {
			return "v2c inform " + t.V2.TrapOID
		}
		return "v2c inform"
	case t.V2 != nil && t.V2.TrapOID != "":
		return "v2c trap " + t.V2.TrapOID
	default:
		return "SNMP trap"
	}
}

// Preview is the one-line description a list row and the store's search
// index see: how many varbinds the trap carried, and the first one's OID.
func (t *Trap) Preview() string {
	if t == nil {
		return ""
	}
	if len(t.Varbinds) == 0 {
		return t.DecodeError
	}
	n := len(t.Varbinds)
	return strconv.Itoa(n) + " varbind" + plural(n) + ", starting " + t.Varbinds[0].OID
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
