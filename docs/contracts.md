# Tommy core contracts

The core interfaces as they actually exist in the code, kept current as waves
land. **This is the authoritative reference** - where it disagrees with
`docs/implementation-plan.md`, this document is right. If a contract needs to
change, raise it rather than patching it in place; nearly every real gap found so
far was reported by an implementer rather than worked around, and several were
found independently by two or three tasks at once.

Module: `github.com/can3p/tommy`, Go 1.26. Core depends on `spf13/cobra` and
`pelletier/go-toml/v2` (plus `can3p/kleiner`, pre-existing) with
`PuerkitoBio/goquery` for tests; individual providers add their own protocol
libraries. The vendor SDKs are deliberately kept out of this module - they live
in `test/integration`, a nested module of its own.

---

## Deviations from the plan

Three, all reported rather than silent.

1. **`plugin.Mux` replaces `*http.ServeMux`** in `RegisterAPI`, `RegisterUI` and
   `RegisterIngress`.

   ```go
   type Mux interface {
       Handle(pattern string, handler http.Handler)
       HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
   }
   ```

   `*http.ServeMux` satisfies it, so provider code is written exactly as the
   plan shows and a test can still pass `http.NewServeMux()`. The change is
   required by the plan's own hard requirement that ingress registration
   "detect collisions between providers AND against reserved core prefixes,
   failing loudly at startup naming both claimants": routes written straight
   onto a concrete `ServeMux` are invisible to the core, `ServeMux` panics
   without naming the claimant on an exact duplicate, and shadowing patterns
   (`/v3/mail/send` vs `/v3/{x}/send`) resolve silently. With an interface the
   ingress sees every registration before it happens.

2. **Blobs are never evicted.** `blob.BlobStore.Put` returns
   `blob.ErrCapacityExceeded` once the cap is reached instead of dropping the
   oldest blob. The plan requires the blob store's lifetime to be independent of
   ring-buffer eviction (§7.1); evicting on our own would break the download
   link of a message still listed in the UI. A loud error is the lesser evil.
   Clearing events therefore does **not** clear blobs.

3. **`Snippet` interpolation.** `SnippetCtx` keeps every field the plan lists and
   adds an `Addrs map[string]string` with `Addr(plugin, provider)` and
   `Port(plugin, provider)` helpers, so a listener provider added later (TFTP,
   MLLP, …) needs no new struct field.

Everything else matches the plan.

---

## `core/event`

```go
type ID string

type Event struct {
    ID         ID             `json:"id"`
    Plugin     string         `json:"plugin"`
    Provider   string         `json:"provider"`
    Type       string         `json:"type"`        // "<plugin>.<resource>", free-form
    ReceivedAt time.Time      `json:"received_at"`
    Summary    Summary        `json:"summary"`
    Meta       map[string]any `json:"meta,omitempty"`
    Payload    any            `json:"payload,omitempty"`
    Raw        Raw            `json:"raw"`
}

type Summary struct {
    From    string   `json:"from,omitempty"`
    To      []string `json:"to,omitempty"`
    Title   string   `json:"title,omitempty"`
    Snippet string   `json:"snippet,omitempty"`
}

type Raw struct {
    Transport string      `json:"transport"`            // http | tcp | udp | smtp | ftp | ssh
    PeerAddr  string      `json:"peer_addr,omitempty"`
    Method    string      `json:"method,omitempty"`     // http only
    Path      string      `json:"path,omitempty"`       // http only
    Headers   http.Header `json:"headers,omitempty"`    // http/smtp; nil elsewhere
    Body      []byte      `json:"body,omitempty"`       // bytes, may be binary
    Text      bool        `json:"text"`                 // render as text, else hex viewer
}

func (e *Event) Clone() *Event          // shallow copy; Summary.To is copied
func (e *Event) WithoutRawBody() *Event // what the SSE stream sends
func NewID() string                     // 24 hex chars, time-sortable, collision-resistant
```

Rules:

- **Events are immutable once appended.** The store hands out `Clone()`s of the
  envelope, but `Meta`, `Payload` and `Raw.Body` are shared. Never mutate them
  after `Append`.
- **`Type` is not an enum.** A plugin may emit several types and its UI/API must
  switch on the type rather than assume one payload shape.
- **Always populate `Raw`** with the untouched request.
- **Bytes never go inline.** Attachments and uploads hold a `blob.Ref`.

## `core/store`

```go
type Query struct {
    Plugin, Provider, Type, Search string
    Since                          time.Time
    Limit, Offset                  int
}
func (q Query) Matches(e *event.Event) bool

type Store interface {
    Append(ctx context.Context, e *event.Event) error
    List(ctx context.Context, q Query) ([]*event.Event, error) // newest first
    Get(ctx context.Context, id event.ID) (*event.Event, error)
    Delete(ctx context.Context, id event.ID) error
    Clear(ctx context.Context, plugin string) error // "" clears everything
    Subscribe(ctx context.Context) <-chan *event.Event
}

var ErrNotFound = errors.New("store: event not found")
```

- `Append` assigns `ID` and `ReceivedAt` when empty, **writing them back onto the
  caller's event**, then stores its own copy. A duplicate id is an error.
- `Search` is a case-insensitive substring match over `Summary.From`,
  `Summary.To`, `Summary.Title`, `Summary.Snippet` and `Type`.
- `Since` is exclusive. `Limit <= 0` means no limit.
- Ordering is `ReceivedAt` descending, with arrival order breaking ties.
- `Subscribe` closes the channel when `ctx` is done. **Delivery is best effort**:
  a subscriber that cannot keep up misses events, because `Append` sits on the
  hot path of a fake API answering a real SDK and must never block.

`core/store/memory.New(capacity int, opts ...Option) *Store` — one ring buffer
per plugin, so a chatty plugin cannot evict a quiet one. Options:
`WithPluginCapacity`, `WithClock`, `WithIDFunc`, `WithSubscriberBuffer`. Extras
for tests: `Len()`, `Dropped()`, `Subscribers()`.

## `core/blob`

```go
type Ref struct {
    ID          string `json:"id"`
    Size        int64  `json:"size"`
    ContentType string `json:"content_type,omitempty"`
    Filename    string `json:"filename,omitempty"`
}

type BlobStore interface {
    Put(ctx context.Context, r io.Reader, meta Ref) (Ref, error)
    Open(ctx context.Context, id string) (io.ReadSeekCloser, Ref, error)
    Delete(ctx context.Context, id string) error
    Stat(ctx context.Context, id string) (Ref, error)
}

var ErrNotFound, ErrCapacityExceeded error
```

`Put` generates an id when `meta.ID` is empty and fills in the real `Size`. It
reads at most one byte past the remaining headroom, so an oversized body cannot
blow up memory before the cap is checked. Re-putting an existing id overwrites
it and reuses its space. `core/blob/memory.New(limit int64)`; extras: `Used()`,
`Limit()`, `Len()`, `Reset()`.

Generic download route: `GET /api/v1/blobs/{id}` (range requests supported,
`?inline=1` for an inline `Content-Disposition`).

## `core/plugin`

```go
type Plugin interface {
    Name() string        // "mail" - url segment, lowercase letters/digits/dashes
    Title() string       // "Mail" - UI tab label
    Description() string // one or two real sentences
    Providers() []Provider
    RegisterAPI(mux Mux, d Deps) // mounted under /api/v1/<name>/
    RegisterUI(mux Mux, d Deps)  // mounted under /ui/<name>/
    Templates() fs.FS            // may be nil
}

type Provider interface {
    Name() string
    Plugin() string        // must equal the plugin that lists it
    Description() string
    Endpoints() []Endpoint // every mounted route is declared here
    Snippets() []Snippet   // at least one
    RegisterIngress(mux Mux, d Deps)
}

type ListenerProvider interface {
    Provider
    Listen(ctx context.Context, d Deps) error // reads its ports from d.Config; blocks until ctx done
}

// AddressableProvider is optional, and every listener provider should implement
// it. A provider that took an ephemeral port, or that fell back to its own
// package default because the config named none, is the only thing that knows
// where it ended up listening; without this the discovery surface advertises no
// address at all in exactly those two cases. Addr blocks until the listener has
// bound or the timeout elapses. The server re-resolves these addresses once the
// providers are up, since none have bound when it is built.
type AddressableProvider interface {
    ListenerProvider
    Addr(timeout time.Duration) (string, error)
}

type Endpoint struct{ Method, Path, Description string }

type Snippet struct{ Title, Lang, Code string } // Code is a template over SnippetCtx
func (s Snippet) Render(ctx SnippetCtx) (string, error)

type SnippetCtx struct {
    Host, IngressURL, UIURL, APIURL   string
    SMTPAddr, FTPAddr, SFTPAddr       string
    Addrs map[string]string           // "<plugin>.<provider>" -> host:port
}
func (c SnippetCtx) Addr(plugin, provider string) string
func (c SnippetCtx) Port(plugin, provider string) string
func (c *SnippetCtx) SetAddr(plugin, provider, addr string)

type Deps struct {
    Store  store.Store
    Blobs  blob.BlobStore
    Config ProviderConfig // = config.ProviderConfig
    Logger *slog.Logger
    Now    func() time.Time
    NewID  func() string
}
func (d Deps) Normalize() Deps                       // fills Now/NewID/Logger
func (d Deps) WithConfig(pc ProviderConfig) Deps
func (d Deps) WithLogger(args ...any) Deps
func (d Deps) Append(ctx context.Context, e *event.Event) error // stamp + store
```

Templates rendered with `missingkey=error`, so a typo in a snippet is a failure
rather than a blank.

### Registry

```go
func New(cfg *config.Config, plugins ...Plugin) (*Registry, error)

func (r *Registry) Plugins() []Plugin                  // enabled, registration order
func (r *Registry) AllPlugins() []Plugin
func (r *Registry) DisabledPlugins() []Plugin
func (r *Registry) Plugin(name string) (Plugin, bool)  // enabled only
func (r *Registry) Providers(plugin string) []Provider // enabled only
func (r *Registry) Refs() []Ref                        // {Plugin, Provider}
func (r *Registry) IngressRefs() []Ref                 // HTTP providers
func (r *Registry) ListenerRefs() []Ref                // ListenerProviders
func (r *Registry) ProviderConfig(plugin, provider string) ProviderConfig
func (r *Registry) DepsFor(base Deps, plugin, provider string) Deps
func (r *Registry) Describe(ctx SnippetCtx) ([]PluginInfo, error)
```

`New` fails on a duplicate plugin name, a duplicate provider inside a plugin, a
non-URL-safe name, or a provider whose `Plugin()` disagrees with the plugin that
lists it. `Describe` is what `/api/v1/plugins`, the UI panel and
`tommy providers` all render, so those three never drift.

Registration stays explicit in `plugins/all/all.go`; no `init()` magic.

## `core/plugin/plugintest`

```go
func Conformance(t *testing.T, p plugin.Plugin)
func ConformanceProvider(t *testing.T, prov plugin.Provider)
func CheckPlugin(p plugin.Plugin) []error     // same checks, no *testing.T
func CheckProvider(prov plugin.Provider) []error
func NewDeps() plugin.Deps                    // fixed clock, counting ids
func Deps(t testing.TB) plugin.Deps
func SnippetCtx() plugin.SnippetCtx
```

Fails on: empty or boilerplate descriptions (also on `Endpoint.Description`),
descriptions under 24 characters, a name that is not URL-safe, zero snippets, a
snippet without a title or language, a snippet that fails to parse or render, a
**declared endpoint that is never mounted**, and a **mounted route that is not
declared**. A `ListenerProvider` with no endpoints is exempt from the route
checks. **A task is not done until this passes.**

## `core/config`

TOML and the CLI build the same struct and run one bootstrap.

```toml
bind = "127.0.0.1"   # default interface for the core listeners
host = "localhost"   # hostname used in printed URLs and snippets

[ui]      port = 8811
[api]     port = 8811      # omit to share the UI listener
[ingress] port = 8822

[storage]
capacity   = 500            # events retained per plugin
blob_limit = "256MB"        # integer bytes or a string like "1.5GiB"
# [storage.plugin_capacity]  mail = 2000

default_enabled = true      # what an unmentioned plugin/provider does

[plugins.mail]
enabled = true
[plugins.mail.providers.smtp]
enabled = true
port    = 1025              # a listener provider's own port
```

```go
func Default() *Config
func Ephemeral() *Config          // port 0 everywhere; defaults NOT applied yet
func Load(path string) (*Config, error)
func Parse(data []byte) (*Config, error)
func (c *Config) ApplyDefaults()  // idempotent
func (c *Config) Validate() error // reports every problem at once
func (c *Config) PluginEnabled(name string) bool
func (c *Config) ProviderEnabled(plugin, provider string) bool
func (c *Config) Provider(plugin, provider string) ProviderConfig
func (c *Config) SetProvider(plugin, provider string, pc ProviderConfig)
func (c *Config) SetPluginEnabled(plugin string, enabled bool)
func (c *Config) APISharesUIListener() bool
func (c *Config) IngressSharesUIListener() bool
```

`ProviderConfig` keeps the unknown keys of its section:

```go
type ProviderConfig struct {
    Enabled *bool // nil means "inherit"
    Port    int
}
func NewProviderConfig(values map[string]any) ProviderConfig
func (p ProviderConfig) Decode(v any) error // into your own toml-tagged struct
func (p ProviderConfig) String(key, def string) string
func (p ProviderConfig) Int(key string, def int) int
func (p ProviderConfig) Bool(key string, def bool) bool
```

So a provider adds a setting by declaring it in its own struct — the core config
never grows a field for it. `tommy mail --enabled-providers mailjet` is built by
setting `DefaultEnabled = false` and switching on just what was asked for.

Ports: `0` means "bind an ephemeral port". Build the config first, then call
`ApplyDefaults` — "unset" and "0" are only distinguishable before defaulting.

## `core/server/ingress`

```go
func New(logger *slog.Logger) *Ingress
func (i *Ingress) For(plugin, provider string) plugin.Mux
func (i *Ingress) Mount(reg *plugin.Registry, base plugin.Deps) error
func (i *Ingress) Err() error
func (i *Ingress) Routes() []Route
func (i *Ingress) Has(method, path string) bool
func (i *Ingress) SetNotFound(h http.Handler)
func NotFoundHandler(info InfoFunc) http.Handler
func ParsePattern(pattern string) (Pattern, error)

var ReservedPrefixes = []string{"/api/v1/", "/ui/", "/_tommy/"}
```

Registration fails, naming both claimants, when two providers claim the same
route (wildcard names are normalised, so `/a/{id}` and `/a/{sid}` collide;
different methods on one path do not), when a route falls under a reserved core
prefix, when a route would swallow everything (`/`, `/{x...}`), or when
`net/http` rejects the pattern (the panic is turned into an error). `Mount` also
fails when a declared `Endpoint` is not reachable.

Note the reserved prefix is `/api/v1/`, **not** `/api/`: a chat provider
legitimately wants `POST /api/chat.postMessage`.

Unmatched ingress requests get a 404 whose body lists every enabled provider,
its endpoints and a pointer to `tommy providers` — text, or JSON for a JSON
client.

## `core/server/api` — `/api/v1`

| Route | Notes |
|---|---|
| `GET /health` | `{status, uptime, plugins[], events, version}` |
| `GET /plugins` | descriptions, endpoints, and snippets **rendered against the live ports** |
| `GET /events` | `?plugin=&provider=&type=&search=&since=&limit=&offset=&include_raw=` |
| `GET /events/{id}` | the full event, raw body included |
| `GET /events/stream` | SSE, same filters |
| `DELETE /events` | `?plugin=` to narrow; 204 |
| `DELETE /events/{id}` | 204, or 404 |
| `GET /blobs/{id}` | streams a blob with range support |
| `/api/v1/<plugin>/…` | whatever the plugin mounted in `RegisterAPI` |

`since` accepts an RFC3339 timestamp, a duration (`5m` = "in the last five
minutes"), or unix milliseconds. **Listings omit `Raw.Body`** — they can be
megabytes each; pass `include_raw=1` or fetch the single event.

### SSE frame format

Each event produces two frames:

```
id: <event id>
data: {"id":"…","plugin":"mail",…}     ← default "message" frame, full JSON, no Raw.Body

event: mail.message
data: <event id>                        ← named frame, for hx-trigger="sse:mail.message"
```

Plus `: ping` every 25s. Any plain `EventSource` gets everything through
`onmessage`; htmx triggers on the type-named frame.

## `core/server/ui` — `/ui`

The shell renders the tab bar from the registry, holds **one** SSE connection
per page (`sse-connect="/ui/stream"` on `<body>`), and serves the vendored
assets from `/ui/static/` (htmx 2.0.4 and its SSE extension are committed, never
hotlinked — see `core/server/ui/static/VENDOR.md`).

Each plugin gets its own mux mounted at `/ui/<name>/`. **Whatever it does not
claim falls back to the generic event view**, probed route by route:

| Route | Falls back to |
|---|---|
| `GET /{$}` | the generic page |
| `GET /list` | the list fragment |
| `GET /events/{id}` | the detail fragment (full page on a deep link) |
| `DELETE /events` | clear, returning the list fragment |

So a new protocol plugin is useful on day one with zero UI code, and a bespoke
tab is an upgrade rather than a prerequisite. `/ui/` itself is the cross-plugin
overview.

For a plugin handler:

```go
func PluginTemplates(pluginFS fs.FS, patterns ...string) (*template.Template, error)
func Render(w http.ResponseWriter, r *http.Request, title string, body template.HTML) error
func IsPartial(r *http.Request) bool // htmx asked for a fragment
func ShellFrom(r *http.Request) *Shell
func (s *Shell) Info() []plugin.PluginInfo // the plugins in scope of the active tab
```

`Render` writes the full shell for a normal request and a bare fragment for an
htmx swap. The shell arrives through the request context, so `Deps` did not have
to grow a field.

`Shell.Info()` describes the active tab's plugin — every provider, with snippets
already rendered against the ports this instance actually bound. A bespoke tab
needs it to build the how-to-test panel and a snippet-carrying empty state;
without it, writing your own tab silently costs you the one thing that tells a
newcomer how to fill it. Evaluated on demand, since rendering snippets is not
free and most requests never ask.

### `core/server/ui/components`

`components.Template()` returns every component with the helpers installed;
`PluginTemplates` gives you those plus your own files. After parsing your own
templates you must call `components.Bind(tpl)` (PluginTemplates does it), which
installs `render`, the helper that makes the layouts compositional:

```gotemplate
{{template "master-detail" (dict
    "Title"  "Inbox"
    "List"   (render "my-list" .)
    "Detail" (render "my-detail" .)
    "ListID" "list" "DetailID" "detail")}}
```

Components: `badge`, `badges`, `copy-button`, `kv-table`, `table`,
`json-inspector` (collapsible, copyable), `hex-viewer`, `raw-viewer` (text or
hex, decided by `Raw.Text` with a content sniff as fallback), `master-detail`,
`stream`, `snippet`, `provider-card`, `how-to-test`, `empty-state`,
`event-list`, `event-detail`, `event-pane`, `generic-event-view`.

Helpers: `jsonView`, `hexView`, `isText`, `pretty`, `bytesHuman`, `truncate`,
`timeShort`, `timeFull`, `since`, `join`, `lower`, `asString`, `dict`, `badge`,
`kv`, `add`, `hasPrefix`, `int64`, `rawMeta`, `summaryOf`, `render`.

Go types behind them: `Badge`, `KV`, `KVTable`, `Cell`, `Row`, `Table`,
`MasterDetail`, `Stream`, `JSONView`, `HexView`, `EmptyState`, `HowToTest`,
`EventView`, `EventFilter`.

`how-to-test` takes a `components.HowToTest{Info []plugin.PluginInfo, Open bool}`.
**Pass `Open` true when your tab has nothing to show** - an empty tab is exactly
when someone needs to know how to fill it - and false once it has content. Build
it from `ui.ShellFrom(r).Info()`; `plugins/mail/ui.go` and `plugins/sms/ui.go`
both do, and every tab should use this shared component rather than cloning it.

## `core/server` — lifecycle

```go
func New(opts Options) (*Server, error) // binds the core listeners; does not serve
func (s *Server) Start(ctx context.Context) error
func (s *Server) Shutdown(ctx context.Context) error
func (s *Server) Addrs() Addrs
func (s *Server) URLs() (uiURL, apiURL, ingressURL string)
func (s *Server) SnippetCtx() plugin.SnippetCtx
func (s *Server) Store() store.Store
func (s *Server) Blobs() blob.BlobStore
func (s *Server) Registry() *plugin.Registry
func (s *Server) Describe() ([]plugin.PluginInfo, error)
func Run(ctx context.Context, opts Options) error
```

`New` binds first, so `Addrs()` is valid before anything is served — that is
what makes ephemeral ports usable. It fails when an ingress route collides or a
declared endpoint is unreachable. `Start` runs every HTTP listener plus every
`ListenerProvider` in its own goroutine; `Shutdown` cancels their context,
gracefully stops the HTTP servers, and waits (5s by default).

Listener composition: the API shares the UI listener unless it has a port of its
own; the ingress shares it when the ports match. On a shared listener the core
prefixes win over the ingress catch-all.

## `core/testutil`

```go
func Start(t testing.TB, cfg *config.Config, plugins ...plugin.Plugin) *Instance
```

`cfg == nil` means ephemeral ports **including listener providers**: their config
sections are pinned to port 0 so nothing binds a well-known port. That matters -
`config.Ephemeral()` alone only zeroes the three core listeners, so a provider
falls back to its package default and a test asking for an ephemeral server binds
1025 or 2121, colliding with a real mail catcher, another test binary, or a stray
server, and failing only sometimes and only on some machines. Pass a config of
your own and an explicit port in it is left alone.

Returns resolved `UIURL`, `APIURL`, `IngressURL`, plus `Store`, `Blobs`,
`Registry`, `Config` and an `http.Client`. Every instance is fresh and is shut
down through `t.Cleanup`.

Helpers: `Get`, `GetBody`, `GetJSON`, `PostJSON`, `Do`, `Events(q)`,
`WaitForEvents(n, q, timeout)`, and the URL builders `API`, `UI`, `Ingress`.

`core/testutil/fakeplugin` is a complete worked example — an HTTP provider, a
TCP `ListenerProvider`, endpoints, snippets, a plugin API route and no UI (so it
exercises the generic view). Read it before writing a plugin.

## CLI

- `tommy serve [-c tommy.toml] [--ui-port] [--api-port] [--ingress-port] [--bind] [--host] [--log-level]`
- `tommy providers [plugin|plugin/provider] [--json] [-c tommy.toml]` — prints
  descriptions, endpoints and snippets rendered against the ports the current
  configuration would bind.

## Checklist for a plugin or provider task

1. `Description()` on the plugin and on every provider: one or two real sentences.
2. Every mounted ingress route is declared in `Endpoints()`, with a description,
   and every declared endpoint is mounted.
3. At least one `Snippet` that works from a cold start, written as a template
   over `SnippetCtx` — never a hardcoded port.
4. `plugintest.Conformance(t, p)` in the package tests.
5. A `README.md` in the directory carrying the same snippet.
6. Bytes in `blob.BlobStore`, never inline in an event. `Raw` always populated.
7. Read-back endpoints serve from the `Store`, so an SDK that writes then fetches
   sees its own write.
8. Never import another provider's package.
9. A listener provider implements `AddressableProvider` and treats `port: 0` as
   ephemeral, so tests never bind a well-known port.
10. A bespoke tab renders the shared `how-to-test` component from
    `ui.ShellFrom(r).Info()`, open when the tab is empty.
11. Anything captured is untrusted: interpolate as a plain string through
    `html/template`, never `template.HTML`; check URLs against a scheme allowlist
    before they reach an `href`/`src`; keep captured HTML out of the page DOM.
12. Verify wire formats against **live vendor documentation**, and test a wire
    protocol with a **real client over a socket** - both have repeatedly caught
    errors that hand-built tests did not.
13. Keep the CLI level with the config: a new plugin gets a `tommy <plugin>`
    subcommand, a new provider is selectable through `--enabled-providers`, and a
    provider option worth setting gets a flag. Nothing should be reachable only
    through a config file.
