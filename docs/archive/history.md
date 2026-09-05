# History

What has been built, why, and what changed under contact with real code.
Oldest first. The forward-looking plan is `docs/implementation-plan.md`; the
interfaces as built are `docs/contracts.md`; the transferable lessons are
`docs/lessons.md`.

All of this was built by a coordinating session dispatching subagents, one task
per agent, with exclusive file ownership per task.

> **Adding to this file.** When a wave is finished, move its section here from
> `docs/implementation-plan.md` and rewrite it in the past tense: what was built,
> and — the part worth the space — what turned out to be wrong, what a real client
> or a live vendor doc contradicted, and any contract that had to change. An
> inventory of files is not worth recording; git already has it. See
> `CLAUDE.md` → *Finishing a wave* for the full ritual.
>
> The shape that has worked, per wave: a one-line summary of what shipped and how
> many agents ran; **Found** for anything that turned out to be wrong — a vendor
> behaviour, a library default, a bug in a dependency; **Contract gap** for
> anything that had to change in `core`, and whether more than one task hit it
> independently; **Deliberate non-implementation** for what was skipped and why.
> The Wave 2 and Wave 5 entries below are the fullest examples.

## Wave 0 — foundation and CI · 2 agents

`core/**`, `go.mod`, `cmd/serve.go`, `cmd/providers.go`, plus the CI pipeline,
`.golangci.yml`, `Makefile`, and the GoReleaser v1→v2 migration.

Delivered the event/store/blob/plugin/config packages, the ring buffer with
subscriber fan-out, the size-capped blob store, the path-routed ingress mux with
collision detection, the SSE hub, the UI shell and component library, the
lifecycle supervisor, `core/testutil`, and `plugintest.Conformance`.

**Found:** `can3p/kleiner`'s `MaybeNotifyAboutNewVersion` prints the error when it
cannot reach the GitHub API but does **not** return, then calls `Newer` on the nil
version it just failed to fetch. `Newer` has a value receiver, so a released
binary panics at startup whenever GitHub is unreachable or rate-limiting. Dev
builds skip the check, which is why it never showed up locally. Contained in
`cmd/root.go` with a recover and `TOMMY_NO_UPDATE_CHECK`; **the real fix belongs
upstream in kleiner**, and there is a second latent nil deref on the same path
(`version.Parse(*releaseObject.TagName)` when GitHub returns a rate-limit body).

**Also found:** `.goreleaser.yaml` declared `version: 1`, which GoReleaser v2
rejects outright, and used the removed `archives.format` key. There was no release
workflow at all, so it had never run.

## Wave 1 — mail and sms plugin cores · 2 agents in parallel

Canonical models, API surfaces, and the first two bespoke tabs.

**Mail** — addresses, subject, text and HTML parts, headers, attachments as
`blob.Ref`. `ReplyTo` is a slice because RFC 5322 permits a list. One `Message`
means one *delivered* message, so fan-out happens in providers.

**SMS** — sender, recipient, body, MMS media, status, and segment counting that is
**greedy-packed rather than divided**: an escape pair or surrogate pair cannot
straddle a boundary, so 153 `€` characters are 306 septets but occupy three
segments, not two. Same for astral-plane emoji under UCS-2.

**Contract gap, found independently by both agents:** a bespoke plugin tab could
not render the how-to-test panel or a snippet-carrying empty state, because
neither the shell nor `Deps` exposed the registry — only the generic view could,
through an unexported accessor. Writing your own tab silently cost you the one
thing that tells a newcomer how to fill it. Fixed with `Shell.Info()`.

**Also:** `plugintest.Conformance` rejects a plugin with zero providers, which is
why `mail.New` is variadic.

## Wave 2 — four provider fakes · 4 agents in parallel

Mailjet, SendGrid and Twilio on Sonnet; SMTP on Opus (MIME parsing is where
subtle bugs hide).

Every agent was required to verify wire formats against **live vendor
documentation**. Three things the plan had wrong:

- **Mailjet** issues a separate `MessageID`/`MessageUUID`/`MessageHref` **per
  recipient** across To/Cc/Bcc, not one per logical message. Per-message failures
  ride inside a **200** at that message's position; only a malformed request as a
  whole gets a top-level 4xx.
- **Twilio** returns `num_media`/`num_segments` as **quoted strings**, and
  `error_code`/`price`/an unused sender as `null` rather than `""`. Get these
  wrong and the official SDK fails to parse.
- **SendGrid** rejects `reply_to` and `reply_to_list` set together with a 400
  rather than merging them. The docs also settled the override direction for
  personalization-level `subject`/`headers`/`custom_args`/`send_at`.

**Go constraint worth remembering:** Twilio's fetch URL ends `.json`, but a
`net/http.ServeMux` wildcard must occupy a whole path segment, so `{Sid}.json`
**panics at registration**. The route captures the segment whole and trims the
suffix; a literal `.json` request is served identically.

**Contract gap:** a listener provider's address was derived from configuration
alone, which is wrong both for a provider given no port (it falls back to its own
default) and one given port 0 (ephemeral) — both were advertised with *no address
at all*. Fixed with `AddressableProvider`, plus a resolve pass after listeners
start, since none have bound when the server is built. Two providers had already
converged on the identical `Addr(timeout)` signature independently.

## Wave 3 — CLI, UI polish, SDK helpers, integration tests · 5 agents

`tommy mail` / `tommy sms` single-plugin shortcuts sharing one bootstrap with
`tommy serve`, a commented `tommy.toml`, a rewritten README, cross-plugin e2e
tests, the how-to-test panel on both tabs, `clienthelp`, and the vendor-SDK
faithfulness suite.

**The SDK story, verified rather than assumed:** Mailjet takes a base URL
directly; SendGrid takes one through `GetRequest` but *not* through
`NewSendClient`; **twilio-go has no override at all** — `RequestHandler.BuildUrl`
reparses and rebuilds the hostname as `product[.edge][.region].twilio.com`, and
`TWILIO_EDGE`/`TWILIO_REGION` cannot escape `twilio.com`. Its supported hook is a
custom `*http.Client`, which is why `clienthelp` ships a `RoundTripper`.

**Deviation from the plan:** the proposed `clienthelp/twiliohelp` package was
**not** built. It would have put `twilio-go` in tommy's own `go.mod` and handed it
to everyone importing tommy, to save six lines of wiring. The wiring is
documented in `docs/clients.md` instead. For the same reason `test/integration` is
a nested module.

**Found by compiling the doc snippets against the real SDKs:**
`mailjet-apiv3-go` builds its URL as `apiBase + ".1/send"`, so the base URL must
already end in `/v3`. Passing plain `:8822` lands nowhere.

**Two more contract gaps, each found by multiple agents:**
- Both UI agents *cloned* the core how-to-test panel because it decided open/closed
  from "are any plugins configured" rather than "does this tab have content". Made
  a parameter; both clones deleted.
- **Three** separate suites hand-pinned SMTP's port, because `config.Ephemeral()`
  only zeroed the three core listeners — a test asking for an ephemeral server
  still bound the well-known **1025**. `testutil` now pins listener providers too.

**Result:** all seven vendor-SDK tests passed against the fakes with no
accommodation anywhere in the test code. No fidelity gap.

## Wave 4 — the `files` plugin · 1 then 2 agents

The first **stateful** plugin. Mail and SMS are pure event streams; file transfer
means creating directories, overwriting files, listing the current tree and
downloading something uploaded an hour ago.

**The design that made it work:** the VFS is *state*, the event log is *history*,
with independent lifetimes. A file stays downloadable long after its upload event
has fallen out of the ring buffer — asserted directly by
`TestFileOutlivesItsEvent`, and the whole reason the blob store was separated from
the event store back in the design phase.

**Path resolution is one gate.** Every operation funnels through `VFS.Resolve`,
which rejects control characters and invalid UTF-8, normalises backslashes so they
cannot smuggle a parent reference past cleaning, then clamps `..` at the root like
a chroot. There is no host filesystem underneath in any case. 42 hostile-path
subtests.

**One lock, no blob I/O under it.** Uploads stream into the blob store first and
install in one short locked step, so a slow upload cannot block a listing.

**FTP** — ftpserverlib's `ClientDriver` *is* an `afero.Fs`, and FS1 had already
made `*files.File` satisfy `afero.File`, so the adapter is thin.
**SFTP** — `pkg/sftp`'s four handlers map almost directly; the host key is
generated once and persisted, verified by booting twice and comparing the key *as
the client sees it in the handshake*.

**Bug only a real client could find:** ftpserverlib defaults a new connection to
**ASCII transfer type**, which refuses `SIZE` and silently rewrites LF→CRLF on
download — quietly corrupting a store whose whole promise is byte-exactness.

**Cross-protocol sharing verified end to end:** a file uploaded with `curl` over
FTP downloads over OpenSSH `sftp`, and vice versa. That is the payoff from naming
the plugin `files` rather than `ftp`.

**Coordination mistake worth not repeating:** dependencies were pre-added to
`go.mod` before any code imported them, so both agents could run in parallel.
Unused requires do not survive `go mod tidy` — they were dropped, and both agents
hit build failures.

## Wave 5 — the `chat` plugin · 1 then 3 agents

Slack and Teams, and the third distinct view shape after mail's inbox and sms's
phone conversation.

**Threads are derived, not stored.** Slack's `thread_ts` is a relation and the
event store deliberately has none, so channels and threads are computed from the
flat event list at render time. A reply whose parent was never captured, or has
been evicted, keeps its thread and the view says the parent is missing and why —
a bounded store guarantees this eventually happens.

**Three schemas, kept separate.** Slack Block Kit, Teams MessageCard and Teams
Adaptive Card are genuinely different; the canonical message keeps each payload
byte for byte with a format tag rather than flattening them. Plain text is always
populated, harvested from the card when the payload carried none.

**Rendering is a seam, not a dependency:**
`func(format string, data json.RawMessage) (template.HTML, bool)` — primitive on
purpose so the renderer package never imports `chat`. Returning `false` falls back
to text plus JSON inspector, so partial schema coverage is safe.

**Response contracts that are easy to get wrong:** Slack's incoming webhook
answers with the literal text `ok`, not JSON; `chat.postMessage` returns HTTP 200
with `ok:false` for application-level errors. Teams' O365 connector answers with
the literal `1` as text/plain while a workflow post answers `202` — and since both
generations share the same URL shape in the wild, the **payload** decides, not the
path.

**The plan was wrong about Teams:** `webhookb2` is the *legacy O365 connector*
shape, not Power Automate, whose workflow triggers are unrelated `logic.azure.com`
URLs.

**Two deliberate non-implementations, both correct:** Slack's `chat.update` and
`chat.delete` were skipped because events are immutable and threads are derived,
so those endpoints could only answer `ok` while nothing changed. And modern Slack
rejects webhook `channel`/`username`/`icon` overrides, but tommy keeps accepting
them — a lenient fake is more useful than a faithful rejection — with the
divergence documented rather than silent.

**The riskiest code in the project** is `plugins/chat/ui/blocks`, whose output is
injected unescaped. Verified independently by firing scripts, attribute
breakouts, `javascript:` and `data:text/html` URLs through every text and link
slot, then parsing the resulting page for live tags, event-handler attributes and
dangerous schemes in real attribute positions. Zero. Hostile input survives only
as inert escaped text.

## Wave 6·0 — CLI catch-up

The CLI had fallen two plugins behind the config. `tommy mail` and `tommy sms`
existed; `files` and `chat` had shipped without a subcommand and could only be
run from a TOML file. `tommy.toml` had never grown their sections either. This
wave closed both gaps and turned "keep the CLI level with the config" into
`CLAUDE.md` rule 10, so it never has to be a wave of its own again.

**Built:** `tommy files` and `tommy chat`, following `cmd/mail.go` exactly — the
same `Config` built in memory, the same `server.New`/`Start`/`Shutdown`
bootstrap, no second code path. Provider options reach the CLI through a
`providerOptionBuilder` that records a key only when `Flags().Changed` is true,
so an unset flag never overrides a provider's own default, and flags are named
`--<provider>-<option>` so two providers of one plugin cannot collide. Flags
given for a provider that `--enabled-providers` excluded are a hard error rather
than a silent no-op. `tommy.toml` gained documented `files` and `chat` sections.

**`ProviderConfig.Port` turned out to be inert for HTTP providers.** `tommy.toml`
had advertised `# port = 9001` under mailjet, sendgrid and twilio since the
config landed, and the field's own doc comment promised "its own listener instead
of the shared ingress (HTTP providers)". Nothing implemented it. The ingress
path-routes every HTTP provider onto one listener and has no per-provider port;
the field's only reader, `listenerAddr`, uses it to report where a *listener*
provider bound. `Validate` range- and collision-checks the value, which is
exactly what kept the gap hidden for three waves — the config looked like it was
being taken seriously. It surfaced only when the agent, reasonably trusting the
documentation, built `--mailjet-port`/`--sendgrid-port`/`--twilio-port`/
`--slack-port`/`--msteams-port`, and a coordinator check of
`--sendgrid-port 45999` against the real binary found nothing listening there.
The five flags were removed and the claim deleted from the documentation rather
than implemented: giving a provider a listener of its own is core work, not a CLI
catch-up. It is in the plan's backlog as implement-or-delete.

**A stray positional argument silently started a server.** No command set `Args`,
so cobra passed unrecognised positionals to `RunE` and they were ignored:
`tommy mail help` booted a server instead of printing help. This was not
theoretical — a `main mail help` process was found still holding 1025, 8811 and
8822 nine hours after an earlier session, and it is the most likely identity of
the "pre-existing mail catcher on 1025" an agent reported working around.
`Args: cobra.NoArgs` now covers `mail`, `sms`, `files`, `chat` and `serve`.

**Deliberately left file-only, and documented as such** in each `tommy.toml`
section rather than silently: every listener's `bind`, and the tuning knobs
(SMTP's `domain` and size/timeout caps, FTP's idle and connection timeouts,
SFTP's `server_version`, timeouts and connection/auth caps). The reasoning is
that nobody flips these per test run, and moving a listener off loopback is a
security-relevant change better made explicitly in a versioned file. Credentials
went the other way and got flags on every provider that has them, because pinning
one is how an application's error path gets exercised — `--mailjet-api-key` with
a mismatched pair returns Mailjet's real `mj-0015` 401, verified against the
running binary.

## Wave 6a — the HL7 v2 plugin core and the TFTP provider · 2 agents in parallel

Two tasks with nothing in common but a branch: a new plugin core defining a model
the next wave codes against, and a fourth transport for a plugin that already
existed. They ran in parallel because their directories are disjoint and only one
of them touched `go.mod`.

**Built — `plugins/hl7`:** a segment → field → repetition → component →
subcomponent tree, parsed with the separators each message declares in `MSH-1`
and `MSH-2` rather than the `|^~\&` everyone assumes. Escape sequences resolve to
*that message's* separators. A collapsible segment-tree tab showing each field at
its own position with a small dictionary name, repetitions kept visibly apart, and
a badge naming the declared separators whenever they are unconventional — which is
exactly the message somebody else's parser is getting wrong.

**Built — `plugins/files/providers/tftp`:** RFC 1350 over the existing VFS. No new
plugin, no new UI, no new canonical model — the payoff of having named the plugin
`files` rather than `ftp` back in Wave 4, now collected for the third time.

**Parsing recovers; it does not fail.** The one error case is an empty message.
Everything else returns a message carrying coded `Issue`s (`no-header`,
`duplicate-separator`, `segment-id`, …), because the consumer of this decision is
MLLP choosing between `AA`, `AE` and `AR`, and that needs a parsed message and a
reason rather than an error. The sharpest case: a separator declared *twice* in
`MSH-2` is dropped rather than split on, since splitting anyway would shred every
value in the message instead of just the header.

**`ListenerProvider` turned out to be genuinely transport-agnostic.** TFTP is the
first UDP provider, and the contract needed no change: `net.ListenPacket` plus
`AddressableProvider.Addr` fit as they stand, because the core only ever starts
`Listen` in a goroutine and waits for it to return. A contract written against
three TCP providers survived contact with a datagram one, which is worth knowing
before NFS and SNMP arrive.

**A plugin core cannot carry a snippet, and that is correct.** `Snippets()` is a
`Provider` member, so with no provider the tab's how-to-test panel has nothing
runnable. The alternative — moving snippets onto the plugin — would let a core
advertise a listener that does not exist. The consequence is that `hl7` is
deliberately **not** wired into `plugins/all/all.go` this wave: `plugintest`
rejects a plugin with no providers, and is right to, since it could never receive
anything. Wiring and `cmd/hl7.go` land with MLLP.

**What a real client proved that a test double would not have.** The TFTP round
trip was driven with `curl` over UDP on a payload carrying `\r\n`, a lone `\n`, a
NUL and a high byte, asserting byte-identical bytes back. TFTP has a `netascii`
mode, so the exact class of bug that made ftpserverlib silently rewrite line
endings in Wave 4 was live here too. It also surfaced a library limitation worth
recording: `pin/tftp` parses a client's requested `timeout` option but never
echoes it in an OACK, so it is documented as not granted rather than claimed —
`blksize` and `tsize` are genuinely negotiated.

**Coordinator work the agents were rightly barred from.** Wiring `tftp.New()` into
`plugins/all/all.go` — without which the provider existed but nothing shipped it,
and `tommy providers files/tftp` reported no such provider — and updating the
`files` plugin description, which still said "over FTP or SFTP". A wave that adds
a provider is not finished at the provider's directory boundary.

## Wave 6b — MLLP and NFSv3 · 2 agents in parallel

The two providers that make Wave 6a's work usable: the HL7 core got its first
real listener, and the `files` VFS got its fourth and most structurally demanding
transport.

**Both agents were killed by a session rate limit inside their first few tool
calls**, before either had written a file. Resuming them with their transcripts
intact, plus an explicit statement that nothing had reached disk and they should
start from the beginning rather than hunt for partial work, cost one round trip
each. This is the second kind of interruption the project has survived on the
same principle — the earlier one was machine sleep — and the handling is the
same: check what is actually on disk, say so, and resume rather than re-brief.

**Built — `plugins/hl7/providers/mllp`:** the `0x0B … 0x1C 0x0D` framing, and an
ACK generated entirely from the request. The framing itself is trivial; the work
is the edges, each with its own socket-driven test — a message split across
packets, several pipelined into one read, junk before a start byte or after a
trailer, a connection closing mid-frame, and a frame that never terminates being
bounded and dropped.

**The ACK uses the inbound message's separators.** A message declaring `!` as its
field separator gets an ACK delimited by `!`. Emitting the conventional `|^~\&`
would have been precisely the bug the HL7 plugin exists to expose, so this was
verified independently rather than taken on trust.

**The core's "recover and record" design paid off exactly where it was aimed.**
`AA`/`AE`/`AR` are chosen from `HasHeader()` and `HasIssue(code)`, never from an
error: no usable header is `AR`, a header whose separators cannot be trusted is
`AE`, and an unrelated segment issue leaves it `AA` because it does not
compromise the header. With no header there is no control id to echo, so `MSA-2`
is left empty rather than invented — a fake that makes up an id would be lying to
the one system best placed to notice.

**Built — `plugins/files/providers/nfs`:** NFSv3 over the same tree. The backend
interface is **go-billy, not afero**, so this is a second adapter shape rather
than a copy of the FTP one — worth knowing before a fifth transport is attempted.

**Handle-based addressing is the interesting part** for a project whose one path
invariant is that `VFS.Resolve` decides everything. Handles are random UUIDs
minted per path into an LRU and encode no path at all; an unknown or evicted
handle is `STALE`; the components behind a handle the server did mint are
re-resolved through the VFS on every operation. The escape tests had to be driven
with **raw RPC**, because the client library `path.Clean`s before anything
reaches the wire — a test through the client alone would have proved nothing.

**No portmapper, deliberately.** Both RPC programs are served on one port. rpcbind
lives on privileged 111, and registering a fake there would advertise it to
everything on the machine — so a client must be given `port=` and `mountport=`
explicitly. That is the least obvious thing about using this provider, which is
why the mount command leads its snippets in both Linux and macOS spellings. 2049
needed no unprivileged stand-in; unlike 21, 22 and 69 it is already unprivileged.

**Two limitations recorded rather than papered over.** One logical NFS upload is a
CREATE plus one event per WRITE chunk, because NFS has no open or close on the
wire and `COMMIT` never reaches the backend; the alternative was a debounce that
would make reads see stale content. And because billy's methods take neither a
context nor a connection, the recorded peer is the *mounting* connection's
address. Both are in the plan's backlog.

**A library edge worth remembering:** go-nfs opens `O_WRONLY|O_EXCL` without
`O_CREATE` on its truncate path, meaning "do not create". POSIX leaves that
undefined; the VFS read it strictly as "must not exist", so truncate over a mount
failed outright until the adapter dropped the flag. Also, go-nfs compares
filesystems with `reflect.DeepEqual`, which would deep-walk the whole VFS tree —
the adapter puts a cheap discriminator first so it never gets that far.

**Neither agent found a core gap**, and both were asked directly. `ListenerProvider`
being transport-agnostic held for a second time, `AddressableProvider` supplied the
ephemeral port, and `VFSBinder` the shared tree. The `hl7` core needed nothing added
for its first real consumer, which is the useful signal about a contract written one
wave ahead of its use.

## Wave 6c — the SNMP trap receiver · 1 agent

The last of Wave 6, and the plugin the roadmap had picked as its cheapest: pure
capture, no state, and no reply at all for the main case.

**Built:** an own plugin with a UDP trap provider. v1 traps, v2c traps and v2c
informs, every varbind decoded by its real wire type, default port 1162 rather
than the privileged 162.

**gosnmp's own `TrapListener` could not be used, and finding that out was the
task's one real decision.** It owns its UDP socket internally and never reports
the address it bound, which makes `AddressableProvider` impossible — and that
matters here more than it sounds, because the whole discovery surface (snippets,
`tommy providers`, the how-to-test panel) renders against the address a provider
actually bound. With `--trap-port 0` the snippets would have pointed at nothing.
The provider therefore opens its own `net.PacketConn`, the way TFTP does, and
calls gosnmp's *exported* wire functions — `UnmarshalTrap` to decode,
`MarshalMsg` to build the inform's `GetResponse`. A convenience wrapper being
unusable does not mean the library is: the layer underneath was the right one.

**v1 and v2c are modelled separately on purpose.** A v1 trap carries enterprise
OID, agent address, generic and specific trap numbers and a timestamp in its PDU
header; a v2c trap has none of those and instead carries `sysUpTime.0` and
`snmpTrapOID.0` as its first two varbinds. Two mutually exclusive structs, and
neither version is ever handed a field it does not have — flattening them would
have destroyed exactly what someone reads a trap to find out.

**The "no UI on day one" claim was tested honestly and held.** The plan allowed
the generic event view as a starting point and asked whether a new plugin is
genuinely useful without bespoke UI code. This plugin shipped with `RegisterUI`
and `RegisterAPI` empty, and the answer is yes: the list line names the version,
trap OID and varbind count, and the detail pane renders every varbind's OID, type
and value beside a hex dump of the datagram. A varbind *table* is nicer and is in
the backlog — recorded as unbuilt rather than quietly claimed.

**Verified against a second implementation.** net-snmp's `snmptrap` and
`snmpinform` were used alongside gosnmp's client, which is worth more than either
alone: `snmpinform` blocks until it receives a `GetResponse`, so its clean exit is
end-to-end proof that the one reply this plugin makes actually arrives. A
non-SNMP datagram is captured with a decode error recorded rather than dropped —
a fake that silently discards what it cannot parse teaches its user nothing.

## Wave 7 — the `push` plugin · 4 tasks, sequenced

The largest remaining gap against the original brief, and the first wave with a
real internal ordering rather than a convenient one: the core had to land before
either provider, and APNs could not exist at all until the ingress spoke HTTP/2.

**Built:** the `push` plugin core (canonical model, API, lock-screen tab),
cleartext HTTP/2 on the ingress, the FCM provider, and the APNs provider.

**The plan was wrong about FCM targeting, and the live discovery document said
so.** The addressing union has **four** members, not three: `token` is marked
*"Deprecated: Use `fid` instead"* and both are still accepted. The core author
found it; the coordinator re-checked it against
`https://fcm.googleapis.com/$discovery/rest?version=v1` before two providers were
built on top. The plan also conflated *"category/collapse key"* as one concept —
`aps.category` picks action buttons, `apns-collapse-id`/`collapse_key` supersede
an undelivered message — and treated `apns-topic` as a topic when it is the app's
bundle ID. All three would have been baked into two providers.

**The plan was also wrong about how to serve HTTP/2, and this one cost nothing
only because the agent checked.** The plan called for `golang.org/x/net/http2/h2c`
and a new dependency. That package is marked *"Deprecated: This package is
deprecated"* in the x/net version **already in the module graph**, its
`NewHandler` says to set `http.Server.Protocols` instead, and staticcheck is
enabled here — so it would have failed the lint gate. Go 1.26 serves h2c
natively. Zero new dependencies, and the `go.mod` ownership constraint the wave
was sequenced around turned out to be unnecessary.

**The most valuable bug of the wave was found by driving the fake, not reading
it.** FCM v1 is proto3-backed, and the proto3 JSON mapping accepts both the
canonical lowerCamelCase name and the original snake_case proto field name. The
provider, following the discovery document, accepted only camelCase — so a
request carrying `android.collapse_key`, which real FCM accepts, got a **200 and
silently lost the field**. For a tool whose entire purpose is showing people what
they sent, silent loss is the worst failure mode there is, worse than a
rejection, because nothing tells the user anything went missing. It was found by
posting both spellings at the running binary and diffing the captures.

Two things that nearly became project fact and did not: the provider author's
conclusion that the core documented the *deprecated Legacy API* (it did not —
snake_case is equally valid v1 input), and the fix's first instinct to rewrite
keys throughout the tree, which would have renamed a caller's own `user_id` data
key and corrupted the very thing they opened the tab to inspect. The normaliser
is scoped to known keys and leaves `data`, `headers` and `payload` alone.

**h2c defaults on for the ingress and off for the UI and API.** It is recognised
by an exact match on the HTTP/2 client preface, which no HTTP/1.1 client ever
sends, so existing providers were unaffected; default-off would have made every
APNs user's first experience a connection error. Because h2c is settled from the
first bytes of a connection — before routing knows which surface a request is for
— it cannot be a per-path decision: a shared listener speaks it when any surface
on it asks, which is now logged at startup rather than left to be discovered.

**Refusals worth recording.** FCM declines to fake `404 UNREGISTERED` and
`403 SENDER_ID_MISMATCH`; APNs declines the errors that need delivery state.
Both require a token registry or a delivery pipeline tommy does not have, so the
replies could only be invented — the scenario simulation the charter rules out.
Applied without being asked, which suggests the boundary in `CLAUDE.md` is
carrying its weight.

**The APNs agent was stopped mid-task by the user, and its work was cancelled
rather than resumed.** The implementation was on disk and complete — including
the `content-available` → `KindSilent` trap the core author had flagged hardest —
but it had written no tests at all. The coordinator wrote them: table-driven
fixtures asserting both the canonical message and the exact HTTP response, and a
real HTTP/2 suite that fails on anything but `ProtoMajor == 2`, which matters
precisely because the provider deliberately still answers HTTP/1.1. A test that
only proved HTTP/1.1 worked would have tested nothing about an HTTP/2-only API.

**A lesson about interruption, from three separate kills.** Two agents died to a
session rate limit inside their first tool calls, and one was stopped by the
user. In every case the first move was the same and the cheap one: look at what
actually reached disk, then say so explicitly — resuming an agent that wrote
nothing sends it hunting for work that does not exist, and resuming one that
wrote a great deal without saying what is missing gets the gap re-guessed.

## Wave 8 — the `as2` plugin · 3 tasks, sequenced

The first wave whose subject was a *standard* rather than a vendor, and the
first to need a credential of its own. AS2 is RFC 4130: a trading partner POSTs
an S/MIME message carrying an EDI document and blocks on a synchronous MDN
receipt. It fits the charter because everything in that receipt is derivable
from the request, and it is worth having because there is no Go AS2
implementation at all and standing one up for real is famously miserable.

**Built:** `Deps.ConfigDir` in core, the `as2` plugin core (canonical model,
byte-exact MIME, three-rule MIC, MDN construction, certificate identity, API and
tab), the `as2/http` provider, and the `tommy as2` shortcut.

**Two specifications contradict each other, and the fix was to keep both
answers.** RFC 4130 §7.3.1 says the MIC of a plain unsigned message is computed
"without the MIME or any other RFC 2822 headers"; RFC 5402 §4.3 says "including
all MIME header fields and any applied Content-Transfer-Encoding". Verbatim from
both, and mutually exclusive. 4130 is Standards Track and 5402 an Informational
Independent Submission, so 4130 wins — but the losing digest is kept on the
model as `AlternateMICs`, because the person reading it is chasing a mismatch
with a partner and needs both numbers rather than our verdict about which is
right. Resolving a spec conflict by discarding the loser's *data* would have
thrown away the only thing that makes the disagreement diagnosable.

**The MIC has three coverage rules and compression adds a fourth trap.** For a
signed message the MIC covers what was actually signed — which, under
compress-then-sign, is the *compressed* bytes. Decompressing first yields a
digest every real partner rejects while every round-trip test against our own
code passes. RFC 5402 also permits compression inside *or* outside the
signature, never both, and requires receivers to handle both placements.

**OpenSSL was the independent implementation, and it contradicted the plan
three times.** It writes mixed line endings — bare LF for outer headers and
multipart delimiters, CRLF for part headers and bodies — so a strict RFC 2046
splitter finds *zero* parts in a message OpenSSL has just produced. It writes
`micalg="sha-256"` with a hyphen, which RFC 4130's own grammar has no room for.
And Homebrew's build has no zlib, so `cms -compress` simply fails and the
compression fixtures had to be assembled from `asn1parse -genconf` over a Python
zlib stream. Separately: the line break *before* a boundary delimiter belongs to
the delimiter, which is worth a byte-exact test because including it makes every
MIC one or two bytes too long.

**The worst defect was ours, and it was about *when* code runs, not what it
does.** `Identity.Configure` generated a key pair and wrote it to disk, and it
is called from `RegisterIngress` — which runs for anything that merely *builds*
a server. `plugintest.Conformance` builds a server, so a plain `make check` left
a real private key in the user's own config directory. It survived review
because the provider's own tests happened to sandbox `HOME`; the containment was
in the test rather than in the code. Generation now happens on first genuine use
— an arriving message, a partner fetching the certificate, the tab showing a
fingerprint — while reading explicitly configured files stays eager, because a
path that does not resolve is a startup complaint. **The general rule, now in
`docs/contracts.md`: registration may validate, but it may not create.**

**Checking for a side effect means knowing the platform's path.** The first
check for stray files looked in `~/.config/tommy` and found nothing, which
proved nothing: `os.UserConfigDir()` on macOS is `~/Library/Application
Support`. The key pair was sitting there the whole time.

**A tool's own output format is not necessarily the protocol's.** In AS2 the
`Content-Type` and `Content-Transfer-Encoding` are HTTP headers, so the body is
bare base64 with no MIME block. `openssl cms -encrypt -outform SMIME` writes
three headers above its body, and the obvious way to remove them — `tail -n +2`
— strips only `MIME-Version` and leaves two behind. Tommy answers 200 with
`processed/Error: unexpected-processing-error` and "illegal base64 data at input
byte 7", which looks enough like success to waste an afternoon. The plugin
core's README shipped with exactly that broken pipeline and was corrected only
because the provider author *ran* it. A snippet nobody executed is a guess.

**The RFC declined to let us gatekeep.** §6.2: "There is no required response to
a client request containing invalid or unknown AS2-From or AS2-To header
values." So the `as2_to` pin is not a rejection pin — a mismatch is captured,
answered with a normal MDN, and flagged on the event, which is the opposite of
the usual "pin implies reject" instinct and is recorded in the flag's own help
text so nobody later "fixes" it into a 403. §7.4.4 likewise reserves `failed`
for being unable to produce an MDN at all, so an undecryptable message is
`processed/Error: decryption-failed`, not a failure.

**The identity design came from a stated requirement, not from the protocol.**
Certificate paths must be configurable because tommy may run in a container in a
cluster that already has its own CA, and someone running only the `mail` plugin
must never meet a certificate at all. The second half fell out of the core's own
shape: a plugin's `RegisterAPI` is handed an empty `Config` — only providers get
a `ProviderConfig` — so the identity is created unconfigured by the plugin and
configured by a provider through an `IdentityBinder`, mirroring
`files.VFSBinder`. A disabled provider therefore cannot cause a certificate to
exist.

**Deliberate non-implementations.** Asynchronous MDNs
(`Receipt-Delivery-Option`) are an outbound callback and outside the charter:
recorded, flagged as an issue, answered synchronously — never silently ignored.
Encrypted private keys, because there is no terminal to prompt on; the error
names the `openssl pkey` one-liner instead.

**Reported, not patched — three core gaps.** A provider cannot contribute an
issue to a message, so §6.2's "MAY return an MDN with an explanation" is
unreachable from provider code. Route paths cannot depend on config, because
`Endpoints()` takes no `Deps`, while real AS2 partners do configure each other's
URLs. And `PluginConfig` has no free-form options bag, so plugin-level settings
have no home — worked around by the binder rather than by growing the core.

**An agent died to a session rate limit mid-task, for the third time in three
waves.** The handling was the same and cost one round trip: read the disk first,
then tell it exactly what survived and what did not. Here the provider was
complete and the test file held a harness with no test functions at all, so the
resume message said precisely that. It also caught something worth naming — the
dead agent had left a comment claiming its snippets were "run against a live
tommy before being committed", which was not yet true. An unverified claim in a
comment outlives the session that wrote it.

## Wave 8·1 — the documentation catch-up · 5 tasks, parallel

A catch-up wave in the shape of wave 6·0, which found that driving the config
from the command line was how you discovered it had never been implemented.
This one asked the equivalent question of the documentation, and the answer was
the same: the coverage looked complete and the useful half was missing.

**Built:** user-facing documentation for all 23 plugins and providers, a
`README.md` for APNs which had none, `docs/catalogue.md` as an index, and
`CLAUDE.md` rule 12 plus a step in the wave ritual so this does not rot again.

**The gap was audience, not volume.** Every component except one already had a
README, several of them long. But they were written for the next implementer —
canonical models, internal seams, locking, path resolution — and three plugin
READMEs had no "how to test" section at all. A reader arriving with "what is
this and how do I poke it" had to infer the answer from a description of a
canonical model. The fix was three required sections at the top of each file:
what it is, what it's for, and how to test it for real. Internals stayed, below.
They were worth keeping; they were just never the whole job.

**Four documented commands did not work, and every one was found by running
it.** The AS2 encrypt pipeline stripped one MIME header of three and produced a
200 with "illegal base64 data at input byte 7". `curl -T file
tftp://localhost:6969/...` hangs, because curl's TFTP client resolves localhost
to `::1` and tries UDP over IPv6 where nothing listens — `127.0.0.1` works.
`jq '.[].security'` against the AS2 readback returns null; the field is at
`.meta.security`. And the HL7 README told the reader the plugin could not
receive anything yet.

The TFTP one is the most instructive: the provider's own `Snippets()` template
already rendered `127.0.0.1` correctly. **The code was right and only the prose
was wrong, in two files.** A snippet that lives in a template beside its live
port is rendered against reality on every run and has a reason to stay true; the
same snippet pasted into a README has none. That is the argument for the rule,
and for preferring `tommy providers` over any static copy.

**Staleness does not need a wave to set in.** Three READMEs carried "not yet"
claims that had since become false: HL7's "there is no listener yet, the MLLP
provider is the next task" (true in wave 6a, wrong from 6b), push's "no
endpoints yet, not wired into plugins/all/all.go" (wrong since wave 7), and
AS2's "no provider exists yet" — **written earlier in the very same session**,
by the task that built the core before the task that built the provider. A
document can be obsolete by the end of the wave that wrote it, which is why the
ritual now checks the documentation of everything a wave touched at the end
rather than trusting what each task wrote as it went.

**A real regression, found by convergence.** Two agents independently reported
that the nested `test/integration` module no longer built: wave 8 added
`smallstep/pkcs7` to the root module, and that module depends on the root one,
so its `go.sum` went stale and *every* vendor-SDK test stopped compiling. It was
invisible to `make check` by design — `test/integration` is deliberately a
separate module so vendor SDKs stay out of tommy's `go.mod`, which also means
`./...` never reaches it. **Adding a dependency to the root module is a
two-module change**, and the second half is silent. Two independent reports on
the same wall remains this project's most reliable signal that something is
genuinely wrong.

**Parallelism needed port assignments, not just file ownership.** Five agents
running concurrently would each have booted servers on the same defaults, so
each was given an exclusive port range and told which well-known ports were
forbidden. No collisions, where previous waves lost real debugging time to a
stray server on 1025 or 2121. Exclusive *file* ownership is necessary and was
not sufficient once tasks started running the binary rather than only testing it.

**Documented honestly rather than plausibly.** NFS mounting needs root on every
OS and none was available, so those commands are marked unverified while what
*could* be checked was: a raw ONC RPC NULL call proving both MOUNT (100005) and
NFS (100003) answer on the single port, which is the actual claim that section
makes. One agent also wrote "could not be verified" caveats about the broken
integration module, which became stale the moment the module was fixed and had
to be rewritten — a caveat is a fact with a shelf life.

## Wave 9 — the event page and the link that reaches it · 1 session, 4 tasks

The first of the surface waves: eight plugins captured plenty, and nothing they
captured could be linked to.

**Built:** `/ui/events/{id}` as a page for one event rather than a deep link
into a list; a `url` field on every event the API returns, on both SSE streams
and on every plugin's read-back resource; and `X-Tommy-Event-URL` on ingress
responses, so the link reaches an application's own log without it calling the
API back.

**The route already existed, which was the problem.** `/ui/events/{id}` and
`/ui/mail/messages/{id}` both answered a browser with the whole tab and one row
selected. The pieces were all there and none of them joined up: a person had a
URL that showed a list, and a program had an id and no URL at all. Nothing here
was a missing feature so much as a missing *connection*, which is why the wave
is mostly small edits in many files rather than one new subsystem.

**Reusing the fragment route beat adding an interface.** A mail event has to
render as a mail, not as a JSON dump, and the obvious design was an optional
`EventRenderer` on `Plugin` — which every plugin would then have to implement,
and which could not easily reach the `Deps` a plugin only sees inside
`RegisterUI`. Instead the page dispatches an in-process request to the fragment
route the plugin already serves, with `HX-Request: true`, and embeds what comes
back. Zero plugin changes, and mail's sandboxed HTML frame came along
untouched. A tiny `http.ResponseWriter` recorder does the capture, because
`net/http/httptest` is a testing package and this runs in the shipped binary.
The client-side alternative — an `hx-get` div — was rejected on the grounds
that a link somebody pastes should render without JavaScript.

**The URL is not on `event.Event`, and that was the decision worth making
first.** Events are immutable and stored, so a URL on one would put a UI concern
into the store contract and be wrong the moment a port moved. It lives in an
envelope that embeds the event, defined once in `core/server/ui` next to the
function that builds the link, so the REST routes, the API stream and the UI
stream cannot drift into three shapes. The link is absolute — the caller is
usually talking to the ingress, on a port with no UI on it — and falls back to
the request host, then to a site-relative path.

**Plugin API handlers cannot learn where the UI is.** They are handed
`plugin.Deps`, which carries a `ProviderConfig` rather than the server's
addresses, and those addresses are not known until the listeners have bound.
Rather than widen `Deps`, the core stashes the origin in the request context
when it mounts a plugin's API, and `api.EventURL(r, id)` reads it — degrading to
a relative path for a handler mounted on a bare mux in a test. The same trick
the UI already used to pass its `Shell` around.

**The response header cost no provider changes**, which is why it was worth
doing at all. Ingress middleware puts a collector in the request context,
`Deps.Append` drops the id it just assigned into it, and the wrapper stamps the
header before the first write. Every HTTP provider already opens its handler
with `ctx := r.Context()` and passes that to `Append`, so all fifteen got it for
free; a provider that appends with a context of its own simply gets no link
rather than a wrong one. One request that fans out repeats the header once per
event, which is the honest rendering of Mailjet `Messages[]`.

**Deliberately not built:** the equivalent for listener providers. SMTP, FTP and
MLLP have no response that can carry a header, and the only alternative was a
log line per captured event — useful in local development, noise in CI, and
nobody asked for it. It is in the backlog with that reasoning rather than
shipped on a guess.

**A phantom test failure cost real time, again.** `plugins/files/providers/nfs`
started failing intermittently with `bind: address already in use` on the
*client's* source port. It was not the change: a `tommy` left running from a
manual check was still holding sockets, and killing the `go run` parent had left
its child alive. Five clean runs after killing by port. `CLAUDE.md` already says
to clean up background servers; what it did not say is that `go run` is not the
process you need to kill.

## Open items carried forward

- **Upstream:** the kleiner startup panic (Wave 0), which affects every project
  scaffolded from it.
- **Unverifiable shapes in the Mailjet fake:** the 401 error code (`mj-0015`,
  inferred from secondary sources) and the response to exhausted blob capacity,
  which is not a Mailjet concept at all (`tommy-capacity-exceeded`, commented as
  non-Mailjet).
- **SFTP auth default:** `NoClientAuth` is on, so `sftp any@host` connects with no
  prompt — which is what makes the cold-start snippet work with real OpenSSH. The
  consequence is that credentials are only presented, and therefore only recorded,
  when something is pinned.
- **Cosmetic inconsistency:** `mail.New(providers...)` and `files.New(providers...)`
  are variadic while `sms.New(sms.WithProviders(...))` uses an options pattern.
