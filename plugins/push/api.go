package push

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	coreapi "github.com/can3p/tommy/core/server/api"
	"github.com/can3p/tommy/core/store"
)

// APIBase is where the plugin's routes end up once the core has mounted them.
// The handlers see the prefix stripped, so they need it spelled out to build
// the links they hand back to clients.
const APIBase = APIPrefix

// MessageEnvelope is what /messages returns: the canonical push plus the event
// fields a client needs to correlate it with the generic event log, and the
// derived labels so a client never has to re-derive them.
//
// Displays and Explain are on the envelope rather than left implicit because
// they are the answer to the question this plugin exists for, and a client
// scripting an assertion ("this push should not have displayed") should not
// have to reimplement the rule.
type MessageEnvelope struct {
	ID         event.ID       `json:"id"`
	ReceivedAt time.Time      `json:"received_at"`
	Provider   string         `json:"provider"`
	Type       string         `json:"type"`
	Meta       map[string]any `json:"meta,omitempty"`
	// URL is this message's own page in the UI: the link to open, or to print
	// in a log line, once something has been sent.
	URL string `json:"url,omitempty"`

	Title    string `json:"title"`
	Preview  string `json:"preview,omitempty"`
	Displays bool   `json:"displays"`
	Explain  string `json:"explain,omitempty"`
	// RawURL fetches the request body exactly as it arrived.
	RawURL string `json:"raw_url"`

	Message *Message `json:"message"`
}

// NewMessageEnvelope builds the API view of a captured push.
func NewMessageEnvelope(base string, c Captured) MessageEnvelope {
	return MessageEnvelope{
		ID:         c.Event.ID,
		ReceivedAt: c.Event.ReceivedAt,
		Provider:   c.Event.Provider,
		Type:       c.Event.Type,
		Meta:       c.Event.Meta,
		Title:      c.Message.Title(),
		Preview:    c.Message.Preview(),
		Displays:   c.Message.Displays(),
		Explain:    c.Message.Kind.Explain(),
		RawURL:     base + "/messages/" + string(c.Event.ID) + "/raw",
		Message:    c.Message,
	}
}

// apiHandler serves /api/v1/push/.
type apiHandler struct {
	deps plugin.Deps
	// base is the absolute prefix the routes are mounted at, used to build the
	// raw URLs. Overridable so a test can mount the handler bare.
	base string
}

// APIEndpoints documents what RegisterAPI mounts, and is what this plugin's
// own OpenAPI description is generated from.
func (p *Plugin) APIEndpoints() []plugin.APIEndpoint {
	list := append(plugin.CommonListParams(),
		plugin.APIParam{Name: "displays", Description: "Only pushes that would show something on a lock screen, or only those that would not.", Type: "boolean"},
		plugin.APIParam{Name: "kind", Description: "alert, silent or background."},
		plugin.APIParam{Name: "target_kind", Description: "How the push was addressed: token, topic or condition."},
		plugin.APIParam{Name: "target", Description: "Substring of the target itself."},
		plugin.APIParam{Name: "app", Description: "The application or Firebase project the push was sent to."},
		plugin.APIParam{Name: "push_type", Description: "The APNs push type: alert, background, voip and the rest."},
		plugin.APIParam{Name: "priority", Description: "The priority, by level or by its raw value."},
		plugin.APIParam{Name: "data_key", Description: "Only pushes whose data payload carries this key."},
	)
	return []plugin.APIEndpoint{
		{Method: "GET", Path: "/messages", Description: "Every captured push, newest first. Paging is applied after the filters, so a limit never counts what a filter excluded.",
			Query: list, Response: []MessageEnvelope{}},
		{Method: "GET", Path: "/messages/{id}", Description: "One push, with the displays/explain verdict alongside the model.",
			Response: MessageEnvelope{}},
		{Method: "GET", Path: "/messages/{id}/raw", Description: "The request body exactly as it arrived, as text/plain with nosniff.",
			Produces: "text/plain"},
		{Method: "DELETE", Path: "/messages", Description: "Clear every captured push.",
			Status: http.StatusNoContent},
		{Method: "DELETE", Path: "/messages/{id}", Description: "Delete one captured push.",
			Status: http.StatusNoContent},
	}
}

func (h *apiHandler) mount(mux plugin.Mux) {
	if h.base == "" {
		h.base = APIBase
	}
	mux.HandleFunc("GET /messages", h.list)
	mux.HandleFunc("GET /messages/{id}", h.get)
	mux.HandleFunc("GET /messages/{id}/raw", h.raw)
	mux.HandleFunc("DELETE /messages", h.clear)
	mux.HandleFunc("DELETE /messages/{id}", h.delete)
}

// filters are the push-specific narrowings, applied after the store's own.
type filters struct {
	kind       string
	targetKind string
	target     string
	app        string
	pushType   string
	priority   string
	dataKey    string
	// displays narrows to pushes that do or do not put something on screen.
	// "Show me everything that displayed nothing" is the query this plugin is
	// for, and neither a kind nor a search string expresses it.
	displays *bool
}

func parseFilters(r *http.Request) filters {
	v := r.URL.Query()
	f := filters{
		kind:       strings.TrimSpace(v.Get("kind")),
		targetKind: strings.TrimSpace(v.Get("target_kind")),
		target:     strings.TrimSpace(v.Get("target")),
		app:        strings.TrimSpace(v.Get("app")),
		pushType:   strings.TrimSpace(v.Get("push_type")),
		priority:   strings.TrimSpace(v.Get("priority")),
		dataKey:    strings.TrimSpace(v.Get("data_key")),
	}
	switch strings.ToLower(strings.TrimSpace(v.Get("displays"))) {
	case "1", "true", "yes":
		yes := true
		f.displays = &yes
	case "0", "false", "no":
		no := false
		f.displays = &no
	}
	return f
}

// match narrows a message. target matches on a substring, because a device
// token is long enough that nobody pastes the whole thing.
func (f filters) match(m *Message) bool {
	if f.kind != "" && !strings.EqualFold(f.kind, string(m.Kind)) {
		return false
	}
	if f.targetKind != "" && !strings.EqualFold(f.targetKind, string(m.Target.Kind)) {
		return false
	}
	if f.target != "" && !strings.Contains(strings.ToLower(m.Target.Value), strings.ToLower(f.target)) {
		return false
	}
	if f.app != "" && !strings.EqualFold(f.app, m.App) {
		return false
	}
	if f.pushType != "" && !strings.EqualFold(f.pushType, m.PushType) {
		return false
	}
	if f.priority != "" && !strings.EqualFold(f.priority, string(m.Delivery.Priority)) &&
		!strings.EqualFold(f.priority, m.Delivery.PriorityRaw) {
		return false
	}
	if f.dataKey != "" && !hasKey(m.DataKeys(), f.dataKey) {
		return false
	}
	if f.displays != nil && *f.displays != m.Displays() {
		return false
	}
	return true
}

func hasKey(keys []string, want string) bool {
	for _, k := range keys {
		if strings.EqualFold(k, want) {
			return true
		}
	}
	return false
}

func (h *apiHandler) list(w http.ResponseWriter, r *http.Request) {
	q, err := coreapi.ParseQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// This is the push plugin whatever the caller asked for, and paging is
	// applied after the push-specific filters so a limit never counts messages
	// the caller excluded.
	q.Plugin = Name
	limit, offset := q.Limit, q.Offset
	q.Limit, q.Offset = 0, 0

	events, err := h.deps.Store.List(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	f := parseFilters(r)
	out := make([]MessageEnvelope, 0, len(events))
	for _, c := range Messages(events) {
		if !f.match(c.Message) {
			continue
		}
		v := NewMessageEnvelope(h.base, c)
		v.URL = coreapi.EventURL(r, c.Event.ID)
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, page(out, limit, offset))
}

func page(in []MessageEnvelope, limit, offset int) []MessageEnvelope {
	if offset >= len(in) {
		return []MessageEnvelope{}
	}
	if offset > 0 {
		in = in[offset:]
	}
	if limit > 0 && limit < len(in) {
		in = in[:limit]
	}
	return in
}

func (h *apiHandler) get(w http.ResponseWriter, r *http.Request) {
	c, ok := h.captured(w, r)
	if !ok {
		return
	}
	v := NewMessageEnvelope(h.base, c)
	v.URL = coreapi.EventURL(r, c.Event.ID)
	writeJSON(w, http.StatusOK, v)
}

// raw serves the request body exactly as it arrived, which is what somebody
// comparing tommy's reading against Apple's or Google's actually needs.
//
// It is served as text/plain with nosniff even though it is JSON: a push
// payload is attacker-supplied content as far as this process is concerned, and
// a browser that sniffed it into HTML would be running whatever a body carried.
func (h *apiHandler) raw(w http.ResponseWriter, r *http.Request) {
	e, ok := h.eventOf(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `inline; filename="`+string(e.ID)+`.json"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(e.Raw.Body)
}

// eventOf resolves the {id} path value into this plugin's event, writing the
// error response itself when it cannot.
func (h *apiHandler) eventOf(w http.ResponseWriter, r *http.Request) (*event.Event, bool) {
	e, err := h.deps.Store.Get(r.Context(), event.ID(r.PathValue("id")))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "message not found")
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if e.Plugin != Name {
		writeError(w, http.StatusNotFound, "message not found")
		return nil, false
	}
	return e, true
}

// captured resolves the {id} path value into a captured push.
func (h *apiHandler) captured(w http.ResponseWriter, r *http.Request) (Captured, bool) {
	e, ok := h.eventOf(w, r)
	if !ok {
		return Captured{}, false
	}
	m, ok := MessageOf(e)
	if !ok {
		writeError(w, http.StatusNotFound, "event "+string(e.ID)+" carries no push message")
		return Captured{}, false
	}
	return Captured{Event: e, Message: m}, true
}

func (h *apiHandler) clear(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Store.Clear(r.Context(), Name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *apiHandler) delete(w http.ResponseWriter, r *http.Request) {
	e, ok := h.eventOf(w, r)
	if !ok {
		return
	}
	if err := h.deps.Store.Delete(r.Context(), e.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
