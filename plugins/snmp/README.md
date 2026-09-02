# snmp

Captures the SNMP v1 and v2c traps and informs your infrastructure sends
instead of a real network management station. Every varbind is decoded by its
actual wire type - integers, object identifiers, counters, gauges, timeticks,
IP addresses and octet strings - rather than flattened to one string, and an
octet string that is not printable text is hex-dumped rather than mangled.

There is no listener built into the core: the trap provider (`providers/trap`)
does that. Shortcut: `tommy snmp`.

## v1 and v2c are not the same shape

A v1 trap (RFC 1157 §4.1.6) is a PDU with its own header fields: an
enterprise OID, the sending agent's address, a generic/specific trap-type
pair, and a sysUpTime timestamp. A v2c trap or inform (RFC 3416 §4.2.6-4.2.7)
carries none of that - the entire notification is a varbind list, whose first
two entries are `sysUpTime.0` and `snmpTrapOID.0` by convention.

`Trap.V1` and `Trap.V2` are two separate, mutually exclusive structs rather
than one struct with fields that mean nothing for the other version:

```go
type Trap struct {
    Version   Version // "1" or "2c"
    Inform    bool    // only an inform gets a reply - see providers/trap
    Community string  // recorded, never checked
    RequestID uint32
    Varbinds  []Varbind

    V1 *V1Info // nil for a v2c trap or inform
    V2 *V2Info // nil for a v1 trap
}

type V1Info struct {
    EnterpriseOID   string
    AgentAddress    string
    GenericTrap     int    // 0-6: coldStart .. enterpriseSpecific
    GenericTrapName string
    SpecificTrap    int
    Timestamp       uint32
}

type V2Info struct {
    SysUpTime uint32
    TrapOID   string
}

type Varbind struct {
    OID    string
    Type   string // "Integer", "OctetString", "Counter32", ... - the wire type's own name
    Value  string // human-rendered; a hex dump when Type is a binary OctetString/Opaque
    Binary bool   // true when Value is a hex dump, not the bytes themselves
}
```

## What is deliberately not here: a bespoke API or UI tab

SNMP traps and informs are fire-and-forget UDP notifications - there is no
query/response protocol for a client to read anything back over, unlike mail
or sms. So this plugin mounts nothing of its own under `/api/v1/snmp/` or
`/ui/snmp/`; it gets the generic cross-plugin surfaces for free:

- `GET /api/v1/events?plugin=snmp` lists every captured trap; `GET
  /api/v1/events/{id}` returns one in full, `Payload` included.
- `/ui/snmp/` is the generic event view: a filterable list, and a detail pane
  whose payload panel is a collapsible JSON inspector - which already reads as
  the varbind table this plugin's roadmap entry asks for, since `Trap`'s JSON
  shape puts every binding's OID/Type/Value right there. The raw panel
  hex-dumps the untouched UDP datagram.

This is a deliberate choice, not an unfinished one - see `plugin.go`'s comment
on `RegisterUI` for the reasoning, and `docs/implementation-plan.md` §6c: this
plugin exists partly to test whether a new protocol plugin really is useful on
day one with zero bespoke UI code, and it holds up.

## Security

SNMP varbinds are attacker-controlled - an `OctetString` can hold anything at
all, arbitrary binary included. All of it reaches a page only through the
generic view's `json-inspector`, which interpolates through `html/template`
like every other captured value in this codebase; `providers/trap`'s
`TestGenericViewRendersHostileOctetStringInert` sends a real trap carrying
`<script>`/`onerror=` text and asserts against the **parsed** rendered
fragment that no script element or event-handler attribute survives.

## Files

- `model.go` - `Trap`, `Varbind`, `V1Info`, `V2Info`, `Version`,
  `GenericTrapName`, and `Title()`/`Preview()` for the generic view's list and
  search index.
- `event.go` - `NewEvent`, `TrapOf`, `Traps`: what a provider uses to build the
  event, and what a read surface uses to get the model back out.
- `plugin.go` - the `plugin.Plugin` implementation. `RegisterAPI` and
  `RegisterUI` are both deliberately empty; see above.

The trap-receiving listener is `providers/trap`; see its own README for the
wire-format decisions (varbind type rendering, the inform reply) and how it is
tested.
