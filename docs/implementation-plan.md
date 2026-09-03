# Tommy — Implementation Plan (forward-looking)

Waves 0–8 are built. This document plans what comes next.

- **What was built and why:** `docs/archive/history.md`
- **The interfaces as built (authoritative):** `docs/contracts.md`
- **What the work taught us:** `docs/lessons.md`
- **The original brief:** `docs/plan.md`
- **What each plugin and provider is for:** `docs/catalogue.md`

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
| `files` | ftp, sftp, tftp, nfs | done |
| `chat` | slack, msteams | done |
| `hl7` | mllp | done |
| `snmp` | trap | done |
| `push` | fcm, apns | done |
| `as2` | http | done |

Every plugin has a `tommy <plugin>` subcommand, and every provider option worth
setting has a flag. Every plugin and provider carries user-facing documentation
— what it is, what it is for, and commands that have been run — indexed in
`docs/catalogue.md` and required from here on by `CLAUDE.md` rule 12.

Waves 0–6·0 are merged to `main`. Waves 6a, 6b, 6c, 7 and 8 are on
`feat/hl7-and-tftp`, `feat/mllp-and-nfs`, `feat/snmp-traps`, `feat/push-plugin`
and `feat/as2-plugin`, each branched off the last and all awaiting review. That
stack is now five deep; it is worth merging before it grows again, since a new
wave branched off the tip inherits every unreviewed diff below it. **Start each
new wave on its own branch**, named for what it builds, so a wave stays a
reviewable unit.

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

The pattern that worked for waves 0–8, in short. `docs/lessons.md` has the
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

## Wave 8 — tier 2 protocols

Bigger, still worth doing, roughly in this order. Each is a self-contained agent
task once its plugin core exists; none block each other.

**AS2 is built** and is in `docs/archive/history.md`. What it proved for the rest
of this list: a plugin whose providers need a *generated credential* now has a
worked pattern — `Deps.ConfigDir`, an identity handed to providers through a
binder, and generation deferred to first use. Reuse it rather than reinventing
it, and read the laziness rule in `docs/contracts.md` before writing anything
that creates a file.

| Candidate | Shape | Notes |
|---|---|---|
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
| **TLS ingress** | `core/server/**`, config | `--tls` with a self-signed certificate generated on first run and written beside the config so it can be trusted once. Print the fingerprint. **Wave 8 already built the half you need**: `Deps.ConfigDir` is the directory of the config file (empty for a config built in memory), and `plugins/as2/identity.go` is a worked example of loading-or-generating a key pair with the paths configurable — which they must be, because tommy may run in a cluster that already has its own CA. Generate on **first use, not at startup**: doing it eagerly is what put a private key in the user's own config directory during `make check`. This is the documented route for non-Go SDKs that will not take a base URL (see `docs/clients.md`). **The seam already exists**: Wave 7 built `newHTTPServer` + `listenerOptions` in `core/server/httpserver.go`, and TLS is a field added there rather than a second construction path. Use `net/http`'s `Server.Protocols` for ALPN, not `golang.org/x/net/http2` — that module's `h2c` package is deprecated and would fail the staticcheck gate. |
| **Persistence** | `core/store/**`, `core/blob/**` | Opt-in `--persist <path>` snapshotting events and blobs. The `Store` and `BlobStore` interfaces were built for this; no plugin should need to change. Keep it dependency-free — files on disk, not SQLite — unless a real need appears. |
| **Search** | `core/server/api`, `core/server/ui` | Full-text across captured bodies. Currently `Query.Search` is a substring match; if that stops being enough, this is where it goes. |
| **Upstream: kleiner** | — | Fix `MaybeNotifyAboutNewVersion` in `can3p/kleiner`: it prints the error and falls through to dereference a nil version, panicking a released binary at startup when GitHub is unreachable. Second latent deref on the same path. Affects every project scaffolded from kleiner. |

## Backlog — small, unblocked, good first tasks

- **NFS event granularity.** One logical upload over NFS is a CREATE plus one
  `files.upload` per WRITE chunk, because NFS has no open/close on the wire and
  `COMMIT` never reaches the backend. Deliberate — a debounce would make reads
  see stale content — but if the files tab ever looks noisy, this is why.
- **NFS records the mounting connection's peer.** `go-billy`'s methods take
  neither a context nor a connection, so per-client identity can only come from
  `Handler.Mount`'s `net.Conn`. For a kernel client that is the same host on a
  possibly different source port. Fixing it needs a core change nobody else
  wants.
- **A varbind table for the `snmp` tab.** The plugin deliberately shipped with no
  UI of its own, and the generic event view carries it: the list line names the
  version, trap OID and varbind count, and the detail pane renders each varbind's
  OID, type and value. A table would read better if anyone spends real time in
  that tab; nothing is blocked on it.
- **The `push` tab has had no polish pass**, like `files` and `chat` before it.
  The lock-screen archetype works and distinguishes a silent push honestly, but
  it has not been looked at with fresh eyes.
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
