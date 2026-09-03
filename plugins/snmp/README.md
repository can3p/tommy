# snmp

## What it is

Stands in for a network management station's trap receiver — the thing an
SNMP agent normally fires alerts at. It is a **trap receiver, not a trap
sender**: it accepts v1 traps, v2c traps and v2c informs on its own UDP port,
decodes every varbind by its actual wire type — integers, object identifiers,
counters, gauges, timeticks, IP addresses and octet strings — rather than
flattening everything to one string, and captures it. An octet string that
is not printable text is hex-dumped rather than mangled.

There is no listener built into the core: the trap provider (`providers/trap`)
does that. Shortcut: `tommy snmp`.

## What it's for

The thing under test here is your own application's or device's *outbound*
alerting, not tommy. The concrete question this answers: "my monitoring agent
claims it sends a trap when the disk fills up — does it actually fire, and
what varbinds does it put in it?" Point the agent's trap destination at tommy
instead of a real NMS, trigger the condition, and read back exactly what went
out on the wire — including the cases that are easy to get wrong by hand,
like whether a v1 trap's generic/specific trap-type pair says what you think
it says, or whether an OID that should carry a printable hostname arrived as
binary garbage instead.

Note honestly: this plugin ships with **no bespoke UI tab**. It deliberately
rides the generic cross-plugin event view — see below — which was itself part
of what this plugin exists to test.

## How to test it for real

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . snmp --ui-port 18913 --in-port 18914 --trap-port 11162
```

```
tommy is running (snmp only)
  ui       http://127.0.0.1:18913/ui/
  api      http://127.0.0.1:18913/api/v1
  ingress  http://127.0.0.1:18914
  plugin   snmp ([trap])
```

If net-snmp is installed (`which snmptrap`; `brew install net-snmp` /
`apt-get install snmp` otherwise), drive it with the real tool:

```bash
snmptrap -v 2c -c public localhost:11162 '' 1.3.6.1.6.3.1.1.5.3 \
  1.3.6.1.2.1.1.5.0 s "host01"
```

An inform, which gets a reply, works the same way with `snmpinform`:

```bash
snmpinform -v 2c -c public localhost:11162 '' 1.3.6.1.6.3.1.1.5.3 \
  1.3.6.1.2.1.1.5.0 s "host01"
```

and a v1 trap, which carries the enterprise OID / agent address / generic-
specific trap-type pair as header fields rather than varbinds:

```bash
snmptrap -v 1 -c public localhost:11162 1.3.6.1.4.1.8072.9999.9999 localhost 6 17 '55' \
  1.3.6.1.2.1.1.5.0 s "host01"
```

Each of the three shows up distinctly in the capture — the v1 trap keeps its
header fields under `v1`, the v2c trap and inform lead their varbind list with
`sysUpTime.0`/`snmpTrapOID.0` and keep those under `v2`:

```bash
curl -s "http://127.0.0.1:18913/api/v1/events?plugin=snmp"
```

or open `http://127.0.0.1:18913/ui/snmp/` — the generic event view, filterable
by plugin, with a collapsible JSON payload inspector that already shows every
varbind's OID/type/value without any bespoke rendering code.

`tommy providers snmp` also prints a Go snippet using `gosnmp` directly if
net-snmp is not available.

Kill the server when done (`go run` forks a child `tommy` binary — kill that
child, not just the shell job, or the ports stay held).

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
