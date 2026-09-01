package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	netmail "net/mail"
	"sort"
	"strings"
	"unicode"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
)

// PluginName is the plugin's name, which is also its URL segment: the API lives
// under /api/v1/mail/ and the tab under /ui/mail/.
const PluginName = "mail"

// TypeMessage is the event.Type carried by every delivered mail message.
// Providers that grow new resources later add new types rather than
// overloading this one, so consumers must switch on Type.
const TypeMessage = "mail.message"

// SnippetLimit is how many runes of body text end up in event.Summary.Snippet.
const SnippetLimit = 240

// Address is one mail address with an optional display name.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// String renders the address in RFC 5322 form: `Bob <bob@example.com>`, or just
// the bare address when there is no display name.
func (a Address) String() string {
	switch {
	case a.Name == "" && a.Email == "":
		return ""
	case a.Name == "":
		return a.Email
	default:
		return (&netmail.Address{Name: a.Name, Address: a.Email}).String()
	}
}

// IsZero reports whether the address carries nothing at all.
func (a Address) IsZero() bool { return a.Name == "" && a.Email == "" }

// ParseAddress parses a single `Name <addr>` or bare address.
func ParseAddress(s string) (Address, error) {
	parsed, err := netmail.ParseAddress(strings.TrimSpace(s))
	if err != nil {
		return Address{}, fmt.Errorf("mail: parse address %q: %w", s, err)
	}
	return Address{Name: parsed.Name, Email: parsed.Address}, nil
}

// ParseAddressList parses a comma-separated address list.
func ParseAddressList(s string) ([]Address, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	parsed, err := netmail.ParseAddressList(s)
	if err != nil {
		return nil, fmt.Errorf("mail: parse address list %q: %w", s, err)
	}
	out := make([]Address, 0, len(parsed))
	for _, a := range parsed {
		out = append(out, Address{Name: a.Name, Email: a.Address})
	}
	return out, nil
}

// Emails flattens an address list to bare addresses, which is what
// event.Summary.To wants.
func Emails(list []Address) []string {
	if len(list) == 0 {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		if a.Email != "" {
			out = append(out, a.Email)
		}
	}
	return out
}

// FormatAddressList renders an address list the way a header would carry it.
func FormatAddressList(list []Address) string {
	parts := make([]string, 0, len(list))
	for _, a := range list {
		if s := a.String(); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

// Headers carries the arbitrary headers a sender attached to a message.
//
// It is multi-valued because a parsed MIME message legitimately repeats a
// header (Received, References); the JSON send APIs only ever produce one value
// per name, so their providers can use Set throughout. Keys keep the casing the
// sender used - a fake exists to show you exactly what you sent - and lookups
// are case-insensitive.
type Headers map[string][]string

// Get returns the first value of key, matched case-insensitively.
func (h Headers) Get(key string) string {
	if v := h.Values(key); len(v) > 0 {
		return v[0]
	}
	return ""
}

// Values returns every value of key, matched case-insensitively.
func (h Headers) Values(key string) []string {
	if v, ok := h[key]; ok {
		return v
	}
	for k, v := range h {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return nil
}

// Set replaces every value of key.
func (h *Headers) Set(key, value string) {
	h.remove(key)
	if *h == nil {
		*h = Headers{}
	}
	(*h)[key] = []string{value}
}

// Add appends a value, keeping any already present under that name.
func (h *Headers) Add(key, value string) {
	if *h == nil {
		*h = Headers{}
	}
	for k := range *h {
		if strings.EqualFold(k, key) {
			(*h)[k] = append((*h)[k], value)
			return
		}
	}
	(*h)[key] = []string{value}
}

func (h *Headers) remove(key string) {
	for k := range *h {
		if strings.EqualFold(k, key) {
			delete(*h, k)
		}
	}
}

// Keys returns the header names sorted, so rendering is stable.
func (h Headers) Keys() []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Clone returns a deep copy.
func (h Headers) Clone() Headers {
	if h == nil {
		return nil
	}
	out := make(Headers, len(h))
	for k, v := range h {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// Attachment is one attached or embedded part of a message.
//
// The bytes live in the blob store and are addressed by Blob; they are never
// inlined into an event, so event JSON stays small and a download can be
// streamed with a correct Content-Length and range support. Filename,
// ContentType and Size mirror the fields of Blob and are the canonical copy for
// the plugin's own API and UI.
type Attachment struct {
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
	// Inline marks a part meant to be rendered inside the HTML body
	// (Content-Disposition: inline) rather than offered as a download.
	Inline bool `json:"inline,omitempty"`
	// ContentID is the cid the HTML body references, without angle brackets.
	ContentID string   `json:"content_id,omitempty"`
	Blob      blob.Ref `json:"blob"`
}

// Disposition returns the Content-Disposition type this attachment wants.
func (a Attachment) Disposition() string {
	if a.Inline {
		return "inline"
	}
	return "attachment"
}

// Name returns something usable as a filename even when the sender omitted one.
func (a Attachment) Name() string {
	if a.Filename != "" {
		return a.Filename
	}
	if a.ContentID != "" {
		return a.ContentID
	}
	return "attachment"
}

// TrimContentID normalises the many spellings of a content id - `<cid>`,
// `cid:name`, `name` - down to the bare token the HTML body references.
func TrimContentID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSuffix(s, ">")
	s = strings.TrimPrefix(s, "cid:")
	return strings.TrimSpace(s)
}

// Message is tommy's canonical email: one *delivered* message, not one API
// request. A single Mailjet `Messages[]` entry or SendGrid `personalizations[]`
// entry becomes one Message, so a request that fans out to three recipients
// appends three events.
//
// Provider-specific metadata (Mailjet CustomID, SendGrid categories, the auth
// that was presented, ...) belongs in event.Meta, never here: this struct is
// the part every provider agrees on.
type Message struct {
	From    Address   `json:"from"`
	To      []Address `json:"to,omitempty"`
	Cc      []Address `json:"cc,omitempty"`
	Bcc     []Address `json:"bcc,omitempty"`
	ReplyTo []Address `json:"reply_to,omitempty"`

	Subject string `json:"subject,omitempty"`
	// Text and HTML are the two body parts. Either may be empty; a
	// multipart/alternative message carries both.
	Text string `json:"text,omitempty"`
	HTML string `json:"html,omitempty"`

	Headers     Headers      `json:"headers,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Recipients returns every address the message was delivered to: To, Cc and
// Bcc, in that order.
func (m *Message) Recipients() []Address {
	out := make([]Address, 0, len(m.To)+len(m.Cc)+len(m.Bcc))
	out = append(out, m.To...)
	out = append(out, m.Cc...)
	out = append(out, m.Bcc...)
	return out
}

// RecipientEmails is Recipients reduced to bare addresses.
func (m *Message) RecipientEmails() []string { return Emails(m.Recipients()) }

// HasAttachments reports whether anything was attached or embedded.
func (m *Message) HasAttachments() bool { return len(m.Attachments) > 0 }

// AttachmentByContentID finds the embedded part an HTML `cid:` URL refers to.
func (m *Message) AttachmentByContentID(cid string) (Attachment, bool) {
	want := TrimContentID(cid)
	if want == "" {
		return Attachment{}, false
	}
	for _, a := range m.Attachments {
		if strings.EqualFold(a.ContentID, want) {
			return a, true
		}
	}
	return Attachment{}, false
}

// Snippet is the short body preview the listings show. It prefers the text
// part and falls back to the HTML part reduced to readable text.
func (m *Message) Snippet() string {
	body := m.Text
	if strings.TrimSpace(body) == "" {
		body = stripHTML(m.HTML)
	}
	return truncateRunes(collapseSpace(body), SnippetLimit)
}

// Summary builds the provider-agnostic listing data every event carries. Cc and
// Bcc recipients are included in To: a Message is a single delivered message,
// and searching for a bcc'd address must find it.
func (m *Message) Summary() event.Summary {
	return event.Summary{
		From:    m.From.String(),
		To:      m.RecipientEmails(),
		Title:   m.Subject,
		Snippet: m.Snippet(),
	}
}

// Attach stores the attachment bytes in the blob store and appends the
// attachment to the message, filling in Blob and Size from what was actually
// written. It is the single place mail bytes cross into a blob, so every
// provider records size, content type and content id the same way.
func (m *Message) Attach(ctx context.Context, blobs blob.BlobStore, a Attachment, data io.Reader) (Attachment, error) {
	if blobs == nil {
		return a, errors.New("mail: cannot attach without a blob store")
	}
	if data == nil {
		data = strings.NewReader("")
	}
	a.ContentID = TrimContentID(a.ContentID)
	if a.ContentType == "" {
		a.ContentType = "application/octet-stream"
	}
	ref, err := blobs.Put(ctx, data, blob.Ref{ContentType: a.ContentType, Filename: a.Filename})
	if err != nil {
		return a, fmt.Errorf("mail: store attachment %q: %w", a.Name(), err)
	}
	a.Blob = ref
	a.Size = ref.Size
	m.Attachments = append(m.Attachments, a)
	return a, nil
}

// AttachBytes is Attach for data already in memory, which is what the base64
// fields of the JSON send APIs decode to.
func (m *Message) AttachBytes(ctx context.Context, blobs blob.BlobStore, a Attachment, data []byte) (Attachment, error) {
	return m.Attach(ctx, blobs, a, bytes.NewReader(data))
}

// NewEvent builds the event for a delivered message, with the plugin, type,
// summary and payload already filled in. The caller adds Raw - always - and any
// provider metadata in Meta, then hands it to Deps.Append.
func NewEvent(provider string, m *Message) *event.Event {
	return &event.Event{
		Plugin:   PluginName,
		Provider: provider,
		Type:     TypeMessage,
		Summary:  m.Summary(),
		Payload:  m,
	}
}

// MessageOf returns the canonical message carried by an event, if it carries
// one. The in-memory store shares payloads, so the common path is a type
// assertion; the JSON fallback keeps the API honest for an event that came back
// through a serializing store.
func MessageOf(e *event.Event) (*Message, bool) {
	if e == nil || e.Payload == nil {
		return nil, false
	}
	switch p := e.Payload.(type) {
	case *Message:
		return p, true
	case Message:
		return &p, true
	}
	encoded, err := json.Marshal(e.Payload)
	if err != nil {
		return nil, false
	}
	var m Message
	if err := json.Unmarshal(encoded, &m); err != nil {
		return nil, false
	}
	return &m, true
}

// stripHTML reduces an HTML body to readable text for the listing snippet. It
// is deliberately crude: it feeds a preview, never the rendered body, which is
// served as-is into a sandboxed iframe.
func stripHTML(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '<' {
			out.WriteByte(s[i])
			i++
			continue
		}
		end := strings.IndexByte(s[i:], '>')
		if end < 0 {
			break
		}
		tag := strings.ToLower(strings.TrimSpace(s[i+1 : i+end]))
		i += end + 1
		switch {
		case strings.HasPrefix(tag, "script"), strings.HasPrefix(tag, "style"):
			name := "script"
			if strings.HasPrefix(tag, "style") {
				name = "style"
			}
			if skip := indexAfterCloseTag(s[i:], name); skip >= 0 {
				i += skip
			} else {
				i = len(s)
			}
		case strings.HasPrefix(tag, "br"), strings.HasPrefix(tag, "hr"), tag == "/p",
			tag == "/div", tag == "/tr", tag == "/li", tag == "/h1", tag == "/h2", tag == "/h3":
			out.WriteByte('\n')
		default:
			out.WriteByte(' ')
		}
	}
	return html.UnescapeString(out.String())
}

// indexAfterCloseTag returns the offset just past `</name...>`, or -1.
func indexAfterCloseTag(s, name string) int {
	lower := strings.ToLower(s)
	start := strings.Index(lower, "</"+name)
	if start < 0 {
		return -1
	}
	end := strings.IndexByte(s[start:], '>')
	if end < 0 {
		return -1
	}
	return start + end + 1
}

func collapseSpace(s string) string {
	var out strings.Builder
	space := true // leading whitespace is dropped
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !space {
				out.WriteByte(' ')
				space = true
			}
			continue
		}
		out.WriteRune(r)
		space = false
	}
	return strings.TrimRight(out.String(), " ")
}

func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
