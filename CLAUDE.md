# CLAUDE.md

Instructions for working in this repository.

## What tommy is

A single binary that stands in for services an application talks to but which are
awkward to run locally — mail providers, SMS gateways, file transfer, chat
webhooks, EDI trading partners — and shows you exactly what your code sent.
Think mailcatcher, but for more than mail.

It **captures and displays what was sent**, and answers with whatever the
protocol requires so the client proceeds. It does **not** simulate scenarios,
drive inbound traffic, or make policy decisions. That boundary decides what
belongs here; see `docs/implementation-plan.md` §2.

## Commands

```bash
make check          # everything CI runs: gofmt, vet, lint, build, test -race
make test           # go test -race -coverprofile=coverage.out ./...
make lint           # golangci-lint run ./...
go run . serve      # boot with defaults: UI :8811, ingress :8822, smtp :1025, ftp :2121, sftp :2222
go run . providers  # every provider's description, endpoints and runnable snippets
go run . openapi    # the OpenAPI 3.1 description of the events API
make openapi        # regenerate docs/openapi.json; required whenever an events route changes
```

Set `TOMMY_NO_UPDATE_CHECK=1` when running the binary in tests, CI or scripts.

`test/integration` is a **separate Go module** so the vendor SDKs it drives never
enter tommy's `go.mod`. It is not covered by the root `./...`:

```bash
cd test/integration && go test -tags integration ./...
```

It `replace`s the root module, so **any wave that adds a dependency to the root
`go.mod` must `go mod tidy` this module in the same commit** — the new package
becomes a transitive requirement here too, and CI's integration leg is the only
one that notices. Verify with `go test`, never `go build`: the tests reach the
providers through `plugins/all` from a `_test.go` file, so `go build` never
compiles the import that fails.

## Where things are

| Path | What |
|---|---|
| `docs/plan.md` | The original product brief. Requirements, not design. |
| `docs/contracts.md` | **The core interfaces as built. Authoritative.** Read this before writing a plugin or provider. |
| `docs/implementation-plan.md` | Forward-looking plan: remaining waves, task breakdown, protocol roadmap. |
| `docs/archive/history.md` | What was built and why, wave by wave, with the decisions that changed under contact with real code. |
| `docs/lessons.md` | What this codebase taught us. Read before orchestrating more work. |
| `docs/clients.md` | Pointing official vendor SDKs at tommy. |
| `docs/catalogue.md` | **Index of every plugin and provider**, what each is for, and a link to its own README. Start here when asking whether tommy covers something. |
| `docs/openapi.json` | The OpenAPI 3.1 description of the **events API**. **Generated — never edit it**; run `make openapi`. |
| `core/` | Event, store, blob, plugin contracts, config, server (ui/api/ingress), testutil. |
| `plugins/` | One directory per content type; providers nested under each. |
| `plugins/all/all.go` | The single shared wiring file. Every plugin and provider is registered here explicitly. |
| `clienthelp/` | A stdlib-only `http.RoundTripper` for pointing SDKs at tommy. |

## Architecture in one paragraph

Three listeners in one process. A path-routed **ingress** mux hosts the fake
vendor HTTP APIs; protocol providers (SMTP, FTP, SFTP) get their own listeners.
Everything lands in an in-memory **event store** (ring buffer with pub/sub) with
payload bytes in a separate **blob store**, so retention of the two can differ.
A **UI** and a **REST + SSE API** read from both. A *plugin* owns a content type
(canonical model, API routes, UI tab); a *provider* translates one vendor's wire
format into that model. Providers never import each other — that is what lets
them be built in parallel.

## Rules for plugin and provider code

1. **Accept any credentials by default.** Parse and record them into `Event.Meta`;
   never reject unless config pins an expected value. A fake that 401s is useless.
2. **Respond with the vendor's real response shape** — status, headers, body — so
   official SDKs work unmodified. Verify against **live vendor documentation**,
   never from memory. This has caught real errors repeatedly. The one exception
   is `X-Tommy-Event-URL`, which the *ingress* adds to every response naming the
   page of what it captured; SDKs ignore unknown response headers, and no
   provider writes it — see rule 4.
3. **One request may produce several events.** Mailjet `Messages[]`, SendGrid
   `personalizations[]` both fan out. One event per delivered message.
4. **Always populate `Raw`** with the untouched request, and **append with the
   request's own context** (`d.Append(r.Context(), ev)`) — besides cancellation,
   that is how the new event's id reaches the collector behind
   `X-Tommy-Event-URL`. Appending with a context of your own still captures; the
   caller just gets no link back.
5. **Read-back endpoints serve from the store**, so an SDK that writes then fetches
   sees its own write.
6. **Ship a `Description()` and at least one working `Snippet()`.** Snippets are Go
   templates over `SnippetCtx` — use `{{.IngressURL}}` / `{{.Addr "files" "ftp"}}`,
   never a hardcoded port. Enforced by `plugintest.Conformance`.
7. **Every mounted route must be declared in `Endpoints()` and vice versa.** A
   mismatch fails conformance *and* server startup.
8. **Never import another provider's package.**
9. **Bytes go in the blob store**, never inline in an event.
10. **Keep the CLI level with the config.** Anything expressible in `tommy.toml`
    must be reachable from the command line. A new plugin needs its own
    `tommy <plugin>` subcommand; a new provider must be selectable through that
    command's `--enabled-providers`, and any provider option worth setting needs a
    flag. A plugin that can only be configured through a file is half-delivered -
    the single-plugin shortcut is how most people will actually run tommy.
    `cmd/mail.go` is the pattern and the shared helpers already exist.
11. **Registration may validate, but it may not create.** `RegisterIngress` runs
    for anything that merely *builds* a server, `plugintest.Conformance`
    included — so a provider that generates a key or a certificate there
    generates it during `make check`, in the user's own config directory. Read a
    configured path eagerly, because a path that does not resolve is a startup
    complaint; defer *creating* anything to first genuine use. Any such path
    must also be configurable: tommy may run in a cluster that already has its
    own CA, and someone running a different plugin must never meet a credential
    at all. `plugins/as2/identity.go` is the worked example.
12. **Ship user-facing documentation, not just implementation notes.** Every
    plugin and every provider carries a `README.md` that opens with these three
    sections, in this order, before any internals:

    - **What it is** — what real service this stands in for, and what it
      captures. One short paragraph.
    - **What it's for** — the situation in which somebody reaches for this.
      Concrete ("your app sends order confirmations through SendGrid and you
      want to see them in CI"), never a restatement of the name.
    - **How to test it for real** — runnable commands, from a cold start, that
      drive the actual thing: the vendor's own SDK or CLI where one exists,
      otherwise `curl`, `openssl`, `snmptrap`, OpenSSH `sftp`. **Every command
      must have been executed against a running tommy**, because a snippet
      nobody ran is a guess — this project has shipped two that could not work.

    Internals go below, and are still worth writing. The failure mode this
    rule exists to prevent is documentation aimed at the next implementer
    while the next *user* is never told what the thing is for: the coverage
    looks complete and the useful half is missing.

13. **The OpenAPI description is generated, never edited.** `docs/openapi.json`
    describes the **events API** — `/events`, `/events/{id}`, `/events/stream`,
    `/blobs/{id}` — which is the surface every consumer of tommy programs
    against, whatever it is capturing. It is the output of `tommy openapi`, so a
    change to one of those routes or to a Go type it serves is finished only
    when `make openapi` has been run and the result committed. A test fails
    otherwise, and it names the line. Deliberately not in it: the fake vendor
    endpoints (the vendors' specifications, not tommy's), each plugin's own
    read-back routes (documented in that plugin's README), and the operational
    routes `/health` and `/plugins`.

## Security invariants — do not weaken these

- **Untrusted content never enters the page DOM.** A captured HTML mail body is
  served from its own API route under a restrictive CSP and framed in a fully
  sandboxed iframe. Tests assert the markup is absent from the page.
- **All captured text is interpolated as plain strings** through `html/template`,
  never `template.HTML`. Message bodies, author names, filenames, subjects.
- **`plugins/chat/ui/blocks` is the one exception and the one danger.** Its output
  is injected unescaped because emitting markup is the point. Everything it lifts
  from a payload must be escaped, every URL checked against a scheme allowlist
  before reaching an `href`/`src`, and recursion bounded. Its hostile-input suite
  asserts against parsed markup, not substrings.
- **Path traversal is rejected in exactly one place** (`files.VFS.Resolve`). No
  provider may interpret a path itself.

## Testing conventions

- `core/testutil.Start(t, cfg, plugins...)` boots the real server on ephemeral
  ports. Pass `nil` config and listener providers are pinned to ephemeral too.
- **Never bind a well-known port in a test.** 1025, 2121, 2222 collide with a real
  mail catcher, another test binary, or a stray server.
- Provider tests are table-driven over golden fixtures in `testdata/`, asserting
  **both** the canonical model produced **and** the exact HTTP response.
- Protocol providers must be tested with a **real client over a socket**, not a
  mocked driver. This is what caught ftpserverlib silently corrupting downloads.
- Call `plugintest.Conformance` from every plugin and provider package.

## Finishing a wave — required, not optional

**A wave is not finished when its code lands. It is finished when the documents
tell the truth again.** Do this before reporting the wave complete, in the same
session that built it — a later session cannot reconstruct what an agent
reported, and stale docs cost the next session more than they cost you.

1. **`docs/implementation-plan.md`** — delete the wave's section. Update the
   status table at the top and the branch line. If what you learned changes a
   later wave (a dependency discovered, an ordering constraint, a task that
   turned out to be unnecessary), edit that wave now while you still know why.
2. **`docs/archive/history.md`** — append the wave, past tense, oldest-first
   order. Record what was **built**, and then the part that is actually worth the
   space: what turned out to be wrong, what a real client or a live vendor
   document contradicted, which contracts had to change and why, and any
   deliberate non-implementation with its reasoning. Do not inventory files; git
   has that.
3. **`docs/contracts.md`** — if any core interface changed, update it. This is
   the document every future agent is pointed at as authoritative, so drift here
   is the most expensive kind. It went stale once already, missing four additions
   made after it was written.
4. **`docs/lessons.md`** — add anything that generalises beyond this wave. A
   finding that would change how someone works, not a fact about one provider.
5. **`CLAUDE.md`** — update if a rule, invariant, command or convention changed.
6. **`docs/openapi.json`** — run `make openapi` and commit the result if an
   events route, or any Go type one of them serves, changed. A test fails
   otherwise, so this is a matter of when you find out, not whether.
7. **Update the documentation of every plugin and provider the wave touched** —
   rule 12's three sections, plus anything the wave changed underneath them. A
   new plugin or provider needs its `README.md` written in the same wave, and a
   changed endpoint, flag or default needs it corrected there too. This is the
   step most easily skipped, because the code works without it and nothing
   fails; it is also the one a user notices first.
8. **Commit the documentation** as its own commit, so the history shows the plan
   moving in step with the code.
9. **Hand the branch over.** Report what landed and stop; merging is the user's
   call. The next wave starts from a fresh branch off whatever they merged.

A useful test for step 2: if a new session read only your archive entry, would it
avoid repeating the mistakes this wave made? If not, it is an inventory, not a
history.

## Orchestrating more work with subagents

This project was built by a coordinating session dispatching subagents per task.
`docs/implementation-plan.md` carries the remaining waves already split into
independently-ownable chunks. If you continue that way:

- **Give each agent exclusive file ownership** and name what it must not touch.
  Disjoint directories are what makes parallelism safe.
- **Subagents must run no git commands.** The coordinator commits, in
  self-contained chunks, so two agents never race the index.
- **Only one agent at a time may modify `go.mod`,** and it must add its
  dependencies *with* the code that imports them. Pre-staging dependencies does
  not work: unused requires are dropped by any `go mod tidy`.
- **Point agents at `docs/contracts.md`** and an existing worked example
  (`plugins/mail/providers/smtp` for listener providers, `plugins/sms/providers/twilio`
  for HTTP ones, `core/testutil/fakeplugin` for the plugin contract).
- **Tell agents to report contract gaps rather than patch around them.** Most real
  core gaps in this project were found this way, and several were found
  independently by two or three agents at once — that convergence is the reliable
  signal that something is genuinely wrong.
- **Verify agent reports independently.** Re-run the gate, and exercise the claim
  the agent makes. Reports have been accurate but occasionally mis-attribute a
  failure (a stray process, a sibling's in-flight code).
- **Clean up background servers you start.** Leftover processes holding 1025/2121
  cost two different agents real debugging time in this session.
- **Start each wave on its own branch**, named for what it builds
  (`feat/hl7-plugin`, `feat/push-plugin`). A wave is a reviewable unit: one
  branch keeps its diff readable and lets it be inspected, or abandoned, without
  disturbing anything else. Do not run two waves on one branch.
