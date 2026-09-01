// Package sendgrid imitates the Twilio SendGrid v3 Mail Send API
// (https://www.twilio.com/docs/sendgrid/api-reference/mail-send/mail-send).
//
// It mounts POST /v3/mail/send on the shared ingress and converts each
// personalizations[] entry into one canonical mail.Message: SendGrid's fan-out
// is exactly the "one request, several delivered messages" case the mail
// plugin's Message is built for. Everything SendGrid-specific - categories,
// custom_args, send_at, batch_id, asm, ip_pool_name and the credentials that
// were presented - is recorded on Event.Meta, never on the canonical model.
package sendgrid

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/mail"
)

// ProviderName is this provider's name: the URL segment under which it is
// listed and the value stamped on every event it produces.
const ProviderName = "sendgrid"

// SendPath is the route this provider mounts, matching the real API exactly.
const SendPath = "/v3/mail/send"

// MaxBody caps the request body. SendGrid's own documented limit for a mail
// send request (attachments included, base64 inflates them by ~4/3) is 30MB;
// a little headroom keeps a body sitting exactly on the line from failing for
// the wrong reason.
const MaxBody = 32 << 20

// Provider is the SendGrid v3 Mail Send fake.
type Provider struct{}

// New returns the SendGrid provider.
func New() *Provider { return &Provider{} }

// Name implements plugin.Provider.
func (p *Provider) Name() string { return ProviderName }

// Plugin implements plugin.Provider.
func (p *Provider) Plugin() string { return mail.PluginName }

// Description implements plugin.Provider.
func (p *Provider) Description() string {
	return "Imitates the Twilio SendGrid v3 Mail Send API. It fans personalizations[] out into one event per delivered message, " +
		"stores attachments through the blob store, and answers with the real 202-empty-body-plus-X-Message-Id contract."
}

// Endpoints implements plugin.Provider.
func (p *Provider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{{
		Method: "POST",
		Path:   SendPath,
		Description: "Accept a v3 Mail Send request, fan personalizations[] out into one event per delivered message, " +
			"and answer 202 with an empty body and an X-Message-Id header.",
	}}
}

// Snippets implements plugin.Provider.
func (p *Provider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{{
		Title: "Send an email via the SendGrid v3 API",
		Lang:  "bash",
		Code: `curl -si {{.IngressURL}}` + SendPath + ` \
  -H 'Authorization: Bearer SG.fake-key' \
  -H 'Content-Type: application/json' -d '{
  "personalizations": [{"to": [{"email": "bob@example.com", "name": "Bob"}], "subject": "Hello from tommy"}],
  "from": {"email": "alice@example.com", "name": "Alice"},
  "content": [
    {"type": "text/plain", "value": "It works."},
    {"type": "text/html", "value": "<p>It <b>works</b>.</p>"}
  ]
}'`,
	}}
}

// RegisterIngress implements plugin.Provider.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()

	mux.HandleFunc("POST "+SendPath, func(w http.ResponseWriter, r *http.Request) {
		handleSend(w, r, d)
	})
}

// --- wire format -----------------------------------------------------------

// wireAddress is one `{"email":"…","name":"…"}` object.
type wireAddress struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

func (a wireAddress) toAddress() mail.Address {
	return mail.Address{Name: a.Name, Email: a.Email}
}

func toAddresses(list []wireAddress) []mail.Address {
	if len(list) == 0 {
		return nil
	}
	out := make([]mail.Address, 0, len(list))
	for _, a := range list {
		out = append(out, a.toAddress())
	}
	return out
}

// personalization is one entry of the top-level personalizations[] array. Per
// the spec, subject and headers here override the message-level values; to,
// cc, bcc, custom_args and send_at are personalization-specific.
type personalization struct {
	To         []wireAddress     `json:"to"`
	CC         []wireAddress     `json:"cc,omitempty"`
	BCC        []wireAddress     `json:"bcc,omitempty"`
	Subject    string            `json:"subject,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	CustomArgs map[string]any    `json:"custom_args,omitempty"`
	SendAt     *int64            `json:"send_at,omitempty"`
}

// wireContent is one entry of content[]: {"type":"text/plain","value":"…"}.
type wireContent struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// wireAttachment is one entry of attachments[].
type wireAttachment struct {
	Content     string `json:"content"` // base64
	Type        string `json:"type,omitempty"`
	Filename    string `json:"filename"`
	Disposition string `json:"disposition,omitempty"` // "attachment" | "inline"
	ContentID   string `json:"content_id,omitempty"`
}

// sendRequest is the full v3 Mail Send request body. Fields tommy has no use
// for beyond echoing back into Meta (mail_settings, tracking_settings,
// template_id, substitutions, dynamic_template_data) are deliberately left
// unparsed: the contract (§6.3) is that a provider owns its namespace and can
// grow, not that every SendGrid field needs a home on day one.
type sendRequest struct {
	Personalizations []personalization `json:"personalizations"`
	From             *wireAddress      `json:"from"`
	ReplyTo          *wireAddress      `json:"reply_to,omitempty"`
	ReplyToList      []wireAddress     `json:"reply_to_list,omitempty"`
	Subject          string            `json:"subject,omitempty"`
	Content          []wireContent     `json:"content,omitempty"`
	Attachments      []wireAttachment  `json:"attachments,omitempty"`
	Categories       []string          `json:"categories,omitempty"`
	CustomArgs       map[string]any    `json:"custom_args,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	SendAt           *int64            `json:"send_at,omitempty"`
	BatchID          string            `json:"batch_id,omitempty"`
	ASM              map[string]any    `json:"asm,omitempty"`
	IPPoolName       string            `json:"ip_pool_name,omitempty"`
}

// --- error shape -------------------------------------------------------

// sgError is one entry of SendGrid's {"errors":[...]} error body.
type sgError struct {
	Message string  `json:"message"`
	Field   *string `json:"field"`
	Help    any     `json:"help,omitempty"`
}

// errorBody is the envelope every non-2xx SendGrid response carries.
type errorBody struct {
	Errors []sgError `json:"errors"`
}

func fieldErr(status int, message, field string) (int, errorBody) {
	f := field
	return status, errorBody{Errors: []sgError{{Message: message, Field: &f}}}
}

func plainErr(status int, message string) (int, errorBody) {
	return status, errorBody{Errors: []sgError{{Message: message, Field: nil}}}
}

func writeError(w http.ResponseWriter, status int, body errorBody) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writePlainError(w http.ResponseWriter, status int, message string) {
	status, body := plainErr(status, message)
	writeError(w, status, body)
}

func writeFieldError(w http.ResponseWriter, status int, message, field string) {
	status, body := fieldErr(status, message, field)
	writeError(w, status, body)
}

// --- handler -------------------------------------------------------------

func handleSend(w http.ResponseWriter, r *http.Request, d plugin.Deps) {
	ctx := r.Context()

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil {
		writePlainError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	if len(body) > MaxBody {
		writePlainError(w, http.StatusRequestEntityTooLarge, "request body exceeds the maximum allowed size")
		return
	}

	presentedAuth := r.Header.Get("Authorization")
	if status, ebody, ok := checkAuth(d, presentedAuth); !ok {
		writeError(w, status, ebody)
		return
	}

	var req sendRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writePlainError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	if status, ebody, ok := validate(req); !ok {
		writeError(w, status, ebody)
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

	messageID := d.NewID() + ".filter-001.pop-sendgrid"

	// Decode every attachment once; each personalization's message gets its
	// own copy of the bytes through Message.AttachBytes, which is the only
	// door mail bytes go through into the blob store.
	type decoded struct {
		wireAttachment
		data []byte
	}
	decodedAttachments := make([]decoded, 0, len(req.Attachments))
	for i, a := range req.Attachments {
		data, err := base64.StdEncoding.DecodeString(a.Content)
		if err != nil {
			writeFieldError(w, http.StatusBadRequest,
				fmt.Sprintf("attachments[%d].content is not valid base64: %v", i, err),
				fmt.Sprintf("attachments.%d.content", i))
			return
		}
		decodedAttachments = append(decodedAttachments, decoded{wireAttachment: a, data: data})
	}

	for i, pz := range req.Personalizations {
		msg := buildMessage(req, pz)

		for j, a := range decodedAttachments {
			if _, err := msg.AttachBytes(ctx, d.Blobs, mail.Attachment{
				Filename:    a.Filename,
				ContentType: a.Type,
				Inline:      strings.EqualFold(a.Disposition, "inline"),
				ContentID:   a.ContentID,
			}, a.data); err != nil {
				if errors.Is(err, blob.ErrCapacityExceeded) {
					writeFieldError(w, http.StatusRequestEntityTooLarge,
						fmt.Sprintf("attachments[%d] could not be stored: %v", j, err),
						fmt.Sprintf("attachments.%d", j))
					return
				}
				writePlainError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}

		ev := mail.NewEvent(ProviderName, msg)
		ev.Raw = raw
		ev.Meta = buildMeta(req, pz, presentedAuth, messageID, i)

		if err := d.Append(ctx, ev); err != nil {
			writePlainError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	w.Header().Set("X-Message-Id", messageID)
	w.WriteHeader(http.StatusAccepted)
}

// checkAuth accepts anything by default and only rejects when the provider
// config pins an expected key (config key "api_key"), using SendGrid's real
// 401 error shape on a mismatch.
func checkAuth(d plugin.Deps, presented string) (int, errorBody, bool) {
	expected := d.Config.String("api_key", "")
	if expected == "" {
		return 0, errorBody{}, true
	}
	if presented == "" {
		status, body := plainErr(http.StatusUnauthorized, "Permission denied, wrong credentials")
		return status, body, false
	}
	if presented != "Bearer "+expected {
		status, body := plainErr(http.StatusUnauthorized, "The provided authorization grant is invalid, expired, or revoked")
		return status, body, false
	}
	return 0, errorBody{}, true
}

// validate reproduces just enough of SendGrid's own validation to give a
// realistic error shape for the mistakes an SDK actually makes.
func validate(req sendRequest) (int, errorBody, bool) {
	if req.From == nil || strings.TrimSpace(req.From.Email) == "" {
		status, body := fieldErr(http.StatusBadRequest, "The from field is required.", "from.email")
		return status, body, false
	}
	if len(req.Personalizations) == 0 {
		status, body := fieldErr(http.StatusBadRequest, "personalizations array must contain at least one personalization", "personalizations")
		return status, body, false
	}
	if len(req.Personalizations) > 1000 {
		status, body := fieldErr(http.StatusBadRequest, "personalizations array can contain a maximum of 1000 personalizations", "personalizations")
		return status, body, false
	}
	for i, pz := range req.Personalizations {
		if len(pz.To) == 0 {
			status, body := fieldErr(http.StatusBadRequest,
				fmt.Sprintf("The personalizations.%d.to field is required.", i),
				fmt.Sprintf("personalizations.%d.to", i))
			return status, body, false
		}
	}
	if req.ReplyTo != nil && len(req.ReplyToList) > 0 {
		status, body := fieldErr(http.StatusBadRequest,
			"cannot set both reply_to and reply_to_list", "reply_to_list")
		return status, body, false
	}
	for i, c := range req.Content {
		if strings.TrimSpace(c.Type) == "" || strings.TrimSpace(c.Value) == "" {
			status, body := fieldErr(http.StatusBadRequest,
				fmt.Sprintf("content[%d] must have both a type and a value", i),
				fmt.Sprintf("content.%d", i))
			return status, body, false
		}
	}
	for i, a := range req.Attachments {
		if strings.TrimSpace(a.Content) == "" {
			status, body := fieldErr(http.StatusBadRequest,
				fmt.Sprintf("attachments[%d].content is required", i),
				fmt.Sprintf("attachments.%d.content", i))
			return status, body, false
		}
		if strings.TrimSpace(a.Filename) == "" {
			status, body := fieldErr(http.StatusBadRequest,
				fmt.Sprintf("attachments[%d].filename is required", i),
				fmt.Sprintf("attachments.%d.filename", i))
			return status, body, false
		}
		if a.Disposition != "" && a.Disposition != "attachment" && a.Disposition != "inline" {
			status, body := fieldErr(http.StatusBadRequest,
				fmt.Sprintf("attachments[%d].disposition must be either 'inline' or 'attachment'", i),
				fmt.Sprintf("attachments.%d.disposition", i))
			return status, body, false
		}
	}
	return 0, errorBody{}, true
}

// buildMessage converts one personalization plus the message-level fields
// into tommy's canonical Message. Per-personalization subject and headers
// override the top-level ones; everything else on a personalization
// (to/cc/bcc/custom_args/send_at) has no top-level equivalent to merge with.
func buildMessage(req sendRequest, pz personalization) *mail.Message {
	msg := &mail.Message{
		To:  toAddresses(pz.To),
		Cc:  toAddresses(pz.CC),
		Bcc: toAddresses(pz.BCC),
	}
	if req.From != nil {
		msg.From = req.From.toAddress()
	}

	msg.Subject = req.Subject
	if pz.Subject != "" {
		msg.Subject = pz.Subject
	}

	switch {
	case len(req.ReplyToList) > 0:
		msg.ReplyTo = toAddresses(req.ReplyToList)
	case req.ReplyTo != nil:
		msg.ReplyTo = []mail.Address{req.ReplyTo.toAddress()}
	}

	for _, c := range req.Content {
		switch strings.ToLower(strings.TrimSpace(c.Type)) {
		case "text/plain":
			msg.Text = c.Value
		case "text/html":
			msg.HTML = c.Value
		}
	}

	// Merge direction: start from the message-level headers, then let the
	// personalization's headers override same-named entries, matching the
	// spec's "personalizations[].headers... override the individual field
	// values" rule.
	for k, v := range req.Headers {
		msg.Headers.Set(k, v)
	}
	for k, v := range pz.Headers {
		msg.Headers.Set(k, v)
	}

	return msg
}

// buildMeta assembles the provider metadata for one fanned-out event.
// custom_args merges the same direction as headers: personalization-level
// overrides message-level.
func buildMeta(req sendRequest, pz personalization, presentedAuth, messageID string, index int) map[string]any {
	customArgs := map[string]any{}
	for k, v := range req.CustomArgs {
		customArgs[k] = v
	}
	for k, v := range pz.CustomArgs {
		customArgs[k] = v
	}

	sendAt := req.SendAt
	if pz.SendAt != nil {
		sendAt = pz.SendAt
	}

	meta := map[string]any{
		"message_id":            messageID,
		"personalization_index": index,
		"batch_id":              req.BatchID,
		"ip_pool_name":          req.IPPoolName,
	}
	if len(req.Categories) > 0 {
		meta["categories"] = req.Categories
	}
	if len(customArgs) > 0 {
		meta["custom_args"] = customArgs
	}
	if sendAt != nil {
		meta["send_at"] = *sendAt
	}
	if len(req.ASM) > 0 {
		meta["asm"] = req.ASM
	}
	if presentedAuth != "" {
		meta["authorization"] = presentedAuth
	}
	return meta
}
