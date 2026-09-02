# Tommy — Implementation Plan (forward-looking)

Waves 0–6a are built. This document plans what comes next.

- **What was built and why:** `docs/archive/history.md`
- **The interfaces as built (authoritative):** `docs/contracts.md`
- **What the work taught us:** `docs/lessons.md`
- **The original brief:** `docs/plan.md`

## 0. Keeping this document current

**This plan is a working document, not a record.** It describes only what is
still to do.

- When a wave finishes, its section is **deleted from here** and appended to
  `docs/archive/history.md` in the past tense. The full ritual is in `CLAUDE.md`
  → *Finishing a wave*; it is required, not optional.
- When something learned mid-wave changes a later wave, **edit that wave now**,
  while the reason is still known. A dependency discovered, an ordering
  constraint, a task that turned out to be unnecessary — all of it belongs here
  rather than in someone's memory.
- The status table below is the single source of truth for what exists. Keep it
  accurate before anything else.

If you are reading a wave section, it has not been built. If you cannot find a
wave you expected, look in the history.

## 1. Where things stand

| Plugin | Providers | State |
|---|---|---|
| `mail` | mailjet, sendgrid, smtp | done |
| `sms` | twilio | done |
| `files` | ftp, sftp, tftp | done |
| `chat` | slack, msteams | done |
| `hl7` | — | **core done (Wave 6a); unwired until MLLP lands in 6b** |
| `push` | — | **not started; named in the original brief** |

Every *shipping* plugin has a `tommy <plugin>` subcommand, and every provider
option worth setting has a flag. `hl7` is the one exception, and deliberately so:
it has no provider yet, so there is nothing for a subcommand to run. Both land
together in 6b.

Waves 0–6·0 are merged to `main`; 6a is on `feat/hl7-and-tftp`. **Start each new
wave on its own branch**, named for what it builds, so a wave stays a reviewable
unit.

## 2. The scoping rule

It does most of the sorting, so it comes first.

Tommy captures and displays what an application *sent*, and answers with whatever
the protocol requires so the client proceeds. It does not simulate scenarios,
drive inbound traffic, or make policy decisions.

A protocol fits when its reply is **mechanical** — derivable from the request: an
HL7 `ACK`, an AS2 MDN receipt, Slack's `ok`, SMTP's `250`. It fits badly when the
reply encodes a **decision** somebody has to configure (approve or decline this
card payment; accept or reject this login), because that is scenario machinery by
another name.

Explicitly out of scope, and the plugin interface must not preclude them:
inbound traffic of any kind — webhooks and callbacks (Stripe events, Twilio
`StatusCallback`, SendGrid event webhooks, Slack interactivity and Events API,
async AS2 MDN) — which would need outbound HTTP and a scenario definition format.

## 3. How to run a wave

The pattern that worked for waves 0–6a, in short. `docs/lessons.md` has the
reasoning; `CLAUDE.md` has the rules.

1. **Branch first.** One branch per wave, named for what it builds
   (`feat/hl7-plugin`). A wave is a reviewable unit; two waves on one branch stop
   being one.
2. **Split by directory.** One agent per task, exclusive ownership, and name what
   it must not touch. Disjoint paths are what makes parallelism safe.
3. **A core task blocks its providers.** Plugin cores define the canonical model
   several providers code against, so they run alone and land first.
4. **Stagger anything that touches `go.mod`.** At most one dependency-adding agent
   at a time, and its dependencies land *with* the code that imports them.
5. **Subagents run no git commands.** The coordinator commits in coherent chunks.
6. **Point each agent at `docs/contracts.md` plus a worked example** — the closest
   existing provider of the same shape.
7. **Require live-documentation verification** for anything imitating a third-party
   API, and a **real client over a socket** for anything speaking a wire protocol.
8. **Verify reports independently**, re-run the gate, and clean up stray servers.
9. **Finish by updating the documents** — §0. The wave is not done until the plan,
   the history, the contracts and the lessons match the code.

Model guidance: contract-defining and subtle-parsing work to the stronger model;
well-specified translation against a fixed contract to the cheaper one.

---

## Wave 6 — tier 1 protocols

The CLI catch-up that opened this wave is done and is in the history. Keep it
closed: **each new provider carries its own CLI flags as part of its task**
(`CLAUDE.md` rule 10), so this never has to be repeated as a wave of its own.

Four additions were planned, chosen because each removes real pain and each has
a mechanical reply. **6a is done** — the HL7 plugin core and the TFTP provider
are in the history. What remains is 6b and 6c below.

Sequenced only by `go.mod` ownership — see rule 4 above.

### 6b · two agents in parallel

| Task | Owns | `go.mod` | Model | Depends on |
|---|---|---|---|---|
| **P-mllp** | `plugins/hl7/providers/mllp/**` | no | cheaper | HL1 |
| **P-nfs** | `plugins/files/providers/nfs/**` | **yes** — `willscott/go-nfs` | stronger | — |

**P-mllp inherits three obligations from HL1**, none of them optional:

1. **Wire `hl7` into `plugins/all/all.go`** — the core is deliberately unwired,
   because `plugintest` rejects a plugin with no providers and is right to: a
   plugin that can never receive anything is not shippable. It goes in with the
   provider.
2. **Add `cmd/hl7.go`** (`CLAUDE.md` rule 10), with `--mllp-port` as the option
   worth a flag, plus the `[plugins.hl7]` section in `tommy.toml` and the README
   rows. Follow `cmd/files.go`'s `providerOptionBuilder` pattern.
3. **Decide `AA`/`AE`/`AR` from the parsed message, not from an error.** `Parse`
   fails only on an empty message; everything else returns a message carrying
   coded `Issue`s. Use `HasHeader()` and `HasIssue(code)` — that API exists for
   this task.

**P-mllp.** MLLP framing is three control bytes — `0x0B` … `0x1C 0x0D` — around
each message on a TCP connection. The substance is correctness at the edges:
partial reads, several messages pipelined in one connection, a message split
across packets, and a missing trailer.

- Generate the **ACK** from the request: `MSH` echoed with sender and receiver
  swapped, `MSA|AA|<original control id>`. Support `AE`/`AR` for a message that
  fails to parse, since that is still a mechanical reply, not a policy decision.
- Bound message size and reject a frame that never terminates.
- Test by speaking MLLP over a socket, including the pipelining and split-packet
  cases, and assert the ACK a real integration engine would accept.

**P-nfs.** `willscott/go-nfs` is a genuine NFSv3 server library with a pluggable
filesystem backend, which is what makes this affordable; without it this would be
ONC RPC plumbing and would not be worth doing.

- Adapt the `files` VFS to the library's backend interface, the way the FTP
  provider adapts it to `afero.Fs`. Check whether the library wants a `billy`
  filesystem and write the adapter accordingly.
- NFS needs a portmapper/mount story — confirm what the library provides and what
  must be configured, and document the client mount command in the snippet, since
  it is the least obvious part for a user.
- Stronger model because the RPC/mount layer has more room for subtle error than
  the other three tasks here.

### 6c · one agent

| Task | Owns | `go.mod` | Model |
|---|---|---|---|
| **SNMP trap receiver** | `plugins/snmp/**` | **yes** — `gosnmp` | cheaper |

Pure capture, no state, no reply at all for v2c traps — the simplest fit in the
roadmap. `gosnmp` already decodes traps.

- Own plugin, UDP listener, default port 1162 (not the privileged 162).
- Support v1 traps, v2c traps and informs; an inform **does** require a response,
  which is mechanical.
- Canonical model: the varbind list with OIDs, types and values, plus the trap
  OID, uptime and community. Decode value types properly (integer, octet string,
  OID, counter, timeticks, IP address) rather than stringifying everything.
- UI: a varbind table per trap. The generic event view is an acceptable start —
  this plugin is a good test of whether a new plugin really is useful on day one
  with no UI code, as designed.

---

## Wave 7 — the `push` plugin

Named in the original brief and still unbuilt, which makes it the largest gap
against `docs/plan.md`.

**A hard constraint discovered up front: APNs requires HTTP/2.** Apple dropped the
binary protocol in 2021; the provider API is `POST /3/device/{deviceToken}` over
HTTP/2 with an ES256 JWT (or a client certificate). Go's `net/http` server speaks
HTTP/2 only over TLS (via ALPN) or through an explicit h2c wrapper. The shared
ingress is plain HTTP/1.1 today, so **APNs cannot mount on it as it stands.**

That gives Wave 7 a real internal ordering, and pulls a piece of Wave 9 forward.

### 7a · one agent

| Task | Owns | Model |
|---|---|---|
| **PU1 — push plugin core** | `plugins/push/**` (not `providers/`) | stronger |

- Canonical model covering both ecosystems without flattening them: device token
  or topic, title/body/subtitle, badge, sound, category/collapse key, priority,
  TTL, and a verbatim `json.RawMessage` for the platform-specific payload with a
  format discriminator — the same shape the `chat` plugin uses for cards, which
  worked well.
- Distinguish a **notification** (display) from a **data/silent** push, since the
  difference is most of what people debug.
- UI: a phone-notification archetype — the lock-screen shape, with the raw payload
  in an inspector. Fourth distinct view; lean on the component library.

### 7b · two agents in parallel

| Task | Owns | `go.mod` | Model |
|---|---|---|---|
| **P-fcm** | `plugins/push/providers/fcm/**` | probably not | cheaper |
| **H2C — HTTP/2 on the ingress** | `core/server/ingress/**` + config | **yes** — `x/net/http2` | stronger |

**P-fcm.** Firebase Cloud Messaging HTTP v1: `POST /v1/projects/{project}/messages:send`
with an OAuth2 bearer. Plain HTTP/1.1-compatible JSON, so it mounts on the ingress
today. Accept any bearer, record it. Handle the `message` envelope with its
`token`/`topic`/`condition` targeting and the `android`/`apns`/`webpush` override
blocks. Return FCM's real success (`{"name":"projects/.../messages/..."}`) and
error shapes. Verify against live documentation.

**H2C.** Enable HTTP/2 on the ingress listener via `golang.org/x/net/http2/h2c`,
so an HTTP/2 client can reach it without TLS, and make it configurable. This is
narrow, self-contained core work and does not touch any plugin. It also unblocks
the optional `--tls` mode in Wave 9, which should reuse the same server
construction.

### 7c · one agent, after 7b

| Task | Owns | Model |
|---|---|---|
| **P-apns** | `plugins/push/providers/apns/**` | stronger |

`POST /3/device/{deviceToken}` over HTTP/2, with the `apns-topic`,
`apns-push-type`, `apns-priority`, `apns-expiration`, `apns-collapse-id` and
`apns-id` headers — the headers carry most of the meaning here, so record them
all. Parse but never verify the ES256 JWT in `authorization`; record its claims
(`iss`, `iid`, `kid`) as metadata, since a wrong key id is a common real-world
mistake and seeing it is the point. Success is `200` with an `apns-id` header and
an empty body; errors are `{"reason":"BadDeviceToken"}` with the right status.
Test with `sideshow/apns2` if it can be pointed at a custom host, otherwise a
hand-driven HTTP/2 client.

---

## Wave 8 — tier 2 protocols

Bigger, still worth doing, roughly in this order. Each is a self-contained agent
task once its plugin core exists; none block each other.

| Candidate | Shape | Notes |
|---|---|---|
| **AS2** | own plugin, HTTP | Underrated fit: HTTP POST with S/MIME plus a **synchronous MDN receipt** — decrypt, verify, store the EDI document, sign a receipt back. Mechanical reply, high inspection value, famously miserable to set up for real. Needs certificate handling (self-generate on first run). **Sync MDN only**; async is an outbound callback and out of scope. |
| **Modbus TCP** | own plugin, TCP | Small protocol, good Go libraries, and it lands on the **state-plus-event pattern** the `files` VFS proved: the register bank is state, writes are events, reads are polls. View is an editable register grid. Note it inverts the usual value — clients mostly *read*, so tommy supplies data rather than capturing it. |
| **SNMP agent** | extends `plugins/snmp` | Same state-plus-event pattern with an OID tree; pairs with the Wave 6c trap receiver but is a bigger lift. |
| **ISO 8583** | own plugin, TCP | `moov-io/iso8583` is excellent and bitmap decoding is painful enough that a decoded-field view has real value. Reply with a fixed approval and stop there, or it becomes scenario mocking. |
| **SMB2** | `files` provider | Perfect conceptual fit — another provider over the same VFS — and the highest cost here. Server-side SMB2 in Go is thin (negotiate, session setup with NTLMSSP, tree connect, create/read/write/close, find) and it drags NTLM along. Worth it eventually because the Docker-Samba pain is real. |

**Not planned** (and why): Kerberos KDC is the wrong *shape* — there is nothing to
inspect, the value is "authentication succeeded", which is a stub not a catcher.
RADIUS/TACACS+ are held back by accept-versus-reject being policy, though RADIUS
is the most likely to graduate. NTLM is not a plugin at all — it is a mechanism
inside SMB, implemented if and when SMB2 is. IBM MQ is a proprietary binary
protocol without a usable public specification. BACnet is large with thin Go
support; strictly after Modbus.

---

## Wave 9 — cross-cutting

Independent of each other and of the protocol work; each is one agent.

| Task | Owns | Notes |
|---|---|---|
| **TLS ingress** | `core/server/**`, config | `--tls` with a self-signed certificate generated on first run and written beside the config so it can be trusted once. Print the fingerprint. This is the documented route for non-Go SDKs that will not take a base URL (see `docs/clients.md`); it reuses the Wave 7b server construction. |
| **Persistence** | `core/store/**`, `core/blob/**` | Opt-in `--persist <path>` snapshotting events and blobs. The `Store` and `BlobStore` interfaces were built for this; no plugin should need to change. Keep it dependency-free — files on disk, not SQLite — unless a real need appears. |
| **Search** | `core/server/api`, `core/server/ui` | Full-text across captured bodies. Currently `Query.Search` is a substring match; if that stops being enough, this is where it goes. |
| **Upstream: kleiner** | — | Fix `MaybeNotifyAboutNewVersion` in `can3p/kleiner`: it prints the error and falls through to dereference a nil version, panicking a released binary at startup when GitHub is unreachable. Second latent deref on the same path. Affects every project scaffolded from kleiner. |

## Backlog — small, unblocked, good first tasks

- `event.DecodePayload[T]` in core. `sms`, `chat` and `hl7` each carry a
  near-identical pointer/value/JSON-round-trip payload decoder, ~25 lines apiece.
  A convenience rather than a gap, reported by the third plugin to write one.

- `sms.New(sms.WithProviders(...))` uses an options pattern while `mail.New`,
  `files.New` and `chat.New` are variadic. Unify.
- Two shapes in the Mailjet fake have no primary source and are marked as such in
  the code: the 401 error code (`mj-0015`), and the response to exhausted blob
  capacity, which is not a Mailjet concept at all.
- SFTP defaults to `NoClientAuth`, so credentials are only presented — and
  therefore only recorded — when something is pinned. Deliberate, since it makes
  the cold-start snippet work with real OpenSSH, but revisit if capture matters
  more than convenience.
- The `files` and `chat` tabs have not had the polish pass mail and sms got.
- **`ProviderConfig.Port` is inert for HTTP providers — implement it or delete
  it.** `tommy.toml` advertised a dedicated listener per HTTP provider from the
  moment the config landed, but nothing ever built one: the ingress path-routes
  every HTTP provider onto one listener, and the field's only reader reports
  where a *listener* provider bound. `Validate` range- and collision-checks the
  value, which is what kept the gap hidden. Wave 6·0 removed the claim rather
  than implementing it. Either give a provider a listener of its own (worth
  having when a client cannot be pointed at a path, and it shares shape with the
  Wave 9 TLS work) or drop the field and its validation.
