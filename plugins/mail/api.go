package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
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

// MessageView is the JSON resource /api/v1/mail/messages serves: the canonical
// message plus the envelope fields a caller needs to fetch it again.
type MessageView struct {
	ID         event.ID       `json:"id"`
	ReceivedAt time.Time      `json:"received_at"`
	Provider   string         `json:"provider"`
	Type       string         `json:"type"`
	Meta       map[string]any `json:"meta,omitempty"`
	// URL is this message's own page in the UI: the link to open, or to print
	// in a log line, once something has been sent.
	URL     string       `json:"url,omitempty"`
	Message *Message     `json:"message"`
	Links   MessageLinks `json:"links"`
}

// MessageLinks are absolute paths for the parts of a message that are served as
// bytes rather than JSON.
type MessageLinks struct {
	Self        string   `json:"self"`
	HTML        string   `json:"html,omitempty"`
	Text        string   `json:"text,omitempty"`
	Raw         string   `json:"raw"`
	Attachments []string `json:"attachments,omitempty"`
}

// NewMessageView builds the API resource for one captured message.
func NewMessageView(e *event.Event, m *Message) MessageView {
	base := APIPrefix + "/messages/" + string(e.ID)
	links := MessageLinks{Self: base, Raw: base + "/raw"}
	if m.HTML != "" {
		links.HTML = base + "/html"
	}
	if m.Text != "" {
		links.Text = base + "/text"
	}
	for i := range m.Attachments {
		links.Attachments = append(links.Attachments, fmt.Sprintf("%s/attachments/%d", base, i))
	}
	return MessageView{
		ID:         e.ID,
		ReceivedAt: e.ReceivedAt,
		Provider:   e.Provider,
		Type:       e.Type,
		Meta:       e.Meta,
		Message:    m,
		Links:      links,
	}
}

// RegisterAPI mounts the mail read-back API. The core strips /api/v1/mail, so
// the patterns here are relative to it.
func (p *Plugin) RegisterAPI(mux plugin.Mux, d plugin.Deps) {
	h := &apiHandler{d: d.Normalize()}
	mux.HandleFunc("GET /messages", h.list)
	mux.HandleFunc("GET /messages/{id}", h.get)
	mux.HandleFunc("GET /messages/{id}/html", h.html)
	mux.HandleFunc("GET /messages/{id}/text", h.text)
	mux.HandleFunc("GET /messages/{id}/raw", h.raw)
	mux.HandleFunc("GET /messages/{id}/attachments/{idx}", h.attachment)
	mux.HandleFunc("DELETE /messages", h.clear)
}

type apiHandler struct{ d plugin.Deps }

// errNotMail marks an event that exists but is not a mail message, which is a
// 404 for these routes rather than a server error.
var errNotMail = errors.New("mail: event does not carry a mail message")

// load fetches one captured message from the store, so a client that sends
// through the ingress and immediately reads back sees its own write.
func (h *apiHandler) load(ctx context.Context, id string) (*event.Event, *Message, error) {
	e, err := h.d.Store.Get(ctx, event.ID(id))
	if err != nil {
		return nil, nil, err
	}
	if e.Plugin != PluginName {
		return nil, nil, errNotMail
	}
	m, ok := MessageOf(e)
	if !ok {
		return nil, nil, errNotMail
	}
	return e, m, nil
}

// resolve is load plus the error responses every byte-serving route shares.
func (h *apiHandler) resolve(w http.ResponseWriter, r *http.Request) (*event.Event, *Message, bool) {
	e, m, err := h.load(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, errNotMail):
		writeError(w, http.StatusNotFound, "message not found")
		return nil, nil, false
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	return e, m, true
}

// listFilter narrows a listing by the fields only the mail plugin knows about.
type listFilter struct {
	to             string
	from           string
	subject        string
	hasAttachments *bool
}

func newListFilter(r *http.Request) listFilter {
	q := r.URL.Query()
	f := listFilter{
		to:      strings.ToLower(q.Get("to")),
		from:    strings.ToLower(q.Get("from")),
		subject: strings.ToLower(q.Get("subject")),
	}
	if v := q.Get("has_attachments"); v != "" {
		want := v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
		f.hasAttachments = &want
	}
	return f
}

func (f listFilter) matches(m *Message) bool {
	if f.to != "" && !containsFold(FormatAddressList(m.Recipients()), f.to) {
		return false
	}
	if f.from != "" && !containsFold(m.From.String(), f.from) {
		return false
	}
	if f.subject != "" && !containsFold(m.Subject, f.subject) {
		return false
	}
	if f.hasAttachments != nil && m.HasAttachments() != *f.hasAttachments {
		return false
	}
	return true
}

func containsFold(haystack, needleLower string) bool {
	return strings.Contains(strings.ToLower(haystack), needleLower)
}

// list serves GET /messages: every captured message, newest first.
//
// The store query runs unpaginated because the mail-specific filters below can
// only be applied to a decoded message; limit and offset are then applied to
// the filtered result, which is what a caller paging through them expects. The
// ring buffer bounds the listing either way.
func (h *apiHandler) list(w http.ResponseWriter, r *http.Request) {
	q, err := coreapi.ParseQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, offset := q.Limit, q.Offset
	q.Plugin, q.Limit, q.Offset = PluginName, 0, 0

	events, err := h.d.Store.List(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	filter := newListFilter(r)
	out := make([]MessageView, 0, len(events))
	for _, e := range events {
		m, ok := MessageOf(e)
		if !ok || !filter.matches(m) {
			continue
		}
		v := NewMessageView(e, m)
		v.URL = coreapi.EventURL(r, e.ID)
		out = append(out, v)
	}

	if offset >= len(out) {
		out = nil
	} else {
		out = out[offset:]
	}
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	if out == nil {
		out = []MessageView{}
	}
	writeJSON(w, http.StatusOK, out)
}

// get serves GET /messages/{id}.
func (h *apiHandler) get(w http.ResponseWriter, r *http.Request) {
	e, m, ok := h.resolve(w, r)
	if !ok {
		return
	}
	v := NewMessageView(e, m)
	v.URL = coreapi.EventURL(r, e.ID)
	writeJSON(w, http.StatusOK, v)
}

// html serves the HTML body as HTML.
//
// This is untrusted content written by the application under test, so it is
// served with a policy that permits no script, no framing controls of its own
// and no same-origin privileges, and the UI only ever shows it inside a
// sandboxed iframe.
func (h *apiHandler) html(w http.ResponseWriter, r *http.Request) {
	_, m, ok := h.resolve(w, r)
	if !ok {
		return
	}
	if m.HTML == "" {
		writeError(w, http.StatusNotFound, "message has no html body")
		return
	}
	head := w.Header()
	head.Set("Content-Type", "text/html; charset=utf-8")
	head.Set("Content-Security-Policy",
		"default-src 'none'; img-src data: blob: http: https:; style-src 'unsafe-inline' data: http: https:; font-src data: http: https:; media-src data: http: https:")
	head.Set("X-Content-Type-Options", "nosniff")
	head.Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write([]byte(m.HTML))
}

// text serves the plain-text body.
func (h *apiHandler) text(w http.ResponseWriter, r *http.Request) {
	_, m, ok := h.resolve(w, r)
	if !ok {
		return
	}
	if m.Text == "" {
		writeError(w, http.StatusNotFound, "message has no text body")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(m.Text))
}

// raw serves the untouched request that produced the message: the JSON body a
// vendor SDK posted, or the RFC 5322 source an SMTP client sent.
func (h *apiHandler) raw(w http.ResponseWriter, r *http.Request) {
	e, _, ok := h.resolve(w, r)
	if !ok {
		return
	}
	head := w.Header()
	if e.Raw.Transport == "smtp" {
		head.Set("Content-Type", "message/rfc822")
	} else if e.Raw.Text {
		head.Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		head.Set("Content-Type", "application/octet-stream")
	}
	head.Set("X-Content-Type-Options", "nosniff")
	if boolParam(r, "download") {
		head.Set("Content-Disposition", disposition("attachment", "message-"+string(e.ID)+".eml"))
	}
	_, _ = w.Write(e.Raw.Body)
}

// attachment streams one attachment out of the blob store, which is where every
// mail byte lives - the event itself never carries them.
func (h *apiHandler) attachment(w http.ResponseWriter, r *http.Request) {
	_, m, ok := h.resolve(w, r)
	if !ok {
		return
	}
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil || idx < 0 || idx >= len(m.Attachments) {
		writeError(w, http.StatusNotFound, "attachment not found")
		return
	}
	att := m.Attachments[idx]

	if h.d.Blobs == nil {
		writeError(w, http.StatusNotFound, "no blob store")
		return
	}
	rc, ref, err := h.d.Blobs.Open(r.Context(), att.Blob.ID)
	if errors.Is(err, blob.ErrNotFound) {
		writeError(w, http.StatusNotFound, "attachment bytes are gone")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = rc.Close() }()

	ct := att.ContentType
	if ct == "" {
		ct = ref.ContentType
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	kind := att.Disposition()
	if boolParam(r, "inline") {
		kind = "inline"
	} else if boolParam(r, "download") {
		kind = "attachment"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", disposition(kind, att.Name()))
	// ServeContent gives range requests and a correct Content-Length for free.
	http.ServeContent(w, r, att.Name(), time.Time{}, rc)
}

// clear serves DELETE /messages. Attachment blobs deliberately survive: the
// event log is history, the blob store is state, and a download link that is
// still open must keep working.
func (h *apiHandler) clear(w http.ResponseWriter, r *http.Request) {
	if err := h.d.Store.Clear(r.Context(), PluginName); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// disposition builds a Content-Disposition value, RFC 2231-encoding a filename
// that is not plain ASCII rather than mangling it.
func disposition(kind, filename string) string {
	if filename == "" {
		return kind
	}
	return mime.FormatMediaType(kind, map[string]string{"filename": filename})
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
	writeJSON(w, status, map[string]string{"error": msg})
}
