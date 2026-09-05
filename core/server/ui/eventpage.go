package ui

import (
	"bytes"
	"errors"
	"html/template"
	"net/http"
	"strings"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/server/ui/components"
	"github.com/can3p/tommy/core/store"
)

// EventPath is the path of one event's own page, relative to the UI mount.
const EventPath = "/events/"

// EventURL is the canonical page for one captured event.
//
// origin is an absolute origin ("http://localhost:8811"), which is what a link
// meant to be pasted somewhere else needs; pass "" for a site-relative path,
// which is right for a link inside a page tommy served itself.
//
// It lives here rather than in the API because the UI owns the route:
// everything that hands out this link - the REST API, the event stream, a
// plugin's own read-back API - builds it from this one function, so they
// cannot disagree about where an event can be read.
func EventURL(origin string, id event.ID) string {
	return strings.TrimSuffix(origin, "/") + Prefix + EventPath + string(id)
}

// EventJSON is an event on the wire: everything the store holds, plus the URL
// of the page that shows it.
//
// The URL is deliberately not a field of event.Event. Events are immutable and
// stored, so a URL baked into one would put a UI concern in the store contract
// and would be wrong the moment a port moved. Embedding the event inlines its
// fields, so the wire shape gains exactly one key.
//
// It lives beside EventURL rather than in the API because the REST routes, the
// API stream and the UI stream all send it, and one definition is what keeps
// those three the same shape.
type EventJSON struct {
	*event.Event
	URL string `json:"url,omitempty"`
}

// WithURL wraps one event for the wire. origin is as EventURL takes it.
func WithURL(e *event.Event, origin string) EventJSON {
	return EventJSON{Event: e, URL: EventURL(origin, e.ID)}
}

// WithURLs wraps a listing for the wire.
func WithURLs(events []*event.Event, origin string) []EventJSON {
	out := make([]EventJSON, len(events))
	for i, e := range events {
		out[i] = WithURL(e, origin)
	}
	return out
}

// eventPageHandler serves an event on a page of its own.
//
// The route is the one htmx already fetches the detail fragment from, so
// selecting an event in a tab and opening its page are the same URL: htmx asks
// for a fragment and gets one, a browser asks for a document and gets a page.
type eventPageHandler struct{ ui *UI }

func (h *eventPageHandler) serve(w http.ResponseWriter, r *http.Request) {
	// An htmx swap wants the fragment the generic view has always returned.
	if IsPartial(r) {
		h.ui.overview.detail(w, r)
		return
	}

	id := event.ID(r.PathValue("id"))
	e, err := h.ui.opts.Store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		h.notFound(w, id)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	page := components.EventPage{
		Event:       e,
		Body:        h.body(r, e),
		PluginTitle: e.Plugin,
		ShareURL:    EventURL(h.ui.origin(r), e.ID),
		APIHref:     h.ui.opts.APIBase + "/events/" + string(e.ID),
	}
	if p, ok := h.ui.plugin(e.Plugin); ok {
		page.PluginTitle = p.Title()
		page.PluginHref = Prefix + "/" + p.Name() + "/"
	}
	page.Newer, page.Older = h.steps(r, e)

	body, err := h.ui.render("event-page", page)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.ui.shell(e.Plugin).Render(w, page.Heading(), body); err != nil {
		h.ui.log.Warn("render event page", "err", err)
	}
}

// body is the detail pane of the owning plugin, or the generic inspector.
//
// The plugin's view is fetched by dispatching an in-process request to the
// fragment route the plugin already serves - the same one htmx uses - so a
// mail event shows a mail without any plugin implementing a second interface.
// A plugin that does not answer, or answers with nothing, falls back to the
// generic inspector rather than to a blank page.
func (h *eventPageHandler) body(r *http.Request, e *event.Event) template.HTML {
	if frag, ok := h.pluginFragment(r, e); ok {
		return frag
	}
	generic, err := h.ui.render("event-detail", e)
	if err != nil {
		h.ui.log.Warn("render event detail", "err", err)
		return ""
	}
	return generic
}

func (h *eventPageHandler) pluginFragment(r *http.Request, e *event.Event) (template.HTML, bool) {
	if e.Plugin == "" {
		return "", false
	}
	if _, ok := h.ui.plugin(e.Plugin); !ok {
		return "", false
	}
	target := "/" + e.Plugin + EventPath + string(e.ID)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		return "", false
	}
	// Ask as htmx would: every plugin's detail route answers a partial with
	// the fragment, and a document with its whole tab.
	req.Header.Set("HX-Request", "true")

	rec := &fragmentRecorder{header: http.Header{}}
	h.ui.mux.ServeHTTP(rec, req)
	if rec.status != 0 && rec.status != http.StatusOK {
		return "", false
	}
	if strings.TrimSpace(rec.body.String()) == "" {
		return "", false
	}
	// The fragment is markup tommy rendered itself, from the same escaping
	// rules as every other view - it is not the captured content.
	return template.HTML(rec.body.String()), true //nolint:gosec // rendered by our own templates
}

// fragmentRecorder captures an in-process response. net/http/httptest would do
// this, but it is a testing package and this runs in the shipped binary.
type fragmentRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (f *fragmentRecorder) Header() http.Header { return f.header }

func (f *fragmentRecorder) Write(p []byte) (int, error) {
	if f.status == 0 {
		f.status = http.StatusOK
	}
	return f.body.Write(p)
}

func (f *fragmentRecorder) WriteHeader(status int) {
	if f.status == 0 {
		f.status = status
	}
}

// steps walk the events of the same plugin, so an inbox can be read from the
// page rather than from the list. The store lists newest first.
func (h *eventPageHandler) steps(r *http.Request, e *event.Event) (newer, older *components.EventPageLink) {
	siblings, err := h.ui.opts.Store.List(r.Context(), store.Query{Plugin: e.Plugin})
	if err != nil {
		h.ui.log.Warn("list neighboring events", "err", err)
		return nil, nil
	}
	at := -1
	for i, s := range siblings {
		if s.ID == e.ID {
			at = i
			break
		}
	}
	if at < 0 {
		return nil, nil
	}
	link := func(s *event.Event) *components.EventPageLink {
		title := s.Summary.Title
		if title == "" {
			title = s.Type
		}
		return &components.EventPageLink{Href: EventURL("", s.ID), Title: title}
	}
	if at > 0 {
		newer = link(siblings[at-1])
	}
	if at+1 < len(siblings) {
		older = link(siblings[at+1])
	}
	return newer, older
}

// notFound is what an expired link looks like. The store is a ring buffer, so
// a link pasted into an issue outlives the event it points at, and saying so
// is more useful than a bare 404.
func (h *eventPageHandler) notFound(w http.ResponseWriter, id event.ID) {
	body, err := h.ui.render("empty-state", components.EmptyState{
		Title: "No such event",
		Message: "Event " + string(id) + " is not in the store. It was either cleared, " +
			"or evicted once this plugin's buffer filled up.",
	})
	if err != nil {
		http.Error(w, "event not found", http.StatusNotFound)
		return
	}
	// Content-Type first: WriteHeader freezes the header map, and Render sets
	// it too late to matter once the status has gone out.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	if err := h.ui.shell("").Render(w, "Event not found", body); err != nil {
		h.ui.log.Warn("render event page", "err", err)
	}
}
