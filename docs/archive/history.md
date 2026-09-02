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
