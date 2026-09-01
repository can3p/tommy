// Package mailjet imitates Mailjet's transactional Send API v3.1.
//
// It mounts POST /v3.1/send on the shared ingress, accepts the real Messages[]
// batch shape (https://dev.mailjet.com/email/guides/send-api-v31/), and fans
// each entry of Messages[] out into its own mail.Message and its own event -
// one HTTP request that sends to three logical messages appends three events,
// exactly as the real API sends one physical message per Messages[] entry.
//
// Everything Mailjet-specific - CustomID, EventPayload, CustomCampaign,
// SandboxMode, the credentials that were presented, and the generated message
// ids - lives in Event.Meta. The canonical mail.Message never carries it.
package mailjet

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync/atomic"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/mail"
)

// ProviderName is this provider's name, both the registry key and the
// Event.Provider every message it hands off is stamped with.
const ProviderName = "mailjet"

// SendPath is the route Mailjet's real SDKs and raw HTTP callers post to.
const SendPath = "/v3.1/send"

// MaxBody caps the request body Mailjet will read, so a broken or hostile
// client cannot exhaust memory decoding attachments. It is independent of the
// blob store's own capacity, which is enforced separately per attachment.
const MaxBody = 32 << 20 // 32MiB

// messageIDBase makes generated MessageID values look like Mailjet's real
// large integers rather than starting at 1.
const messageIDBase = 100_000_000_000_000

// Provider is the fake Mailjet Send API v3.1.
type Provider struct {
	seq atomic.Int64
}

// New returns the Mailjet provider.
func New() *Provider { return &Provider{} }

// Name implements plugin.Provider.
func (p *Provider) Name() string { return ProviderName }

// Plugin implements plugin.Provider.
func (p *Provider) Plugin() string { return mail.PluginName }

// Description implements plugin.Provider.
func (p *Provider) Description() string {
	return "Mailjet's transactional Send API v3.1: accepts a Messages[] batch with attachments, " +
		"fans it out into one event per delivered message, and answers with Mailjet's real success and error envelopes."
}

// Endpoints implements plugin.Provider.
func (p *Provider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{{
		Method: "POST",
		Path:   SendPath,
		Description: "Accept a Mailjet v3.1 { \"Messages\": [...] } batch, store Base64Content/InlinedAttachments " +
			"in the blob store, and record one event per delivered message.",
	}}
}

// Snippets implements plugin.Provider.
func (p *Provider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{{
		Title: "Send an email",
		Lang:  "bash",
		Code: `curl -s {{.IngressURL}}` + SendPath + ` \
  -u "any-key:any-secret" -H 'Content-Type: application/json' -d '{
  "Messages":[{"From":{"Email":"a@example.com","Name":"Alice"},"To":[{"Email":"b@example.com"}],
  "Subject":"Hello from tommy","TextPart":"It works."}]}'`,
	}, {
		Title: "Send with an attachment",
		Lang:  "bash",
		Code: `curl -s {{.IngressURL}}` + SendPath + ` \
  -u "any-key:any-secret" -H 'Content-Type: application/json' -d '{
  "Messages":[{"From":{"Email":"a@example.com"},"To":[{"Email":"b@example.com"}],
  "Subject":"Receipt","TextPart":"See attached.",
  "Attachments":[{"ContentType":"text/plain","Filename":"note.txt","Base64Content":"aGVsbG8="}]}]}'`,
	}}
}

// providerConfig is the optional pinned credential pair. When APIKey is empty
// (the default) the provider accepts any Basic-auth credentials, or none at
// all, and just records what was presented.
type providerConfig struct {
	APIKey    string `toml:"api_key"`
	SecretKey string `toml:"secret_key"`
}

// RegisterIngress mounts the send endpoint.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()

	var cfg providerConfig
	_ = d.Config.Decode(&cfg) // unknown/absent keys leave cfg zero, i.e. "accept anything"

	mux.HandleFunc("POST "+SendPath, func(w http.ResponseWriter, r *http.Request) {
		p.handleSend(w, r, d, cfg)
	})
}

func (p *Provider) handleSend(w http.ResponseWriter, r *http.Request, d plugin.Deps, cfg providerConfig) {
	ctx := r.Context()

	user, pass, hasAuth := r.BasicAuth()
	if cfg.APIKey != "" {
		mismatched := !hasAuth || user != cfg.APIKey || (cfg.SecretKey != "" && pass != cfg.SecretKey)
		if mismatched {
			writeGlobalError(w, http.StatusUnauthorized, "mj-0015", "API key authentication/authorization failed.", nil)
			return
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil {
		writeGlobalError(w, http.StatusBadRequest, "mj-0002", "Malformed JSON, please review the syntax and properties types.", nil)
		return
	}
	if len(body) > MaxBody {
		writeGlobalError(w, http.StatusBadRequest, "mj-0002", "Malformed JSON, please review the syntax and properties types.", nil)
		return
	}

	var req sendRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeGlobalError(w, http.StatusBadRequest, "mj-0002", "Malformed JSON, please review the syntax and properties types.", nil)
		return
	}
	if len(req.Messages) == 0 {
		writeGlobalError(w, http.StatusBadRequest, "mj-0004", `"Messages" property is required and cannot be empty.`, []string{"Messages"})
		return
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

	results := make([]any, len(req.Messages))
	for i, wm := range req.Messages {
		results[i] = p.processMessage(ctx, d, wm, req.SandboxMode, raw, i, user, pass, hasAuth)
	}

	writeJSON(w, http.StatusOK, sendResponse{Messages: results})
}

// processMessage validates and converts one Messages[] entry, appends the
// resulting event on success, and returns the per-message result - success or
// error - that belongs at this index of the response envelope. A validation
// failure never aborts the rest of the batch: Mailjet answers 200 with mixed
// success/error entries for partial failures, and only a malformed request as
// a whole is a top-level 4xx.
func (p *Provider) processMessage(
	ctx context.Context,
	d plugin.Deps,
	wm wireMessage,
	sandbox bool,
	raw event.Raw,
	index int,
	user, pass string,
	hasAuth bool,
) any {
	if verr := validateMessage(wm); verr != nil {
		return verr.result(d)
	}

	msg := &mail.Message{
		From:    mail.Address{Email: wm.From.Email, Name: wm.From.Name},
		To:      toAddresses(wm.To),
		Cc:      toAddresses(wm.Cc),
		Bcc:     toAddresses(wm.Bcc),
		Subject: wm.Subject,
		Text:    wm.TextPart,
		HTML:    wm.HTMLPart,
	}
	if wm.ReplyTo != nil && wm.ReplyTo.Email != "" {
		msg.ReplyTo = []mail.Address{{Email: wm.ReplyTo.Email, Name: wm.ReplyTo.Name}}
	}
	for _, k := range sortedKeys(wm.Headers) {
		msg.Headers.Set(k, wm.Headers[k])
	}

	for _, a := range wm.Attachments {
		if verr := attach(ctx, d, msg, a.ContentType, a.Filename, a.Base64Content, false, "", "Attachments"); verr != nil {
			return verr.result(d)
		}
	}
	for _, a := range wm.InlinedAttachments {
		if verr := attach(ctx, d, msg, a.ContentType, a.Filename, a.Base64Content, true, a.ContentID, "InlinedAttachments"); verr != nil {
			return verr.result(d)
		}
	}

	toResults := p.recipients(wm.To, sandbox)
	ccResults := p.recipients(wm.Cc, sandbox)
	bccResults := p.recipients(wm.Bcc, sandbox)

	ev := mail.NewEvent(ProviderName, msg)
	ev.Raw = raw
	meta := map[string]any{
		"fan_out_index":   index,
		"sandbox_mode":    sandbox,
		"custom_id":       wm.CustomID,
		"custom_campaign": wm.CustomCampaign,
		"message_ids":     recipientMeta(toResults, ccResults, bccResults),
	}
	if hasAuth {
		meta["presented_api_key"] = user
		meta["presented_secret_key"] = pass
	}
	if len(wm.EventPayload) > 0 {
		var payload any
		if err := json.Unmarshal(wm.EventPayload, &payload); err == nil {
			meta["event_payload"] = payload
		} else {
			meta["event_payload"] = string(wm.EventPayload)
		}
	}
	ev.Meta = meta

	if err := d.Append(ctx, ev); err != nil {
		return newValidationError("tommy-internal", http.StatusInternalServerError, err.Error(), nil).result(d)
	}

	return successResult{
		Status:   "success",
		CustomID: wm.CustomID,
		To:       toResults,
		Cc:       ccResults,
		Bcc:      bccResults,
	}
}

// attach decodes one Base64Content attachment and stores it via mail.Message's
// own Attach path, translating a blob.ErrCapacityExceeded or a bad base64
// payload into the right validation error rather than ever panicking.
func attach(
	ctx context.Context,
	d plugin.Deps,
	msg *mail.Message,
	contentType, filename, base64Content string,
	inline bool,
	contentID string,
	field string,
) *validationError {
	data, err := base64.StdEncoding.DecodeString(base64Content)
	if err != nil {
		return newValidationError("mj-0004", http.StatusBadRequest, `"Base64Content" is not valid base64 data.`, []string{field})
	}
	_, err = msg.AttachBytes(ctx, d.Blobs, mail.Attachment{
		Filename:    filename,
		ContentType: contentType,
		Inline:      inline,
		ContentID:   contentID,
	}, data)
	if err != nil {
		if errors.Is(err, blob.ErrCapacityExceeded) {
			// Not a real Mailjet error - Mailjet has no such concept - but
			// tommy's blob store is capacity-capped and must fail loudly
			// rather than silently evict or panic (docs/contracts.md).
			return newValidationError("tommy-capacity-exceeded", http.StatusInsufficientStorage,
				fmt.Sprintf("tommy's blob store capacity is exceeded, cannot store %q: %v", filename, err), []string{field})
		}
		return newValidationError("mj-0004", http.StatusBadRequest,
			fmt.Sprintf("could not store attachment %q: %v", filename, err), []string{field})
	}
	return nil
}

// recipients builds the response's per-recipient result list, always non-nil
// so it marshals as `[]` rather than `null` - the real API always includes
// "To", "Cc" and "Bcc", empty or not.
func (p *Provider) recipients(addrs []wireAddress, sandbox bool) []recipientResult {
	out := make([]recipientResult, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, p.recipientResult(a.Email, sandbox))
	}
	return out
}

// recipientResult mints one recipient's result. Real Mailjet generates a
// distinct MessageID/MessageUUID per recipient, even within one logical
// message, because each recipient becomes its own outbound message
// internally - the guide's own multi-recipient example shows this. In
// SandboxMode the real API omits MessageID/MessageUUID (zero/empty) per
// https://dev.mailjet.com/docs/email-api/send-api-v31/sandbox.mode.
func (p *Provider) recipientResult(email string, sandbox bool) recipientResult {
	if sandbox {
		return recipientResult{Email: email, MessageUUID: "", MessageID: 0, MessageHref: "https://api.mailjet.com/v3/message/0"}
	}
	id := messageIDBase + p.seq.Add(1)
	return recipientResult{
		Email:       email,
		MessageUUID: newUUID(),
		MessageID:   id,
		MessageHref: fmt.Sprintf("https://api.mailjet.com/v3/message/%d", id),
	}
}

// recipientMeta flattens every recipient's generated ids into Event.Meta, so
// the UI/API can show what tommy handed back without re-deriving it from the
// response envelope.
func recipientMeta(groups ...[]recipientResult) []map[string]any {
	var out []map[string]any
	for _, g := range groups {
		for _, r := range g {
			out = append(out, map[string]any{
				"email":        r.Email,
				"message_id":   r.MessageID,
				"message_uuid": r.MessageUUID,
				"message_href": r.MessageHref,
			})
		}
	}
	return out
}

func toAddresses(in []wireAddress) []mail.Address {
	if len(in) == 0 {
		return nil
	}
	out := make([]mail.Address, 0, len(in))
	for _, a := range in {
		out = append(out, mail.Address{Email: a.Email, Name: a.Name})
	}
	return out
}

// sortedKeys returns m's keys in a stable order, so the sequence of
// msg.Headers.Set calls - and therefore the header order a listing renders -
// is deterministic regardless of Go's randomized map iteration.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// newUUID returns a random RFC 4122 v4 UUID, the shape Mailjet's real
// MessageUUID has.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeGlobalError(w http.ResponseWriter, status int, code, message string, relatedTo []string) {
	writeJSON(w, status, globalError{
		ErrorIdentifier: newUUID(),
		ErrorCode:       code,
		StatusCode:      status,
		ErrorMessage:    message,
		ErrorRelatedTo:  relatedTo,
	})
}
