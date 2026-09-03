// Package as2 is tommy's AS2 content type: the canonical Message every AS2
// provider converts into, the S/MIME and MIME processing that produces it, the
// MDN receipt that goes back, the read-back API under /api/v1/as2/ and the tab
// under /ui/as2/.
//
// AS2 (RFC 4130) is EDIINT over HTTP. A trading partner POSTs an S/MIME message
// carrying an EDI document - signed, encrypted, compressed, in any combination -
// and the receiver answers synchronously with an MDN receipt saying what it
// found. Standing up a real AS2 endpoint to test against is famously miserable:
// certificates have to be exchanged, both ends configured, and the failure modes
// are all silent. That is exactly the shape of problem tommy exists for, and the
// MDN fits its charter because the reply is mechanical - everything in it is
// derivable from the request.
//
// Three things decide almost everything in this package:
//
//   - Nothing is ever refused. A signature that does not verify, content that
//     cannot be decrypted, a compression algorithm nobody implements: each is
//     captured, stored, shown, and reported honestly in the MDN's disposition.
//     RFC 4130 §7.4.4 agrees - "failed" is reserved for a failure to produce an
//     MDN at all, and a content problem is "processed" with an error modifier.
//   - Nothing is ever lost quietly. When a layer cannot be opened, the bytes of
//     that layer are kept and an Issue records why. A capture tool that discards
//     what it could not parse teaches its user nothing.
//   - The difference between "these bytes are intact" and "this is who they say
//     they are" is kept explicit everywhere, because with no configured partner
//     certificate only the first is ever provable and a tick that implies the
//     second would be a lie.
//
// Providers live in plugins/as2/providers/... and never import each other. All
// they share is the Receiver in receive.go and the Identity in identity.go.
package as2

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
)

// Name is the plugin name and the URL segment it is mounted under.
const Name = "as2"

// EventType is the event.Type every captured message carries. A provider that
// grows another resource later adds a type rather than overloading this one.
const EventType = "as2.message"

// Transport is the Raw.Transport an AS2 provider records. AS2 is defined over
// HTTP and nothing else.
const Transport = "http"

// Version is the AS2-Version tommy reports. 1.1 is the version that added
// compression (RFC 5402 §6 requires 1.1 or greater from anything that supports
// it), and tommy does.
const Version = "1.1"

// Message is the canonical model every AS2 provider produces and every read
// surface consumes.
//
// Everything in it except MDN came off the wire and is untrusted: AS2 names,
// subjects and filenames are attacker-controlled text and reach the page as
// plain strings through html/template, never as markup.
type Message struct {
	// The AS2 identifiers, kept exactly as sent. RFC 4130 §6.2 makes these
	// case-sensitive, 1 to 128 printable ASCII characters, and explicitly
	// forbids the receiver from restricting their values - so they are
	// recorded, never validated away.
	From string `json:"from"`
	To   string `json:"to"`
	// MessageID is the request's Message-ID verbatim, angle brackets included.
	// The MDN's Original-Message-ID must match it byte for byte, so this is
	// never trimmed or normalized.
	MessageID string `json:"message_id,omitempty"`
	Subject   string `json:"subject,omitempty"`
	// Date is the Date header as the sender wrote it, kept as a string because
	// its exact spelling is part of what was sent. ReceivedAt on the event is
	// the authoritative time.
	Date string `json:"date,omitempty"`
	// AS2Version is the sender's AS2-Version header, "1.0" or "1.1".
	AS2Version string `json:"as2_version,omitempty"`
	UserAgent  string `json:"user_agent,omitempty"`
	// ContentType is the outermost Content-Type, which is what says whether
	// this message was signed, encrypted or compressed before anything is
	// opened.
	ContentType string `json:"content_type,omitempty"`

	// Layers is the S/MIME onion, outermost first, as it was actually peeled.
	// It is the fastest way to see that a message was, say, compressed then
	// signed then encrypted, which no single header states.
	Layers []Layer `json:"layers,omitempty"`

	Security Security `json:"security"`

	// MIC is the Received-Content-MIC returned in the MDN, and Coverage on it
	// says what it was taken over. Nil when nothing could be digested.
	MIC *MIC `json:"mic,omitempty"`
	// AlternateMICs are the same content digested under a different reading of
	// the specification. RFC 4130 §7.3.1 and RFC 5402 §4.3 disagree about
	// whether an unsigned message's MIC includes its MIME headers, and a
	// partner using the other reading gets a different number; showing both is
	// how somebody chasing a MIC mismatch finds out which one they are looking
	// at, rather than being told one of them is correct.
	AlternateMICs []MIC `json:"alternate_mics,omitempty"`

	Payload Payload `json:"payload"`

	// Receipt is what the sender asked for by way of an MDN.
	Receipt ReceiptRequest `json:"receipt"`
	// MDN is what tommy actually sent back.
	MDN *MDNRecord `json:"mdn,omitempty"`

	// Issues is everything that went wrong and was recovered from, in the
	// order it was found. The MDN's disposition is derived from it.
	Issues []Issue `json:"issues,omitempty"`
}

// Layer is one wrapper peeled off an AS2 message.
type Layer struct {
	// Kind is LayerEncrypted, LayerSigned, LayerCompressed or LayerPayload.
	Kind string `json:"kind"`
	// ContentType is the layer's declared Content-Type.
	ContentType string `json:"content_type,omitempty"`
	// SMIMEType is the smime-type parameter when there was one.
	SMIMEType string `json:"smime_type,omitempty"`
	// Bytes is the size of the layer as it arrived, before it was opened.
	Bytes int `json:"bytes"`
	// Opened is false when tommy could not get inside this layer, in which
	// case it is the last one listed.
	Opened bool `json:"opened"`
}

// Layer kinds.
const (
	LayerEncrypted  = "encrypted"
	LayerSigned     = "signed"
	LayerCompressed = "compressed"
	LayerPayload    = "payload"
)

// Security is what protection the message actually carried, as opposed to what
// its headers claimed.
type Security struct {
	Signed      bool             `json:"signed"`
	Signature   *Signature       `json:"signature,omitempty"`
	Encrypted   bool             `json:"encrypted"`
	Encryption  *Encryption      `json:"encryption,omitempty"`
	Compressed  bool             `json:"compressed"`
	Compression *CompressionInfo `json:"compression,omitempty"`
}

// Summary is the short phrase the list and the badges use: "signed, encrypted"
// or "unprotected".
func (s Security) Summary() string {
	var parts []string
	if s.Encrypted {
		parts = append(parts, "encrypted")
	}
	if s.Signed {
		parts = append(parts, "signed")
	}
	if s.Compressed {
		parts = append(parts, "compressed")
	}
	if len(parts) == 0 {
		return "unprotected"
	}
	return strings.Join(parts, ", ")
}

// Payload is the business document that was inside all the wrapping.
//
// The bytes are in the blob store, never on the event: an EDI interchange can
// be tens of megabytes and the ring buffer would carry it around for the life
// of the event. Preview is a short, already-truncated excerpt for the list.
type Payload struct {
	ContentType      string `json:"content_type,omitempty"`
	Filename         string `json:"filename,omitempty"`
	TransferEncoding string `json:"transfer_encoding,omitempty"`
	// Format is what the bytes turned out to be: FormatX12, FormatEDIFACT,
	// FormatXML, FormatJSON, FormatText or FormatBinary. It is sniffed from
	// the content rather than trusted from the header, because senders label
	// EDI as application/octet-stream constantly.
	Format string `json:"format,omitempty"`
	Size   int64  `json:"size"`
	// Blob is where the decrypted, decompressed document lives.
	Blob *blob.Ref `json:"blob,omitempty"`
	// Preview is a short excerpt for the list, plain text, already truncated.
	Preview string `json:"preview,omitempty"`
	// Recovered says the payload is the innermost layer tommy could open
	// rather than the business document itself - the ciphertext of a message
	// it could not decrypt, say. The bytes are kept either way; this is what
	// stops the UI presenting them as an EDI interchange.
	Recovered bool `json:"recovered,omitempty"`
}

// Payload formats.
const (
	FormatX12     = "edi-x12"
	FormatEDIFACT = "edifact"
	FormatXML     = "xml"
	FormatJSON    = "json"
	FormatText    = "text"
	FormatBinary  = "binary"
)

// DetectFormat identifies a payload from its bytes, with the declared content
// type only as a tie-breaker.
//
// Sniffing first is deliberate. An EDI interchange arrives labeled
// application/octet-stream, application/edi-x12, application/EDIFACT and
// text/plain depending on whose software sent it, and the one thing that is
// reliable is that an X12 interchange starts "ISA" and an EDIFACT one starts
// "UNA" or "UNB".
func DetectFormat(contentType string, body []byte) string {
	trimmed := body
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\r' || trimmed[0] == '\n' || trimmed[0] == '\t') {
		trimmed = trimmed[1:]
	}
	switch {
	case len(trimmed) >= 3 && string(trimmed[:3]) == "ISA":
		return FormatX12
	case len(trimmed) >= 3 && (string(trimmed[:3]) == "UNA" || string(trimmed[:3]) == "UNB"):
		return FormatEDIFACT
	case len(trimmed) >= 5 && string(trimmed[:5]) == "<?xml":
		return FormatXML
	}

	mt, _ := parseMediaType(contentType)
	switch mt {
	case "application/edi-x12", "application/edi-x12; charset=utf-8":
		return FormatX12
	case "application/edifact":
		return FormatEDIFACT
	case "application/xml", "text/xml":
		return FormatXML
	case "application/json":
		return FormatJSON
	}

	if len(trimmed) > 0 {
		switch trimmed[0] {
		case '<':
			return FormatXML
		case '{', '[':
			// Only claim JSON if it really parses; an EDIFACT segment can
			// start with a brace in a mangled file.
			var v any
			if json.Unmarshal(trimmed, &v) == nil {
				return FormatJSON
			}
		}
	}
	if isMostlyText(body) {
		return FormatText
	}
	return FormatBinary
}

// isMostlyText decides whether the raw viewer should show text or hex. A NUL
// byte or a stretch of invalid UTF-8 settles it; EDI is full of unusual
// separators but they are all printable.
func isMostlyText(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	sample := b
	if len(sample) > 8192 {
		sample = sample[:8192]
	}
	if !utf8.Valid(sample) {
		return false
	}
	control := 0
	for _, c := range sample {
		if c == 0 {
			return false
		}
		if c < 0x20 && c != '\r' && c != '\n' && c != '\t' {
			control++
		}
	}
	return control*32 < len(sample)
}

// Text reports whether the payload should be rendered as text rather than hex.
func (p Payload) Text() bool { return p.Format != FormatBinary }

// ReceiptRequest is what the sender asked for by way of an MDN.
type ReceiptRequest struct {
	// Requested is true when Disposition-Notification-To was present. RFC 4130
	// §7.3 makes the header's *value* irrelevant for a synchronous MDN - the
	// receipt goes back on the same connection - so the address is recorded
	// and never used.
	Requested bool   `json:"requested"`
	NotifyTo  string `json:"notify_to,omitempty"`
	// Options is Disposition-Notification-Options verbatim.
	Options string `json:"options,omitempty"`
	// SignedRequested is true when the options asked for a signed receipt.
	SignedRequested bool `json:"signed_requested"`
	// SignedImportance is "required" or "optional" as the sender marked it.
	SignedImportance string `json:"signed_importance,omitempty"`
	// Protocol is the requested signature format, normally "pkcs7-signature".
	Protocol string `json:"protocol,omitempty"`
	// MICAlgs are the digest algorithms the sender will accept, in the order
	// it listed them.
	MICAlgs []string `json:"mic_algs,omitempty"`
	// AsyncURL is Receipt-Delivery-Option, which asks for the MDN to be
	// delivered later to a URL of the sender's choosing.
	//
	// tommy does not do that, and says so rather than ignoring it. Delivering
	// an asynchronous MDN means making an outbound HTTP request to a partner,
	// and tommy's charter is to answer what is sent to it, never to originate
	// traffic. A synchronous MDN is returned instead and an Issue records that
	// the request was seen and not honored.
	AsyncURL string `json:"async_url,omitempty"`
}

// Async reports whether the sender asked for an asynchronous MDN.
func (r ReceiptRequest) Async() bool { return r.AsyncURL != "" }

// MDNRecord is the receipt tommy returned, kept on the message so the tab can
// show both halves of the exchange and a test can assert on the reply without
// re-deriving it.
type MDNRecord struct {
	MessageID string `json:"message_id"`
	Signed    bool   `json:"signed"`
	// MICAlg is the micalg parameter on a signed MDN's Content-Type.
	MICAlg string `json:"micalg,omitempty"`
	// Disposition is the whole disposition-field value, which is the one line
	// that says what tommy concluded.
	Disposition string `json:"disposition"`
	// ReceivedContentMIC is the field as it went on the wire, "<digest>, <alg>".
	ReceivedContentMIC string `json:"received_content_mic,omitempty"`
	// HumanText is the text/plain part, which is what a person reads first.
	HumanText string `json:"human_text,omitempty"`
	// Status and Headers are the HTTP response tommy wrote.
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	// Blob holds the MDN body bytes, so the exact receipt can be fetched back
	// and diffed against what the partner's software says it received.
	Blob *blob.Ref `json:"blob,omitempty"`
	Size int64     `json:"size"`
}

// Issue is something that went wrong and was recovered from.
//
// Like the HL7 parser's issues, these exist because the consumer is an
// acknowledgement that needs both the message and the reason: an MDN's
// disposition-field is chosen by switching on Code, and the UI shows Detail.
// Returning an error instead would leave a captured message unshown, which is
// the one outcome that helps nobody.
type Issue struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
	// Severity is "error", "warning" or "info". Only "error" changes the MDN's
	// disposition-type modifier; a warning is reported as processed/warning
	// per RFC 4130 §7.5.5, and info is for things worth showing that the
	// partner does not need told.
	Severity string `json:"severity"`
}

// Issue codes. They are stable and machine-readable: a provider or a test
// switches on these rather than on the prose, and each maps to exactly one
// RFC 4130 disposition modifier.
const (
	// IssueDecryptionFailed: the enveloped-data layer would not open with
	// tommy's key. RFC 4130 §7.5.4, "Error: decryption-failed".
	IssueDecryptionFailed = "decryption-failed"
	// IssueIntegrityCheckFailed: a signature was present and did not verify
	// over the content. RFC 4130 §7.5.4, "Error: integrity-check-failed" -
	// which is the receiver being unable to verify content integrity, as
	// opposed to authentication-failed, which is being unable to establish who
	// the sender is. A bad signature is the former.
	IssueIntegrityCheckFailed = "integrity-check-failed"
	// IssueAuthenticationFailed: the signature verified but the signer is not
	// the configured partner. RFC 4130 §7.5.4 calls that authentication-failed;
	// §7.5.5 says a receiver that carries on anyway reports it as a warning,
	// which is what tommy does, because refusing is a policy decision and
	// tommy makes none.
	IssueAuthenticationFailed = "authentication-failed"
	// IssueDecompressionFailed: a compressed-data layer would not inflate.
	// RFC 5402 §5, "Error: decompression-failed".
	IssueDecompressionFailed = "decompression-failed"
	// IssueUnsupportedCompression: the compression algorithm is not zlib, the
	// only one RFC 3274 defines. Reported as decompression-failed to the
	// partner but kept separate here so the UI can say which it was.
	IssueUnsupportedCompression = "unsupported-compression"
	// IssueMalformedMIME: the MIME structure could not be walked - a multipart
	// whose boundary appears nowhere, an entity with no header separator.
	IssueMalformedMIME = "malformed-mime"
	// IssueTruncatedMultipart: the closing --boundary-- never arrived. The
	// parts found are still used.
	IssueTruncatedMultipart = "truncated-multipart"
	// IssueMissingMessageID: no Message-ID header, so the MDN's
	// Original-Message-ID cannot correlate with anything.
	IssueMissingMessageID = "missing-message-id"
	// IssueMissingIdentifier: AS2-From or AS2-To was absent, which RFC 4130
	// §6.2 requires. Recorded, never refused.
	IssueMissingIdentifier = "missing-as2-identifier"
	// IssueAsyncReceiptRequested: Receipt-Delivery-Option asked for an
	// asynchronous MDN. tommy answers synchronously instead.
	IssueAsyncReceiptRequested = "async-receipt-not-delivered"
	// IssueUnsupportedMICAlgorithm: the sender asked for a MIC algorithm tommy
	// cannot compute. RFC 4130 §7.5.3 makes this one of the two predefined
	// "failed" cases: "Failure: unsupported MIC-algorithms".
	IssueUnsupportedMICAlgorithm = "unsupported-mic-algorithm"
	// IssueNoIdentity: no certificate and key are configured, so nothing could
	// be decrypted and no MDN could be signed.
	IssueNoIdentity = "no-identity"
	// IssueTransferEncoding: the Content-Transfer-Encoding would not decode.
	IssueTransferEncoding = "transfer-encoding"
	// IssueEmptyBody: the request carried no body at all.
	IssueEmptyBody = "empty-body"
)

// Issue severities.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
	SeverityInfo    = "info"
)

// AddIssue records a problem, keeping the first detail for a repeated code so
// the earliest and most specific explanation survives.
func (m *Message) AddIssue(code, severity, detail string) {
	for _, i := range m.Issues {
		if i.Code == code {
			return
		}
	}
	m.Issues = append(m.Issues, Issue{Code: code, Severity: severity, Detail: detail})
}

// HasIssue reports whether an issue with the given code was recorded.
func (m *Message) HasIssue(code string) bool {
	for _, i := range m.Issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

// FirstError returns the first error-severity issue, which is the one the MDN
// reports. Issues are appended outermost-layer first, so the first error is the
// outermost thing that went wrong - a message that could not be decrypted has
// nothing useful to say about its signature.
func (m *Message) FirstError() (Issue, bool) {
	for _, i := range m.Issues {
		if i.Severity == SeverityError {
			return i, true
		}
	}
	return Issue{}, false
}

// Title is the one-line name a message goes by in every list.
func (m *Message) Title() string {
	route := m.Route()
	switch {
	case m.Subject != "" && route != "":
		return m.Subject + " (" + route + ")"
	case m.Subject != "":
		return m.Subject
	case route != "":
		return route
	case m.MessageID != "":
		return m.MessageID
	default:
		return "AS2 message"
	}
}

// Route is "sender → receiver" from the AS2 identifiers.
func (m *Message) Route() string {
	switch {
	case m.From != "" && m.To != "":
		return m.From + " → " + m.To
	case m.From != "":
		return m.From + " → (no AS2-To)"
	case m.To != "":
		return "(no AS2-From) → " + m.To
	default:
		return ""
	}
}

// Preview is the short excerpt the list and the store's search index see.
func (m *Message) Preview() string { return m.Payload.Preview }

// PreviewLimit caps the excerpt kept on the model. It is small on purpose: the
// full document is a blob fetch away, and the ring buffer holds hundreds of
// these.
const PreviewLimit = 240

// MakePreview builds the excerpt: printable, single-line, truncated.
func MakePreview(body []byte, format string) string {
	if format == FormatBinary {
		return ""
	}
	s := string(body)
	if len(s) > PreviewLimit*4 {
		s = s[:PreviewLimit*4]
	}
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\r' || r == '\n' || r == '\t':
			return ' '
		case r < 0x20:
			return -1
		case r == utf8.RuneError:
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > PreviewLimit {
		// Trim on a rune boundary so the excerpt is never invalid UTF-8.
		s = strings.ToValidUTF8(s[:PreviewLimit], "") + "…"
	}
	return s
}

// Captured pairs an event with the message decoded from it.
type Captured struct {
	Event   *event.Event
	Message *Message
}

// MessageOf extracts the canonical message from an event.
//
// It accepts the in-process payload a provider appended (*Message), a value
// copy, and a payload that has been through JSON, so a store that round-trips
// events later does not break every read surface.
func MessageOf(e *event.Event) (*Message, bool) {
	if e == nil || e.Payload == nil {
		return nil, false
	}
	switch p := e.Payload.(type) {
	case *Message:
		if p == nil {
			return nil, false
		}
		clone := *p
		return &clone, true
	case Message:
		clone := p
		return &clone, true
	default:
		encoded, err := json.Marshal(e.Payload)
		if err != nil {
			return nil, false
		}
		var decoded Message
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return nil, false
		}
		if decoded.From == "" && decoded.To == "" && decoded.MessageID == "" && decoded.Payload.Size == 0 {
			return nil, false
		}
		return &decoded, true
	}
}

// Messages extracts every decodable message from a list of events, keeping the
// event alongside it. Events of another type are skipped rather than guessed
// at.
func Messages(events []*event.Event) []Captured {
	out := make([]Captured, 0, len(events))
	for _, e := range events {
		if e.Type != EventType {
			continue
		}
		m, ok := MessageOf(e)
		if !ok {
			continue
		}
		out = append(out, Captured{Event: e, Message: m})
	}
	return out
}

// ReceivedWindow is how far back the tab and the API look by default when no
// filter narrows the query. It is a duration rather than a count so a busy
// instance still shows a useful slice.
const ReceivedWindow = 7 * 24 * time.Hour
