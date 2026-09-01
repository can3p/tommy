package smtp

import (
	"strings"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/ianaindex"
)

// text turns the bytes of a text part into a Go string, honoring the charset
// the sender declared.
//
// It never fails. A charset nobody has heard of, or bytes that do not match the
// charset that was declared, degrade to a reading that is at worst ugly - every
// byte still maps to a character - rather than to replacement characters or an
// empty body. Whatever was assumed is recorded as a warning.
func (p *parser) text(charset string, b []byte) string {
	name := strings.ToLower(strings.Trim(strings.TrimSpace(charset), `"'`))
	switch name {
	case "", "utf-8", "utf8", "us-ascii", "ascii", "unicode-1-1-utf-8":
		if isTextUTF8(b) {
			return string(b)
		}
		if name != "" {
			p.warn("body declared %s but is not valid %s: it was read as ISO-8859-1", charset, charset)
		}
		return decodeLatin1(b)
	}

	enc, err := ianaindex.MIME.Encoding(name)
	if err != nil || enc == nil {
		// x/text knows the IANA registry but not every alias in the wild.
		if alias, ok := charsetAliases[name]; ok {
			enc = alias
		} else {
			p.warn("unknown charset %q: the body was read as ISO-8859-1", charset)
			if isTextUTF8(b) {
				return string(b)
			}
			return decodeLatin1(b)
		}
	}

	out, err := enc.NewDecoder().Bytes(b)
	if err != nil {
		p.warn("charset %q: %v; the body was read as ISO-8859-1", charset, err)
		if isTextUTF8(b) {
			return string(b)
		}
		return decodeLatin1(b)
	}
	return string(out)
}

// charsetAliases covers the spellings x/text's IANA index rejects but that mail
// clients keep sending.
var charsetAliases = map[string]encoding.Encoding{
	"cp1251":       charmap.Windows1251,
	"cp1252":       charmap.Windows1252,
	"win-1251":     charmap.Windows1251,
	"win-1252":     charmap.Windows1252,
	"windows-1251": charmap.Windows1251,
	"windows-1252": charmap.Windows1252,
	"latin1":       charmap.ISO8859_1,
	"latin-1":      charmap.ISO8859_1,
	"iso8859-1":    charmap.ISO8859_1,
	"iso-latin-1":  charmap.ISO8859_1,
}

// decodeLatin1 maps every byte to the code point of the same value, which is
// the one decoding that cannot fail and cannot lose a byte.
func decodeLatin1(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		sb.WriteRune(rune(c))
	}
	return sb.String()
}
