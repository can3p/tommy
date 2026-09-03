package as2

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/textproto"
	"strings"
)

// This file exists because mime/multipart cannot be used for the part of AS2
// that matters most.
//
// The MIC an AS2 receiver returns is a hash over the exact bytes of a MIME
// entity - its own headers included - and the sender computed the same hash
// over the same bytes before signing them. mime/multipart hands back a parsed
// textproto.MIMEHeader and a body reader; reconstructing an entity from those
// re-orders headers, re-folds continuations, normalizes whitespace after the
// colon and loses the sender's line endings. Every one of those changes the
// digest, and a MIC that no partner agrees with is worse than no MIC at all,
// because it looks like it worked.
//
// So everything here works on offsets into the original byte slice and only
// ever hands out subslices of it. The parsed headers are for reading; RawHeader
// and Body are for hashing.
//
// The second reason is line endings. OpenSSL's `cms -sign -outform SMIME`
// writes its outer headers and its multipart boundary delimiters with a bare
// LF while the part headers and the part body keep CRLF - verified against
// openssl 3.6.1, and it is what a partner driving the OpenSSL command line
// actually puts on the wire. RFC 2046 specifies CRLF, so a strict splitter
// finds no parts at all in a message OpenSSL just produced. Everything below
// accepts either.

// Entity is one MIME entity kept byte for byte.
//
// Header is the parsed view, for reading values. RawHeader and Body are the
// bytes as they arrived and are what any digest must be taken over. Raw is the
// two of them plus the blank line between, which is the entity as a whole and
// is what the MIC covers for a signed or decrypted message.
type Entity struct {
	// RawHeader is the header block without the blank line that ends it. It is
	// empty for an entity built from HTTP headers, where the header bytes
	// belong to the HTTP request rather than to the entity.
	RawHeader []byte
	// Header is the parsed header block. Lookups are canonicalized the usual
	// textproto way, so Get("content-type") works.
	Header textproto.MIMEHeader
	// Order is the header field names in the order they arrived, so a view can
	// show the entity the way the sender wrote it.
	Order []string
	// Body is the entity body exactly as received, with its
	// Content-Transfer-Encoding still applied.
	Body []byte
	// Raw is RawHeader, the blank line, and Body: the whole entity.
	Raw []byte
}

// ErrNoBody is returned by ParseEntity for bytes that carry no blank line and
// so cannot be split into a header block and a body. It is recoverable - the
// caller keeps the bytes and records an issue - which is why it is a sentinel
// rather than a formatted message.
var ErrNoBody = errors.New("as2: MIME entity has no header/body separator")

// ParseEntity splits raw into a header block and a body without rewriting
// either. It is used for nested entities, whose bytes are the unit a digest is
// taken over; the outermost entity of an AS2 message comes from the HTTP
// request instead and is built with NewEntity.
func ParseEntity(raw []byte) (*Entity, error) {
	sep, sepLen := findSeparator(raw)
	if sep < 0 {
		// No blank line. Everything is header or everything is body and we
		// cannot tell, so hand it back as a body: the bytes survive, which is
		// the point, and the caller records the problem.
		return &Entity{Header: textproto.MIMEHeader{}, Body: raw, Raw: raw}, ErrNoBody
	}
	e := &Entity{
		RawHeader: raw[:sep],
		Body:      raw[sep+sepLen:],
		Raw:       raw,
	}
	e.Header, e.Order = parseHeaderBlock(e.RawHeader)
	return e, nil
}

// NewEntity builds an entity from headers that arrived out of band - the HTTP
// request's own headers - and a body. RawHeader is left empty because those
// bytes are the HTTP request's, not the entity's, and hashing them would be
// wrong: RFC 4130 §7.3.1 is explicit that the MIC of an unsigned, unencrypted
// message covers the content without the RFC 2822 headers.
func NewEntity(h textproto.MIMEHeader, order []string, body []byte) *Entity {
	if h == nil {
		h = textproto.MIMEHeader{}
	}
	return &Entity{Header: h, Order: order, Body: body, Raw: body}
}

// findSeparator returns the offset of the blank line ending the header block
// and the length of that separator, or -1. CRLFCRLF and LFLF are both accepted
// because real senders emit both, sometimes in the same message.
func findSeparator(raw []byte) (int, int) {
	crlf := bytes.Index(raw, []byte("\r\n\r\n"))
	lf := bytes.Index(raw, []byte("\n\n"))
	switch {
	case crlf < 0 && lf < 0:
		return -1, 0
	case crlf < 0:
		return lf, 2
	case lf < 0:
		return crlf, 4
	case crlf <= lf:
		return crlf, 4
	default:
		// A bare LFLF came first. It really is the end of the headers even
		// though a CRLFCRLF exists further on inside the body.
		return lf, 2
	}
}

// parseHeaderBlock reads a header block, unfolding continuation lines. It never
// fails: a line that is not a header is skipped rather than aborting the parse,
// because a capture tool that refuses to show a slightly malformed message has
// failed at its only job.
func parseHeaderBlock(block []byte) (textproto.MIMEHeader, []string) {
	h := textproto.MIMEHeader{}
	var order []string
	for _, line := range unfold(block) {
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		name := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(line[:colon]))
		value := strings.TrimLeft(line[colon+1:], " \t")
		h[name] = append(h[name], value)
		order = append(order, name)
	}
	return h, order
}

// unfold splits a header block into logical lines, joining RFC 5322
// continuation lines (a following line starting with space or tab) onto the
// line they continue.
func unfold(block []byte) []string {
	raw := strings.Split(strings.ReplaceAll(string(block), "\r\n", "\n"), "\n")
	var out []string
	for _, line := range raw {
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(out) > 0 {
			out[len(out)-1] += " " + strings.TrimLeft(line, " \t")
			continue
		}
		out = append(out, line)
	}
	return out
}

// Get returns a header value, or "".
func (e *Entity) Get(name string) string {
	if e == nil || e.Header == nil {
		return ""
	}
	return e.Header.Get(name)
}

// MediaType is the parsed Content-Type: the media type lowercased and its
// parameters. A Content-Type that will not parse yields whatever prefix is
// usable, because knowing "it said multipart/signed but the boundary is
// unquoted rubbish" is more useful than knowing nothing.
func (e *Entity) MediaType() (string, map[string]string) {
	return parseMediaType(e.Get("Content-Type"))
}

func parseMediaType(v string) (string, map[string]string) {
	if v == "" {
		return "", map[string]string{}
	}
	mt, params, err := mime.ParseMediaType(v)
	if err == nil {
		return strings.ToLower(mt), params
	}
	// Salvage the media type and any parameter that does parse. Partner
	// software has been seen sending unquoted boundaries containing "=".
	params = map[string]string{}
	fields := strings.Split(v, ";")
	mt = strings.ToLower(strings.TrimSpace(fields[0]))
	for _, f := range fields[1:] {
		k, val, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		params[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(val), `"`)
	}
	return mt, params
}

// SMIMEType is the smime-type Content-Type parameter, which is how an
// application/pkcs7-mime entity says whether it holds enveloped, signed or
// compressed data.
func (e *Entity) SMIMEType() string {
	_, params := e.MediaType()
	return strings.ToLower(params["smime-type"])
}

// TransferEncoding is the declared Content-Transfer-Encoding, lowercased,
// defaulting to the MIME default of 7bit.
func (e *Entity) TransferEncoding() string {
	v := strings.ToLower(strings.TrimSpace(e.Get("Content-Transfer-Encoding")))
	if v == "" {
		return "7bit"
	}
	return v
}

// Filename is the name the sender gave the content, from Content-Disposition
// first and the Content-Type name parameter second. It is attacker-controlled
// text and is never used to open anything - only shown.
func (e *Entity) Filename() string {
	if _, params := parseMediaType(e.Get("Content-Disposition")); params["filename"] != "" {
		return params["filename"]
	}
	_, params := e.MediaType()
	return params["name"]
}

// Decoded applies the declared Content-Transfer-Encoding. base64 and
// quoted-printable are decoded; 7bit, 8bit and binary are the identity. An
// unrecognized encoding returns the bytes untouched together with an error, so
// the caller can record the problem and still keep the content.
func (e *Entity) Decoded() ([]byte, error) {
	switch enc := e.TransferEncoding(); enc {
	case "base64":
		// Real senders wrap base64 at 64 or 76 columns with CRLF, and some
		// pad sloppily, so strip whitespace and be lenient about padding.
		clean := stripSpace(e.Body)
		// Sized for the unpadded decoding, which is the larger of the two:
		// StdEncoding.DecodedLen rounds down to whole 4-byte groups, and
		// handing Decode a buffer that short panics rather than erroring.
		out := make([]byte, base64.RawStdEncoding.DecodedLen(len(clean)))
		n, err := base64.StdEncoding.Decode(out, clean)
		if err != nil {
			// Senders that drop the "=" padding are common enough to be worth
			// a second attempt rather than a lost payload.
			n, err = base64.RawStdEncoding.Decode(out, bytes.TrimRight(clean, "="))
			if err != nil {
				return e.Body, fmt.Errorf("as2: decode base64 body: %w", err)
			}
		}
		return out[:n], nil
	case "quoted-printable":
		out, err := readAllLimit(quotedprintable.NewReader(bytes.NewReader(e.Body)))
		if err != nil {
			return e.Body, fmt.Errorf("as2: decode quoted-printable body: %w", err)
		}
		return out, nil
	case "7bit", "8bit", "binary", "":
		return e.Body, nil
	default:
		return e.Body, fmt.Errorf("as2: unsupported Content-Transfer-Encoding %q", enc)
	}
}

func stripSpace(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
		default:
			out = append(out, c)
		}
	}
	return out
}

// Boundary is the multipart boundary this entity declared, or "".
func (e *Entity) Boundary() string {
	_, params := e.MediaType()
	return params["boundary"]
}

// Parts splits a multipart body into its parts, each kept byte for byte.
//
// The subtlety that decides whether the MIC is right: RFC 2046 makes the line
// break *before* a boundary delimiter part of the delimiter, not part of the
// preceding body. Include it and every digest is one or two bytes too long.
// OpenSSL writes that line break as a bare LF, so both forms are accepted.
func (e *Entity) Parts() ([]*Entity, error) {
	boundary := e.Boundary()
	if boundary == "" {
		return nil, errors.New("as2: multipart entity declares no boundary")
	}
	// A truncated multipart still yields the parts that did arrive, so the
	// error is carried past the loop rather than returned in place of them.
	chunks, splitErr := splitMultipart(e.Body, boundary)
	if splitErr != nil && !errors.Is(splitErr, errTruncatedMultipart) {
		return nil, splitErr
	}
	parts := make([]*Entity, 0, len(chunks))
	for _, c := range chunks {
		p, err := ParseEntity(c)
		if err != nil && !errors.Is(err, ErrNoBody) {
			return nil, err
		}
		parts = append(parts, p)
	}
	return parts, splitErr
}

// splitMultipart returns the exact bytes of each part between the boundary
// delimiters.
func splitMultipart(body []byte, boundary string) ([][]byte, error) {
	marker := []byte("--" + boundary)
	type delim struct {
		breakStart, contentStart int
		closing                  bool
	}

	var delims []delim
	for off := 0; off < len(body); {
		i := bytes.Index(body[off:], marker)
		if i < 0 {
			break
		}
		i += off
		off = i + len(marker)
		// A delimiter only counts at the start of a line.
		if i != 0 && body[i-1] != '\n' {
			continue
		}
		breakStart := i
		switch {
		case i >= 2 && body[i-2] == '\r' && body[i-1] == '\n':
			breakStart = i - 2
		case i >= 1 && body[i-1] == '\n':
			breakStart = i - 1
		}
		j := i + len(marker)
		closing := false
		if j+1 < len(body) && body[j] == '-' && body[j+1] == '-' {
			closing = true
			j += 2
		}
		// Transport padding: linear whitespace up to the line terminator.
		for j < len(body) && (body[j] == ' ' || body[j] == '\t') {
			j++
		}
		switch {
		case j+1 < len(body) && body[j] == '\r' && body[j+1] == '\n':
			j += 2
		case j < len(body) && body[j] == '\n':
			j++
		case j >= len(body):
			// The message ended on the delimiter line, with no terminator.
		default:
			// Something other than a line break follows: this was a run of
			// dashes inside the content, not a delimiter.
			continue
		}
		delims = append(delims, delim{breakStart: breakStart, contentStart: j, closing: closing})
		off = j
	}

	if len(delims) == 0 {
		return nil, fmt.Errorf("as2: no part delimiter for boundary %q", boundary)
	}
	var parts [][]byte
	for k := 0; k < len(delims); k++ {
		if delims[k].closing {
			break
		}
		end := len(body)
		if k+1 < len(delims) {
			end = delims[k+1].breakStart
		}
		if delims[k].contentStart > end {
			continue
		}
		parts = append(parts, body[delims[k].contentStart:end])
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("as2: boundary %q opens no part", boundary)
	}
	if !delims[len(delims)-1].closing {
		return parts, errTruncatedMultipart
	}
	return parts, nil
}

// errTruncatedMultipart says the closing "--boundary--" never arrived. The
// parts found before that point are still returned and still usable, so this
// is reported as an issue on the message rather than as a refusal.
var errTruncatedMultipart = errors.New("as2: multipart body has no closing delimiter")

// maxDecoded caps a single transfer-decoding, so a quoted-printable bomb in a
// captured message cannot exhaust the process. 64 MiB is far past any real EDI
// document and far short of trouble.
const maxDecoded = 64 << 20

func readAllLimit(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(&limitedReader{r: r, n: maxDecoded})
	return buf.Bytes(), err
}

type limitedReader struct {
	r interface{ Read([]byte) (int, error) }
	n int
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, fmt.Errorf("as2: decoded body exceeds %d bytes", maxDecoded)
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= n
	return n, err
}

// filenameSuffix is the lowercased extension of whatever name the sender gave
// the content, including the dot, or "". Used only to recognize the smime.p7z
// and smime.p7m conventions when a sender omits the smime-type parameter -
// never to open or write anything.
func filenameSuffix(e *Entity) string {
	name := strings.ToLower(e.Filename())
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i:]
	}
	return ""
}
