# Lessons

What building tommy taught us. Written for whoever continues the work — the
specifics are in `docs/archive/history.md`; this is what generalises.

## On the design

**Separate stores earn their keep the moment a plugin becomes stateful.** Putting
payload bytes in a blob store rather than inline in events looked like tidiness
when only mail existed. It became load-bearing with `files`: the event log is
history and gets evicted, the filesystem is state and must not. A file stays
downloadable long after its upload event is gone. If the two had shared a
lifetime, the `files` plugin would have needed a different core.

**Name a thing for what it holds, not for the first way you reach it.** The
plugin was going to be called `ftp` until SFTP appeared. SFTP is an SSH subsystem
and FTPS is a third thing again, so `ftp` would have been wrong within one wave.
`files` fits FTP, SFTP, and later TFTP, NFS and SMB. The test that a plugin is
drawn at the right level: can two sibling providers write the same canonical model
and share one view? For `files`, a `curl` FTP upload downloads over OpenSSH `sftp`.

**Keep relations out of a flat store.** Slack threads are a parent/child relation
and the event store has none. Deriving channels and threads at render time kept
the core simple and put the logic in the one plugin that needs it. The cost is
recomputation; the benefit is that no other plugin pays for a concept it does not
have.

**Design the fallback before the fidelity.** Chat cards render as text plus a JSON
inspector, and rich rendering is a seam that returns `false` for anything it does
not handle. That means partial coverage ships safely, and rendering fidelity never
blocks message capture. The same shape would suit any "make this pretty later"
feature.

**A discoverability surface has to be a contract member, not a convention.**
`Description()` and `Snippets()` are on the interface and enforced by a conformance
test, because a fake nobody can figure out how to poke is worthless, and
conventions rot by the third provider. Rendering snippets as templates over the
live addresses matters as much: a snippet with a hardcoded port is wrong the
moment someone passes `--in-port`.

**Driving a config from the command line is how you find out it was never
implemented.** `ProviderConfig.Port` promised every HTTP provider a listener of
its own in `tommy.toml`, in the field's doc comment, and in a `Validate` that
range- and collision-checked the value. Nothing bound it. Three waves of TOML
readers believed the documentation because validation made the setting look
alive; the gap surfaced the first time someone tried to reach the setting through
a flag and then checked whether anything answered on the port. A config key that
is parsed and validated but never read by the thing it configures is
indistinguishable from a working one until something tries to use it end to end.

**Make an ignored input an error, not a no-op.** Two variants of the same bug
showed up in one wave: a flag naming a provider that `--enabled-providers` had
excluded, and cobra accepting a stray positional argument because no command set
`Args`. Both silently did nothing, and the second is what left a server holding
1025, 8811 and 8822 for nine hours after `tommy mail help` was typed instead of
`tommy mail --help`. Silence is the expensive answer: the first behaves as though
you configured something you did not, and the second as though you asked for help
and got a daemon.

**A contract written against one transport is only proven when a different one
arrives.** `ListenerProvider` was designed against three TCP providers (SMTP, FTP,
SFTP). TFTP, the first UDP provider, needed no change to it: the core only starts
`Listen` in a goroutine and waits for it to return, and `net.ListenPacket` plus
`AddressableProvider.Addr` fit as they stand. The interface was accidentally right
because it never described a connection — worth remembering as the cheap test of
any transport abstraction before NFS, SNMP or MLLP arrive.

**A plugin that can never receive anything should not ship, and the conformance
test should say so.** The HL7 core landed a wave before its only provider, so it
was deliberately left out of the shared wiring: `plugintest` rejects a plugin with
no providers. The related discovery is that `Snippets()` belongs to `Provider`,
not `Plugin`, so a core genuinely *cannot* advertise a listener that does not
exist yet. Both are the contract refusing to let a half-built thing look finished.

**Recover and record, don't fail, when a later stage needs the reason.** The HL7
parser fails on exactly one input — an empty message — and otherwise returns a
message carrying coded issues. The consumer is an `ACK` deciding between `AA`,
`AE` and `AR`, which needs a parsed message *and* a reason; an `error` would have
thrown away the half that makes the decision possible. Where a parser sits
upstream of a protocol reply, the error type is part of the protocol design.

## On the protocols

**Verify wire formats against live vendor documentation, never from memory.**
Every wave that did this found something wrong with the plan: Mailjet issues an id
per recipient and reports per-message failures inside a 200; Twilio quotes
`num_segments` as a string and nulls empty fields; SendGrid rejects two reply-to
forms set together; `webhookb2` is the legacy connector shape, not Power Automate.
Model knowledge of third-party APIs is confidently wrong in exactly the details
that decide whether an official SDK can parse the response.

**Test protocol servers with a real client over a socket.** The single best bug of
the project — ftpserverlib defaulting to ASCII transfer type and silently
rewriting line endings on download — is invisible to a mocked driver test and
obvious the first time `curl` fetches a file back. Anything with a wire format
deserves a real client: `curl`, OpenSSH `sftp`, stdlib `net/smtp`, the vendor's
own SDK.

**Drive the official SDK, not just the wire format.** Hand-built requests prove the
fake matches what you *believe* the format to be. The vendor's client proves the
fake is *usable*. That is what catches quoted-string numbers and null-vs-empty,
and it is what found that `mailjet-apiv3-go` builds its URL as `base + ".1/send"`,
so the base must already end in `/v3`.

**Be lenient where the real service is strict, and say so.** Modern Slack rejects
webhook `channel`/`username` overrides; tommy accepts them. A testing fake that
mirrors production's rejections is less useful than one that accepts what people
send. Document each divergence rather than letting it be silent.

**Refuse to fake what you cannot back.** Slack's `chat.update`/`chat.delete` were
skipped because events are immutable and threads are derived — they could only
have answered `ok` while nothing changed. Two correct routes beat four shaky ones.

**A test that goes through a well-behaved client can prove nothing about
hostile input.** The NFS path-escape tests had to be driven with raw RPC, because
the client library `path.Clean`s every path before it reaches the wire — so a
climbing lookup driven through the client never actually tests the server. The
same is true of any protocol whose reference client normalises: to test what the
server does with a hostile request, you have to be able to *send* one, which
usually means dropping below the client library.

**Handle-based protocols move the path check, they do not remove it.** NFS
addresses files by opaque handle rather than path, which looks like it sidesteps
a traversal invariant and does not. Minting random handles that encode no path,
treating an unknown one as stale, and re-resolving the components behind a minted
handle through the same one gate on every operation is what keeps the invariant —
the gate stays in one place, the lookup just arrives differently.

**When a library's convenience wrapper does not fit, look one layer down before
giving up on the library.** gosnmp ships a `TrapListener` that owns its socket and
never reveals the port it bound, which is incompatible with a discovery surface
that renders every snippet against the address a provider actually bound. The
exported marshal/unmarshal functions underneath it were exactly right. The same
question is worth asking of any library whose top-level helper assumes it owns the
process.

**Silent acceptance is the worst failure a capture tool has.** A fake that
answers 200 and quietly drops a field it did not recognise is worse than one that
rejects the request, because the user has no signal at all — they see a success
and an incomplete capture, and conclude their own code is at fault. This surfaced
as an FCM provider that took only the camelCase spelling of a proto3 field while
the real API accepts both, and it was found by posting both forms at the running
binary and diffing the captures, not by reading the code.

**A vendor's canonical spelling is not always its only accepted one.** Google's
discovery documents list lowerCamelCase because that is the canonical *output*
name; proto3's JSON mapping requires parsers to accept the original snake_case
field name too. Reading one and inferring the other is rejected is an easy and
expensive mistake — and the follow-on temptation, normalising key names
everywhere, will corrupt caller-owned data blocks. Normalise the keys you know;
never touch the ones that belong to the user.

**A deprecation in a dependency you already have can delete a planned task.**
Wave 7 was sequenced around one agent owning `go.mod` to add
`golang.org/x/net/http2/h2c`; the package turned out to be deprecated in the
version already present, with the standard library having absorbed the feature.
Checking what is already in the module graph, and whether the ecosystem has moved
on, is worth doing before planning a wave around a dependency.

## On the runtime

**A graceful shutdown must close connections that never asked for anything.**
`http.Server.Shutdown` will not call itself quiescent while any connection is in
`StateNew`, and only writes one off after five seconds — longer than any
shutdown budget tommy sets. Clients hand over such connections as a matter of
course: Go's `http.Transport` dials a spare while a request waits for an idle
connection, and browsers preconnect. The loser is parked in a pool having sent
no bytes, and one of them was enough to make every shutdown burn its whole
timeout and report failure. The server now tracks connections that have not
carried a request and closes them itself, after a short grace so one whose
first request is still arriving still gets served.

**A Go-version matrix in CI earns its cost the first time it disagrees.** That
shutdown weakness was two waves old and silent. It surfaced as `--- FAIL:
TestDeleteEvents … shutdown: ui: context deadline exceeded` on Go 1.27 while the
oldstable job stayed green — a client-side change in how eagerly connections are
dialled, exposing a server-side bug that had always been there. When one matrix
leg fails and another passes, the difference between the legs is the lead, and
the bug is usually still yours. Reproducing it meant fetching the exact
toolchain (`GOTOOLCHAIN=go1.27.1 go test`) and squeezing the scheduler with
`GOMAXPROCS=1`, which turned a CI-only flake into a local one that failed every
third run. Both are cheap; reach for them before calling something flaky.

## On orchestrating agents

**Exclusive file ownership is what makes parallelism safe.** Every wave ran
multiple agents concurrently with no merge conflicts, because each owned a
directory and was told explicitly what not to touch. The one shared file
(`plugins/all/all.go`) is always the coordinator's.

**The coordinator does all the git.** Subagents run no git commands at all. Two
agents committing into one worktree race the index, and the coordinator wants to
group work into coherent commits anyway.

**Only one agent at a time may touch `go.mod`, and dependencies must land with the
code that imports them.** Pre-adding dependencies so that two agents could run in
parallel failed: unused requires do not survive `go mod tidy`, they were dropped,
and both agents hit build failures and worked around it. Either stagger the
dependency-adding tasks or give one agent ownership.

**"Report contract gaps, don't patch around them" works, and the convergence
signal is reliable.** Nearly every real core gap was found by an agent that
reported rather than worked around. More usefully: when *two or three agents
independently hit the same wall*, it is genuinely a design flaw, not one agent
misreading. That happened four times — the shell accessor, the listener address,
the how-to-test open flag, the ephemeral port pinning — and each was worth fixing
in core rather than in three places.

**Check what is actually missing before dispatching.** The Wave 3 UI tasks were
written months-equivalent earlier and half of what they specified had already been
built by the plugin cores. Auditing the running server first turned two vague
"polish" tasks into one precise, real gap (the missing how-to-test panel).

**Verify agent reports; they are accurate but not omniscient.** Reports have been
honest, but several attributed a failure to the wrong cause — usually a sibling
agent's in-flight code, twice a **stray server process the coordinator itself had
left running** holding 1025 or 2121. Re-run the gate yourself, and exercise the
specific claim.

**Match the model to where the risk is, not to the size of the task.** Contract-
defining work (core, plugin models) and subtle parsing (MIME, SSH host keys) went
to the stronger model; well-specified translation against a fixed contract (three
of the four vendor providers, CI config, UI polish) went to the cheaper one and
came back with equal rigour — the Sonnet agents verified action versions against
GitHub's API and downloaded a real GoReleaser to validate a migration.

**When an agent is interrupted, the first move is always to read the disk.**
Three kills across two waves — two rate limits, one stop by the user — and the
handling was identical each time: look at what actually landed, then tell the
agent explicitly what is and is not there. An agent resumed with nothing on disk
will hunt for partial work that does not exist; one resumed after writing a lot
will re-guess what is missing. And when an agent cannot be resumed at all, its
unfinished half is ordinary work — the APNs implementation was complete and
untested, so the tests got written by hand rather than by spawning a replacement.

**Interruptions are cheap if the work is on disk — but check whether it is.**
Machine sleep killed five agent runs in one wave; a session rate limit killed two
more in another, that time inside their first few tool calls, before either had
written anything. The handling is the same either way and takes one round trip:
look at what actually landed, tell the resumed agent explicitly what is and is
not there, and let it continue rather than re-briefing it from scratch. Telling an
agent to "resume" when nothing was written sends it hunting for partial work that
does not exist.

**Interruptions are cheap if the work is on disk.** Machine sleep killed five
agent runs. Resuming the same agent with its transcript intact — plus an explicit
list of what was still missing — cost one round trip each and lost nothing,
because file writes had already landed. Checkpoint-committing a wave in progress
is worth it for the same reason.

## Small things worth remembering

- A `net/http.ServeMux` wildcard must occupy a whole path segment. `{Sid}.json`
  panics at registration; capture the segment and trim the suffix.
- `path.Clean("/" + name)` clamps `..` at the root, which is exactly chroot
  semantics — but normalise backslashes first or `..\..` slips past on the way in.
- Never bind a well-known port in a test. 1025 collides with a real mail catcher,
  and on macOS a system process may hold it as an outbound source port.
- Go's ephemeral-port trick (`:0`) only helps if *every* listener honours it,
  including ones that fall back to their own package default.
- Substring-grepping a rendered page for `<script` proves nothing: the page has
  its own scripts and escaped content will not match. Parse the markup.
- A cobra command with no subcommands and no `Args` validator accepts any
  positional argument and ignores it, so `tommy mail help` runs the command
  rather than printing help. `cobra.NoArgs` on every leaf command.
- `http.Server.ConnState` is the only way to see a connection that has not sent
  a request yet; nothing else in the API exposes one.
- Before blaming a port collision on "something else on this machine", check
  the age and command line of what holds it (`lsof -nP -iTCP -sTCP:LISTEN`,
  then `ps -o lstart,command -p <pid>`). It is often a previous session's own
  stray server, not a real mail catcher.
- Wiring a new provider into `plugins/all/all.go` is the coordinator's job and is
  easy to forget: the provider's own tests all pass while `tommy providers
  <plugin>/<provider>` still reports no such provider, because nothing ships it.
  A plugin's own `Description()` usually needs updating in the same breath.
