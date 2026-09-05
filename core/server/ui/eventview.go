package ui

import (
	"bytes"
	"html/template"
	"net/http"
	"strings"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/server/ui/components"
	"github.com/can3p/tommy/core/store"
)

// DefaultListLimit caps how many events the generic list renders at once.
const DefaultListLimit = 200

// eventViewHandler serves the generic event view: a filterable event table plus
// a raw/JSON/hex inspector. It is mounted for the cross-plugin overview and for
// every plugin that does not override the corresponding route itself.
type eventViewHandler struct {
	ui     *UI
	plugin string // "" for the cross-plugin overview
	base   string // "/ui" or "/ui/<plugin>"
	title  string
}

// routes are the patterns the generic view needs. A plugin that registers any
// of them keeps its own; the rest fall back here.
func (h *eventViewHandler) routes() []struct {
	pattern string
	handler http.HandlerFunc
} {
	return []struct {
		pattern string
		handler http.HandlerFunc
	}{
		{"GET /{$}", h.page},
		{"GET /list", h.list},
		{"GET /events/{id}", h.detail},
		{"DELETE /events", h.clear},
	}
}

func (h *eventViewHandler) mount(mux *http.ServeMux, _ string) {
	for _, r := range h.routes() {
		mux.HandleFunc(r.pattern, r.handler)
	}
}

// mountMissing registers only the routes the plugin left unclaimed, probing the
// mux rather than guessing.
func (h *eventViewHandler) mountMissing(mux *http.ServeMux) {
	for _, r := range h.routes() {
		method, path := splitPattern(r.pattern)
		if handled(mux, method, path) {
			continue
		}
		mux.HandleFunc(r.pattern, r.handler)
	}
}

func splitPattern(pattern string) (method, path string) {
	method, path = "GET", pattern
	if i := strings.IndexByte(pattern, ' '); i > 0 {
		method, path = pattern[:i], pattern[i+1:]
	}
	// {$} means "exactly this path"; {id} is a wildcard we probe with a sample.
	path = strings.ReplaceAll(path, "{$}", "")
	path = strings.ReplaceAll(path, "{id}", "probe")
	if path == "" {
		path = "/"
	}
	return method, path
}

func handled(mux *http.ServeMux, method, path string) bool {
	req, err := http.NewRequest(method, "http://ui.invalid"+path, nil)
	if err != nil {
		return false
	}
	_, pattern := mux.Handler(req)
	return pattern != ""
}

func (h *eventViewHandler) view(r *http.Request) (components.EventView, error) {
	q := store.Query{
		Plugin:   h.plugin,
		Provider: r.URL.Query().Get("provider"),
		Type:     r.URL.Query().Get("type"),
		Search:   r.URL.Query().Get("search"),
		Limit:    DefaultListLimit,
	}
	events, err := h.ui.opts.Store.List(r.Context(), q)
	if err != nil {
		return components.EventView{}, err
	}

	// The dropdowns are built from an unfiltered listing, so narrowing the
	// filter never removes the option you would need to widen it again.
	all, err := h.ui.opts.Store.List(r.Context(), store.Query{Plugin: h.plugin})
	if err != nil {
		return components.EventView{}, err
	}
	types, providers := distinct(all)

	return components.EventView{
		Base:     trimTrailingSlash(h.base),
		PageBase: Prefix + EventPath,
		Plugin:   h.plugin,
		Title:    h.title,
		Filter: components.EventFilter{
			Search:   q.Search,
			Type:     q.Type,
			Provider: q.Provider,
		},
		Events:        events,
		Info:          h.ui.info(h.plugin),
		Types:         types,
		ProviderNames: providers,
	}, nil
}

func (h *eventViewHandler) page(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if id := r.URL.Query().Get("event"); id != "" {
		if e, err := h.ui.opts.Store.Get(r.Context(), event.ID(id)); err == nil {
			v.Selected = e
		}
	}
	body, err := h.render("generic-event-view", v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := Render(w, r, h.title, body); err != nil {
		h.ui.log.Warn("render page", "err", err)
	}
}

func (h *eventViewHandler) list(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.writeFragment(w, "event-list", v)
}

func (h *eventViewHandler) detail(w http.ResponseWriter, r *http.Request) {
	e, err := h.ui.opts.Store.Get(r.Context(), event.ID(r.PathValue("id")))
	if err != nil {
		http.Error(w, "event not found", http.StatusNotFound)
		return
	}
	if !IsPartial(r) {
		// A deep link renders the whole page with the event selected, so an
		// event URL can be pasted into a bug report.
		v, verr := h.view(r)
		if verr != nil {
			http.Error(w, verr.Error(), http.StatusInternalServerError)
			return
		}
		v.Selected = e
		body, rerr := h.render("generic-event-view", v)
		if rerr != nil {
			http.Error(w, rerr.Error(), http.StatusInternalServerError)
			return
		}
		if err := ShellFrom(r).Render(w, h.title, body); err != nil {
			h.ui.log.Warn("render detail page", "err", err)
		}
		return
	}
	h.writeFragment(w, "event-detail", e)
}

func (h *eventViewHandler) clear(w http.ResponseWriter, r *http.Request) {
	if err := h.ui.opts.Store.Clear(r.Context(), h.plugin); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v, err := h.view(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.writeFragment(w, "event-list", v)
}

func (h *eventViewHandler) render(name string, data any) (template.HTML, error) {
	var buf bytes.Buffer
	if err := h.ui.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

func (h *eventViewHandler) writeFragment(w http.ResponseWriter, name string, data any) {
	body, err := h.render(name, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(body))
}
