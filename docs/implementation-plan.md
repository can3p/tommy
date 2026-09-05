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

Everything through wave 8·1 is merged to `main`; the six-deep review stack that
carried waves 6a–8·1 is gone, and a new wave now branches off a clean trunk.
**Start each wave on its own branch**, named for what it builds, so a wave stays
a reviewable unit — and merge it before starting the next, because a wave
branched off an unreviewed tip inherits every diff below it.

What comes next is a change of emphasis. Waves 0–8 built breadth: eight plugins
and fifteen providers, each capturing one more thing. Waves 9–12 build the
*surface* instead — how a person and a program reach what tommy captured — and
only wave 11 adds a provider. The protocol backlog is still there, renumbered,
behind them.

**Waves 9 and 10 are built**, on `feat/event-page` and `feat/openapi-spec`:
every event has a page at `/ui/events/{id}`, every API representation of an
event carries its `url`, an ingress response names what it captured in
`X-Tommy-Event-URL`, and `/api/v1` has a generated OpenAPI 3.1 description that
CI holds to the code.

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

## 4. What is left of the surface work

Waves 11 and 12 are what is left of the surface work; **waves 9 and 10 are
built** and are in `docs/archive/history.md`. What they leave behind:

- **Wave 12 has its API reference already.** `docs/openapi.json` is generated
  and CI-checked, so the website renders it rather than hand-writing anything.
- **Wave 11 is unaffected by wave 10's contract change.** `Plugin` gained
  `APIEndpoints()`, and a *provider* does not implement `Plugin` — so a new
  mail provider needs no declaration of its own, and the mail plugin's existing
  one already covers the routes it will be read back through. A new *plugin*,
  on the other hand, now has one more method to implement, and
  `plugintest.Conformance` will say so.
- **Nothing here blocks waves 13 and 14**, and nothing there blocks these. If a
  protocol is wanted sooner than the surface work, take it.

Each of these waves also has a *keep it true* half — a generated artifact plus a
test that fails when it stops matching the code. That half is the deliverable,
not the polish: a spec or a website that is updated by remembering to update it
is one that is wrong within two waves. Where a wave adds such a gate, it also
adds the `CLAUDE.md` rule that names it, because the rule is what survives into
the next session.

---

## Wave 11 — the Resend provider

**Goal.** `plugins/mail/providers/resend`: a fourth mail provider, the same
shape as `sendgrid`, standing in for `api.resend.com`.

This wave is independent of waves 9, 10 and 12 and can run on its own branch at
any time. It is the cheapest of the four, and it is a good one to hand to a
single agent with `plugins/mail/providers/sendgrid` as the worked example.

### What to build

Verify every one of these against **live Resend documentation** before writing
the response — the shapes below were read once, while planning, and are a
starting point, not a source:

| Route | Notes |
|---|---|
| `POST /emails` | The main one. `200` with `{"id": "<uuid>"}`. |
| `POST /emails/batch` | Up to 100 messages; `{"data":[{"id":…},…]}`, index-aligned with the request. **One event per message** — rule 3. |
| `GET /emails/{id}` | Read-back, served **from the store** (rule 5), returning `object`, `id`, `from`, `to`, `cc`, `bcc`, `reply_to`, `subject`, `html`, `text`, `created_at`, `last_event`, `scheduled_at`, `tags`. |

Wire details that need care:

- **Auth is `Authorization: Bearer re_…`.** Record it in `Event.Meta`; accept
  anything (rule 1).
- **`to`, `cc`, `bcc` and `reply_to` are each a string *or* an array of
  strings.** A union decode is the sharp edge of this provider; table-test both
  forms.
- **`from` is an RFC 5322 address**, `Name <email@example.com>` or bare. The
  mail plugin's `Address` parsing already exists — reuse it, do not re-write it.
- **`Idempotency-Key`** is a request header worth recording in `Meta`. Tommy
  does not deduplicate: that is state, and state is scenario machinery. Say so
  in the README.
- **Attachments** carry either `content` (base64) or `path` (a URL Resend
  fetches). The base64 form goes to the blob store like every other attachment.
  **The `path` form must not be fetched** — tommy makes no outbound requests.
  Record the URL in `Meta`, put nothing in the blob store, and document the
  refusal in the README next to the other deliberate non-implementations.
- **`scheduled_at`, `tags`, `topic_id`, `template`** are recorded, never acted
  on. Scheduling is not simulated.
- **The error shape** (`statusCode`/`message`/`name`, apparently) must be read
  from the live error reference before anything returns a 4xx.
- Resend ids are **UUIDs**, not tommy's 24-hex event ids. `sms/twilio` already
  solves exactly this — it mints an `SM…`/`MM…` Sid from the event id and maps
  back on read (`plugins/sms/providers/twilio/twilio.go`, `sidFor`/`idFromSid`).
  Use a reversible mapping of the same kind rather than a second index.

### Around the provider

- **`plugins/all/all.go`** registration, and `cmd/mail.go` — the provider must be
  selectable through `--enabled-providers` and any option worth setting needs a
  flag (rule 10).
- **`README.md` for the provider** with the three required sections, and
  commands that were actually run.
- **`docs/catalogue.md`** gains a row; `docs/clients.md` gains a Resend section.
  The SDK is `github.com/resend/resend-go/v4` and its `Client.BaseURL` is an
  exported field, so this is the *easy* kind of SDK: `client := resend.NewClient(key);
  client.BaseURL, _ = url.Parse("http://localhost:8822")` — verify the exact
  field and type against the source before writing it down.
- **An integration test** in `test/integration`, driving the real SDK, alongside
  the Mailjet and SendGrid ones.
- **No new dependency in the root `go.mod`.** The provider parses JSON with the
  standard library, like its siblings. `resend-go` belongs to
  `test/integration` only — and adding it there still means
  `cd test/integration && go mod tidy && go test -tags integration ./...` in the
  same commit.

---

## Wave 12 — the project website on GitHub Pages

**Goal.** `can3p.github.io/tommy` (or a custom domain): a landing page that
shows what tommy is and why anyone would want it, plus the full documentation,
regenerated from the repository on every push to `main`.

**The constraint that shapes the whole wave:** the site holds no prose of its
own except the landing page. `docs/catalogue.md` already states the principle —
*the authoritative text is each component's own `README.md`, so there is one
copy of every claim rather than two that drift apart* — and a website is the
most tempting place in a project to break it. The generator renders the files
that already exist; it does not restate them.

### Tasks

1. **The generator.** A small static-site generator in `website/`, **its own Go
   module**, exactly as `test/integration` is one, so a Markdown library never
   enters tommy's `go.mod`. It reads:
   - `docs/*.md` and every `plugins/**/README.md` (fifteen providers and eight
     plugins carry one; `clienthelp` has only a package doc, and
     `docs/clients.md` is where it is explained),
   - the provider catalogue as data, from `tommy providers --json`, which
     already exists,
   - `docs/openapi.json` from wave 10, rendered as an API reference page.

   It rewrites the repo-relative links inside those files (`../plugins/mail/README.md`
   and friends) into site paths — that rewriting is the fiddly part and the
   place to put the tests.

   **The website module must not `replace` the root module**, and must not
   import tommy. `test/integration` does both, which is why every wave that adds
   a root dependency has to re-tidy it — a standing cost `CLAUDE.md` now carries
   a rule about. A generator that shells out to `go run . providers --json`
   instead has no such coupling: it consumes tommy's output, not its packages,
   and a root dependency change can never break it.

2. **The landing page**, hand-written, and the only hand-written content: what
   tommy is, the eight plugins and what each stands in for, the 30-second
   quickstart, and the install line. Feature highlights should be *specific* —
   "see the HTML your password-reset mail actually rendered", not "captures
   email".

3. **The workflow.** `.github/workflows/pages.yml`: build on push to `main`,
   publish with `actions/deploy-pages`. It needs Pages enabled on the repository
   with source *GitHub Actions* — a settings change only the repository owner can
   make, so ask rather than assume it is done.

4. **The coverage test.** Every plugin and every provider that
   `tommy providers --json` reports must appear on the site with its README
   rendered, and every internal link must resolve. This is the same drift gate
   as wave 10, applied to documentation: a provider added in a later wave that
   nobody remembered to link fails the build rather than quietly going missing.

### Decisions

- **A Go generator in its own module** rather than Jekyll or Hugo. Jekyll is
  what GitHub Pages does for free, but it can only see files under `/docs`, and
  tommy's documentation deliberately lives *next to the code it describes* — a
  Jekyll site would need copies, which is the one thing this wave must not
  create. Hugo and Docusaurus solve that but bring a toolchain and a
  configuration surface out of proportion to a documentation site for a single
  binary. A generator that can also run `tommy providers --json` and consume the
  OpenAPI description keeps every claim on the site traceable to something the
  build verified.
- **Screenshots go stale silently**, which is the failure this project keeps
  meeting in other forms. Either generate them in CI from a running tommy, or
  ship the landing page with none and let the copy carry it. Do not hand-paste
  a screenshot and hope.
- **Versioning is out of scope.** The site describes `main`. Released binaries
  are on the releases page; a docs-per-version site is a different project.

### Done when

The usual ritual, plus: `README.md` links the site, and `CLAUDE.md` gains a rule
that the site is generated and that adding a plugin or provider without its
README breaks the build — which, by then, is true.

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
| **Upstream: kleiner** | — | Fix `MaybeNotifyAboutNewVersion` in `can3p/kleiner`: it prints the error and falls through to dereference a nil version, panicking a released binary at startup when GitHub is unreachable. Second latent deref on the same path. Affects every project scaffolded from kleiner. |

## Backlog — small, unblocked, good first tasks

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
  Wave 9 TLS work) or drop the field and its validation.
