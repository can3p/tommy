package sms

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	coreapi "github.com/can3p/tommy/core/server/api"
	"github.com/can3p/tommy/core/store"
)

// APIBase is where the plugin's routes end up once the core has mounted them.
// The handlers see the prefix stripped, so they need it to build the media URLs
// they hand back to clients.
const APIBase = coreapi.Prefix + "/" + Name

// MediaRef is one MMS attachment as the API reports it: enough to show it and a
// URL to fetch it from.
type MediaRef struct {
	Index       int    `json:"index"`
	ContentType string `json:"content_type,omitempty"`
	Filename    string `json:"filename,omitempty"`
	Size        int64  `json:"size,omitempty"`
	// URL streams the bytes from tommy when they were stored, and is the
	// provider's own URL when all we were given was a link.
	URL string `json:"url"`
	// Stored says which of the two the URL is, so a client knows whether the
	// bytes are actually here.
	Stored bool `json:"stored"`
}

// MessageEnvelope is what /messages returns: the canonical message plus the
// event fields a client needs to correlate it with the generic event log.
type MessageEnvelope struct {
	ID         event.ID       `json:"id"`
	ReceivedAt time.Time      `json:"received_at"`
	Provider   string         `json:"provider"`
	Type       string         `json:"type"`
	Meta       map[string]any `json:"meta,omitempty"`
	Media      []MediaRef     `json:"media,omitempty"`
	Message    *Message       `json:"message"`
}

// NewMessageEnvelope builds the API view of a captured message. base is the URL
// prefix the plugin API is mounted at.
func NewMessageEnvelope(base string, c Captured) MessageEnvelope {
	return MessageEnvelope{
		ID:         c.Event.ID,
		ReceivedAt: c.Event.ReceivedAt,
		Provider:   c.Event.Provider,
		Type:       c.Event.Type,
		Meta:       c.Event.Meta,
		Media:      MediaRefs(base, c.Event.ID, c.Message),
		Message:    c.Message,
	}
}

// MediaRefs turns a message's attachments into fetchable references.
func MediaRefs(base string, id event.ID, m *Message) []MediaRef {
	if len(m.Media) == 0 {
		return nil
	}
	out := make([]MediaRef, 0, len(m.Media))
	for i, media := range m.Media {
		ref := MediaRef{
			Index:       i,
			ContentType: media.Type(),
			Filename:    media.Name(),
			Size:        media.Size(),
			Stored:      media.Stored(),
		}
		if media.Stored() {
			ref.URL = base + "/messages/" + string(id) + "/media/" + strconv.Itoa(i)
		} else {
			ref.URL = media.URL
		}
		out = append(out, ref)
	}
	return out
}

// apiHandler serves /api/v1/sms/.
type apiHandler struct {
	deps plugin.Deps
	// base is the absolute prefix the routes are mounted at, used to build the
	// media URLs. Overridable so a test can mount the handler bare.
	base string
}

func (h *apiHandler) mount(mux plugin.Mux) {
	if h.base == "" {
		h.base = APIBase
	}
	mux.HandleFunc("GET /messages", h.list)
	mux.HandleFunc("GET /messages/{id}", h.get)
	mux.HandleFunc("GET /messages/{id}/media/{idx}", h.media)
	mux.HandleFunc("DELETE /messages", h.clear)
}

// filters are the sms-specific narrowings, applied after the store's own.
type filters struct {
	to        string
	from      string
	status    string
	direction string
	encoding  string
	mmsOnly   bool
	mmsSet    bool
}

func parseFilters(r *http.Request) filters {
	v := r.URL.Query()
	f := filters{
		to:        strings.TrimSpace(v.Get("to")),
		from:      strings.TrimSpace(v.Get("from")),
		status:    strings.ToLower(strings.TrimSpace(v.Get("status"))),
		direction: strings.ToLower(strings.TrimSpace(v.Get("direction"))),
		encoding:  strings.ToUpper(strings.TrimSpace(v.Get("encoding"))),
	}
	if raw := v.Get("mms"); raw != "" {
		f.mmsSet = true
		f.mmsOnly = raw == "1" || strings.EqualFold(raw, "true")
	}
	return f
}

func (f filters) match(m *Message) bool {
	if f.to != "" && !strings.EqualFold(f.to, m.To) {
		return false
	}
	if f.from != "" && !strings.EqualFold(f.from, m.Sender()) {
		return false
	}
	if f.status != "" && !strings.EqualFold(f.status, string(m.Status)) {
		return false
	}
	if f.direction != "" && !strings.EqualFold(f.direction, string(m.Direction)) {
		return false
	}
	if f.encoding != "" && !strings.EqualFold(f.encoding, string(m.Segments.Encoding)) {
		return false
	}
	if f.mmsSet && f.mmsOnly != m.IsMMS() {
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
	// This tab is the sms plugin, whatever the caller asked for, and paging is
	// applied after the sms-specific filters so a limit never counts messages
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
		out = append(out, NewMessageEnvelope(h.base, c))
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
	writeJSON(w, http.StatusOK, NewMessageEnvelope(h.base, c))
}

// captured resolves the {id} path value into a captured sms message, writing
// the error response itself when it cannot.
func (h *apiHandler) captured(w http.ResponseWriter, r *http.Request) (Captured, bool) {
	e, err := h.deps.Store.Get(r.Context(), event.ID(r.PathValue("id")))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "message not found")
		return Captured{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return Captured{}, false
	}
	if e.Plugin != Name {
		writeError(w, http.StatusNotFound, "message not found")
		return Captured{}, false
	}
	m, ok := MessageOf(e)
	if !ok {
		writeError(w, http.StatusNotFound, "event "+string(e.ID)+" carries no sms message")
		return Captured{}, false
	}
	return Captured{Event: e, Message: m}, true
}

// media streams one MMS attachment out of the blob store with its real content
// type, so a browser can show the picture the SDK thought it was sending.
func (h *apiHandler) media(w http.ResponseWriter, r *http.Request) {
	c, ok := h.captured(w, r)
	if !ok {
		return
	}
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil || idx < 0 || idx >= len(c.Message.Media) {
		writeError(w, http.StatusNotFound, "no media at that index")
		return
	}
	media := c.Message.Media[idx]
	if !media.Stored() {
		// The provider was handed a link rather than bytes, and tommy never
		// fetches somebody else's URL. Say so, and hand the link back.
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "media was supplied as a URL, so tommy has no bytes for it",
			"url":   media.URL,
		})
		return
	}
	if h.deps.Blobs == nil {
		writeError(w, http.StatusNotFound, "no blob store")
		return
	}

	rc, ref, err := h.deps.Blobs.Open(r.Context(), media.Blob.ID)
	if errors.Is(err, blob.ErrNotFound) {
		writeError(w, http.StatusNotFound, "media bytes are no longer in the blob store")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = rc.Close() }()

	contentType := media.Type()
	if contentType == "" {
		contentType = ref.ContentType
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "inline; filename="+strconv.Quote(media.Name()))
	// ServeContent gets an empty name so it never sniffs a type of its own over
	// the one the provider recorded; it still gives us range support.
	http.ServeContent(w, r, "", time.Time{}, rc)
}

func (h *apiHandler) clear(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Store.Clear(r.Context(), Name); err != nil {
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
