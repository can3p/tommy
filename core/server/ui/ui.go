// Package ui serves tommy's web shell: the tab bar built from the plugin
// registry, one SSE connection per page, the "How to test" panel, and the
// generic event view any plugin gets for free.
//
// The shell deliberately makes no assumption about a tab's shape. A plugin
// renders whatever view suits its content - that is the whole reason tabs
// exist - and composes it from core/server/ui/components.
package ui

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/server/sse"
	"github.com/can3p/tommy/core/server/ui/components"
	"github.com/can3p/tommy/core/store"
)

// Prefix is where the UI is mounted.
const Prefix = "/ui"

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// StaticFS exposes the vendored assets (htmx, the stylesheet, the favicon).
func StaticFS() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // impossible: the directory is embedded above
	}
	return sub
}

// Tab is one entry of the tab bar.
type Tab struct {
	Name   string
	Title  string
	Href   string
	Active bool
}

// Page is the data the layout renders. Body is already-rendered HTML, so a
// plugin can build its tab with its own template set and still get the shell.
type Page struct {
	Title     string
	Active    string
	Tabs      []Tab
	Body      template.HTML
	UIBase    string
	APIBase   string
	StreamURL string
	Version   string
}

// Options configures the UI server.
type Options struct {
	Store    store.Store
	Blobs    blob.BlobStore
	Registry *plugin.Registry
	Deps     plugin.Deps
	Logger   *slog.Logger
	Version  string

	// APIBase is where the browser reaches the API. Defaults to /api/v1, which
	// is right whenever the API shares the UI listener.
	APIBase string

	// SnippetCtx supplies the live addresses snippets render against.
	SnippetCtx func() plugin.SnippetCtx
}

// UI is the web shell.
type UI struct {
	opts Options
	mux  *http.ServeMux
	tpl  *template.Template
	log  *slog.Logger

	// overview is the cross-plugin event view, kept because the event page
	// hands htmx fragment requests back to it.
	overview *eventViewHandler
}

// New builds the shell and mounts every enabled plugin's tab. A plugin that
// does not register a handler for its own tab root gets the generic event view.
func New(opts Options) (*UI, error) {
	if opts.Store == nil {
		return nil, errors.New("ui: Store is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.APIBase == "" {
		opts.APIBase = "/api/v1"
	}
	if opts.SnippetCtx == nil {
		opts.SnippetCtx = func() plugin.SnippetCtx { return plugin.SnippetCtx{} }
	}

	tpl, err := Templates()
	if err != nil {
		return nil, err
	}

	u := &UI{opts: opts, mux: http.NewServeMux(), tpl: tpl, log: opts.Logger}

	u.mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler()))

	u.mux.HandleFunc("GET /stream", u.stream)

	// The cross-plugin overview, which is also what an empty tommy shows. It is
	// the catch-all, so /static/ and the plugin prefixes still win: net/http
	// picks the most specific pattern.
	overviewMux := http.NewServeMux()
	u.overview = &eventViewHandler{ui: u, plugin: "", base: Prefix, title: "All events"}
	u.overview.mount(overviewMux, "")
	u.mux.Handle("/", u.withShell("", overviewMux))

	// One event, on a page of its own. Registered ahead of the overview
	// because a browser asking for this URL wants the event, not the list it
	// happens to be in; the handler still returns the overview's fragment when
	// htmx is the one asking.
	u.mux.HandleFunc("GET "+EventPath+"{id}", (&eventPageHandler{ui: u}).serve)

	if opts.Registry != nil {
		for _, p := range opts.Registry.Plugins() {
			if err := u.mountPlugin(p); err != nil {
				return nil, err
			}
		}
	}
	return u, nil
}

// Templates returns the shell template set: the layout plus every component,
// with the render helper bound.
func Templates() (*template.Template, error) {
	tpl, err := template.New("ui").Funcs(components.FuncMap()).ParseFS(components.FS(), "*.html")
	if err != nil {
		return nil, err
	}
	tpl, err = tpl.ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return components.Bind(tpl), nil
}

// PluginTemplates returns a template set carrying every component plus the
// plugin's own templates, ready to execute. This is what a plugin tab uses.
func PluginTemplates(pluginFS fs.FS, patterns ...string) (*template.Template, error) {
	if len(patterns) == 0 {
		patterns = []string{"*.html"}
	}
	tpl, err := template.New("plugin").Funcs(components.FuncMap()).ParseFS(components.FS(), "*.html")
	if err != nil {
		return nil, err
	}
	if pluginFS != nil {
		if tpl, err = tpl.ParseFS(pluginFS, patterns...); err != nil {
			return nil, err
		}
	}
	return components.Bind(tpl), nil
}

func staticHandler() http.Handler {
	fileServer := http.FileServerFS(StaticFS())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assets are versioned with the binary, so a short cache is safe and
		// keeps a busy SSE page from refetching htmx on every navigation.
		w.Header().Set("Cache-Control", "public, max-age=300")
		fileServer.ServeHTTP(w, r)
	})
}

// Handler returns the UI handler, expecting to be mounted at Prefix.
func (u *UI) Handler() http.Handler { return u.mux }

// Mount registers the UI on a mux under Prefix, plus a redirect from /.
func (u *UI) Mount(mux plugin.Mux) {
	mux.Handle(Prefix+"/", http.StripPrefix(Prefix, u.mux))
	mux.Handle(Prefix, http.RedirectHandler(Prefix+"/", http.StatusMovedPermanently))
	mux.Handle("GET /{$}", http.RedirectHandler(Prefix+"/", http.StatusFound))
}

// mountPlugin gives a plugin its own mux, then fills in whatever it left out.
func (u *UI) mountPlugin(p plugin.Plugin) error {
	sub := http.NewServeMux()
	d := u.opts.Deps.Normalize()
	d.Logger = d.Logger.With("plugin", p.Name())
	p.RegisterUI(sub, d)

	base := Prefix + "/" + p.Name()
	generic := &eventViewHandler{ui: u, plugin: p.Name(), base: base, title: p.Title()}

	// Anything the plugin did not claim falls back to the generic event view,
	// so a brand-new protocol plugin is useful with zero UI code and a bespoke
	// tab is an upgrade rather than a prerequisite.
	generic.mountMissing(sub)

	prefix := "/" + p.Name()
	u.mux.Handle(prefix+"/", http.StripPrefix(prefix, u.withShell(p.Name(), sub)))
	u.mux.Handle(prefix, http.RedirectHandler(base+"/", http.StatusMovedPermanently))
	return nil
}

// withShell puts the shell in the request context so a plugin handler can call
// ui.Render without holding a reference to the UI server.
func (u *UI) withShell(active string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), shellKey{}, u.shell(active))))
	})
}

type shellKey struct{}

// Shell is what a plugin handler needs to render a full page: the tab bar, the
// base URLs, and the layout template.
type Shell struct {
	Active  string
	Tabs    []Tab
	UIBase  string
	APIBase string
	Stream  string
	Version string

	tpl  *template.Template
	info func() []plugin.PluginInfo
}

// Info describes the plugins in scope of the active tab: just that plugin, or
// every one of them on the overview. A bespoke tab needs this to render the
// how-to-test panel and an empty state carrying runnable snippets, which the
// generic view gets for free - without it, writing your own tab means losing
// them. Evaluated on demand, since rendering snippets is not free and most
// requests never ask.
func (s *Shell) Info() []plugin.PluginInfo {
	if s == nil || s.info == nil {
		return nil
	}
	return s.info()
}

func (u *UI) shell(active string) *Shell {
	return &Shell{
		Active:  active,
		Tabs:    u.tabs(active),
		UIBase:  Prefix,
		APIBase: u.opts.APIBase,
		Stream:  Prefix + "/stream",
		Version: u.opts.Version,
		tpl:     u.tpl,
		info:    func() []plugin.PluginInfo { return u.info(active) },
	}
}

func (u *UI) tabs(active string) []Tab {
	var tabs []Tab
	if u.opts.Registry == nil {
		return tabs
	}
	for _, p := range u.opts.Registry.Plugins() {
		tabs = append(tabs, Tab{
			Name:   p.Name(),
			Title:  p.Title(),
			Href:   Prefix + "/" + p.Name() + "/",
			Active: p.Name() == active,
		})
	}
	return tabs
}

// Render writes a full page around body.
func (s *Shell) Render(w http.ResponseWriter, title string, body template.HTML) error {
	page := Page{
		Title:     title,
		Active:    s.Active,
		Tabs:      s.Tabs,
		Body:      body,
		UIBase:    s.UIBase,
		APIBase:   s.APIBase,
		StreamURL: s.Stream,
		Version:   s.Version,
	}
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, "layout", page); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := buf.WriteTo(w)
	return err
}

// ShellFrom returns the shell attached to a request. It never returns nil, so a
// plugin handler used outside the server still renders something.
func ShellFrom(r *http.Request) *Shell {
	if s, ok := r.Context().Value(shellKey{}).(*Shell); ok && s != nil {
		return s
	}
	tpl, err := Templates()
	if err != nil {
		tpl = template.Must(template.New("empty").Parse(`{{define "layout"}}{{.Body}}{{end}}`))
	}
	return &Shell{UIBase: Prefix, APIBase: "/api/v1", Stream: Prefix + "/stream", tpl: tpl}
}

// IsPartial reports whether htmx asked for a fragment rather than a page.
func IsPartial(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true"
}

// Render is the one-liner for a plugin handler: full page for a normal request,
// bare fragment for an htmx swap.
func Render(w http.ResponseWriter, r *http.Request, title string, body template.HTML) error {
	if IsPartial(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, err := w.Write([]byte(body))
		return err
	}
	return ShellFrom(r).Render(w, title, body)
}

func (u *UI) stream(w http.ResponseWriter, r *http.Request) {
	q := store.Query{Plugin: r.URL.Query().Get("plugin")}
	ch := u.opts.Store.Subscribe(r.Context())
	origin := u.origin(r)
	sse.Stream(w, r, ch, sse.Options{
		Filter: q,
		// The same shape the API stream sends, so a script can subscribe to
		// either one and follow the link without knowing which it got.
		Envelope: func(e *event.Event) any { return WithURL(e, origin) },
	})
}

// render executes one component template into a fragment.
func (u *UI) render(name string, data any) (template.HTML, error) {
	var buf bytes.Buffer
	if err := u.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil //nolint:gosec // our own templates
}

// plugin looks an enabled plugin up by name.
func (u *UI) plugin(name string) (plugin.Plugin, bool) {
	if u.opts.Registry == nil || name == "" {
		return nil, false
	}
	return u.opts.Registry.Plugin(name)
}

// Origin is the absolute origin a link handed to somebody else should carry.
//
// The configured UI URL wins, because the caller is often talking to the
// ingress on another port entirely and its own idea of the host would send it
// somewhere with no UI on it. The request's host is the fallback for a tommy
// that was never told what it is called, and "" - a site-relative link - is
// the fallback for a request that has no host either.
func Origin(uiURL string, r *http.Request) string {
	if uiURL != "" {
		return uiURL
	}
	if r == nil || r.Host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (u *UI) origin(r *http.Request) string {
	return Origin(u.opts.SnippetCtx().UIURL, r)
}

// info describes the plugins in scope of a view: one plugin, or all of them.
func (u *UI) info(pluginName string) []plugin.PluginInfo {
	if u.opts.Registry == nil {
		return nil
	}
	all, err := u.opts.Registry.Describe(u.opts.SnippetCtx())
	if err != nil {
		u.log.Warn("cannot describe registry", "err", err)
		return nil
	}
	if pluginName == "" {
		return all
	}
	for _, p := range all {
		if p.Name == pluginName {
			return []plugin.PluginInfo{p}
		}
	}
	return nil
}

// distinct collects the type and provider values present in a set of events, so
// the filter dropdowns only ever offer choices that would match something.
func distinct(events []*event.Event) (types, providers []string) {
	tset, pset := map[string]bool{}, map[string]bool{}
	for _, e := range events {
		if e.Type != "" {
			tset[e.Type] = true
		}
		if e.Provider != "" {
			pset[e.Provider] = true
		}
	}
	for t := range tset {
		types = append(types, t)
	}
	for p := range pset {
		providers = append(providers, p)
	}
	sort.Strings(types)
	sort.Strings(providers)
	return types, providers
}

func trimTrailingSlash(s string) string { return strings.TrimSuffix(s, "/") }
