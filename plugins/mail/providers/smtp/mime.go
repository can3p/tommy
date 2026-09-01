package smtp

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"net/textproto"
	"strings"
	"unicode/utf8"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/plugins/mail"
)

// Limits on what one message may cost us. A message arrives from an untrusted
// client, so every loop in here is bounded: an attacker cannot make us recurse
// forever, allocate a part per byte, or expand a small body into a large one.
const (
	// maxDepth bounds nested multiparts. Real mail nests three deep at most
	// (mixed > related > alternative); ten is generous.
	maxDepth = 10
	// maxParts bounds how many parts one message may contain.
	maxParts = 200
	// budgetFactor bounds the total decoded bytes across every part, relative
	// to the size of the message that arrived.
	budgetFactor = 8
	// maxWarnings bounds what a deliberately broken message can add to Meta.
	maxWarnings = 32
	// maxEncodedWord bounds one RFC 2047 encoded word's decoded size.
	maxEncodedWord = 1 << 20
)

// headerField is one header exactly as the sender wrote it: the name with its
// original casing, and the unfolded value still in wire form.
type headerField struct {
	Name  string
	Value string
}

// ParseMessage turns the bytes of one received message into the canonical
// model, storing every attachment in the blob store.
//
// It never fails on malformed input: anything it cannot make sense of is
// reported as a warning and the best available reading is returned, because a
// tool for looking at broken mail is useless if broken mail makes it give up.
// An error means the blob store itself failed, which the caller turns into a
// temporary SMTP failure.
func ParseMessage(ctx context.Context, raw []byte, blobs blob.BlobStore) (*mail.Message, []string, error) {
	p := newParser(len(raw))
	fields, body := splitMessage(raw)
	if len(fields) == 0 && len(body) == 0 && len(raw) > 0 {
		// No header/body separator at all: treat the whole thing as a body.
		body = raw
		p.warn("no header/body separator: the whole message was read as a body")
	}

	msg := &mail.Message{}
	header := toMIMEHeader(fields)
	for _, f := range fields {
		msg.Headers.Add(f.Name, p.decodeHeader(f.Value))
	}

	msg.Subject = p.decodeHeader(header.Get("Subject"))
	if from := p.addresses(header.Get("From")); len(from) > 0 {
		msg.From = from[0]
	} else if sender := p.addresses(header.Get("Sender")); len(sender) > 0 {
		msg.From = sender[0]
	}
	msg.To = p.addresses(header.Get("To"))
	msg.Cc = p.addresses(header.Get("Cc"))
	msg.Bcc = p.addresses(header.Get("Bcc"))
	msg.ReplyTo = p.addresses(header.Get("Reply-To"))

	root := p.parsePart(header, body, 0)
	b := &builder{ctx: ctx, blobs: blobs, msg: msg, p: p}
	b.walk(root, false)
	if b.err != nil {
		return nil, p.warns, b.err
	}
	return msg, p.warns, nil
}

// parser holds the per-message decoding state: the warnings collected so far
// and the budgets that bound a hostile message.
type parser struct {
	warns  []string
	dec    *mime.WordDecoder
	ap     *netmail.AddressParser
	budget int64
	parts  int
}

func newParser(size int) *parser {
	p := &parser{budget: int64(size)*budgetFactor + (1 << 16)}
	p.dec = &mime.WordDecoder{CharsetReader: func(charset string, r io.Reader) (io.Reader, error) {
		// Never returns an error: DecodeHeader gives up on the whole header
		// when a charset conversion fails, and losing a subject because one
		// word used a charset x/text does not know is worse than a rough
		// transliteration.
		encoded, err := io.ReadAll(io.LimitReader(r, maxEncodedWord))
		if err != nil {
			return nil, err
		}
		return strings.NewReader(p.text(charset, encoded)), nil
	}}
	p.ap = &netmail.AddressParser{WordDecoder: p.dec}
	return p
}

// warn records a parse problem, deduplicated and bounded.
func (p *parser) warn(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, existing := range p.warns {
		if existing == msg {
			return
		}
	}
	if len(p.warns) >= maxWarnings {
		return
	}
	p.warns = append(p.warns, msg)
}

// spend draws n bytes from the decoding budget, reporting whether there was
// room. It is what stops a small message from expanding into a large one.
func (p *parser) spend(n int) bool {
	if int64(n) > p.budget {
		p.budget = 0
		return false
	}
	p.budget -= int64(n)
	return true
}

// splitMessage separates the header block from the body and unfolds every
// header, keeping the name's original casing.
//
// net/mail would do this too, but it rejects a malformed header block outright
// and canonicalizes names, and both are things this provider exists to show.
func splitMessage(raw []byte) ([]headerField, []byte) {
	head, body := splitHeaderBlock(raw)

	var fields []headerField
	for _, line := range strings.Split(string(head), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			if len(fields) == 0 {
				continue // continuation of nothing; drop it
			}
			// Unfolding replaces the fold with a single space, which is what
			// keeps split encoded-words and split parameters readable.
			fields[len(fields)-1].Value += " " + strings.TrimLeft(line, " \t")
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "" {
			continue // not a header at all
		}
		fields = append(fields, headerField{
			Name:  strings.TrimSpace(name),
			Value: strings.TrimSpace(value),
		})
	}
	return fields, body
}

// splitHeaderBlock finds the blank line that ends the headers, tolerating both
// CRLF and bare LF because a hand-written test client emits either.
func splitHeaderBlock(raw []byte) (head, body []byte) {
	if bytes.HasPrefix(raw, []byte("\r\n")) {
		return nil, raw[2:]
	}
	if bytes.HasPrefix(raw, []byte("\n")) {
		return nil, raw[1:]
	}
	crlf := bytes.Index(raw, []byte("\r\n\r\n"))
	lf := bytes.Index(raw, []byte("\n\n"))
	switch {
	case crlf >= 0 && (lf < 0 || crlf <= lf):
		return raw[:crlf+2], raw[crlf+4:]
	case lf >= 0:
		return raw[:lf+1], raw[lf+2:]
	default:
		return raw, nil
	}
}

// toMIMEHeader builds the canonicalized view used for structural lookups.
func toMIMEHeader(fields []headerField) textproto.MIMEHeader {
	h := textproto.MIMEHeader{}
	for _, f := range fields {
		h.Add(f.Name, f.Value)
	}
	return h
}

// part is one node of the MIME tree, with its content already decoded out of
// its transfer encoding.
type part struct {
	header    textproto.MIMEHeader
	mediaType string
	params    map[string]string
	content   []byte
	children  []*part
	multipart bool
}

// parsePart decodes one part's transfer encoding and, when it is a multipart,
// recursively parses its children.
func (p *parser) parsePart(header textproto.MIMEHeader, body []byte, depth int) *part {
	pt := &part{header: header}
	pt.mediaType, pt.params = p.mediaType(header.Get("Content-Type"), "text/plain")
	pt.content = p.decodeTransfer(header.Get("Content-Transfer-Encoding"), body)

	boundary := pt.params["boundary"]
	if !strings.HasPrefix(pt.mediaType, "multipart/") {
		return pt
	}
	if boundary == "" {
		p.warn("%s without a boundary parameter: it was kept as one part", pt.mediaType)
		return pt
	}
	pt.multipart = true
	if depth >= maxDepth {
		p.warn("multipart nested deeper than %d levels: the rest was left as one part", maxDepth)
		return pt
	}

	mr := multipart.NewReader(bytes.NewReader(pt.content), boundary)
	for {
		// NextRawPart, not NextPart: NextPart silently undoes a
		// quoted-printable transfer encoding and deletes the header that said
		// so, and this provider decodes every encoding in one place.
		child, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			p.warn("multipart %q: %v", pt.mediaType, err)
			break
		}
		p.parts++
		if p.parts > maxParts {
			p.warn("more than %d MIME parts: the rest were skipped", maxParts)
			break
		}
		// A truncated message still yields whatever of the part did arrive,
		// which is the half someone is usually trying to look at.
		childBody, err := io.ReadAll(child)
		if err != nil {
			p.warn("multipart %q: reading a part: %v", pt.mediaType, err)
		}
		if !p.spend(len(childBody)) {
			p.warn("message expands to more than %d times its size: the rest was skipped", budgetFactor)
			break
		}
		if len(childBody) > 0 || err == nil {
			pt.children = append(pt.children, p.parsePart(child.Header, childBody, depth+1))
		}
		if err != nil {
			break
		}
	}

	if len(pt.children) == 0 {
		// A boundary that never appears leaves a container with nothing in it,
		// and dropping the bytes would hide the whole message. Read them as the
		// body instead, which is what they look like.
		p.warn("%s declared boundary %q but contained no parts: it was read as a plain body", pt.mediaType, boundary)
		pt.multipart = false
		pt.mediaType = "text/plain"
	}
	return pt
}

// mediaType parses a Content-Type or Content-Disposition value, salvaging what
// it can from a malformed one rather than discarding the part.
func (p *parser) mediaType(value, def string) (string, map[string]string) {
	if strings.TrimSpace(value) == "" {
		return def, map[string]string{}
	}
	mt, params, err := mime.ParseMediaType(value)
	if err == nil {
		return strings.ToLower(mt), params
	}
	p.warn("malformed %q: %v", firstToken(value), err)

	// Salvage: the type is the part before the first ';', and the parameters
	// are whatever looks like key=value after it.
	mt = strings.ToLower(strings.TrimSpace(firstToken(value)))
	if mt == "" || !strings.Contains(mt, "/") {
		mt = def
	}
	params = map[string]string{}
	if _, rest, ok := strings.Cut(value, ";"); ok {
		for _, kv := range strings.Split(rest, ";") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			k = strings.ToLower(strings.TrimSpace(k))
			v = strings.TrimSpace(v)
			v = strings.TrimSuffix(strings.TrimPrefix(v, `"`), `"`)
			if k != "" && v != "" {
				params[k] = v
			}
		}
	}
	return mt, params
}

func firstToken(value string) string {
	if i := strings.IndexByte(value, ';'); i >= 0 {
		return value[:i]
	}
	return value
}

// decodeTransfer undoes a Content-Transfer-Encoding. A body that will not
// decode cleanly yields as much as could be recovered plus a warning: half a
// visible message beats none.
func (p *parser) decodeTransfer(encoding string, body []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "7bit", "8bit", "binary":
		return body
	case "base64":
		return p.decodeBase64(body)
	case "quoted-printable":
		out, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
		if err != nil {
			p.warn("quoted-printable body: %v", err)
		}
		return out
	default:
		p.warn("unknown Content-Transfer-Encoding %q: the part was kept as it arrived", strings.TrimSpace(encoding))
		return body
	}
}

// decodeBase64 tolerates the three things real senders get wrong: embedded
// whitespace, missing padding, and trailing junk.
func (p *parser) decodeBase64(body []byte) []byte {
	clean := make([]byte, 0, len(body))
	for _, c := range body {
		switch c {
		case '\r', '\n', ' ', '\t':
		default:
			clean = append(clean, c)
		}
	}
	if out, err := base64.StdEncoding.DecodeString(string(clean)); err == nil {
		return out
	}
	if out, err := base64.RawStdEncoding.DecodeString(string(bytes.TrimRight(clean, "="))); err == nil {
		p.warn("base64 body was missing its padding")
		return out
	}
	// Salvage whatever prefix does decode.
	out, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, bytes.NewReader(clean)))
	p.warn("base64 body could not be fully decoded: %v", err)
	return out
}

// decodeHeader resolves RFC 2047 encoded words, including several in a row and
// several charsets in one header.
func (p *parser) decodeHeader(value string) string {
	if value == "" {
		return ""
	}
	decoded, err := p.dec.DecodeHeader(value)
	if err != nil {
		p.warn("header %q: %v", truncate(value, 60), err)
		return value
	}
	return decoded
}

// addresses parses an address list, salvaging the addresses it can when the
// list as a whole does not parse - which is common in the mail this provider
// exists to look at.
func (p *parser) addresses(value string) []mail.Address {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if list, err := p.ap.ParseList(value); err == nil {
		out := make([]mail.Address, 0, len(list))
		for _, a := range list {
			out = append(out, mail.Address{Name: a.Name, Email: a.Address})
		}
		return out
	}

	var out []mail.Address
	for _, token := range splitAddressList(value) {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if a, err := p.ap.Parse(token); err == nil {
			out = append(out, mail.Address{Name: a.Name, Email: a.Address})
			continue
		}
		p.warn("address %q did not parse: it was kept verbatim", truncate(token, 60))
		out = append(out, salvageAddress(p.decodeHeader(token)))
	}
	return out
}

// splitAddressList splits on the commas that separate addresses, ignoring the
// ones inside quoted display names or angle brackets.
func splitAddressList(value string) []string {
	var (
		out    []string
		cur    strings.Builder
		quoted bool
		angle  bool
	)
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c == '\\' && quoted && i+1 < len(value):
			cur.WriteByte(c)
			i++
			cur.WriteByte(value[i])
			continue
		case c == '"':
			quoted = !quoted
		case c == '<' && !quoted:
			angle = true
		case c == '>' && !quoted:
			angle = false
		case c == ',' && !quoted && !angle:
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}

// salvageAddress makes something displayable out of a token net/mail rejected.
func salvageAddress(token string) mail.Address {
	token = strings.TrimSpace(token)
	if open := strings.IndexByte(token, '<'); open >= 0 {
		if end := strings.IndexByte(token[open:], '>'); end > 0 {
			email := strings.TrimSpace(token[open+1 : open+end])
			name := strings.TrimSpace(token[:open])
			name = strings.Trim(name, `"`)
			return mail.Address{Name: name, Email: email}
		}
	}
	return mail.Address{Email: strings.Trim(token, "<> ")}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// isTextUTF8 reports whether b is already usable UTF-8 text.
func isTextUTF8(b []byte) bool { return utf8.Valid(b) }
