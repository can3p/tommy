# `mllp` hl7 provider

## What it is

A real MLLP server - the three-control-byte framing (`0x0B` … `0x1C 0x0D`)
almost every HL7 v2 integration engine speaks on the wire - on its own TCP
port. It parses every message with whatever separators it declared for
itself, captures it as an `hl7.Message`, and answers with a mechanical
`AA`/`AE`/`AR` acknowledgement built from the request.

Point an interface engine's outbound HL7 connection at `localhost:2575` and
everything it sends shows up in the **HL7** tab.

## What it's for

MLLP is a framed TCP protocol, not HTTP — there is no request/response
cycle an HTTP client can drive, so there is no way to point `curl` at it.
When your integration (or Mirth, Rhapsody, or any other engine) needs an
outbound MLLP target to test against, this is that target: it never refuses
a connection or a malformed frame, so you can see exactly what your engine
put on the wire — field by field — and check that your code handles an
`AE`/`AR` ack correctly, not just the happy-path `AA`.

## How to test it for real

Boot the plugin with just this provider (every hl7 provider is on by
default, and mllp is the only one today):

```bash
TOMMY_NO_UPDATE_CHECK=1 go run . hl7 --ui-port 18933 --in-port 18934 --mllp-port 12575
```

MLLP frames a message between three control bytes: a start block `0x0B`, the
message itself, then the trailer `0x1C 0x0D` (`<FS><CR>`). No HTTP tool speaks
this, so a plain `curl` will not work — the two options that do are a raw
socket client, or piping the framed bytes at a raw TCP tool.

With Python's stdlib (verified against the running server above; it prints
the real `AA` ack tommy sends back):

```bash
python3 -c '
import socket
MSH = "MSH|^~\\&|SendingApp|SendingFac|ReceivingApp|ReceivingFac|20240101120000||ADT^A01|MSG00001|P|2.5\r"
PID = "PID|1||123456^^^MRN||DOE^JOHN^A||19800101|M\r"
with socket.create_connection(("localhost", 12575)) as s:
    s.sendall(b"\x0b" + (MSH + PID).encode() + b"\x1c\r")
    print(s.recv(65536).decode(errors="replace"))
'
```

This printed, byte for byte:

```
MSH|^~\&|ReceivingApp|ReceivingFac|SendingApp|SendingFac|20260903182626||ACK^A01|01a06817d1a6000270a8dd4e|P|2.5
MSA|AA|MSG00001
```

(the ack's separators, sender/receiver swap and echoed control id are exactly
what "The acknowledgement" section below spells out).

With `nc` and `printf`, spelling out the framing bytes yourself — `\x0b`
before the message, `\x1c\r` after it — also works (macOS/BSD `nc`; on
`nc` builds without a hard timeout add `-w 1`):

```bash
printf '\x0bMSH|^~\\&|SendingApp|SendingFac|ReceivingApp|ReceivingFac|20240101120000||ADT^A01|MSG00002|P|2.5\rPID|1||123456^^^MRN||DOE^JOHN^A||19800101|M\r\x1c\r' \
  | nc -w 1 localhost 12575 | xxd
```

which returns the same ack, framed the same way, as raw bytes (hence `xxd`
rather than treating it as text).

Both are `Snippets()` entries rendered against the live `SnippetCtx`, so
`tommy providers hl7/mllp` prints them with the port this instance actually
bound - the ports above are pinned for this README; a real run would use
whatever it actually bound. The tests drive both shapes over a real socket.

See the capture over HTTP:

```bash
curl -s http://localhost:18933/api/v1/hl7/messages | jq '.[0].meta'
curl -s 'http://localhost:18933/api/v1/events?plugin=hl7' | jq '.[0].summary'
```

or open the tab at `http://localhost:18933/ui/hl7/`.

Both snippets above are `Snippets()` entries rendered against the live
`SnippetCtx`, so `tommy providers hl7/mllp` prints them with the port this
instance actually bound — `12575` above is only what this README's example
run happened to bind. The tests drive both shapes over a real socket.

## Configuration

```toml
[plugins.hl7.providers.mllp]
enabled = true
port    = 2575          # 0 binds an ephemeral port
bind    = "127.0.0.1"

# max_message_bytes = 10485760   # one frame's payload, unterminated or not
# read_timeout       = 60        # seconds, idle between (and mid-) frames
# write_timeout      = 10        # seconds, writing the ack back
```

An **absent** `port` means 2575 - IANA's registered port for the `hl7`
service (TCP *and* UDP; this provider only speaks TCP), and what every
integration engine defaults to. Unlike `ftp`'s 2121-for-21 or `tftp`'s
6969-for-69, this is not an unprivileged stand-in: 2575 is already
unprivileged, so there is nothing to substitute. `port = 0` binds an
ephemeral port, which is what the test harness uses.

MLLP has no login step at all, so there is nothing to accept or pin the way
`ftp`'s `USER`/`PASS` or `sftp`'s key/password auth do.

## The acknowledgement

Every captured frame gets a same-connection, same-transaction acknowledgement,
built from the request and nothing else:

- `MSH-3`/`MSH-4` (sending app/facility) and `MSH-5`/`MSH-6` (receiving
  app/facility) are the original message's, **swapped** - tommy was the
  receiver of the message and is the sender of the ack.
- `MSH-9` is `ACK`, or `ACK^<trigger event>` when the original declared one
  (`ADT^A01` gets `ACK^A01`).
- `MSH-10` is a fresh id; `MSH-11`/`MSH-12` (processing id / version) echo the
  original's.
- `MSA-1` is `AA`, `AE` or `AR` - see below. `MSA-2` echoes the original
  `MSH-10`.
- **Every separator is the message's own**, taken from `Message.Separators`,
  not the conventional `|^~\&` - using the wrong ones here would be the exact
  bug this plugin exists to expose. `TestListenerCustomSeparators` proves it
  with a message that uses none of the conventional characters.

`classify` in `ack.go` decides the code from `Message.HasHeader()` and
`Message.HasIssue(code)` - the accessors `hl7`'s core exposes for exactly
this - never from a decision about what the message means:

| | When | Separators used | `MSA-2` |
|---|---|---|---|
| **AA** | a usable `MSH` was found, with nothing wrong with the header itself | the message's own | the original control id |
| **AE** | `MSH` is present but its declared separators are unreliable (`no-encoding-characters`, `header-not-first`, `duplicate-separator`) | the message's own | the original control id |
| **AR** | no `MSH`, `FHS` or `BHS` segment anywhere in the frame | the conventional `\|^~\&` - there is nothing else to take them from | empty - there is no control id to echo, and a sender that could not say what it sent is best placed to notice `""` coming back |

An issue that does not compromise the header itself - an unrecognized segment
id, a segment with no fields - does not change the acknowledgement; it is
still visible on the captured event, which is where this project is allowed
to editorialize.

## Framing correctness

`framing.go`'s `frameReader` is the whole reason this provider is worth
writing carefully: it is built on `bufio.Reader` so that a message split
across several TCP packets and several messages pipelined back to back both
just work without any code of their own - `ReadByte` blocks for more data
mid-frame, and the buffered reader's own internal buffer holds whatever came
after one trailer for the next call. Everything the tests in
`listener_test.go` exercise over a real socket:

- a message split across several separate writes
  (`TestListenerMessageSplitAcrossPackets`)
- two messages pipelined in a single write
  (`TestListenerPipelinedMessages`)
- a frame that never sends a trailer, bounded by `max_message_bytes` and the
  connection closed (`TestListenerMissingTrailerBounded`)
- a connection that closes after a start byte but before a trailer
  (`TestListenerConnectionClosesMidFrame`)
- junk before the first start byte, and between one trailer and the next
  start byte, both discarded rather than corrupting either message
  (`TestListenerLeadingJunkBeforeStart`,
  `TestListenerJunkBetweenTrailerAndNextHeader`)
- an empty frame (`hl7.Parse`'s one real failure mode), dropped with no event
  and no stray ack ahead of the next real message
  (`TestListenerEmptyFrameProducesNoEventOrAck`)

## What lands where

`Event.Raw.Transport` is `"tcp"` (from `hl7.NewEvent`); `Event.Raw.Body` is
the frame's payload with the MLLP control bytes stripped, exactly as it
arrived. `Event.Meta` carries everything `Message.EventMeta()` derives from
the message itself, plus this provider's own transport details layered on
top rather than replacing them: `peer_addr`, `local_addr` and `framing`
(`"mllp"`).

## Files

- `provider.go` - `plugin.Provider` / `plugin.ListenerProvider` /
  `plugin.AddressableProvider`: configuration, descriptions, snippets, the
  listener lifecycle.
- `framing.go` - `frameReader`, the MLLP accumulate-and-find-the-trailer
  logic described above.
- `ack.go` - `classify` (AA/AE/AR) and `buildACK`, including
  `escapeValue`, which re-encodes an already-decoded value (an echoed
  control id, most plausibly) back into the ack's own wire form.
- `conn.go` - the per-connection loop: read a frame, parse, append the
  event, write the ack, repeat.
