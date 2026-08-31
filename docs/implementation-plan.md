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
- **No `go.mod` yet** — module path is `github.com/can3p/tommy` (per existing imports)

Everything below is new code.

## 1. Decisions

| Area | Decision | Consequence |
|---|---|---|
| UI | Go `html/template` + `go:embed` + htmx + SSE | No node toolchain; goreleaser stays a plain `go build`; UI is testable with `httptest` + `goquery` |
| Storage | In-memory ring buffer behind a `Store` interface | No CGO, trivial reset between tests; persistence can be added later without touching plugins |
| Ingress | One shared HTTP ingress port, path-routed | Real provider API paths do not collide; matches the `--in-port 8822 --enabled-providers mailjet,sendgrid` example. Per-provider port override in TOML; SMTP/FTP get their own listeners |
| Routing | stdlib `net/http.ServeMux` (Go 1.22+ patterns) | `POST /2010-04-01/Accounts/{sid}/Messages.json` works without a router dependency |
| Deps | cobra, `pelletier/go-toml/v2`, `emersion/go-smtp` (SMTP provider only), `PuerkitoBio/goquery` (tests only) | Stays CGO-free and single-binary |

## 2. Architecture

Three listeners, one process:

```
  provider SDK / curl ──► ingress mux  :8822   (fake Mailjet/SendGrid/Twilio APIs)
                                │
                                ▼
                          Store (in-memory ring buffer + pub/sub)
                                │
                    ┌───────────┴───────────┐
                    ▼                       ▼
              UI server :8811         API server :8811 (or its own port)
              /ui/... + SSE           /api/v1/...
```

A **plugin** owns a content type (mail, sms, later ftp/push): a canonical model,
an API surface, a UI tab, and a set of **providers**. A **provider** imitates one
real vendor API and converts its wire format into the plugin's canonical model.
Providers never talk to each other and never share files — that is what makes the
work parallelizable.

### Directory layout

```
cmd/
  root.go            # exists
  serve.go           # tommy serve --config tommy.toml
  mail.go            # tommy mail  --ui-port --in-port --enabled-providers
  sms.go             # tommy sms   ...
core/
  event/             # Event envelope, IDs, Summary, Raw
  store/             # Store interface
    memory/          # ring-buffer implementation + fan-out pub/sub
  plugin/            # Plugin, Provider, ListenerProvider, Deps, Registry
  config/            # TOML + CLI config, defaults, validation
  server/
    ui/              # layout, tab registry, embedded static assets, SSE hub
    api/             # generic /api/v1 routes
    ingress/         # shared ingress mux + per-provider listener supervision
    lifecycle.go     # start/stop all listeners, graceful shutdown
  testutil/          # boot tommy in-process on ephemeral ports for tests
plugins/
  all/all.go         # the ONLY shared wiring file — owned by the integration task
  mail/
    message.go       # canonical Message + Attachment
    plugin.go  api.go  ui/
    providers/
      mailjet/  sendgrid/  smtp/
  sms/
    message.go  plugin.go  api.go  ui/
    providers/
      twilio/
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
    Plugin     string         // "mail", "sms"
    Provider   string         // "mailjet", "sendgrid", "smtp", "twilio"
    Type       string         // "mail.message", "sms.message"
    ReceivedAt time.Time
    Summary    Summary        // provider-agnostic listing data
    Meta       map[string]any // provider metadata (Mailjet CustomID, SendGrid categories, ...)
    Payload    any            // *mail.Message, *sms.Message — marshalled to JSON by the API
    Raw        Raw            // original request: method, path, headers, body
}

type Summary struct {
    From    string
    To      []string
    Title   string // subject / first line
    Snippet string
}
```

```go
// core/store
type Query struct {
    Plugin, Provider, Search string
    Since                    time.Time
    Limit, Offset            int
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
// core/plugin
type Plugin interface {
    Name() string   // "mail" — url segment
    Title() string  // "Mail" — UI tab label
    Providers() []Provider
    RegisterAPI(mux *http.ServeMux, d Deps) // mounted under /api/v1/<name>/
    RegisterUI(mux *http.ServeMux, d Deps)  // mounted under /ui/<name>/
    Templates() fs.FS                       // embedded templates for the tab
}

type Provider interface {
    Name() string   // "mailjet"
    Plugin() string // "mail"
    Endpoints() []Endpoint // for /api/v1/plugins discovery + UI hints
    RegisterIngress(mux *http.ServeMux, d Deps)
}

// Providers that need their own listener (SMTP, later FTP) also implement:
type ListenerProvider interface {
    Provider
    Listen(ctx context.Context, addr string, d Deps) error // blocks until ctx is done
}

type Deps struct {
    Store   store.Store
    Config  ProviderConfig    // per-provider TOML section, decoded on demand
    Logger  *slog.Logger
    Now     func() time.Time  // injectable for deterministic tests
    NewID   func() string     // injectable id generator
}
```

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
5. **Never import another provider's package.**

## 4. Config

```toml
[ui]
port = 8811

[api]
port = 8811        # same listener as UI by default

[ingress]
port = 8822

[plugins.mail]
enabled  = true
capacity = 500     # ring buffer size

[plugins.mail.providers.mailjet]
enabled = true
# port  = 9001     # optional dedicated listener instead of the shared ingress

[plugins.mail.providers.sendgrid]
enabled = true

[plugins.mail.providers.smtp]
enabled = true
port    = 1025     # ListenerProvider: always its own port

[plugins.sms]
enabled = true

[plugins.sms.providers.twilio]
enabled     = true
account_sid = "AC00000000000000000000000000000000"  # echoed back; any accepted if unset
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

Unmatched ingress paths return 404 with a body naming the enabled providers — it
is the single most common misconfiguration and worth a good error.

### API (`/api/v1`)

Generic:
- `GET /health`
- `GET /plugins` → enabled plugins, providers, and their ingress endpoints
- `GET /events?plugin=&provider=&search=&since=&limit=&offset=`
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

## 6. Work breakdown

Ownership is exclusive per row. Nothing outside "Owns" may be edited by that task.

### Wave 0 — Foundation · 1 agent · blocking

| Task | Owns | Delivers |
|---|---|---|
| **F1 Core** | `go.mod`, `core/**`, `cmd/serve.go`, `plugins/all/all.go` (empty list) | `go mod init`; event/store/plugin/config packages; memory store with pub/sub and its tests; UI shell + tab registry + vendored htmx + SSE hub; generic API routes; ingress mux + listener supervision; graceful shutdown; `core/testutil` harness; `docs/contracts.md` restating §3 |

Gate: `go build ./... && go test ./...` green; `tommy serve` boots; `/api/v1/events`
returns `[]`; UI renders an empty tab bar. Verified with an in-test fake plugin —
no real plugin is written in this wave. Also confirm kleiner's
`published.MaybeNotifyAboutNewVersion` does not make a blocking network call in
tests, and gate it behind an env var if it does.

**Nothing else starts until F1 lands.** Its interfaces are the whole point.

### Wave 1 — Plugin cores · 2 agents · parallel

| Task | Owns | Delivers |
|---|---|---|
| **M1 Mail core** | `plugins/mail/*.go`, `plugins/mail/ui/**` | Canonical `Message` (from/to/cc/bcc/reply-to, subject, text + html parts, headers, attachments with inline/content-id, size); mail API routes incl. attachment download; a plain-but-working UI tab (list + detail with html/text/raw/headers/attachments); tests driven by a test-only fake provider that injects messages directly into the store |
| **S1 SMS core** | `plugins/sms/*.go`, `plugins/sms/ui/**` | Canonical `Message` (from, to, body, segments, media, status); sms API routes; UI tab; tests via a fake provider |

Both depend only on §3. They share no files.

### Wave 2 — Providers · 4 agents · parallel

| Task | Owns | Delivers |
|---|---|---|
| **P-mailjet** | `plugins/mail/providers/mailjet/**` | `POST /v3.1/send`; `Messages[]` fan-out; `Base64Content` attachments + `InlinedAttachments`; Basic-auth capture; `SandboxMode`; `CustomID`/`EventPayload`/`CustomCampaign` → `Meta`; success response `{"Messages":[{"Status":"success","To":[{"Email","MessageUUID","MessageID","MessageHref"}]}]}`; error shape with `ErrorIdentifier`/`ErrorCode`; golden-fixture tests |
| **P-sendgrid** | `plugins/mail/providers/sendgrid/**` | `POST /v3/mail/send`; `personalizations[]` fan-out with per-personalization to/cc/bcc/subject/headers; `content[]` → text+html; base64 attachments with `disposition`/`content_id`; Bearer capture; `categories`/`custom_args`/`send_at`/`batch_id` → `Meta`; **202 + empty body + `X-Message-Id`**; `{"errors":[{"message","field"}]}` on 400; golden-fixture tests |
| **P-twilio** | `plugins/sms/providers/twilio/**` | `POST /2010-04-01/Accounts/{sid}/Messages.json` (**form-encoded**, repeated `MediaUrl`); 201 with the full message resource (`sid` `SM…`, `status: "queued"`, `num_segments`, `uri`, `subresource_uris`); `GET` list + fetch so SDK follow-ups work; Twilio error shape `{"code":21211,"message","more_info","status"}`; segment counting incl. GSM-7 vs UCS-2 |
| **P-smtp** | `plugins/mail/providers/smtp/**` | `ListenerProvider` on `:1025`; MIME parse (multipart/alternative, multipart/mixed, attachments, encoded-word headers) into `mail.Message`; no auth required, AUTH accepted and recorded |

Each provider agent must verify wire formats against the live vendor docs linked
in `docs/plan.md` before coding; the shapes above are a starting point, not gospel.

### Wave 3 — Integration & polish · 1 sequential + 3 parallel

| Task | Owns | Delivers |
|---|---|---|
| **I1 Wiring** (first) | `plugins/all/all.go`, `cmd/mail.go`, `cmd/sms.go`, `README.md`, `docs/` | Register real plugins; single-plugin CLI commands incl. `--enabled-providers`; cross-plugin e2e tests; example `tommy.toml`; usage docs |
| **U-mail** | `plugins/mail/ui/**` | Sandboxed-iframe HTML preview, header table, raw source view, attachment list, search/filter, live prepend via SSE |
| **U-sms** | `plugins/sms/ui/**` | Conversation-style list, media links, segment/encoding display |
| **T1 SDK tests** | `test/integration/**` (build tag `integration`) | Official `mailjet-apiv3-go`, `sendgrid-go`, `twilio-go` SDKs pointed at a live tommy — the real proof the fakes are faithful |

U-mail/U-sms run after their Wave 1 counterpart lands and after I1, so mail UI
files have one owner at a time.

## 7. Test strategy

- **Store** — ring-buffer eviction, filtering, subscribe fan-out, slow-consumer drop.
- **Providers** — table-driven, golden request fixtures in `testdata/`, asserting
  both the canonical model produced *and* the exact HTTP response returned.
- **Plugin API** — `httptest` against the mounted routes, attachment bytes checked
  round-trip.
- **UI** — `httptest` + `goquery`: tab bar contains every enabled plugin, list
  renders injected messages, SSE endpoint emits a frame when an event is appended.
- **E2E** — `core/testutil.Start(t, cfg)` boots the whole process on ephemeral
  ports; a test POSTs a real Mailjet payload to the ingress and asserts it appears
  on `/api/v1/mail/messages` and in the UI.
- **Integration** (tagged) — the official vendor SDKs, per T1.

`testutil` must return the resolved ports (`:0` binding) so tests never collide,
and every test gets a fresh store.

## 8. Sequencing summary

```
Wave 0   F1                                       (1 agent, blocking)
Wave 1   M1 ∥ S1                                  (2 agents)
Wave 2   P-mailjet ∥ P-sendgrid ∥ P-twilio ∥ P-smtp   (4 agents)
Wave 3   I1 → (U-mail ∥ U-sms ∥ T1)               (1 then 3)
```

Out of scope for now, but the plugin interface must not preclude them: FTP and
push-notification plugins, dynamic templates, persistence, webhook/callback
simulation (Twilio `StatusCallback`, SendGrid event webhook).
