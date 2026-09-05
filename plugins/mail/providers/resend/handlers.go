package resend

import (
	"context"
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

// createdAtLayout is the timestamp format the live reference's retrieve
// example uses - "2026-04-03 22:13:42.674981+00", a space-separated Postgres
// rendering rather than RFC 3339. Reproducing it matters: a client that parses
// what Resend actually sends would break on an RFC 3339 string.
const createdAtLayout = "2006-01-02 15:04:05.000000-07"

// --- shared request plumbing ---------------------------------------------

// readBody reads the request body under MaxBody, answering Resend-shaped
// errors. The 413 is composed rather than quoted: the live error reference
// documents no code for an oversized body, so this reuses validation_error,
// which is the closest documented name. UNVERIFIED.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil {
		writeError(w, newError(http.StatusBadRequest, codeValidationError, "The request body could not be read."))
		return nil, false
	}
	if len(body) > MaxBody {
		writeError(w, newError(http.StatusRequestEntityTooLarge, codeValidationError,
			"The request body is larger than this endpoint accepts."))
		return nil, false
	}
	return body, true
}

// checkAuth accepts any bearer token by default - a fake that 401s is useless
// - and only checks when the provider config pins one under "api_key". A
// missing header then gets Resend's documented missing_api_key; a wrong one
// gets validation_error, which is what the API is widely reported to answer
// and the closest documented name for "this key is not usable". UNVERIFIED:
// the live error reference lists no code for a key that simply does not
// match.
func checkAuth(w http.ResponseWriter, d plugin.Deps, presented string) bool {
	expected := d.Config.String("api_key", "")
	if expected == "" {
		return true
	}
	if strings.TrimSpace(presented) == "" {
		writeError(w, newError(http.StatusUnauthorized, codeMissingAPIKey, msgMissingAPIKey))
		return false
	}
	if presented != "Bearer "+expected {
		writeError(w, newError(http.StatusUnauthorized, codeValidationError, "API key is invalid"))
		return false
	}
	return true
}

// checkIdempotencyKey enforces only the length rule the reference states, and
// records nothing else. tommy never deduplicates on this key: deduplication
// means remembering which keys were seen and for how long, which is state, and
// state is the scenario machinery tommy is deliberately not.
func checkIdempotencyKey(w http.ResponseWriter, key string) bool {
	if len(key) > MaxIdempotencyKey {
		writeError(w, newError(http.StatusBadRequest, codeInvalidIdempotencyKey, msgIdempotencyKey))
		return false
	}
	return true
}

// rawOf captures the untouched request, as every provider must.
func rawOf(r *http.Request, body []byte) event.Raw {
	return event.Raw{
		Transport: "http",
		PeerAddr:  r.RemoteAddr,
		Method:    r.Method,
		Path:      r.URL.Path,
		Headers:   r.Header.Clone(),
		Body:      body,
		Text:      true,
	}
}

// --- POST /emails ---------------------------------------------------------

func handleSend(w http.ResponseWriter, r *http.Request, d plugin.Deps) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	presentedAuth := r.Header.Get("Authorization")
	if !checkAuth(w, d, presentedAuth) {
		return
	}
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if !checkIdempotencyKey(w, idempotencyKey) {
		return
	}

	var req emailRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, newError(http.StatusBadRequest, codeValidationError, msgValidationError))
		return
	}

	built, errBody, ok := build(&req)
	if !ok {
		writeError(w, errBody)
		return
	}

	m := meta{
		IdempotencyKey: idempotencyKey,
		Request:        &req,
		Remote:         built.remote,
		Authorization:  presentedAuth,
	}

	id, errBody, ok := appendOne(r.Context(), d, built, m, rawOf(r, body))
	if !ok {
		writeError(w, errBody)
		return
	}

	writeJSON(w, http.StatusOK, sendResponse{ID: id})
}

// --- POST /emails/batch ---------------------------------------------------

func handleBatch(w http.ResponseWriter, r *http.Request, d plugin.Deps) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	presentedAuth := r.Header.Get("Authorization")
	if !checkAuth(w, d, presentedAuth) {
		return
	}
	idempotencyKey := r.Header.Get("Idempotency-Key")
	if !checkIdempotencyKey(w, idempotencyKey) {
		return
	}

	var reqs []emailRequest
	if err := json.Unmarshal(body, &reqs); err != nil {
		writeError(w, newError(http.StatusBadRequest, codeValidationError, msgValidationError))
		return
	}
	// Both bounds are composed rather than quoted; the reference states the
	// limit ("up to 100 batch emails at once") but not the error it answers
	// with. UNVERIFIED wording, documented status.
	if len(reqs) == 0 {
		writeError(w, newError(http.StatusUnprocessableEntity, codeMissingRequiredField,
			"The request body must contain at least one email."))
		return
	}
	if len(reqs) > MaxBatch {
		writeError(w, newError(http.StatusUnprocessableEntity, codeInvalidParameter,
			fmt.Sprintf("A batch can contain at most %d emails.", MaxBatch)))
		return
	}

	// Validate the whole batch before appending anything: strict validation is
	// the API's own default, and half a batch in the store would be a lie
	// about what the caller was told.
	built := make([]*builtMessage, 0, len(reqs))
	for i := range reqs {
		b, errBody, ok := build(&reqs[i])
		if !ok {
			writeError(w, errBody)
			return
		}
		built = append(built, b)
	}

	raw := rawOf(r, body)
	out := make([]sendResponse, 0, len(built))
	for i, b := range built {
		index := i
		size := len(built)
		m := meta{
			IdempotencyKey:  idempotencyKey,
			BatchIndex:      &index,
			BatchSize:       &size,
			BatchValidation: r.Header.Get("x-batch-validation"),
			Request:         &reqs[i],
			Remote:          b.remote,
			Authorization:   presentedAuth,
		}
		id, errBody, ok := appendOne(r.Context(), d, b, m, raw)
		if !ok {
			writeError(w, errBody)
			return
		}
		out = append(out, sendResponse{ID: id})
	}

	writeJSON(w, http.StatusOK, batchResponse{Data: out})
}

// --- GET /emails/{id} -----------------------------------------------------

func handleGet(w http.ResponseWriter, r *http.Request, d plugin.Deps) {
	emailID := r.PathValue("id")

	eventID, ok := eventIDFromEmailID(emailID)
	if !ok {
		// Not an id this provider minted. A syntactically valid UUID is
		// simply unknown; anything else is not an email id at all, which the
		// reference gives its own 422 for ("The `parameter` must be a valid
		// UUID"). The raw form is tried in between because emailIDFor passes
		// a non-hex event id through unchanged.
		if looksLikeUUID(emailID) {
			writeError(w, newError(http.StatusNotFound, codeNotFound, msgEmailNotFound))
			return
		}
		eventID = emailID
	}

	ev, err := d.Store.Get(r.Context(), event.ID(eventID))
	if err != nil || ev.Plugin != mail.PluginName || ev.Provider != ProviderName || ev.Type != mail.TypeMessage {
		if !looksLikeUUID(emailID) && !isHex24(emailID) {
			writeError(w, newError(http.StatusUnprocessableEntity, codeInvalidParameter,
				"The `id` must be a valid UUID."))
			return
		}
		writeError(w, newError(http.StatusNotFound, codeNotFound, msgEmailNotFound))
		return
	}

	msg, ok := mail.MessageOf(ev)
	if !ok {
		writeError(w, newError(http.StatusNotFound, codeNotFound, msgEmailNotFound))
		return
	}

	writeJSON(w, http.StatusOK, buildResource(d, ev, msg))
}

// buildResource renders one stored event as the retrieve response, straight
// out of the store - so an SDK that sends and then fetches sees its own write.
func buildResource(d plugin.Deps, ev *event.Event, msg *mail.Message) emailResource {
	m := metaOf(ev.Meta)

	res := emailResource{
		Object:      "email",
		ID:          emailIDFor(string(ev.ID)),
		MessageID:   m.MessageID,
		To:          addressStrings(msg.To),
		From:        formatAddress(msg.From),
		CreatedAt:   ev.ReceivedAt.UTC().Format(createdAtLayout),
		Subject:     msg.Subject,
		Bcc:         addressStrings(msg.Bcc),
		Cc:          addressStrings(msg.Cc),
		ReplyTo:     addressStrings(msg.ReplyTo),
		LastEvent:   d.Config.String("last_event", DefaultLastEvent),
		ScheduledAt: nilIfEmpty(m.ScheduledAt),
		Tags:        m.Tags,
	}
	if m.HasHTML {
		html := msg.HTML
		res.HTML = &html
	}
	if m.HasText {
		text := msg.Text
		res.Text = &text
	}
	if res.Tags == nil {
		res.Tags = []wireTag{}
	}
	return res
}

// --- request -> canonical message ----------------------------------------

// builtMessage is one validated email: the canonical message, plus the parts
// of the request that do not belong on it.
type builtMessage struct {
	msg *mail.Message
	// attachments are the ones whose bytes arrived inline and therefore go
	// to the blob store.
	attachments []inlineAttachment
	// remote are the ones that carried a `path` instead. tommy never fetches
	// them - it makes no outbound requests - so only the URL is recorded.
	remote []remoteAttachment
}

type inlineAttachment struct {
	att  mail.Attachment
	data []byte
}

type remoteAttachment struct {
	Filename    string `json:"filename,omitempty"`
	Path        string `json:"path"`
	ContentType string `json:"content_type,omitempty"`
	ContentID   string `json:"content_id,omitempty"`
}

// build validates one email request and converts it into the canonical model.
func build(req *emailRequest) (*builtMessage, errorBody, bool) {
	if strings.TrimSpace(req.From) == "" {
		return nil, missingField("from"), false
	}
	if len(req.To) == 0 {
		return nil, missingField("to"), false
	}
	// A template may carry its own default subject, which is the one case the
	// reference says the payload need not provide one.
	if strings.TrimSpace(req.Subject) == "" && req.Template == nil {
		return nil, missingField("subject"), false
	}
	if req.Template != nil && (req.HTML != nil || req.Text != nil) {
		// The reference states the API returns a validation error here but
		// does not quote it. UNVERIFIED wording.
		return nil, newError(http.StatusUnprocessableEntity, codeValidationError,
			"The `template` field cannot be combined with `html` or `text`."), false
	}
	// Ordered, not a map range: which field is named in the error must not
	// depend on Go's map iteration order.
	for _, f := range []struct {
		name string
		list stringList
	}{{"to", req.To}, {"cc", req.Cc}, {"bcc", req.Bcc}} {
		if len(f.list) > MaxRecipients {
			return nil, newError(http.StatusUnprocessableEntity, codeInvalidParameter,
				fmt.Sprintf("The `%s` field accepts at most %d addresses.", f.name, MaxRecipients)), false
		}
	}

	from, err := mail.ParseAddress(req.From)
	if err != nil {
		return nil, invalidAddress("from"), false
	}
	to, errBody, ok := parseList("to", req.To)
	if !ok {
		return nil, errBody, false
	}
	cc, errBody, ok := parseList("cc", req.Cc)
	if !ok {
		return nil, errBody, false
	}
	bcc, errBody, ok := parseList("bcc", req.Bcc)
	if !ok {
		return nil, errBody, false
	}
	replyTo, errBody, ok := parseList("reply_to", req.ReplyTo)
	if !ok {
		return nil, errBody, false
	}

	msg := &mail.Message{
		From:    from,
		To:      to,
		Cc:      cc,
		Bcc:     bcc,
		ReplyTo: replyTo,
		Subject: req.Subject,
	}
	if req.HTML != nil {
		msg.HTML = *req.HTML
	}
	if req.Text != nil {
		msg.Text = *req.Text
	}
	for k, v := range req.Headers {
		msg.Headers.Set(k, v)
	}

	out := &builtMessage{msg: msg}
	for _, a := range req.Attachments {
		switch {
		case a.Content.present:
			out.attachments = append(out.attachments, inlineAttachment{
				att: mail.Attachment{
					Filename:    a.Filename,
					ContentType: a.ContentType,
					Inline:      a.cid() != "",
					ContentID:   a.cid(),
				},
				data: a.Content.data,
			})
		case strings.TrimSpace(a.Path) != "":
			out.remote = append(out.remote, remoteAttachment{
				Filename:    a.Filename,
				Path:        a.Path,
				ContentType: a.ContentType,
				ContentID:   a.cid(),
			})
		default:
			return nil, newError(http.StatusUnprocessableEntity, codeInvalidAttachment, msgInvalidAttach), false
		}
	}
	return out, errorBody{}, true
}

// parseList turns a to/cc/bcc/reply_to union value into canonical addresses,
// rejecting a malformed one the way the real API does.
func parseList(field string, list stringList) ([]mail.Address, errorBody, bool) {
	if len(list) == 0 {
		return nil, errorBody{}, true
	}
	out := make([]mail.Address, 0, len(list))
	for _, s := range list {
		a, err := mail.ParseAddress(s)
		if err != nil {
			return nil, invalidAddress(field), false
		}
		out = append(out, a)
	}
	return out, errorBody{}, true
}

// appendOne stores the attachments, appends the event, and returns the email
// id the API answers with.
func appendOne(ctx context.Context, d plugin.Deps, b *builtMessage, m meta, raw event.Raw) (string, errorBody, bool) {
	for _, a := range b.attachments {
		if _, err := b.msg.AttachBytes(ctx, d.Blobs, a.att, a.data); err != nil {
			if errors.Is(err, blob.ErrCapacityExceeded) {
				// tommy's own ceiling, not Resend's; invalid_attachment is
				// the closest documented name. UNVERIFIED wording.
				return "", newError(http.StatusUnprocessableEntity, codeInvalidAttachment,
					"Attachment could not be stored: "+err.Error()), false
			}
			return "", newError(http.StatusInternalServerError, codeApplicationError, msgApplicationError), false
		}
	}

	ev := mail.NewEvent(ProviderName, b.msg)
	ev.Raw = raw
	ev.ID = event.ID(d.NewID())

	emailID := emailIDFor(string(ev.ID))
	m.EmailID = emailID
	m.MessageID = messageIDFor(emailID, b.msg.From.Email)
	ev.Meta = m.toMap()

	if err := d.Append(ctx, ev); err != nil {
		return "", newError(http.StatusInternalServerError, codeApplicationError, msgApplicationError), false
	}
	return emailID, errorBody{}, true
}

// messageIDFor mints the RFC 5322 Message-ID the retrieve endpoint reports.
// The live example - "<111-222-333@email.example.com>" - is built from the
// sending domain, so this does the same, falling back to a literal when the
// from address has no domain to take.
func messageIDFor(emailID, fromEmail string) string {
	domain := "resend.tommy.local"
	if at := strings.LastIndex(fromEmail, "@"); at >= 0 && at+1 < len(fromEmail) {
		domain = fromEmail[at+1:]
	}
	return "<" + emailID + "@" + domain + ">"
}

// --- helpers --------------------------------------------------------------

// writeJSON renders a success body. HTML escaping is turned off deliberately:
// Resend's API is a JavaScript service and JSON.stringify does not escape
// "<", ">" or "&", so a captured HTML body comes back off the real wire as
// `"html":"<p>hi</p>"` rather than Go's default `"\u003cp\u003e"`. Both
// decode to the same string, but a fake should put the same bytes on the wire
// as the thing it stands in for. Nothing here reaches a page - the mail
// plugin's own UI serves captured HTML from its sandboxed route.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

// addressStrings renders an address list the way the retrieve response shows
// it: a bare address when there is no display name, RFC 5322 form when there
// is. The reference's own example has a bare `to` and a named `from`, which
// is exactly what this produces.
func addressStrings(list []mail.Address) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		if s := formatAddress(a); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// formatAddress is mail.Address.String() minus two habits of net/mail's own
// formatter, both of which would put bytes on the wire that Resend never
// sends. net/mail quotes every display name, so `Acme <a@example.com>` comes
// back as `"Acme" <a@example.com>`; and it RFC 2047-encodes a non-ASCII name,
// so `Ünicode` becomes `=?utf-8?q?=C3=9Cnicode?=`. Both are correct for a
// message header and wrong for a JSON body, which carries UTF-8 directly and
// which the vendor fills by echoing the string it was given.
func formatAddress(a mail.Address) string {
	if a.Name == "" || a.Email == "" {
		return a.String()
	}
	if !needsQuoting(a.Name) {
		return a.Name + " <" + a.Email + ">"
	}
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a.Name)
	return `"` + escaped + `" <` + a.Email + ">"
}

// needsQuoting reports whether a display name may not appear bare in a
// name-addr. RFC 5322 lets a phrase be a run of atoms separated by spaces;
// anything else - a special character, a non-ASCII rune, a leading or
// trailing space - has to be a quoted-string.
func needsQuoting(name string) bool {
	if name != strings.TrimSpace(name) {
		return true
	}
	for _, r := range name {
		if r == ' ' {
			continue
		}
		if r > 127 || r < 33 {
			return true
		}
		if strings.ContainsRune("()<>[]:;@\\,.\"", r) {
			return true
		}
	}
	return false
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
