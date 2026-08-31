# Tommy — Implementation Plan

Derived from `docs/plan.md`. This document is the working contract for parallel
implementation: it defines the shared interfaces first, then splits the work into
waves of independent tasks with **strict file ownership** so multiple agents can
work concurrently without touching the same files.

## 0. Current state

The repository is bare scaffolding produced by [kleiner](https://github.com/can3p/kleiner):

- `main.go` → `cmd.Execute()`
- `cmd/root.go` — cobra root, kleiner build-info + update-notifier wiring
- `generated/buildinfo/` — version metadata (leave alone)
- `.goreleaser.yaml` — `CGO_ENABLED=0`, linux/darwin, plain `go build`
- **No `go.mod`, no `.github/`** — module path is `github.com/can3p/tommy` (per existing imports)

Everything below is new code.

## 1. Decisions

| Area | Decision | Consequence |
|---|---|---|
| UI | Go `html/template` + `go:embed` + htmx + SSE | No node toolchain; goreleaser stays a plain `go build`; UI is testable with `httptest` + `goquery` |
| Event storage | In-memory ring buffer behind a `Store` interface | No CGO, trivial reset between tests; persistence can be added later without touching plugins |
| Content storage | Separate `BlobStore` for attachments and uploaded files | Ring-buffer eviction must never silently delete a file a user uploaded over FTP (see §7) |
| Ingress | One shared HTTP ingress port, path-routed | Real provider API paths do not collide; matches the `--in-port 8822 --enabled-providers mailjet,sendgrid` example. Per-provider port override in TOML; SMTP/FTP get their own listeners |
| Routing | stdlib `net/http.ServeMux` (Go 1.22+ patterns) | `POST /2010-04-01/Accounts/{sid}/Messages.json` works without a router dependency |
| Deps | cobra, `pelletier/go-toml/v2`, `emersion/go-smtp`, later `fclairamb/ftpserverlib` + `pkg/sftp` + `x/crypto/ssh`, `PuerkitoBio/goquery` (tests only) | Stays CGO-free and single-binary |
| Discoverability | Every plugin and provider ships a description + runnable snippets, enforced by a conformance test | §6.4 — a fake you cannot figure out how to poke is useless |

## 2. Architecture

Three listeners, one process:

```
  provider SDK / curl ──► ingress mux  :8822   (fake Mailjet/SendGrid/Twilio APIs)
  ftp / smtp client   ──► own listeners :1025 :2121
                                │
                                ▼
                 Store (events, ring buffer + pub/sub)
                 BlobStore (attachment + file bytes)
                                │
                    ┌───────────┴───────────┐
                    ▼                       ▼
              UI server :8811         API server :8811 (or its own port)
              /ui/... + SSE           /api/v1/...
```

A **plugin** owns a content type (mail, sms, ftp, later push): a canonical model,
an API surface, a UI tab, and a set of **providers**. A **provider** imitates one
real vendor API or protocol and converts its wire format into the plugin's
canonical model. Providers never talk to each other and never share files — that
is what makes the work parallelizable.

### Directory layout

```
cmd/
  root.go            # exists
  serve.go           # tommy serve --config tommy.toml
  mail.go  sms.go    # single-plugin shortcuts
core/
  event/             # Event envelope, IDs, Summary, Raw
  store/             # Store interface
    memory/          # ring-buffer implementation + fan-out pub/sub
  blob/              # BlobStore interface + in-memory impl (size-capped)
  plugin/            # Plugin, Provider, ListenerProvider, Deps, Registry
  config/            # TOML + CLI config, defaults, validation
  server/
    ui/              # layout, tab registry, embedded static assets, SSE hub
    api/             # generic /api/v1 routes
    ingress/         # shared ingress mux + per-provider listener supervision
    lifecycle.go     # start/stop all listeners, graceful shutdown
  testutil/          # boot tommy in-process on ephemeral ports for tests
clienthelp/          # tiny helpers to point official SDKs at tommy (see §6)
plugins/
  all/all.go         # the ONLY shared wiring file — owned by the integration task
  mail/
    message.go  plugin.go  api.go  ui/
    providers/mailjet/  sendgrid/  smtp/
  sms/
    message.go  plugin.go  api.go  ui/
    providers/twilio/
  files/             # Wave 4 — design validated in §7, not built yet
    vfs.go  plugin.go  api.go  ui/
    providers/ftp/  sftp/
.github/
  workflows/ci.yml  workflows/release.yml
  dependabot.yml
```

## 3. Core contracts

These are the interfaces every task codes against. They are fixed by Wave 0 and
must not be changed unilaterally afterwards — if a provider needs a change, it is
raised, not patched in place.

```go
// core/event
type ID string

type Event struct {
    ID         ID
    Plugin     string         // "mail", "sms", "files"
    Provider   string         // "mailjet", "sendgrid", "smtp", "twilio", "ftp", "sftp"
    Type       string         // "mail.message", "sms.message", "files.upload", "files.mkdir"
    ReceivedAt time.Time
    Summary    Summary        // provider-agnostic listing data
    Meta       map[string]any // provider metadata (Mailjet CustomID, SendGrid categories, ...)
    Payload    any            // *mail.Message, *sms.Message — marshalled to JSON by the API
    Raw        Raw            // original request: method, path, headers, body
}

type Summary struct {
    From    string
    To      []string
    Title   string // subject / first line / file path
    Snippet string
}
```

`Type` is a free-form `"<plugin>.<resource>"` string, **not** an enum. A plugin
may emit several types and its UI/API must never assume it has only one — that is
what lets a provider grow new resources later (§6.3).

```go
// core/store
type Query struct {
    Plugin, Provider, Type, Search string
    Since                          time.Time
    Limit, Offset                  int
}

type Store interface {
    Append(ctx context.Context, e *event.Event) error   // assigns ID + ReceivedAt when empty
    List(ctx context.Context, q Query) ([]*event.Event, error) // newest first
    Get(ctx context.Context, id event.ID) (*event.Event, error)
    Delete(ctx context.Context, id event.ID) error
    Clear(ctx context.Context, plugin string) error     // "" clears everything
    Subscribe(ctx context.Context) <-chan *event.Event  // fan-out; drops for slow consumers
}
```

```go
// core/blob — bytes live here, never inline in an Event
type Ref struct {
    ID          string
    Size        int64
    ContentType string
    Filename    string
}

type BlobStore interface {
    Put(ctx context.Context, r io.Reader, meta Ref) (Ref, error)
    Open(ctx context.Context, id string) (io.ReadSeekCloser, Ref, error)
    Delete(ctx context.Context, id string) error
    Stat(ctx context.Context, id string) (Ref, error)
}
```

Mail attachments and FTP uploads both hold a `blob.Ref`, never `[]byte`. This
caps memory in one place, keeps event JSON small, and lets the API stream
downloads with correct `Content-Length` and range support.

```go
// core/plugin
type Plugin interface {
    Name() string        // "mail" — url segment
    Title() string       // "Mail" — UI tab label
    Description() string // one or two sentences: what this fakes and why
    Providers() []Provider
    RegisterAPI(mux *http.ServeMux, d Deps) // mounted under /api/v1/<name>/
    RegisterUI(mux *http.ServeMux, d Deps)  // mounted under /ui/<name>/
    Templates() fs.FS                       // embedded templates for the tab
}

type Provider interface {
    Name() string        // "mailjet"
    Plugin() string      // "mail"
    Description() string // one or two sentences: which real API, which parts
    Endpoints() []Endpoint // discovery + docs; see §6.3
    Snippets() []Snippet   // copy-paste manual tests; see §6.4
    RegisterIngress(mux *http.ServeMux, d Deps) // may mount any number of routes
}

// Providers that need their own listener (SMTP, FTP, SFTP) also implement:
type ListenerProvider interface {
    Provider
    Listen(ctx context.Context, d Deps) error // reads its own ports from d.Config; blocks until ctx done
}

type Endpoint struct {
    Method, Path, Description string
}

// Snippet.Code is a Go template rendered against the live runtime addresses,
// so a copied command always carries the ports this instance actually bound.
type Snippet struct {
    Title string // "Send an email with curl"
    Lang  string // "bash" | "go" | "python" — used for highlighting and grouping
    Code  string // template over SnippetCtx
}

type SnippetCtx struct {
    Host       string // "localhost"
    IngressURL string // "http://localhost:8822"
    UIURL      string // "http://localhost:8811"
    APIURL     string // "http://localhost:8811/api/v1"
    SMTPAddr   string // "localhost:1025"
    FTPAddr    string // "localhost:2121"
    SFTPAddr   string // "localhost:2222"
}

type Deps struct {
    Store  store.Store
    Blobs  blob.BlobStore
    Config ProviderConfig    // per-provider TOML section, decoded on demand
    Logger *slog.Logger
    Now    func() time.Time  // injectable for deterministic tests
    NewID  func() string     // injectable id generator
}
```

`Listen` takes no `addr`: FTP needs a control port *and* a passive port range,
SMTP may later want a STARTTLS port. Every listener provider reads its own
networking config from `d.Config` so the contract never has to change again.

Registration is **explicit**, not `init()` magic:

```go
// plugins/all/all.go
func Plugins() []plugin.Plugin { return []plugin.Plugin{mail.New(), sms.New()} }
```

### Rules every provider follows

1. **Accept any credentials by default.** Auth is parsed and recorded into `Meta`,
   never rejected — unless the TOML config pins an expected key, in which case a
   mismatch returns the vendor's real auth error shape.
2. **Respond with the vendor's real response shape** (status code, headers, body)
   so the official SDKs work unmodified against tommy.
3. **One request may produce several events** — Mailjet `Messages[]` and SendGrid
   `personalizations[]` both fan out. Append one event per resulting message.
4. **Always populate `Raw`** with the untouched request so the UI can show it.
5. **Read-back endpoints serve from the `Store`**, so an SDK that writes then
   fetches sees its own write.
6. **Ship a description and at least one working snippet** (§6.4).
7. **Never import another provider's package.**

## 4. Config

```toml
[ui]
port = 8811

[api]
port = 8811        # same listener as UI by default

[ingress]
port = 8822

[storage]
capacity  = 500     # events retained per plugin
blob_limit = "256MB" # total bytes held by the blob store

[plugins.mail]
enabled = true

[plugins.mail.providers.mailjet]
enabled = true
# port  = 9001     # optional dedicated listener instead of the shared ingress

[plugins.mail.providers.sendgrid]
enabled = true

[plugins.mail.providers.smtp]
enabled = true
port    = 1025

[plugins.sms]
enabled = true

[plugins.sms.providers.twilio]
enabled     = true
account_sid = "AC00000000000000000000000000000000"  # echoed back; any accepted if unset

# Wave 4
[plugins.files.providers.ftp]
enabled       = true
port          = 2121
passive_ports = "30000-30009"
passive_host  = "127.0.0.1"

[plugins.files.providers.sftp]
enabled       = true
port          = 2222
host_key_path = "~/.config/tommy/sftp_host_ed25519"  # generated on first run
# authorized_keys = "~/.ssh/authorized_keys"          # optional; any password accepted if unset
```

`tommy mail --ui-port 8811 --in-port 8822 --enabled-providers mailjet,sendgrid`
builds the equivalent `Config` struct in memory and runs the exact same code path
as `tommy serve --config`. There is one server bootstrap, never two.

## 5. HTTP surfaces

### Ingress (`:8822`)

| Provider | Route |
|---|---|
| Mailjet | `POST /v3.1/send` |
| SendGrid | `POST /v3/mail/send` |
| Twilio | `POST /2010-04-01/Accounts/{sid}/Messages.json` (+ `GET` list/fetch) |
| SMTP | own listener on `:1025` |

Route registration goes through a wrapper that **detects collisions at startup and
fails loudly**, naming both providers. Unmatched ingress paths return 404 with a
body listing the enabled providers and their endpoints — it is the single most
common misconfiguration and worth a good error.

### API (`/api/v1`)

Generic:
- `GET /health`
- `GET /plugins` → enabled plugins and providers with their descriptions, every
  `Endpoint` they mount, and their snippets **rendered against the live ports**
- `GET /events?plugin=&provider=&type=&search=&since=&limit=&offset=`
- `GET /events/stream` — SSE, also what the UI consumes
- `DELETE /events?plugin=` — clear

Mail:
- `GET /mail/messages`, `GET /mail/messages/{id}`
- `GET /mail/messages/{id}/html|text|raw`
- `GET /mail/messages/{id}/attachments/{idx}` (correct `Content-Type` + `Content-Disposition`)
- `DELETE /mail/messages`

SMS:
- `GET /sms/messages`, `GET /sms/messages/{id}`, `DELETE /sms/messages`

### UI (`/ui`)

Shell renders a tab bar from the plugin registry; each plugin owns its tab body
and its own partial routes. One SSE connection at the shell level; htmx swaps
list fragments on `sse:mail.message` / `sse:sms.message`.

Every tab has a **"How to test" panel** listing each enabled provider's
description and snippets with copy buttons, and an **empty state that shows those
snippets inline** — an empty tab is exactly when someone needs to know how to put
something in it. The shell provides the rendering partial so plugin tabs get this
by including one template.

## 6. Making the official SDKs work

Verified against the current clients. This is the difference between a toy and a
tool, so it gets its own deliverable: a `clienthelp/` package plus `docs/clients.md`.

### 6.1 Per-SDK findings

| SDK | Base URL override | How tommy is used |
|---|---|---|
| **mailjet-apiv3-go/v4** | ✅ First class | `mailjet.NewMailjetClient(pub, priv, "http://localhost:8822")`, or `client.SetBaseURL(...)` / `SetURL(...)` after construction |
| **sendgrid-go** | ✅ Via `GetRequest` | `req := sendgrid.GetRequest(key, "/v3/mail/send", "http://localhost:8822"); sendgrid.API(req)`. `NewSendClient` hardcodes the host, so `GetRequest` (or overriding the embedded `rest.Request.BaseURL`) is the documented path |
| **twilio-go** | ❌ **None** | `RequestHandler.BuildUrl` reparses and rebuilds the hostname as `product[.edge][.region].twilio.com`; `TWILIO_EDGE`/`TWILIO_REGION` are the only knobs and neither escapes `twilio.com`. The supported hook is `twilio.NewRestClientWithParams(twilio.ClientParams{Client: ...})`, which accepts a custom `client.Client` carrying a custom `*http.Client` |

### 6.2 What we ship

`clienthelp/` — a dependency-light package, importable by users' tests:

- `clienthelp.RoundTripper(baseURL string) http.RoundTripper` — rewrites
  scheme+host on every outbound request to point at tommy. Six lines, no TLS
  needed, and it is the answer for **any** SDK that lets you inject an
  `*http.Client` even when it will not let you set a base URL.
- `clienthelp/twiliohelp.NewClient(accountSid, authToken, baseURL) *twilio.RestClient`
  — wraps the above in the `ClientParams{Client: ...}` shape twilio-go expects.
- `docs/clients.md` — copy-pasteable snippets for Go, plus the non-Go story.

**Non-Go SDKs.** Most Twilio helper libraries share this limitation. Two documented
routes, in order of preference: (a) the library's custom-HTTP-client hook, where
one exists; (b) a hosts-file entry for `api.twilio.com` pointing at localhost plus
tommy's optional `--tls` mode serving a self-signed cert the test environment
trusts. Both are documented, neither is required — the raw HTTP API always works,
and that is what most test suites actually exercise.

An optional `--tls` / `[tls]` config block (self-signed cert generated on first
run, written next to the config so it can be trusted once) is **Wave 4 scope**,
listed here because it is the reason the ingress server must be constructed so
that adding TLS later is a config flag, not a refactor.

### 6.3 Providers must be open to more APIs

Each provider currently implements one endpoint, but the contract already assumes
it will grow (Twilio message read/list, Verify, Lookups; SendGrid suppressions):

- `RegisterIngress` receives the mux and may mount **any number** of routes.
- `Endpoints()` is the discovery surface — every mounted route is declared there,
  surfaced on `/api/v1/plugins` and in the UI as "what this fake supports". Adding
  an endpoint means adding a route and an `Endpoint` entry, nothing else.
- A provider owns a **path namespace** (`/v3.1/…`, `/2010-04-01/…`); collisions
  fail at startup rather than silently shadowing.
- New resources get a new `Event.Type`; plugins must switch on type rather than
  assume one payload shape.

No extra endpoints are built now — this is a constraint on the design, not scope.

### 6.4 Every plugin and provider is self-documenting

A fake nobody can figure out how to poke is worthless, so this is a contract
member rather than a docs convention. Each plugin and provider supplies a
`Description()`, and each provider supplies `Snippets()` — at least one command
that puts real data into tommy from a cold start, with no other setup.

Snippet code is a **Go template rendered against `SnippetCtx`**, so a copied
command always carries the ports this instance actually bound. A snippet with a
hardcoded `8822` is wrong the moment someone passes `--in-port`.

```go
func (p *Provider) Snippets() []plugin.Snippet {
    return []plugin.Snippet{{
        Title: "Send an email",
        Lang:  "bash",
        Code: `curl -s {{.IngressURL}}/v3.1/send \
  -u "any-key:any-secret" -H 'Content-Type: application/json' -d '{
  "Messages":[{"From":{"Email":"a@example.com"},"To":[{"Email":"b@example.com"}],
  "Subject":"Hello from tommy","TextPart":"It works."}]}'`,
    }}
}
```

Surfaced in four places, all fed from the same source:

- **UI** — the "How to test" panel and tab empty state (§5).
- **API** — `GET /api/v1/plugins`, rendered.
- **CLI** — `tommy providers [name]` prints descriptions, endpoints and snippets
  for the current configuration. Useful before starting the server, and in CI logs.
- **Repo** — a `README.md` in each plugin and provider directory, whose snippet
  block is the same string, so the source tree explains itself too.

**Enforcement.** F1 ships `core/plugin/plugintest.Conformance(t, p)`, which fails
when a description is empty or boilerplate, when a provider has no snippets, when
a snippet fails to parse or render against a `SnippetCtx`, or when a declared
`Endpoint` is not actually mounted on the mux. Every plugin and provider task
calls it in its own package tests, and I1 runs it across the whole registry as a
backstop. **A task is not done until `Conformance` passes** — that is what keeps
this from rotting into a rubber stamp two providers later.

Stronger still, and cheap: the e2e suite executes each `bash` snippet against a
live `testutil` instance and asserts an event appears. It makes the snippets
tested code rather than prose. T1 owns this.

## 7. Do FTP and SFTP fit? — validated, with two contract changes

FTP is the interesting stress test because, unlike mail and SMS, it is **stateful**:
users create folders, overwrite files, list the current tree, and download things
they uploaded an hour ago. An append-only ring buffer of events models none of that.
Checked against the design and it fits, provided two things are true from day one —
both already folded into §3:

1. **Bytes live in `BlobStore`, not in events.** Otherwise ring-buffer eviction
   silently deletes a file the user is about to download. The event log is *history*;
   the blob store and the VFS are *state*, with independent lifetimes.
2. **`ListenerProvider.Listen` takes no `addr`.** FTP needs a control port plus a
   passive port range plus an advertised host, and SFTP needs a port plus a host-key
   path; a single `addr` argument cannot carry either.

### The `files` plugin

Adding SFTP settles the naming: SFTP is an SSH subsystem, not FTP-with-TLS, and
FTPS is a third thing again. Calling the plugin `ftp` and then hanging `sftp` off
it would be wrong, so the plugin is **`files`** (tab label "Files") with `ftp`,
`sftp`, and later `ftps` as sibling providers. `docs/plan.md` says "ftp uploads";
this is the same feature, named for what it holds rather than one protocol.

Three protocols sharing one canonical model and one UI is the strongest validation
of the plugin/provider split so far — it is the same relationship Mailjet and
SendGrid have inside `mail`.

- **State**: `plugins/files/vfs.go` — an in-memory tree of directories and files,
  each file holding a `blob.Ref`. One VFS per plugin, shared by every provider, so
  a file uploaded over SFTP is visible over FTP and in the UI. It is the single
  concurrency-sensitive component in the project: guard it with an `RWMutex` and
  make path resolution reject traversal (`../`, absolute paths, symlink-ish names)
  in one place rather than per protocol.
- **History**: every mutating operation also appends an event (`files.upload`,
  `files.mkdir`, `files.delete`, `files.rename`) tagged with the provider that did
  it, so the SSE stream, `/api/v1/events` and the "what just happened" view stay
  uniform across plugins. This is the generalization the design gains: mail and SMS
  are pure event plugins, `files` is event **+** state, and the interface already
  supports both.
- **API**: `GET /api/v1/files/tree?path=`, `GET /api/v1/files/content/{path...}`
  (stream the blob), `DELETE /api/v1/files/content/{path...}`, plus generic events.
- **UI**: a file-browser tab — breadcrumb, directory listing, size/mtime, uploading
  protocol, download links, live-updating on `sse:files.*`. No shell changes.
- **Auth**: accepted from anyone by default and recorded as metadata, consistent
  with provider rule 1; optionally pinned in TOML.

**FTP provider** — `fclairamb/ftpserverlib` (pure Go, `afero`-style driver), so the
VFS becomes a driver implementation. Covers `STOR`, `RETR`, `MKD`, `RMD`, `DELE`,
`LIST`/`NLST`, `RNFR`/`RNTO`, `SIZE`, `MDTM`, `CWD`. Passive mode is the one
genuinely fiddly part, and why `passive_ports` / `passive_host` are config.

**SFTP provider** — `x/crypto/ssh` for the transport plus `pkg/sftp`'s
`RequestServer` with a custom `Handlers{FileGet, FilePut, FileCmd, FileList}`,
which maps onto the VFS about as directly as an interface can. Simpler than FTP in
one respect (one connection, multiplexed channels — no passive range) and fussier
in another:

- **The host key must persist across restarts.** Generate an ed25519 key on first
  run, write it to `host_key_path`, and print the fingerprint at startup. A
  regenerated key makes every client fail with a changed-host-key error and is the
  single worst UX trap in this plugin.
- Accept any password by default; support `authorized_keys` when configured.
- `Listen` runs the SSH handshake per connection and serves the `sftp` subsystem;
  reject other subsystems and exec requests cleanly rather than hanging.

Snippet-wise (§6.4) these are especially valuable, since neither protocol has an
obvious one-liner:

```bash
# ftp
curl -T ./local.txt ftp://{{.FTPAddr}}/upload/local.txt --ftp-create-dirs -u any:any
# sftp
sftp -P <port> -o StrictHostKeyChecking=no any@{{.Host}}   # then: put ./local.txt
```

## 8. Work breakdown

Ownership is exclusive per row. Nothing outside "Owns" may be edited by that task.

**Definition of done, every plugin and provider task:** package tests green,
`plugintest.Conformance` passing, a `Description()`, at least one `Snippet()` that
works from a cold start, and a directory `README.md` carrying the same snippet.
A task that skips these is not done, however complete the protocol work is.

### Wave 0 — Foundation · 2 agents · blocking

| Task | Owns | Delivers |
|---|---|---|
| **F1 Core** | `go.mod`, `core/**`, `cmd/serve.go`, `cmd/providers.go`, `plugins/all/all.go` (empty list) | `go mod init`; event/store/blob/plugin/config packages; memory store with pub/sub and its tests; size-capped blob store; snippet rendering over `SnippetCtx`; `plugintest.Conformance`; UI shell + tab registry + "How to test" partial + vendored htmx + SSE hub; generic API routes incl. `/plugins` with rendered snippets; `tommy providers`; ingress mux with collision detection; listener supervision; graceful shutdown; `core/testutil` harness; `docs/contracts.md` restating §3 |
| **C1 CI** *(parallel — disjoint files)* | `.github/**`, `.golangci.yml`, `Makefile` | See §9 |

F1 gate: `go build ./... && go test ./...` green; `tommy serve` boots;
`/api/v1/events` returns `[]`; UI renders an empty tab bar. Verified with an
in-test fake plugin — no real plugin is written in this wave. Also confirm
kleiner's `published.MaybeNotifyAboutNewVersion` does not make a blocking network
call in tests, and gate it behind an env var if it does.

C1's files never overlap F1's, so it can be written concurrently; it is only
*verifiable* once `go.mod` exists.

**No plugin work starts until F1 lands.** Its interfaces are the whole point.

### Wave 1 — Plugin cores · 2 agents · parallel

| Task | Owns | Delivers |
|---|---|---|
| **M1 Mail core** | `plugins/mail/*.go`, `plugins/mail/ui/**` | Canonical `Message` (from/to/cc/bcc/reply-to, subject, text + html parts, headers, attachments as `blob.Ref` with inline/content-id); mail API routes incl. streaming attachment download; a plain-but-working UI tab (list + detail with html/text/raw/headers/attachments); tests driven by a test-only fake provider that injects messages directly |
| **S1 SMS core** | `plugins/sms/*.go`, `plugins/sms/ui/**` | Canonical `Message` (from, to, body, segments, media, status); sms API routes; UI tab; tests via a fake provider |

Both depend only on §3. They share no files.

### Wave 2 — Providers · 4 agents · parallel

| Task | Owns | Delivers |
|---|---|---|
| **P-mailjet** | `plugins/mail/providers/mailjet/**` | `POST /v3.1/send`; `Messages[]` fan-out; `Base64Content` attachments + `InlinedAttachments`; Basic-auth capture; `SandboxMode`; `CustomID`/`EventPayload`/`CustomCampaign` → `Meta`; success response `{"Messages":[{"Status":"success","To":[{"Email","MessageUUID","MessageID","MessageHref"}]}]}`; error shape with `ErrorIdentifier`/`ErrorCode`; golden-fixture tests |
| **P-sendgrid** | `plugins/mail/providers/sendgrid/**` | `POST /v3/mail/send`; `personalizations[]` fan-out with per-personalization to/cc/bcc/subject/headers; `content[]` → text+html; base64 attachments with `disposition`/`content_id`; Bearer capture; `categories`/`custom_args`/`send_at`/`batch_id` → `Meta`; **202 + empty body + `X-Message-Id`**; `{"errors":[{"message","field"}]}` on 400; golden-fixture tests |
| **P-twilio** | `plugins/sms/providers/twilio/**` | `POST /2010-04-01/Accounts/{sid}/Messages.json` (**form-encoded**, repeated `MediaUrl`); 201 with the full message resource (`sid` `SM…`, `status: "queued"`, `num_segments`, `uri`, `subresource_uris`); `GET` list + fetch served from the store; Twilio error shape `{"code":21211,"message","more_info","status"}`; segment counting incl. GSM-7 vs UCS-2 |
| **P-smtp** | `plugins/mail/providers/smtp/**` | `ListenerProvider` on `:1025`; MIME parse (multipart/alternative, multipart/mixed, attachments, encoded-word headers) into `mail.Message`; no auth required, AUTH accepted and recorded |

Each provider agent must verify wire formats against the live vendor docs linked
in `docs/plan.md` before coding; the shapes above are a starting point, not gospel.

### Wave 3 — Integration & polish · 1 sequential + 4 parallel

| Task | Owns | Delivers |
|---|---|---|
| **I1 Wiring** (first) | `plugins/all/all.go`, `cmd/mail.go`, `cmd/sms.go`, `README.md` | Register real plugins; single-plugin CLI commands incl. `--enabled-providers`; cross-plugin e2e tests; example `tommy.toml`; usage docs |
| **U-mail** | `plugins/mail/ui/**` | Sandboxed-iframe HTML preview, header table, raw source view, attachment list, search/filter, live prepend via SSE |
| **U-sms** | `plugins/sms/ui/**` | Conversation-style list, media links, segment/encoding display |
| **X1 SDK helpers** | `clienthelp/**`, `docs/clients.md` | §6.2 — the RoundTripper, the twilio-go wrapper, per-SDK docs |
| **T1 SDK tests** | `test/integration/**` (build tag `integration`) | Official `mailjet-apiv3-go`, `sendgrid-go`, `twilio-go` (via X1's helper) pointed at a live tommy — the real proof the fakes are faithful; plus the snippet-execution suite from §6.4 |

T1 depends on X1. U-mail/U-sms run after their Wave 1 counterpart and after I1, so
mail UI files have one owner at a time.

### Wave 4 — Files · not now

Per §7, and it splits the same way the earlier waves did:

| Task | Owns | Notes |
|---|---|---|
| **FS1 Files core** | `plugins/files/*.go`, `plugins/files/ui/**` | VFS + locking + path-traversal guard, API, file-browser tab. Blocks the two providers |
| **P-ftp** | `plugins/files/providers/ftp/**` | ftpserverlib driver over the VFS, passive range |
| **P-sftp** | `plugins/files/providers/sftp/**` | ssh + pkg/sftp `RequestServer`, persisted host key |

P-ftp and P-sftp are parallel once FS1 lands. Optional `--tls` for the HTTP
ingress (§6.2) is independent of all three.

## 9. CI

**`.github/workflows/ci.yml`** — on push and pull_request:

- `actions/setup-go` with `go-version-file: go.mod`, module + build cache enabled
- `gofmt -l .` must be empty; `go vet ./...`
- `golangci-lint` via `golangci/golangci-lint-action` with a checked-in `.golangci.yml`
- `go build ./...`
- `go test -race -coverprofile=coverage.out ./...`
- separate job: `go test -tags integration ./test/integration/...` (once T1 exists)
- `actions/upload-artifact` for the coverage profile

Keep the matrix at the two most recent Go minors. Permissions default to
`contents: read`; all actions pinned by major version at minimum.

**`.github/workflows/release.yml`** — on `v*` tag: checkout with `fetch-depth: 0`,
setup-go, `goreleaser/goreleaser-action` with `BUILD_ENV=production` and
`GITHUB_TOKEN`. `.goreleaser.yaml` currently says `version: 1`, which GoReleaser
v2 rejects, and uses the deprecated `archives.format` key — C1 either pins the
action to v1 or migrates the config to v2 (`version: 2`, `formats: [tar.gz]`).
Migrating is preferred; verify with `goreleaser check`.

**`.github/dependabot.yml`** — weekly, two ecosystems:

```yaml
version: 2
updates:
  - package-ecosystem: gomod
    directory: "/"
    schedule: { interval: weekly }
    groups:
      go-deps: { patterns: ["*"] }
  - package-ecosystem: github-actions
    directory: "/"
    schedule: { interval: weekly }
```

Grouping keeps Go module bumps to one PR a week instead of one per dependency.

**`Makefile`** — `build`, `test`, `lint`, `fmt`, `run`, `check` (what CI runs), so
contributors and CI share one definition.

## 10. Test strategy

- **Store / blob** — ring-buffer eviction, filtering, subscribe fan-out,
  slow-consumer drop, blob size cap and eviction independence from event eviction.
- **Providers** — table-driven, golden request fixtures in `testdata/`, asserting
  both the canonical model produced *and* the exact HTTP response returned.
- **Plugin API** — `httptest` against the mounted routes, attachment bytes checked
  round-trip.
- **UI** — `httptest` + `goquery`: tab bar contains every enabled plugin, list
  renders injected messages, SSE endpoint emits a frame when an event is appended.
- **E2E** — `core/testutil.Start(t, cfg)` boots the whole process on ephemeral
  ports; a test POSTs a real Mailjet payload to the ingress and asserts it appears
  on `/api/v1/mail/messages` and in the UI.
- **Conformance** — `plugintest.Conformance` per package, and once across the whole
  registry: descriptions present and non-boilerplate, snippets parse and render,
  declared endpoints actually mounted (§6.4).
- **Integration** (tagged) — the official vendor SDKs, per T1, plus executing every
  `bash` snippet against a live instance and asserting an event lands.
- **VFS** (Wave 4) — concurrent upload/list/delete under `-race`, and path-traversal
  attempts (`../`, absolute, encoded) rejected for both protocols.

`testutil` must return the resolved ports (`:0` binding) so tests never collide,
and every test gets a fresh store.

## 11. Sequencing summary

```
Wave 0   F1 ∥ C1                                      (2 agents, blocking)
Wave 1   M1 ∥ S1                                      (2 agents)
Wave 2   P-mailjet ∥ P-sendgrid ∥ P-twilio ∥ P-smtp   (4 agents)
Wave 3   I1 → (U-mail ∥ U-sms ∥ X1 → T1)              (1 then 4)
Wave 4   FS1 → (P-ftp ∥ P-sftp);  TLS ingress         (later)
```

Out of scope for now, but the plugin interface must not preclude them: push
notifications, dynamic templates, event persistence, webhook/callback simulation
(Twilio `StatusCallback`, SendGrid event webhook).

Sources for §6: [mailjet-apiv3-go](https://pkg.go.dev/github.com/mailjet/mailjet-apiv3-go/v4),
[sendgrid-go](https://pkg.go.dev/github.com/sendgrid/sendgrid-go),
[twilio-go request_handler.go](https://github.com/twilio/twilio-go/blob/main/client/request_handler.go),
[twilio-go custom HTTP client](https://github.com/twilio/twilio-go/blob/main/advanced-examples/custom-http-client.md).
