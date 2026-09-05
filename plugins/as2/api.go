package as2

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	coreapi "github.com/can3p/tommy/core/server/api"
	"github.com/can3p/tommy/core/store"
)

// APIBase is where the plugin's routes end up once the core has mounted them.
// The handlers see the prefix stripped, so they need it spelled out to build
// the links they hand back.
const APIBase = APIPrefix

// MessageEnvelope is what /messages returns: the parsed message plus the event
// fields a client needs to correlate it with the generic event log, and the
// URLs for the three byte streams that matter.
type MessageEnvelope struct {
	ID         event.ID       `json:"id"`
	ReceivedAt time.Time      `json:"received_at"`
	Provider   string         `json:"provider"`
	Type       string         `json:"type"`
	Meta       map[string]any `json:"meta,omitempty"`
	// URL is this message's own page in the UI: the link to open, or to print
	// in a log line, once something has been sent.
	URL string `json:"url,omitempty"`

	Title   string `json:"title"`
	Preview string `json:"preview,omitempty"`

	// RawURL is the request exactly as it arrived, ciphertext included.
	RawURL string `json:"raw_url"`
	// PayloadURL is the business document after every layer was peeled.
	PayloadURL string `json:"payload_url,omitempty"`
	// MDNURL is the receipt tommy returned.
	MDNURL string `json:"mdn_url,omitempty"`

	Message *Message `json:"message"`
}

// NewMessageEnvelope builds the API view of a captured message.
func NewMessageEnvelope(base string, c Captured) MessageEnvelope {
	e := MessageEnvelope{
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
	if c.Message.Payload.Blob != nil {
		e.PayloadURL = base + "/messages/" + string(c.Event.ID) + "/payload"
	}
	if c.Message.MDN != nil && c.Message.MDN.Blob != nil {
		e.MDNURL = base + "/messages/" + string(c.Event.ID) + "/mdn"
	}
	return e
}

// apiHandler serves /api/v1/as2/.
type apiHandler struct {
	deps     plugin.Deps
	identity *Identity
	// base is the absolute prefix the routes are mounted at, used to build the
	// URLs handed back. Overridable so a test can mount the handler bare.
	base string
}

func (h *apiHandler) mount(mux plugin.Mux) {
	if h.base == "" {
		h.base = APIBase
	}
	mux.HandleFunc("GET /messages", h.list)
	mux.HandleFunc("GET /messages/{id}", h.get)
	mux.HandleFunc("GET /messages/{id}/raw", h.raw)
	mux.HandleFunc("GET /messages/{id}/payload", h.payload)
	mux.HandleFunc("GET /messages/{id}/mdn", h.mdn)
	mux.HandleFunc("DELETE /messages", h.clear)
	mux.HandleFunc("DELETE /messages/{id}", h.delete)
	mux.HandleFunc("GET /certificate", h.certificate)
	mux.HandleFunc("GET /identity", h.identityInfo)
}

// filters are the AS2-specific narrowings, applied after the store's own.
type filters struct {
	from      string
	to        string
	messageID string
	format    string
	// security narrows to "signed", "encrypted", "compressed" or
	// "unprotected".
	security string
	// issue narrows to messages carrying a given issue code, which is how
	// somebody finds every message that failed to decrypt.
	issue string
}

func parseFilters(r *http.Request) filters {
	v := r.URL.Query()
	return filters{
		from:      strings.TrimSpace(v.Get("from")),
		to:        strings.TrimSpace(v.Get("to")),
		messageID: strings.TrimSpace(v.Get("message_id")),
		format:    strings.ToLower(strings.TrimSpace(v.Get("format"))),
		security:  strings.ToLower(strings.TrimSpace(v.Get("security"))),
		issue:     strings.TrimSpace(v.Get("issue")),
	}
}

func (f filters) match(m *Message) bool {
	if f.from != "" && !strings.EqualFold(f.from, m.From) {
		return false
	}
	if f.to != "" && !strings.EqualFold(f.to, m.To) {
		return false
	}
	if f.messageID != "" && !strings.EqualFold(f.messageID, m.MessageID) {
		return false
	}
	if f.format != "" && !strings.EqualFold(f.format, m.Payload.Format) {
		return false
	}
	if f.issue != "" && !m.HasIssue(f.issue) {
		return false
	}
	switch f.security {
	case "":
	case "signed":
		return m.Security.Signed
	case "encrypted":
		return m.Security.Encrypted
	case "compressed":
		return m.Security.Compressed
	case "unprotected":
		return !m.Security.Signed && !m.Security.Encrypted && !m.Security.Compressed
	default:
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
	// This is the as2 plugin whatever the caller asked for, and paging is
	// applied after the AS2-specific filters so a limit never counts messages
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

// raw serves the request exactly as it arrived. For an encrypted message that
// is the ciphertext, which is the copy of record and the thing to hand a
// partner who disputes what they sent.
func (h *apiHandler) raw(w http.ResponseWriter, r *http.Request) {
	e, ok := h.eventOf(w, r)
	if !ok {
		return
	}
	serveBytes(w, e.Raw.Body, "application/octet-stream", string(e.ID)+".as2")
}

// payload serves the business document after every layer was peeled.
func (h *apiHandler) payload(w http.ResponseWriter, r *http.Request) {
	c, ok := h.captured(w, r)
	if !ok {
		return
	}
	if c.Message.Payload.Blob == nil {
		writeError(w, http.StatusNotFound, "this message has no stored payload")
		return
	}
	h.serveBlob(w, r, *c.Message.Payload.Blob)
}

// mdn serves the receipt tommy returned, so it can be diffed against what the
// partner's software says it received.
func (h *apiHandler) mdn(w http.ResponseWriter, r *http.Request) {
	c, ok := h.captured(w, r)
	if !ok {
		return
	}
	if c.Message.MDN == nil || c.Message.MDN.Blob == nil {
		writeError(w, http.StatusNotFound, "this message has no stored MDN")
		return
	}
	h.serveBlob(w, r, *c.Message.MDN.Blob)
}

// certificate serves tommy's own certificate as PEM. This is the endpoint that
// makes AS2 usable at all: a partner has to import this before it can encrypt
// anything, and telling somebody to curl a URL beats telling them to find a
// file inside a container.
func (h *apiHandler) certificate(w http.ResponseWriter, r *http.Request) {
	pemBytes := h.identity.CertificatePEM()
	if len(pemBytes) == 0 {
		writeError(w, http.StatusNotFound,
			"no AS2 certificate exists yet. Certificates are generated only when an AS2 provider is enabled, "+
				"so that nobody running another plugin pays for this one's setup.")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="tommy-as2.pem"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pemBytes)
}

// identityInfo describes the key pair without handing out the key: which
// certificate is in use, where it came from, its fingerprint, and whether a
// partner certificate is configured.
func (h *apiHandler) identityInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.identity.Info())
}

func (h *apiHandler) serveBlob(w http.ResponseWriter, r *http.Request, ref blob.Ref) {
	rc, meta, err := h.deps.Blobs.Open(r.Context(), ref.ID)
	if errors.Is(err, blob.ErrNotFound) {
		writeError(w, http.StatusNotFound, "the stored bytes are no longer available")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = rc.Close() }()

	body, err := io.ReadAll(rc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	name := meta.Filename
	if name == "" {
		name = ref.ID
	}
	serveBytes(w, body, meta.ContentType, name)
}

// serveBytes writes captured content.
//
// Content-Type is whatever tommy decided, never what the sender declared, and
// nosniff is always set: an AS2 payload is attacker-supplied bytes, and a
// browser that sniffed one into HTML would run whatever a "purchase order"
// carried. The disposition is attachment for the same reason.
func serveBytes(w http.ResponseWriter, body []byte, contentType, filename string) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(filename)+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// sanitizeFilename keeps a sender-supplied name from breaking out of the
// Content-Disposition header or naming a path. It is a header value, so quotes,
// control characters and separators all have to go.
func sanitizeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return -1
		case r == '"' || r == '\\' || r == '/' || r == ';':
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "payload.bin"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	return name
}

// eventOf resolves the {id} path value into this plugin's event.
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

// captured resolves the {id} path value into a captured AS2 message.
func (h *apiHandler) captured(w http.ResponseWriter, r *http.Request) (Captured, bool) {
	e, ok := h.eventOf(w, r)
	if !ok {
		return Captured{}, false
	}
	m, ok := MessageOf(e)
	if !ok {
		writeError(w, http.StatusNotFound, "event "+string(e.ID)+" carries no AS2 message")
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

// Endpoints describes the plugin's own API routes, for a provider that wants to
// list them alongside its ingress ones and for the README to stay in step.
func Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{
		{Method: "GET", Path: APIBase + "/messages", Description: "List captured AS2 messages, newest first."},
		{Method: "GET", Path: APIBase + "/messages/{id}", Description: "One captured message, unwrapped."},
		{Method: "GET", Path: APIBase + "/messages/{id}/raw", Description: "The request exactly as it arrived."},
		{Method: "GET", Path: APIBase + "/messages/{id}/payload", Description: "The EDI document after every layer was peeled."},
		{Method: "GET", Path: APIBase + "/messages/{id}/mdn", Description: "The MDN receipt tommy returned."},
		{Method: "GET", Path: APIBase + "/certificate", Description: "Tommy's AS2 certificate as PEM, for a partner to import."},
		{Method: "GET", Path: APIBase + "/identity", Description: "Which certificate is in use, where it came from and its fingerprint."},
	}
}
