// Package mailtest is a test-only mail provider.
//
// It exists so the mail plugin can be exercised end to end before the real
// vendor providers land: it injects messages straight into the store, and it
// serves a small JSON send API that fans one request out into one event per
// delivered message, the way Mailjet's Messages[] and SendGrid's
// personalizations[] do.
//
// Nothing here imitates a real vendor. It is not registered in
// plugins/all/all.go and must never be: it is a fixture for tests.
package mailtest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/mail"
)

// ProviderName is the provider name every injected message is attributed to.
const ProviderName = "fake"

// SendPath is the ingress route the fake send API answers on. It is a
// namespace no real vendor uses, so a test can enable this provider alongside
// the real ones without a route collision.
const SendPath = "/mailtest/v1/send"

// MaxBody caps a request body, so a broken test cannot exhaust memory.
const MaxBody = 8 << 20

// Provider is the test-only mail provider.
type Provider struct{}

// New returns the fake provider.
func New() *Provider { return &Provider{} }

// Name implements plugin.Provider.
func (p *Provider) Name() string { return ProviderName }

// Plugin implements plugin.Provider.
func (p *Provider) Plugin() string { return mail.PluginName }

// Description implements plugin.Provider.
func (p *Provider) Description() string {
	return "A test-only send API that accepts tommy's own canonical message shape, including base64 attachments. " +
		"It fans a request out into one event per delivered message, so the mail plugin can be tested before the real vendor providers exist."
}

// Endpoints implements plugin.Provider.
func (p *Provider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{{
		Method:      "POST",
		Path:        SendPath,
		Description: "Accept one message, or a messages[] array, and record each one as a mail event.",
	}}
}

// Snippets implements plugin.Provider.
func (p *Provider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{{
		Title: "Send a test message",
		Lang:  "bash",
		Code: `curl -s {{.IngressURL}}` + SendPath + ` \
  -H 'Content-Type: application/json' -d '{
  "from": "Alice <alice@example.com>",
  "to": ["bob@example.com"],
  "subject": "Hello from tommy",
  "text": "It works.",
  "html": "<p>It <b>works</b>.</p>"
}'`,
	}}
}

// sendRequest is the fake wire format: one message, or several.
type sendRequest struct {
	Messages []sendMessage `json:"messages,omitempty"`
	sendMessage
}

type sendMessage struct {
	From        string            `json:"from,omitempty"`
	To          []string          `json:"to,omitempty"`
	Cc          []string          `json:"cc,omitempty"`
	Bcc         []string          `json:"bcc,omitempty"`
	ReplyTo     string            `json:"reply_to,omitempty"`
	Subject     string            `json:"subject,omitempty"`
	Text        string            `json:"text,omitempty"`
	HTML        string            `json:"html,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Attachments []sendAttachment  `json:"attachments,omitempty"`
	Meta        map[string]any    `json:"meta,omitempty"`
}

type sendAttachment struct {
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Content     string `json:"content,omitempty"` // base64
	Inline      bool   `json:"inline,omitempty"`
	ContentID   string `json:"content_id,omitempty"`
}

// RegisterIngress mounts the fake send API.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()

	mux.HandleFunc("POST "+SendPath, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		var req sendRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
			return
		}
		batch := req.Messages
		if len(batch) == 0 {
			batch = []sendMessage{req.sendMessage}
		}

		raw := event.Raw{
			Transport: "http",
			PeerAddr:  r.RemoteAddr,
			Method:    r.Method,
			Path:      r.URL.Path,
			Headers:   r.Header.Clone(),
			Body:      body,
			Text:      true,
		}

		ids := make([]map[string]string, 0, len(batch))
		for i, sm := range batch {
			msg, err := sm.message(r.Context(), d)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("messages[%d]: %v", i, err)})
				return
			}
			meta := map[string]any{"fan_out_index": i, "content_type": r.Header.Get("Content-Type")}
			if auth := r.Header.Get("Authorization"); auth != "" {
				meta["authorization"] = auth
			}
			for k, v := range sm.Meta {
				meta[k] = v
			}
			ev, err := Inject(r.Context(), d, msg, WithMeta(meta), WithRaw(raw))
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			ids = append(ids, map[string]string{"id": string(ev.ID), "status": "queued"})
		}
		writeJSON(w, http.StatusCreated, map[string]any{"messages": ids})
	})
}

// message converts the fake wire format into the canonical model, storing every
// attachment in the blob store on the way.
func (s sendMessage) message(ctx context.Context, d plugin.Deps) (*mail.Message, error) {
	msg := &mail.Message{Subject: s.Subject, Text: s.Text, HTML: s.HTML}

	if s.From != "" {
		from, err := mail.ParseAddress(s.From)
		if err != nil {
			return nil, err
		}
		msg.From = from
	}
	for _, set := range []struct {
		in  []string
		out *[]mail.Address
	}{{s.To, &msg.To}, {s.Cc, &msg.Cc}, {s.Bcc, &msg.Bcc}} {
		for _, raw := range set.in {
			addr, err := mail.ParseAddress(raw)
			if err != nil {
				return nil, err
			}
			*set.out = append(*set.out, addr)
		}
	}
	if s.ReplyTo != "" {
		addr, err := mail.ParseAddress(s.ReplyTo)
		if err != nil {
			return nil, err
		}
		msg.ReplyTo = []mail.Address{addr}
	}
	for k, v := range s.Headers {
		msg.Headers.Set(k, v)
	}
	for _, a := range s.Attachments {
		data, err := base64.StdEncoding.DecodeString(a.Content)
		if err != nil {
			return nil, fmt.Errorf("attachment %q: %w", a.Filename, err)
		}
		if _, err := msg.AttachBytes(ctx, d.Blobs, mail.Attachment{
			Filename:    a.Filename,
			ContentType: a.ContentType,
			Inline:      a.Inline,
			ContentID:   a.ContentID,
		}, data); err != nil {
			return nil, err
		}
	}
	return msg, nil
}

// Option tunes an injected event.
type Option func(*event.Event)

// WithProvider attributes the message to a different provider name, which is how a
// test covers filtering across providers before the real ones exist.
func WithProvider(name string) Option {
	return func(e *event.Event) { e.Provider = name }
}

// WithMeta attaches provider metadata, which is where it belongs - never on the
// message itself.
func WithMeta(meta map[string]any) Option {
	return func(e *event.Event) { e.Meta = meta }
}

// WithRaw replaces the synthetic raw request with a real captured one.
func WithRaw(raw event.Raw) Option {
	return func(e *event.Event) { e.Raw = raw }
}

// WithReceivedAt pins the timestamp, so ordering assertions are deterministic.
func WithReceivedAt(t time.Time) Option {
	return func(e *event.Event) { e.ReceivedAt = t }
}

// Inject appends a message exactly as a provider would - canonical model in
// Payload, untouched request in Raw - and returns the stored event.
func Inject(ctx context.Context, d plugin.Deps, msg *mail.Message, opts ...Option) (*event.Event, error) {
	ev := mail.NewEvent(ProviderName, msg)
	ev.Raw = syntheticRaw(msg)
	for _, o := range opts {
		o(ev)
	}
	if err := d.Append(ctx, ev); err != nil {
		return nil, err
	}
	return ev, nil
}

// syntheticRaw stands in for the request a real provider would have captured,
// so an injected message still has a Raw to render.
func syntheticRaw(msg *mail.Message) event.Raw {
	body, err := json.MarshalIndent(msg, "", "  ")
	if err != nil {
		body = []byte("{}")
	}
	return event.Raw{
		Transport: "http",
		PeerAddr:  "127.0.0.1:0",
		Method:    http.MethodPost,
		Path:      SendPath,
		Headers:   http.Header{"Content-Type": []string{"application/json"}},
		Body:      body,
		Text:      true,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
