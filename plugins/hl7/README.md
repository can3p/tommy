# hl7

## What it is

Tommy's HL7 v2 content type. It stands in for a hospital interface engine: the
system on the other end of an ADT, ORU, ORM or any other HL7 v2 feed. It
captures every message exactly as sent and shows it as a **segment tree** —
every field at its own position, every repetition kept apart — next to the
bytes exactly as they arrived, and hands back the acknowledgement the sender
is waiting on. A pipe salad in a log line tells you nothing about whether the
MRN landed in `PID-3.1` or `PID-3.4`, and that is always the question this tab
exists to answer.

## What it's for

Your system emits an ADT (admit/discharge/transfer) or ORU (lab result)
message over MLLP to a hospital interface engine, and standing up a real
Cerner, Epic or Mirth instance to test against is not an option — those
systems aren't yours to run, and the interface engineer who owns the real one
is not going to hand you a sandbox for CI. Point your outbound HL7 connection
at tommy instead: it shows you the segments and fields your code actually
produced, not what you assume it produced, and it answers with a real
`AA`/`AE`/`AR` acknowledgement so you can test how your code reacts to each.

## How to test it for real

There is one provider today, MLLP — see
`plugins/hl7/providers/mllp/README.md` for a listener you can point a real
socket at and read the ack back from. This package alone (model, API, tab)
has no listener of its own; messages reach it only through a provider.

## The separators come from the message

`MSH-1` is the field separator — the character immediately after `MSH` — and
`MSH-2` carries the component, repetition, escape and subcomponent separators in
that order. They are read from every message. Nothing here assumes `|^~\&`,
because plenty of senders do not use it, and assuming it is the single most
common HL7 parsing bug.

A separator that a message did not declare, or that duplicates one it already
claimed, is left empty and **nothing is split on it**. One character cannot mean
two things, and splitting anyway would shred every value in the message rather
than just the header.

## The canonical model

Every provider converts its wire format into `hl7.Message` and puts it in
`Event.Payload`. Transport metadata — the peer address, the framing, the
connection — goes in `Event.Meta`, never in the message.

```go
type Message struct {
    Separators Separators // what MSH-1 and MSH-2 declared
    Header     Header     // MSH, lifted out for the surfaces that name a message
    Segments   []Segment
    Issues     []Issue    // what the parser recovered from
}

type Segment struct {
    ID         string  // "PID"
    Index      int     // 1-based position in the message
    Occurrence int     // 1-based position among segments sharing this id
    Fields     []Field
}

type Field struct {
    Position    int          // PID-5 is Position 5
    Value       string       // decoded, repetitions joined back together
    Repetitions []Repetition // empty for an empty field
}

type Repetition struct {
    Value      string
    Components []Component
}

type Component struct {
    Value         string
    Subcomponents []string
}
```

The hierarchy is kept whole on purpose. A `PID-3` that carries an MRN *and* an
account number arrives as two repetitions, never as one tilde-joined string:
being able to see that it repeats is most of the reason to look at a captured
message at all.

Reach into it with `Value`, which speaks the notation people already write in
interface specifications:

```go
m.Value("MSH-9.1")    // "ADT"
m.Value("PID-5.1")    // "DOE"
m.Value("PID-3[2].1") // the second identifier
m.Value("PV1-3.4.2")  // a subcomponent
m.Value("OBX(3)-5")   // the third OBX
```

`m.EventSummary()` and `m.EventMeta()` build what every read surface lists, and
`hl7.NewEvent(provider, m, raw)` assembles the whole event — a provider should
use all three rather than rolling its own, so a message is named the same way
wherever it appears.

## Escape sequences

`\F\ \S\ \T\ \R\ \E\` expand to the separators **this** message declared, not to
the conventional ones. `\X..\` hex escapes are decoded, and `\.br\` becomes a
newline because free-text `NTE` and `OBX` values lean on it.

Everything else — the rest of the formatting commands, locally defined `\Z..\`
escapes — is left exactly as it arrived. Guessing at a sender's private escape
would be inventing content: an escape nobody decoded is still readable, one
decoded wrongly is not. An unterminated escape is literal text, so a lone
backslash never swallows the rest of a value.

`Raw.Body` is the untouched message. Everything else is derived from it and can
be re-derived; the bytes cannot.

## Parsing never really fails

`Parse` returns an error in exactly one case — `ErrEmpty`, when there was nothing
that could be a segment. Everything else parses to whatever can be recovered,
with an `Issue` recorded against it:

| Code | What happened |
|---|---|
| `no-header` | no `MSH`/`FHS`/`BHS`, so the conventional separators were assumed |
| `header-not-first` | an `MSH` exists but not at the top; it is used anyway |
| `no-encoding-characters` | `MSH-2` was missing or short; the rest were defaulted |
| `duplicate-separator` | a character was claimed twice, so the second claim is dropped |
| `segment-id` | a segment id is not the three characters HL7 requires |
| `no-fields` | a segment carried an id and nothing else |

Refusing to show a malformed message would defeat the point of capturing it: the
reason to look is usually that something about it is wrong. A provider deciding
between an `AA` and an `AE` acknowledgement uses `HasHeader()` and `HasIssue()`
rather than an error.

Segments are split on `\r`, `\n` or `\r\n`, and stray MLLP framing bytes are
tolerated. The standard says `\r`, but anything that has been through a text
editor, a heredoc or an HTTP client will not have one.

## API

Mounted at `/api/v1/hl7/`.

| Route | Notes |
|---|---|
| `GET /messages` | `?message_type=&control_id=&sending_application=&receiving_application=&segment=` plus the core's `search`, `since`, `limit`, `offset` |
| `GET /messages/{id}` | the parsed message with its header and tree |
| `GET /messages/{id}/raw` | the bytes exactly as they arrived, as inert `text/plain` |
| `DELETE /messages` | clear every captured message |
| `DELETE /messages/{id}` | delete one |

Every message carries a `url`: the link to that event's own page in the UI, so
a client that just posted something can open what it sent.

`message_type` accepts the whole thing (`ADT^A01`), just the code (`ADT`) or just
the trigger event (`A01`), because all three are how people describe the message
they are hunting for.

## Security

HL7 carries patient names and free-text notes written by whatever system is under
test. All of it is untrusted:

- Every captured value is interpolated as a plain string through `html/template`.
  Nothing in this plugin uses `template.HTML` for captured content, and the
  hostile-input test asserts against the **parsed** document — no script element,
  no event-handler attribute — rather than grepping the HTML.
- `GET /messages/{id}/raw` is served as `text/plain` with `X-Content-Type-Options:
  nosniff`, so a browser cannot sniff a captured message into HTML.

## What is deliberately not here

- **No message-structure validation.** Tommy shows what was sent; deciding that a
  `ADT^A01` is missing a required `PV1` is a policy decision and belongs to a
  validator, not to a capture tool.
- **No `MSH-7` parsed into a `time.Time`.** HL7 timestamps carry a precision and
  an offset that a Go time would normalise away, and what was sent is what this
  shows.
- **A small dictionary, not a complete one.** `MSH`, `MSA`, `EVN`, `PID`, `PV1`,
  `OBR`, `OBX`, `NTE`, `ORC` and `ERR` have field names; other segments have a
  name only. Field *positions* are what make a message readable, so names are a
  convenience and no view may require one — Z-segments are unknowable by
  definition.

## Testing it

`plugins/hl7`'s own tests drive the plugin through a fake provider
(`fake_test.go`), which is the shortest worked example of what a real
provider has to do — to send a real message over the wire, see
`plugins/hl7/providers/mllp/README.md`:

```bash
go test ./plugins/hl7/...
```

To exercise the model and API directly, without a real MLLP socket:

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . hl7 --ui-port 18933 --in-port 18934 --mllp-port 12575
```

then, in another shell, send a message over MLLP (the framing bytes are
explained in the mllp provider's README) and read it back:

```bash
python3 -c '
import socket
MSH = "MSH|^~\\&|SendingApp|SendingFac|ReceivingApp|ReceivingFac|20240101120000||ADT^A01|MSG00001|P|2.5\r"
PID = "PID|1||123456^^^MRN||DOE^JOHN^A||19800101|M\r"
with socket.create_connection(("localhost", 12575)) as s:
    s.sendall(b"\x0b" + (MSH + PID).encode() + b"\x1c\r")
    print(s.recv(65536).decode(errors="replace"))
'

curl -s http://localhost:18933/api/v1/hl7/messages | jq '.[0].meta'
curl -s 'http://localhost:18933/api/v1/events?plugin=hl7' | jq '.[0].summary'
```

The UI tab is at `http://localhost:18933/ui/hl7/`.
