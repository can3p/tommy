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

**Registration may validate, but it may not create.** A provider's
`RegisterIngress` runs for anything that merely *builds* a server — every
conformance test included — so anything that generates a credential there
generates it during `make check`. The AS2 identity did exactly that and left a
real private key in the user's own config directory. Split the two halves:
reading a configured path is eager, because a path that does not resolve is a
startup complaint the operator wants immediately; *creating* anything waits for
first genuine use. The same split will apply to any future feature that mints a
key, a certificate or a file.

**A generated default has to be overridable, and its cost has to fall only on
the people who enabled it.** Auto-generating a self-signed certificate is a fine
default, but the path to an existing one must be configurable, because tommy may
run in a container inside a cluster that already has its own CA and an operator
needs it to fit the surrounding PKI. And someone running only the `mail` plugin
must never encounter a certificate at all. The core's own shape enforced the
second half here: only providers receive a `ProviderConfig`, so a
plugin-level credential has to be configured *by* a provider, and a disabled
provider therefore cannot cause one to exist.

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

**Two specifications can contradict each other, and the answer is not to pick
one and discard the other's data.** RFC 4130 and RFC 5402 give incompatible
rules for the same MIC — "without the MIME headers" versus "including all MIME
header fields" — and both are quoted verbatim from the documents. Standards
Track beats Informational, so one wins; but the losing digest is computed anyway
and kept beside the winner, because the person reading it is chasing a mismatch
with a trading partner and needs both numbers rather than our verdict. Where a
conflict is genuinely unresolvable, surfacing both readings is more useful than
adjudicating.

**A tool's own output format is not necessarily the protocol's.** `openssl cms
-encrypt -outform SMIME` writes MIME headers above its body; in AS2 those belong
in the HTTP request, so the body must be bare base64. The obvious fix —
stripping the first line — removes one header of three and leaves a body that
fails to decode, answered with a 200 that looks close enough to success to waste
an afternoon. Ask for the raw form (`-outform DER`) and encode it yourself
rather than editing a tool's framing off the top.

**The reference implementation you test against has quirks of its own, and they
are load-bearing.** OpenSSL writes *mixed* line endings — bare LF for outer
headers and multipart delimiters, CRLF for part headers and bodies — so a
strictly correct RFC 2046 splitter finds zero parts in a message OpenSSL just
produced. It also writes `micalg="sha-256"`, a spelling the RFC's grammar has no
room for, and Homebrew's build ships without zlib so `cms -compress` fails
outright. Budget for the independent implementation being non-conforming in
small ways; that is not a reason to stop using it, it is most of why it is worth
using.

**A deprecation in a dependency you already have can delete a planned task.**
Wave 7 was sequenced around one agent owning `go.mod` to add
`golang.org/x/net/http2/h2c`; the package turned out to be deprecated in the
version already present, with the standard library having absorbed the feature.
Checking what is already in the module graph, and whether the ecosystem has moved
on, is worth doing before planning a wave around a dependency.

## On the runtime

**A route two clients disagree about is better than two routes.** `/ui/events/{id}`
serves htmx a fragment and a browser a page, off one registration, because htmx
announces itself with `HX-Request`. The alternative — a second URL for the page —
would have meant every link in the product choosing between them, and every
plugin's list rows choosing again. One canonical URL per thing is worth some
branching inside the handler.

**Before adding an interface to a plugin contract, check whether the plugin
already answers the question over HTTP.** The event page needs each plugin's own
rendering of an event, which looked like an optional `EventRenderer` on
`Plugin`: eight implementations, and awkward because the renderer needs the
`Deps` a plugin only sees inside `RegisterUI`. But every plugin already serves
that rendering as an htmx fragment, so the page dispatches an in-process request
to the mounted handler and embeds the result. No interface, no plugin changes,
and a plugin that answers with nothing degrades to the generic view. The cost is
one small `http.ResponseWriter` recorder — `net/http/httptest` is a testing
package and has no business in a shipped binary — and a standing requirement
that the fragment route stay side-effect free.

**A cross-cutting field is cheaper to add through the request context than
through `Deps`.** Plugin API handlers needed the server's UI origin, which
`Deps` does not carry and cannot: `Deps` holds a `ProviderConfig`, and the
addresses are not known until listeners bind. Middleware puts the value in the
request context and an exported helper reads it, degrading to a sensible default
when it is absent — which is also what makes a handler mounted on a bare mux in
a test still work. The UI had already used the same trick for its `Shell`.


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

## On documentation

**Generate a description from the types, and check the generated copy into the
repository.** Hand-written schemas rot in the one way nothing catches: a field
added to a response struct is simply missing from the document, and no test can
assert about prose nobody wrote. Reflection over the response types costs about
150 lines and removes the failure mode entirely. The checked-in copy is then a
build product — which is exactly the arrangement that goes stale, so a test
regenerates it and fails with the command that fixes it.

**A generated document is not a valid document until a real validator says so.**
Running `npx @redocly/cli lint` over the generated OpenAPI file found two errors
that reading the specification had not: `{path...}` is Go's wildcard syntax and
not OpenAPI's, so several routes described a parameter no client would bind, and
an API with no authentication has to declare an empty `security` or a reader
assumes a scheme was forgotten. Both were invisible to every test in the repo,
because the tests knew only what the generator knew. Warnings that would need an
invented response to silence were left standing instead.

**One document per surface beats one document for everything.** The same
correction arrived twice: first that the OpenAPI description should be the
events API rather than every route under `/api/v1`, then that the plugin APIs
still deserved descriptions of their own. Both are true, and the resolution is
not a compromise between them - it is a document per audience. Somebody
asserting about mail wants seven paths in the mail document; somebody streaming
events wants six in the events document; nobody wants thirty-five in one.
Cutting the scope is only half the answer, and the half that gets forgotten is
asking what happened to what you cut.

**An interface that only some implementers need should be optional.** The first
version made `APIEndpoints()` a member of `Plugin`, so a plugin that mounts no
API had to implement it to say so, along with every test double. As an optional
interface it says the same thing better: implement it if you have routes, and
conformance fails you if you have routes and did not. The codebase already had
the pattern in `AddressableProvider`; it was worth copying rather than
re-deciding.

**Ask what a document is a contract *for* before deciding what goes in it.** The
OpenAPI description first covered every route under `/api/v1` — the events API,
the operational routes, and all 28 of the plugins' read-back routes — on the
reasoning that a description should be complete. It should not: a description is
for the surface somebody programs against, and here that is the events API,
which is the same whatever the plugin captured. The wide version needed a new
method on the `Plugin` interface to carry its prose; the narrow one needs an
unexported table beside the handlers. **A contract addition that exists only to
feed one document dies with that document's scope** — worth checking before
adding the method, not after.


**Ask who the reader is, not whether the file exists.** Every component but one
had a README, several of them long — and they were written for the next
implementer. Canonical models, internal seams, locking. Three plugins had no
"how to test" section at all. Coverage looked complete because the wrong
question was being asked of it. The three sections now required by `CLAUDE.md`
rule 12 — what it is, what it's for, how to test it for real — exist because the
middle one was never written down anywhere and the last one was optional.

**A snippet rendered from a template stays true; a snippet pasted into prose
does not.** The TFTP README told readers to use `tftp://localhost`, which hangs
— curl resolves localhost to `::1` and tries UDP over IPv6, where nothing
listens. The provider's own `Snippets()` already rendered `127.0.0.1` correctly,
because it is a template evaluated against the live configuration on every run.
The code was right and only the prose was wrong. Prefer the generated
discovery surface over any static copy of it, and treat a hand-written command
in a document as something that must be executed to be believed. Four of them
in this repo were not, and none of the four worked.

**Staleness does not need a wave to set in.** Three "not yet" claims had become
false, and the worst was written *in the same session* that falsified it: the
AS2 core's README said no provider existed, written by the task that built the
core before the task that built the provider. A document can be obsolete by the
end of the wave that produced it. Check the documentation of everything a wave
touched at the end, rather than trusting what each task wrote as it went.

**A caveat is a fact with a shelf life.** An agent honestly documented "this
command could not be verified, the module does not build" — accurate when
written, wrong within the hour, and now a false statement in a shipped
document. When a task reports a blocker and the blocker gets fixed, the prose
about it is part of the fix.

## On orchestrating agents

**Exclusive file ownership stops being sufficient once tasks run the binary.**
Five documentation agents in parallel would each have booted tommy on the same
default ports. Disjoint directories prevented every write conflict and would
have prevented none of the port collisions, so each agent was given an
exclusive port range and an explicit list of well-known ports never to bind.
Whenever concurrent tasks *execute* rather than only edit, the shared resource
to divide up is the machine, not just the tree.

**A dependency added to one module can break another one the gate never
builds.** `test/integration` is deliberately a separate module so vendor SDKs
stay out of tommy's `go.mod` — which also means `./...` does not reach it and
`make check` never compiles it. Adding `smallstep/pkcs7` to the root module for
the AS2 plugin left that module's `go.sum` stale and every vendor-SDK test
stopped building, silently, for a whole wave. Adding a dependency is a
two-module change here; run the nested suite by hand after touching `go.mod`.

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

- `kill`ing a `go run` leaves the built binary running and still holding the
  port. It cost an afternoon's worth of confusion twice now: once as "my change
  did not take effect" and once as an unrelated package failing with
  `bind: address already in use` on a *client* source port. Build to a temporary
  path and run that, or kill by port (`lsof -ti tcp:PORT | xargs kill`), and
  check the process is gone before concluding anything about a test.

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
- `os.UserConfigDir()` is `~/Library/Application Support` on macOS, not
  `~/.config`. Checking the wrong one and finding nothing proves nothing — it is
  how a stray private key went unnoticed for several steps in Wave 8.
- A snippet nobody has executed is a guess. Wave 8's plugin README shipped a
  cold-start command that could not work, and it was caught only because the
  next agent ran it rather than read it.
- An unverified claim in a code comment outlives the session that wrote it. An
  agent killed mid-task left a comment saying its snippets had been "run against
  a live tommy"; the testing phase had not happened yet.
- Wiring a new provider into `plugins/all/all.go` is the coordinator's job and is
  easy to forget: the provider's own tests all pass while `tommy providers
  <plugin>/<provider>` still reports no such provider, because nothing ships it.
  A plugin's own `Description()` usually needs updating in the same breath.
- A provider that adds a dependency to the root module must re-tidy
  `test/integration` in the same commit. That module `replace`s the root one, so
  the new package becomes a transitive requirement it has to record, and without
  it CI's integration leg fails on a missing `go.sum` entry while every other
  leg stays green. Four consecutive waves shipped this bug — tftp, nfs, snmp and
  as2 — because it is invisible from the root module and each was fixed by
  accident in a later wave rather than caught in its own.
- `go build` in `test/integration` does not prove that module compiles. The
  tests reach the providers through `plugins/all` from a `_test.go` file, so a
  missing requirement only surfaces under `go test -tags integration ./...`.
  That is the command to run when verifying it, and the one CI runs.
