package hl7

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

// MessageEnvelope is what /messages returns: the parsed message plus the event
// fields a client needs to correlate it with the generic event log, and the
// derived labels so a client never has to re-derive them from the tree.
type MessageEnvelope struct {
	ID         event.ID       `json:"id"`
	ReceivedAt time.Time      `json:"received_at"`
	Provider   string         `json:"provider"`
	Type       string         `json:"type"`
	Meta       map[string]any `json:"meta,omitempty"`
	// URL is this message's own page in the UI: the link to open, or to print
	// in a log line, once something has been sent.
	URL string `json:"url,omitempty"`

	// Title and Preview are what the list and the event log show, computed
	// once here so every surface agrees.
	Title   string `json:"title"`
	Preview string `json:"preview,omitempty"`
	// RawURL fetches the message exactly as it arrived.
	RawURL string `json:"raw_url"`

	Message *Message `json:"message"`
}

// NewMessageEnvelope builds the API view of a captured message.
func NewMessageEnvelope(base string, c Captured) MessageEnvelope {
	return MessageEnvelope{
		ID:         c.Event.ID,
		ReceivedAt: c.Event.ReceivedAt,
		Provider:   c.Event.Provider,
		Type:       c.Event.Type,
		Meta:       c.Event.Meta,
		Title:      c.Message.Title(),
		Preview:    c.Message.Preview(),
		RawURL:     base + "/messages/" + string(c.Event.ID) + "/raw",
		Message:    c.Message,
	}
}

// apiHandler serves /api/v1/hl7/.
type apiHandler struct {
	deps plugin.Deps
	// base is the absolute prefix the routes are mounted at, used to build the
	// raw URLs. Overridable so a test can mount the handler bare.
	base string
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

// filters are the HL7-specific narrowings, applied after the store's own.
type filters struct {
	messageType string
	controlID   string
	sending     string
	receiving   string
	segment     string
}

func parseFilters(r *http.Request) filters {
	v := r.URL.Query()
	return filters{
		messageType: strings.TrimSpace(v.Get("message_type")),
		controlID:   strings.TrimSpace(v.Get("control_id")),
		sending:     strings.TrimSpace(v.Get("sending_application")),
		receiving:   strings.TrimSpace(v.Get("receiving_application")),
		segment:     strings.TrimSpace(v.Get("segment")),
	}
}

// match narrows a message. message_type accepts the whole thing ("ADT^A01"),
// just the code ("ADT") or just the trigger event ("A01"), because all three
// are how people describe the messages they are hunting for.
func (f filters) match(m *Message) bool {
	if f.messageType != "" {
		if !strings.EqualFold(f.messageType, m.Header.MessageType()) &&
			!strings.EqualFold(f.messageType, m.Header.Code) &&
			!strings.EqualFold(f.messageType, m.Header.TriggerEvent) {
			return false
		}
	}
	if f.controlID != "" && !strings.EqualFold(f.controlID, m.Header.ControlID) {
		return false
	}
	if f.sending != "" && !strings.EqualFold(f.sending, m.Header.SendingApplication) {
		return false
	}
	if f.receiving != "" && !strings.EqualFold(f.receiving, m.Header.ReceivingApplication) {
		return false
	}
	if f.segment != "" && len(m.SegmentsByID(f.segment)) == 0 {
		return false
	}
	return true
}

func (h *apiHandler) list(w http.ResponseWriter, r *http.Request) {
	q, err := coreapi.ParseQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// This is the hl7 plugin whatever the caller asked for, and paging is
	// applied after the HL7-specific filters so a limit never counts messages
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

// raw serves the message exactly as it arrived, which is what somebody
// comparing tommy's parse against another engine's actually needs.
//
// It is served as text/plain with nosniff: an HL7 message is attacker-supplied
// content as far as this process is concerned, and a browser that sniffed it
// into HTML would be running whatever a free-text OBX carried.
func (h *apiHandler) raw(w http.ResponseWriter, r *http.Request) {
	e, ok := h.eventOf(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `inline; filename="`+string(e.ID)+`.hl7"`)
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

// captured resolves the {id} path value into a captured HL7 message.
func (h *apiHandler) captured(w http.ResponseWriter, r *http.Request) (Captured, bool) {
	e, ok := h.eventOf(w, r)
	if !ok {
		return Captured{}, false
	}
	m, ok := MessageOf(e)
	if !ok {
		writeError(w, http.StatusNotFound, "event "+string(e.ID)+" carries no HL7 message")
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
