// Package api serves the generic /api/v1 surface: health, discovery, events and
// the event stream. Plugin-specific routes are mounted under /api/v1/<plugin>/.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/server/sse"
	"github.com/can3p/tommy/core/server/ui"
	"github.com/can3p/tommy/core/store"
)

// Prefix is where the API is mounted.
const Prefix = "/api/v1"

// Options configures the API server.
type Options struct {
	Store      store.Store
	Blobs      blob.BlobStore
	Registry   *plugin.Registry
	Deps       plugin.Deps
	Logger     *slog.Logger
	Version    string
	StartedAt  time.Time
	SnippetCtx func() plugin.SnippetCtx
}

// API is the generic API server.
type API struct {
	opts Options
	mux  *http.ServeMux

	// routes is every route the core itself mounts, which is what the OpenAPI
	// description is checked against. A plugin's own routes are not here: they
	// are the plugin's surface, and the description covers the events API.
	routes []string
}

// New builds the API and mounts every plugin's own routes.
func New(opts Options) (*API, error) {
	if opts.Store == nil {
		return nil, errors.New("api: Store is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.StartedAt.IsZero() {
		opts.StartedAt = time.Now()
	}
	if opts.SnippetCtx == nil {
		opts.SnippetCtx = func() plugin.SnippetCtx { return plugin.SnippetCtx{} }
	}

	a := &API{opts: opts, mux: http.NewServeMux()}
	// Registered through handle rather than on the mux directly, so Routes()
	// reports what is actually mounted. The OpenAPI drift test compares that
	// against what coreEndpoints() and the plugins declare, and a comparison
	// of a declaration with itself would prove nothing.
	a.handle("GET /health", a.health)
	a.handle("GET /plugins", a.plugins)
	a.handle("GET /events", a.listEvents)
	a.handle("GET /events/stream", a.streamEvents)
	a.handle("GET /events/{id}", a.getEvent)
	a.handle("DELETE /events", a.clearEvents)
	a.handle("DELETE /events/{id}", a.deleteEvent)
	a.handle("GET /blobs/{id}", a.getBlob)
	a.handle("GET /openapi.json", a.openapi)

	if opts.Registry != nil {
		for _, p := range opts.Registry.Plugins() {
			sub := http.NewServeMux()
			d := opts.Deps.Normalize()
			d.Logger = d.Logger.With("plugin", p.Name())
			p.RegisterAPI(sub, d)
			prefix := "/" + p.Name()
			mounted := a.withOrigin(http.StripPrefix(prefix, sub))
			a.mux.Handle(prefix+"/", mounted)
			a.mux.Handle(prefix+"/{$}", mounted)
		}
	}
	sort.Strings(a.routes)
	return a, nil
}

// handle mounts a core route and records it.
func (a *API) handle(pattern string, h http.HandlerFunc) {
	a.routes = append(a.routes, pattern)
	a.mux.HandleFunc(pattern, h)
}

// Routes returns the core routes mounted under /api/v1, as "METHOD /path". It
// is what the OpenAPI drift test compares the description against: a described
// route that is not mounted is a promise the server does not keep.
func (a *API) Routes() []string { return append([]string(nil), a.routes...) }

// Handler returns the API handler, expecting to be mounted at Prefix.
func (a *API) Handler() http.Handler { return a.mux }

// Mount registers the API on a mux under Prefix.
func (a *API) Mount(mux plugin.Mux) {
	mux.Handle(Prefix+"/", http.StripPrefix(Prefix, a.mux))
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	plugins := []string{}
	if a.opts.Registry != nil {
		plugins = a.opts.Registry.SortedNames()
	}
	// A struct rather than a map, so the OpenAPI schema is generated from the
	// same thing the handler writes.
	body := HealthInfo{
		Status:  "ok",
		Uptime:  time.Since(a.opts.StartedAt).Round(time.Millisecond).String(),
		Plugins: plugins,
		Version: a.opts.Version,
	}
	if counter, ok := a.opts.Store.(interface{ Len() int }); ok {
		n := counter.Len()
		body.Events = &n
	}
	writeJSON(w, http.StatusOK, body)
}

func (a *API) plugins(w http.ResponseWriter, r *http.Request) {
	if a.opts.Registry == nil {
		writeJSON(w, http.StatusOK, []plugin.PluginInfo{})
		return
	}
	info, err := a.opts.Registry.Describe(a.opts.SnippetCtx())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (a *API) listEvents(w http.ResponseWriter, r *http.Request) {
	q, err := ParseQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	events, err := a.opts.Store.List(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Raw bodies are omitted from listings: they can be megabytes each. Fetch a
	// single event, or pass include_raw=1, to get them.
	if !boolParam(r, "include_raw") {
		stripped := make([]*event.Event, len(events))
		for i, e := range events {
			stripped[i] = e.WithoutRawBody()
		}
		events = stripped
	}
	writeJSON(w, http.StatusOK, ui.WithURLs(events, a.origin(r)))
}

func (a *API) getEvent(w http.ResponseWriter, r *http.Request) {
	e, err := a.opts.Store.Get(r.Context(), event.ID(r.PathValue("id")))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "event not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ui.WithURL(e, a.origin(r)))
}

func (a *API) streamEvents(w http.ResponseWriter, r *http.Request) {
	q, err := ParseQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ch := a.opts.Store.Subscribe(r.Context())
	origin := a.origin(r)
	sse.Stream(w, r, ch, sse.Options{
		Filter:   q,
		Envelope: func(e *event.Event) any { return ui.WithURL(e, origin) },
	})
}

func (a *API) clearEvents(w http.ResponseWriter, r *http.Request) {
	pluginName := r.URL.Query().Get("plugin")
	if err := a.opts.Store.Clear(r.Context(), pluginName); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) deleteEvent(w http.ResponseWriter, r *http.Request) {
	err := a.opts.Store.Delete(r.Context(), event.ID(r.PathValue("id")))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "event not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) getBlob(w http.ResponseWriter, r *http.Request) {
	if a.opts.Blobs == nil {
		writeError(w, http.StatusNotFound, "no blob store")
		return
	}
	rc, ref, err := a.opts.Blobs.Open(r.Context(), r.PathValue("id"))
	if errors.Is(err, blob.ErrNotFound) {
		writeError(w, http.StatusNotFound, "blob not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = rc.Close() }()

	ct := ref.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	if ref.Filename != "" {
		disposition := "attachment"
		if boolParam(r, "inline") {
			disposition = "inline"
		}
		w.Header().Set("Content-Disposition", disposition+"; filename="+strconv.Quote(ref.Filename))
	}
	http.ServeContent(w, r, ref.Filename, time.Time{}, rc)
}

// ParseQuery turns the standard filter query parameters into a store.Query.
func ParseQuery(r *http.Request) (store.Query, error) {
	v := r.URL.Query()
	q := store.Query{
		Plugin:   v.Get("plugin"),
		Provider: v.Get("provider"),
		Type:     v.Get("type"),
		Search:   v.Get("search"),
	}
	if s := v.Get("since"); s != "" {
		since, err := parseSince(s)
		if err != nil {
			return q, err
		}
		q.Since = since
	}
	var err error
	if q.Limit, err = intParam(v.Get("limit"), "limit"); err != nil {
		return q, err
	}
	if q.Offset, err = intParam(v.Get("offset"), "offset"); err != nil {
		return q, err
	}
	return q, nil
}

// parseSince accepts an RFC3339 timestamp, a unix millisecond count, or a
// relative duration like "5m" meaning "in the last five minutes".
func parseSince(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms), nil
	}
	return time.Time{}, errors.New("since must be an RFC3339 timestamp, a duration like 5m, or unix milliseconds")
}

func intParam(s, name string) (int, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, errors.New(name + " must be an integer")
	}
	if n < 0 {
		return 0, errors.New(name + " must not be negative")
	}
	return n, nil
}

func boolParam(r *http.Request, name string) bool {
	v := strings.ToLower(r.URL.Query().Get(name))
	return v == "1" || v == "true" || v == "yes"
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, Error{Error: msg})
}
