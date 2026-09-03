package as2

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
)

// Receiver is the seam between the as2 plugin core and any AS2 provider. It is
// shaped like files.Session and for the same reason: the core does the work and
// appends the event, so a provider is a route, a body read and a response
// write, and two providers cannot drift in what they record.
//
// A provider does this and nothing else:
//
//	type Provider struct{ recv *as2.Receiver; ident *as2.Identity }
//
//	func (p *Provider) BindIdentity(id *as2.Identity) { p.ident = id }
//
//	func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
//	    // RegisterIngress is the only place with both a ProviderConfig and a
//	    // ConfigDir, so this is where the identity is configured - and why
//	    // nothing is generated for a provider that is switched off.
//	    _ = p.ident.Configure(as2.IdentityConfig{
//	        CertFile:  d.Config.String("cert_file", ""),
//	        KeyFile:   d.Config.String("key_file", ""),
//	        ConfigDir: d.ConfigDir,
//	    })
//	    p.recv = as2.NewReceiver(p.ident, d, as2.WithProvider(p.Name()))
//	    mux.HandleFunc("POST /as2", p.recv.Handle)
//	}
//
// Handle covers the whole exchange. A provider that needs to see the message,
// or to add its own metadata, calls Receive instead and writes the Response
// itself.
type Receiver struct {
	identity *Identity
	deps     plugin.Deps
	provider string

	reportingUA string
	maxBody     int64
	// meta is stamped on every event this receiver appends, for a provider
	// that wants to record something about itself.
	meta map[string]any
}

// DefaultMaxBody caps a captured AS2 message. EDI interchanges are large but
// not unbounded, and the blob store has a cap of its own; this stops a single
// request filling memory before that cap is consulted.
const DefaultMaxBody = 64 << 20

// ReceiverOption configures a Receiver.
type ReceiverOption func(*Receiver)

// WithProvider names the provider the events are recorded against.
func WithProvider(name string) ReceiverOption {
	return func(r *Receiver) { r.provider = name }
}

// WithReportingUA sets the Reporting-UA field of the MDNs this receiver emits.
func WithReportingUA(ua string) ReceiverOption {
	return func(r *Receiver) { r.reportingUA = ua }
}

// WithMaxBody caps the request body a receiver will read.
func WithMaxBody(n int64) ReceiverOption {
	return func(r *Receiver) {
		if n > 0 {
			r.maxBody = n
		}
	}
}

// WithMeta seeds metadata carried on every event this receiver appends.
func WithMeta(meta map[string]any) ReceiverOption {
	return func(r *Receiver) { r.meta = meta }
}

// NewReceiver binds an identity and the core's dependencies into the object a
// provider works through.
func NewReceiver(id *Identity, d plugin.Deps, opts ...ReceiverOption) *Receiver {
	if id == nil {
		id = NewIdentity()
	}
	r := &Receiver{identity: id, deps: d.Normalize(), maxBody: DefaultMaxBody}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Identity returns the key pair this receiver decrypts and signs with, for a
// provider that wants to publish the certificate itself.
func (r *Receiver) Identity() *Identity { return r.identity }

// Request is one inbound AS2 message as a provider saw it. Everything on it
// comes off the wire and is untrusted.
type Request struct {
	Method   string
	Path     string
	Host     string
	Header   http.Header
	Body     []byte
	PeerAddr string
}

// Response is what the provider writes back: an AS2 MDN, or an error report in
// the same shape.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Write sends the response. Content-Length is set explicitly because some AS2
// clients will not read a chunked MDN.
func (resp Response) Write(w http.ResponseWriter) error {
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(resp.Body)))
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, err := w.Write(resp.Body)
	return err
}

// Result is everything one receive produced: the response to write, the
// canonical message, and the event that was appended for it.
type Result struct {
	Response Response
	Message  *Message
	Event    *event.Event
}

// Handle is the whole provider-side exchange: read the body, capture the
// message, write the MDN. A provider that needs nothing else needs nothing else.
func (r *Receiver) Handle(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, r.maxBody+1))
	if err != nil {
		http.Error(w, "as2: read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(body)) > r.maxBody {
		http.Error(w, fmt.Sprintf("as2: request body exceeds %d bytes", r.maxBody), http.StatusRequestEntityTooLarge)
		return
	}

	res, err := r.Receive(req.Context(), Request{
		Method:   req.Method,
		Path:     req.URL.RequestURI(),
		Host:     req.Host,
		Header:   req.Header.Clone(),
		Body:     body,
		PeerAddr: req.RemoteAddr,
	})
	if err != nil {
		// Receive only errors when the event could not be stored, which is a
		// tommy problem rather than a partner one. Say so plainly instead of
		// pretending the message was received.
		http.Error(w, "as2: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := res.Response.Write(w); err != nil {
		r.deps.Logger.Warn("as2: write MDN", "err", err)
	}
}

// Receive captures one AS2 message and builds its MDN.
//
// It returns an error only when the event could not be appended. Everything a
// partner can get wrong - an unverifiable signature, undecryptable content, a
// compression algorithm nobody implements - is captured, stored and reported in
// the MDN's disposition, because a fake that refuses is a fake that teaches
// nothing.
func (r *Receiver) Receive(ctx context.Context, req Request) (*Result, error) {
	now := r.deps.Now()
	m := messageFromHeaders(req.Header)

	if len(req.Body) == 0 {
		m.AddIssue(IssueEmptyBody, SeverityError, "the request carried no body at all")
	}
	if m.From == "" || m.To == "" {
		m.AddIssue(IssueMissingIdentifier, SeverityWarning,
			"RFC 4130 §6.2 requires both AS2-From and AS2-To; the message was captured anyway")
	}
	if m.MessageID == "" {
		m.AddIssue(IssueMissingMessageID, SeverityWarning,
			"no Message-ID header, so the MDN's Original-Message-ID cannot correlate this exchange")
	}
	if m.Receipt.Async() {
		m.AddIssue(IssueAsyncReceiptRequested, SeverityInfo,
			"Receipt-Delivery-Option asked for the MDN to be delivered asynchronously to "+
				headerSafe(m.Receipt.AsyncURL)+". tommy never originates outbound traffic, "+
				"so the receipt was returned synchronously on this connection instead.")
	}

	top := NewEntity(mimeHeadersFrom(req.Header), nil, req.Body)
	a := unwrap(m, top, r.identity)
	planMIC(m, a, m.Receipt.MICAlgs)

	if a.payload != nil {
		r.storePayload(ctx, m, a)
	}

	resp, rec := BuildMDN(m, mdnOptions{
		identity:    r.identity,
		reportingUA: r.reportingUA,
		now:         now,
		newID:       r.deps.NewID,
		host:        req.Host,
	})
	if ref, err := r.putBlob(ctx, resp.Body, blob.Ref{
		ContentType: rec.contentType(),
		Filename:    "mdn.txt",
	}); err == nil {
		rec.Blob = &ref
	} else {
		r.deps.Logger.Warn("as2: store MDN body", "err", err)
	}
	m.MDN = rec

	ev := NewEvent(r.provider, m, req)
	for k, v := range r.meta {
		ev.Meta[k] = v
	}
	ev.ReceivedAt = now
	if err := r.deps.Append(ctx, ev); err != nil {
		return nil, fmt.Errorf("append event: %w", err)
	}
	return &Result{Response: resp, Message: m, Event: ev}, nil
}

// contentType is the MDN body's own media type, for the blob store.
func (rec *MDNRecord) contentType() string {
	if rec.Signed {
		return "multipart/signed"
	}
	return "multipart/report"
}

// storePayload puts the business document in the blob store and fills in
// Message.Payload.
//
// The bytes go to the blob store rather than onto the event because rule 9 says
// so and because an EDI interchange outlives the event announcing it. Note what
// is kept where: Raw.Body on the event is the request exactly as it arrived -
// ciphertext and all - while this blob is the document after every layer was
// peeled. Both matter and neither substitutes for the other; somebody debugging
// wants the plaintext, and somebody proving what a partner sent wants the
// original bytes.
func (r *Receiver) storePayload(ctx context.Context, m *Message, a analysis) {
	body := a.payload.Body
	decoded, err := a.payload.Decoded()
	if err != nil {
		m.AddIssue(IssueTransferEncoding, SeverityWarning, err.Error())
	} else {
		body = decoded
	}

	p := Payload{
		ContentType:      a.payload.Get("Content-Type"),
		Filename:         a.payload.Filename(),
		TransferEncoding: a.payload.TransferEncoding(),
		Size:             int64(len(body)),
	}
	// A layer that would not open leaves its own bytes as the payload. Saying
	// so stops the UI presenting a blob of ciphertext as an EDI document.
	if _, failed := m.FirstError(); failed {
		p.Recovered = true
	}
	p.Format = DetectFormat(p.ContentType, body)
	p.Preview = MakePreview(body, p.Format)

	if p.ContentType == "" && m.ContentType != "" && len(m.Layers) <= 1 {
		// A plain unsigned message has no MIME headers of its own: the HTTP
		// Content-Type is the entity's Content-Type.
		p.ContentType = m.ContentType
	}

	name := p.Filename
	if name == "" {
		name = defaultFilename(p.Format)
	}
	ref, err := r.putBlob(ctx, body, blob.Ref{ContentType: blobContentType(p), Filename: name})
	if err != nil {
		m.AddIssue(IssueMalformedMIME, SeverityWarning, "the payload could not be stored: "+err.Error())
	} else {
		p.Blob = &ref
	}
	m.Payload = p
}

// blobContentType is what the blob is served as. It is deliberately not the
// sender's Content-Type: that value is attacker-controlled and would decide how
// a browser treats the download. The API serves blobs with nosniff, and this
// keeps the declared type to something inert.
func blobContentType(p Payload) string {
	switch p.Format {
	case FormatBinary:
		return "application/octet-stream"
	default:
		return "text/plain; charset=utf-8"
	}
}

func defaultFilename(format string) string {
	switch format {
	case FormatX12:
		return "payload.x12"
	case FormatEDIFACT:
		return "payload.edi"
	case FormatXML:
		return "payload.xml"
	case FormatJSON:
		return "payload.json"
	case FormatBinary:
		return "payload.bin"
	default:
		return "payload.txt"
	}
}

func (r *Receiver) putBlob(ctx context.Context, body []byte, meta blob.Ref) (blob.Ref, error) {
	if r.deps.Blobs == nil {
		return blob.Ref{}, errors.New("no blob store configured")
	}
	return r.deps.Blobs.Put(ctx, bytes.NewReader(body), meta)
}

// ------------------------------------------------------------ header parsing

// mimeHeadersFrom lifts the entity headers out of the HTTP request.
//
// In AS2 the HTTP headers *are* the outermost MIME entity's headers - there is
// no second header block inside the body - so these three are the ones that
// describe the content. The rest of the request's headers stay on Raw, where
// they belong.
func mimeHeadersFrom(h http.Header) textproto.MIMEHeader {
	out := textproto.MIMEHeader{}
	for _, name := range []string{"Content-Type", "Content-Transfer-Encoding", "Content-Disposition", "Mime-Version"} {
		if v := h.Get(name); v != "" {
			out.Set(name, v)
		}
	}
	return out
}

// messageFromHeaders reads everything RFC 4130 §6 puts in the request headers.
// Nothing is validated away: an AS2-From that breaks the ABNF is still recorded
// as sent, because that is what the operator needs to see.
func messageFromHeaders(h http.Header) *Message {
	m := &Message{
		From:        h.Get("AS2-From"),
		To:          h.Get("AS2-To"),
		MessageID:   h.Get("Message-ID"),
		Subject:     h.Get("Subject"),
		Date:        h.Get("Date"),
		AS2Version:  h.Get("AS2-Version"),
		UserAgent:   h.Get("User-Agent"),
		ContentType: h.Get("Content-Type"),
	}
	m.Receipt = parseReceiptRequest(h)
	return m
}

// parseReceiptRequest reads the MDN request headers.
//
// Disposition-Notification-To's value is deliberately recorded and not used:
// RFC 4130 §7.3 makes a synchronous MDN go back on the same connection, so the
// address is decoration. Receipt-Delivery-Option is the header that asks for
// something else, and it is the one tommy will not honor.
func parseReceiptRequest(h http.Header) ReceiptRequest {
	req := ReceiptRequest{
		NotifyTo: h.Get("Disposition-Notification-To"),
		Options:  h.Get("Disposition-Notification-Options"),
		AsyncURL: h.Get("Receipt-Delivery-Option"),
	}
	req.Requested = req.NotifyTo != "" || req.Options != "" || req.AsyncURL != ""

	for _, param := range strings.Split(req.Options, ";") {
		attr, rest, ok := strings.Cut(param, "=")
		if !ok {
			continue
		}
		attr = strings.ToLower(strings.TrimSpace(attr))
		values := strings.Split(rest, ",")
		importance := strings.ToLower(strings.TrimSpace(values[0]))
		values = values[1:]

		switch attr {
		case "signed-receipt-protocol":
			req.SignedRequested = true
			req.SignedImportance = importance
			if len(values) > 0 {
				req.Protocol = strings.TrimSpace(values[0])
			}
		case "signed-receipt-micalg":
			for _, v := range values {
				if v = strings.TrimSpace(v); v != "" {
					req.MICAlgs = append(req.MICAlgs, v)
				}
			}
		}
	}
	return req
}

// EventMeta is the message's headers and findings as Event.Meta: the fields
// somebody filters a capture by. A provider adds its own keys to this rather
// than replacing it.
func (m *Message) EventMeta() map[string]any {
	meta := map[string]any{
		"security":   m.Security.Summary(),
		"signed":     m.Security.Signed,
		"encrypted":  m.Security.Encrypted,
		"compressed": m.Security.Compressed,
	}
	put := func(key, value string) {
		if value != "" {
			meta[key] = value
		}
	}
	put("as2_from", m.From)
	put("as2_to", m.To)
	put("message_id", m.MessageID)
	put("subject", m.Subject)
	put("as2_version", m.AS2Version)
	put("user_agent", m.UserAgent)
	put("payload_format", m.Payload.Format)
	put("payload_content_type", m.Payload.ContentType)
	if m.MIC != nil {
		meta["mic"] = m.MIC.Header()
		meta["mic_coverage"] = m.MIC.Coverage
	}
	if sig := m.Security.Signature; sig != nil {
		meta["signature_verified"] = sig.Verified
		if sig.Signer != nil {
			meta["signer"] = sig.Signer.Subject
		}
	}
	if m.MDN != nil {
		meta["disposition"] = m.MDN.Disposition
		meta["mdn_signed"] = m.MDN.Signed
	}
	if len(m.Issues) > 0 {
		codes := make([]string, 0, len(m.Issues))
		for _, i := range m.Issues {
			codes = append(codes, i.Code)
		}
		meta["issues"] = codes
	}
	return meta
}

// EventSummary is what the generic event log, the SSE stream and the store's
// search index see.
func (m *Message) EventSummary() event.Summary {
	s := event.Summary{Title: m.Title(), Snippet: m.Preview()}
	if m.From != "" {
		s.From = m.From
	}
	if m.To != "" {
		s.To = []string{m.To}
	}
	if s.Snippet == "" {
		s.Snippet = m.Security.Summary() + " · " + m.Payload.Format
	}
	return s
}

// NewEvent builds the event for one captured message.
//
// Raw.Body is the request body exactly as it arrived - the ciphertext, when the
// message was encrypted - because that is the copy of record and everything
// else can be re-derived from it. The decrypted document is in the blob store,
// reachable from Payload.Blob.
func NewEvent(provider string, m *Message, req Request) *event.Event {
	return &event.Event{
		Plugin:   Name,
		Provider: provider,
		Type:     EventType,
		Summary:  m.EventSummary(),
		Meta:     m.EventMeta(),
		Payload:  m,
		Raw: event.Raw{
			Transport: Transport,
			PeerAddr:  req.PeerAddr,
			Method:    req.Method,
			Path:      req.Path,
			Headers:   req.Header,
			Body:      req.Body,
			Text:      isMostlyText(req.Body),
		},
	}
}
