# `trap` snmp provider

A real SNMP trap receiver on its own UDP port. It accepts v1 traps, v2c traps
and v2c informs, decodes every varbind by its actual wire type, and records it
as an `snmp.Trap`. An inform gets a `GetResponse` back, echoing its request id
and varbinds - the one reply this protocol actually requires. A trap, v1 or
v2c, gets none: RFC 3416 §4.2.6 defines it as unconfirmed.

Point a trap sender at `localhost:1162` and everything it sends shows up in
`GET /api/v1/events?plugin=snmp` and the generic **SNMP** tab.

## Why gosnmp's own `TrapListener` is not used

`github.com/gosnmp/gosnmp` (v1.44.0, verified against its own `trap.go` and
`marshal.go` source rather than from memory) ships a ready-made
`TrapListener.Listen(addr string) error`, but it owns its UDP socket
internally and never exposes the bound `net.Addr` - there is no way to learn
what port it actually bound. That makes `plugin.AddressableProvider`
impossible to implement against it, which every listener provider in this
codebase is required to (`port = 0` has to be discoverable, or a test can only
ever bind a well-known port - see `docs/lessons.md`).

So this provider opens its own `net.PacketConn` with `net.ListenPacket`,
exactly the way `plugins/files/providers/tftp` does, and calls gosnmp's
**exported** wire-format functions directly:

- `(*GoSNMP).UnmarshalTrap(data []byte, false)` to decode an incoming
  datagram into a `*gosnmp.SnmpPacket`.
- `(*SnmpPacket).MarshalMsg()` to encode the `GetResponse` sent back for an
  inform.

Both of gosnmp's own decode/marshal implementations round-trip cleanly for
every varbind type this provider claims to support - proven directly by
`TestReceiveInformGetsResponse`, which sends an inform and gets back the exact
varbinds it sent, decoded and re-encoded by tommy in between.

## Varbind decoding

`decode.go`'s `varbindFrom` renders each of the Go types gosnmp's own decoder
produces (see `decodeValue` in its `helper.go`):

| Wire type | gosnmp's decoded Go type | Rendered as |
|---|---|---|
| `Integer` | `int` | decimal |
| `Counter32`, `Gauge32` | `uint` | decimal |
| `TimeTicks` | `uint32` | decimal (hundredths of a second) |
| `Counter64` | `uint64` | decimal |
| `ObjectIdentifier`, `IPAddress` | `string` | as-is (gosnmp already formats these) |
| `OctetString`, `Opaque` | `[]byte` | **text**, if printable - **lowercase hex**, with `Binary: true`, if not |
| `Null`, `NoSuchObject`, `NoSuchInstance`, `EndOfMibView` | `nil` | empty; `Type` alone says which |

An `OctetString` is exactly where SNMP puts raw hardware addresses, packed
counters and worse, so it is never assumed to be text. `isPrintableText`
requires valid UTF-8 with no control bytes other than tab/newline/CR and no
DEL; `TestVarbindFromMACAddress` and `TestVarbindFromBinaryOctetString` pin
down a 6-byte hardware address and arbitrary binary both landing as hex. The
exact bytes are never lost either way: `Event.Raw.Body` is always the
untouched UDP datagram, decode failures included.

## v1 vs v2c, not flattened

A v1 `Trap-PDU`'s enterprise OID, agent address, generic/specific trap-type
pair and timestamp are header fields with no varbind equivalent; a v2c
`SNMPv2-Trap-PDU`/`InformRequest-PDU` carries none of them and instead leads
its varbind list with `sysUpTime.0` and `snmpTrapOID.0` (RFC 3416
§4.2.6-4.2.7, confirmed against the live RFC text). `fromPacket` in
`decode.go` builds `snmp.V1Info` only for a `Trap` PDU and `snmp.V2Info` only
for `SNMPv2Trap`/`InformRequest`, leaving the other nil rather than
synthesizing zero values - `TestFromPacketV1Trap` and `TestFromPacketV2cTrap`
both assert the other version's field is nil.

`v2InfoFrom` does not assume `sysUpTime.0`/`snmpTrapOID.0` are varbinds 0 and
1 - it scans for them by OID - and leaves `V2` nil rather than rejecting the
trap when a hand-built or malformed sender omits them (CLAUDE.md rule 1's
"accept any credentials by default" applied to malformed input generally, not
just to the community string).

## Testing

Every wire-level test drives a real UDP socket:

- `TestReceiveV1Trap`, `TestReceiveV2cTrap`, `TestReceiveInformGetsResponse` -
  gosnmp's **own client** (`(*GoSNMP).SendTrap`), not a hand-built driver, per
  `docs/lessons.md` ("drive the official SDK, not just the wire format").
  `SendTrap` blocks and returns the response for an inform, so a non-error
  return with the right `PDUType`/`Error`/echoed varbinds is direct proof the
  reply round-tripped, request-id correlation included.
- `TestNetSNMPSnmptrapBinary` drives net-snmp's own `snmptrap` command-line
  tool - a second, independent implementation - when it is on `PATH`
  (`t.Skip` otherwise, since it is not installed everywhere CI runs).
- `TestGarbageDatagramStillRecorded` writes bytes that are not SNMP at all
  over a plain `net.Dial("udp", ...)` socket - deliberately below gosnmp's
  client, which would only ever construct well-formed packets - and proves
  the datagram is still captured with `DecodeError` set rather than dropped.
- `TestGenericViewRendersHostileOctetStringInert` sends a trap whose
  `OctetString` varbind is `<script>...</script><img ... onerror=...>` and
  asserts, against **parsed** markup (`goquery`), that the generic event
  view's rendered fragment carries neither a `<script>` element nor an
  `onerror` attribute - the security invariant holds even with no bespoke UI
  code of this provider's own.

## Configuration

```toml
[plugins.snmp.providers.trap]
enabled = true
port    = 1162          # 0 binds an ephemeral port
bind    = "127.0.0.1"
```

An absent `port` means 1162. SNMP's real trap-receiving port is 162/udp -
registered with IANA as `SNMPTRAP` (`service-names-port-numbers` registry,
checked live against iana.org while writing this) - which is privileged on
every OS tommy runs on; 1162 is the unprivileged stand-in, the same pattern
`ftp` uses with 2121 instead of 21 and `tftp` with 6969 instead of 69.

Any community string is accepted and recorded, never checked - there is
nothing to pin the way `smtp`'s or `ftp`'s username/password are, since a
community string is not meaningfully "wrong" in a fake receiver the way a
mismatched password is.

## Files

- `provider.go` - `plugin.Provider` / `plugin.ListenerProvider` /
  `plugin.AddressableProvider`: configuration, descriptions, snippets, the
  listener lifecycle (see the package doc for why it does not use gosnmp's
  `TrapListener`).
- `decode.go` - `fromPacket`, `varbindFrom`, `v2InfoFrom`: gosnmp's
  `*SnmpPacket` converted into the plugin's gosnmp-free canonical model.
- `handlers.go` - one datagram in, one event appended, and (for an inform
  only) one reply out.
- `reply.go` - the `GetResponse` an inform gets back.
