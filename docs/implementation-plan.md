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
| `mail` | mailjet, sendgrid, resend, smtp | done |
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

Everything through wave 8·1 is merged to `main`; waves 9–13 are built, with
wave 13 on `feat/distribution` (stacked on `docs/distribution-plan`) awaiting
review.
**Start each wave on its own branch**, named for what it builds, so a wave stays
a reviewable unit — and merge it before starting the next, because a wave
branched off an unreviewed tip inherits every diff below it.

What comes next is a change of emphasis. Waves 0–8 built breadth: eight plugins
and sixteen providers, each capturing one more thing. Waves 9–12 build the
*surface* instead — how a person and a program reach what tommy captured — and
only wave 11 adds a provider. The protocol backlog is still there, renumbered,
behind them.

**Waves 9 through 13 are built**, on `feat/event-page`, `feat/openapi-spec`,
`feat/plugin-openapi`, `feat/resend-provider`, `feat/website` and
`feat/distribution`:
every event has a page at `/ui/events/{id}`, every API representation of an
event carries its `url`, an ingress response names what it captured in
`X-Tommy-Event-URL`, and the events API and every plugin API have generated
OpenAPI 3.1 descriptions that CI holds to the code, `mail` has a fourth
provider, the documentation is published as a site generated from the
repository itself, and tommy is MIT-licensed and ships as a container image on
Docker Hub and GHCR.

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
10. **Push the branch and open the pull request**, based on whatever the wave was
    branched from: `main` for a wave off trunk, the predecessor branch for a wave
    stacked on an unmerged one. `CLAUDE.md` → *Finishing a wave* step 9 has the
    mechanics, including rebasing the rest of the stack when a base moves.

Model guidance: contract-defining and subtle-parsing work to the stronger model;
well-specified translation against a fixed contract to the cheaper one.

## 4. The surface and the distribution work are finished

Waves 9 through 13 are built and are in `docs/archive/history.md`. What is left
is the protocol backlog below. Nothing is blocked on the repository owner:
GitHub Pages is enabled, the Docker Hub credentials are configured, and the
release workflow publishes the image and its Docker Hub page on every tag.

**The remaining first-release step is a tag.** Cut `v0.1.0-rc1` first, as a
deliberate prerelease: the push path only runs on a real tag and is untestable
before then. Confirm the manifest lists both architectures
(`docker buildx imagetools inspect can3p/tommy:0.1.0-rc1`) and that `latest` did
*not* move, and only then tag `v0.1.0`. Confirm the `can3p/tommy` repository
exists on Docker Hub and is public before either — the description job needs it
to exist, and push-to-create leaves its visibility at the account default.

- Each of those waves left a *keep it true* half — a generated artifact plus a
  test that fails when it stops matching the code. There are now four families:
  the OpenAPI descriptions, the site's coverage test, rule 12's per-component
  READMEs, and the Docker surface — the image's `EXPOSE` set and the compose
  file against the provider listing, the shipped config against the checked-in
  one, the Docker Hub page against its byte caps, and the commands in
  `docs/docker.md` executed by CI rather than mirrored in it. A wave that adds a
  plugin or provider fails the build if it ships without a README, and one that
  moves a listener port fails until the image follows. Treat the four as one
  family, and when a new surface is added, add its check in the same wave.

---

## Wave 13 — tier 2 protocols

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

## Wave 14 — cross-cutting

Independent of each other and of the protocol work; each is one agent.

| Task | Owns | Notes |
|---|---|---|
| **TLS ingress** | `core/server/**`, config | `--tls` with a self-signed certificate generated on first run and written beside the config so it can be trusted once. Print the fingerprint. **Wave 8 already built the half you need**: `Deps.ConfigDir` is the directory of the config file (empty for a config built in memory), and `plugins/as2/identity.go` is a worked example of loading-or-generating a key pair with the paths configurable — which they must be, because tommy may run in a cluster that already has its own CA. Generate on **first use, not at startup**: doing it eagerly is what put a private key in the user's own config directory during `make check`. This is the documented route for non-Go SDKs that will not take a base URL (see `docs/clients.md`). **The seam already exists**: Wave 7 built `newHTTPServer` + `listenerOptions` in `core/server/httpserver.go`, and TLS is a field added there rather than a second construction path. Use `net/http`'s `Server.Protocols` for ALPN, not `golang.org/x/net/http2` — that module's `h2c` package is deprecated and would fail the staticcheck gate. |
| **Persistence** | `core/store/**`, `core/blob/**` | Opt-in `--persist <path>` snapshotting events and blobs. The `Store` and `BlobStore` interfaces were built for this; no plugin should need to change. Keep it dependency-free — files on disk, not SQLite — unless a real need appears. |
| **Search** | `core/server/api`, `core/server/ui` | Full-text across captured bodies. Currently `Query.Search` is a substring match; if that stops being enough, this is where it goes. |
| **Upstream: kleiner** | — | Fix `MaybeNotifyAboutNewVersion` in `can3p/kleiner`: it prints the error and falls through to dereference a nil version, panicking a released binary at startup when GitHub is unreachable. Second latent deref on the same path. Affects every project scaffolded from kleiner. The container image sets `TOMMY_NO_UPDATE_CHECK=1` so it cannot hit this, which removes the urgency but not the bug. |

## Backlog — small, unblocked, good first tasks

- **Link each plugin's OpenAPI description from its tab.** The shell links the
  events document from the status bar; a per-plugin link needs either the
  how-to-test panel to learn the API base or the shell to learn which plugins
  have a document (`snmp` has none). Left out of wave 10·1 rather than
  half-wired.

- **Listener providers hand out no event link.** Wave 9 answers every *ingress*
  response with `X-Tommy-Event-URL`, but SMTP, FTP, SFTP, TFTP, NFS, MLLP and
  the trap receiver have no response that can carry a header. The equivalent
  would be a log line naming the URL of each captured event — genuinely useful
  in local development, noise in CI, and it needs the UI origin plumbed into
  `Deps`, which nothing else wants. Deliberately not built rather than guessed
  at; if it is wanted, make it a flag rather than a default.
- **The event page renders the plugin fragment on every request.** It dispatches
  an in-process sub-request to `/ui/<plugin>/events/{id}`. That is cheap today
  because every such route reads one event from the store, and it is a
  requirement on plugin authors now written into `docs/contracts.md`: keep the
  fragment route side-effect free. If a plugin ever needs something expensive
  there, the page is where it will show.

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
  TLS ingress task in wave 14) or drop the field and its validation.
