package as2_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/can3p/tommy/plugins/as2"
)

// The MIME layer is tested on bytes rather than on shapes, because the only
// thing that matters about it is that the bytes it hands back are the bytes
// that arrived. A digest over anything else is a MIC no partner agrees with.

func TestPartsAreByteExact(t *testing.T) {
	// Deliberately mixed line endings: bare-LF delimiters with CRLF part
	// headers is exactly what openssl cms -outform SMIME writes.
	body := "--B\n" +
		"Content-Type: application/edi-x12\r\n" +
		"\r\n" +
		"ISA*00*~IEA*1*~" +
		"\n--B\n" +
		"Content-Type: application/pkcs7-signature\r\n" +
		"\r\n" +
		"c2lnbmF0dXJl" +
		"\n--B--\n"

	e, err := as2.ParseEntity([]byte("Content-Type: multipart/signed; boundary=B\r\n\r\n" + body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	parts, err := e.Parts()
	if err != nil {
		t.Fatalf("parts: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(parts))
	}

	want := "Content-Type: application/edi-x12\r\n\r\nISA*00*~IEA*1*~"
	if got := string(parts[0].Raw); got != want {
		t.Errorf("part 0 raw = %q, want %q.\nThe line break before a delimiter belongs to the delimiter, "+
			"not to the content; including it makes every MIC one or two bytes too long.", got, want)
	}
	if got := string(parts[0].Body); got != "ISA*00*~IEA*1*~" {
		t.Errorf("part 0 body = %q", got)
	}
	if got := parts[1].Get("Content-Type"); got != "application/pkcs7-signature" {
		t.Errorf("part 1 content type = %q", got)
	}
}

func TestCRLFDelimiterIsHandledToo(t *testing.T) {
	body := "--B\r\nContent-Type: text/plain\r\n\r\nhello\r\n--B--\r\n"
	e, err := as2.ParseEntity([]byte("Content-Type: multipart/signed; boundary=B\r\n\r\n" + body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	parts, err := e.Parts()
	if err != nil {
		t.Fatalf("parts: %v", err)
	}
	if got := string(parts[0].Body); got != "hello" {
		t.Errorf("body = %q, want %q with the delimiter's CRLF removed", got, "hello")
	}
}

// A run of dashes inside the content must not be mistaken for a delimiter.
func TestDashesInsideContentAreNotDelimiters(t *testing.T) {
	body := "--B\r\nContent-Type: text/plain\r\n\r\nline\r\n--Bogus not a delimiter\r\nmore\r\n--B--\r\n"
	e, _ := as2.ParseEntity([]byte("Content-Type: multipart/x; boundary=B\r\n\r\n" + body))
	parts, err := e.Parts()
	if err != nil {
		t.Fatalf("parts: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1", len(parts))
	}
	if !strings.Contains(string(parts[0].Body), "--Bogus not a delimiter") {
		t.Errorf("body = %q, want the dashed line kept as content", parts[0].Body)
	}
}

func TestMissingClosingDelimiterStillYieldsParts(t *testing.T) {
	body := "--B\r\nContent-Type: text/plain\r\n\r\nhello"
	e, _ := as2.ParseEntity([]byte("Content-Type: multipart/x; boundary=B\r\n\r\n" + body))
	parts, err := e.Parts()
	if err == nil {
		t.Error("a truncated multipart reported no problem")
	}
	if len(parts) != 1 {
		t.Fatalf("got %d parts, want the one part before the truncation", len(parts))
	}
	if string(parts[0].Body) != "hello" {
		t.Errorf("part body = %q, want %q", parts[0].Body, "hello")
	}
}

func TestTransferEncodings(t *testing.T) {
	cases := []struct {
		name, header, body, want string
	}{
		{"base64 folded", "base64", "aGVsbG8g\r\nd29ybGQ=", "hello world"},
		{"base64 unpadded", "base64", "aGVsbG8gd29ybGQ", "hello world"},
		{"quoted-printable", "quoted-printable", "a=3Db", "a=b"},
		{"7bit", "7bit", "plain", "plain"},
		{"8bit", "8bit", "plain", "plain"},
		{"binary", "binary", "plain", "plain"},
		{"absent defaults to 7bit", "", "plain", "plain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := "Content-Type: text/plain\r\n"
			if tc.header != "" {
				raw += "Content-Transfer-Encoding: " + tc.header + "\r\n"
			}
			raw += "\r\n" + tc.body
			e, err := as2.ParseEntity([]byte(raw))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, err := e.Decoded()
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("decoded = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestUnknownTransferEncodingKeepsTheBytes(t *testing.T) {
	e, _ := as2.ParseEntity([]byte("Content-Transfer-Encoding: uuencode\r\n\r\nbegin 644"))
	got, err := e.Decoded()
	if err == nil {
		t.Fatal("an unknown transfer encoding was accepted silently")
	}
	if string(got) != "begin 644" {
		t.Errorf("decoded = %q, want the bytes kept as they arrived", got)
	}
}

func TestFoldedHeadersAreUnfolded(t *testing.T) {
	raw := "Content-Type: multipart/signed; micalg=sha-256;\r\n" +
		"\tprotocol=\"application/pkcs7-signature\";\r\n" +
		" boundary=\"----abc\"\r\n\r\nbody"
	e, err := as2.ParseEntity([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	mt, params := e.MediaType()
	if mt != "multipart/signed" {
		t.Errorf("media type = %q", mt)
	}
	if params["boundary"] != "----abc" {
		t.Errorf("boundary = %q, want ----abc from the folded header", params["boundary"])
	}
	if params["micalg"] != "sha-256" {
		t.Errorf("micalg = %q", params["micalg"])
	}
}

func TestEntityWithNoSeparatorKeepsItsBytes(t *testing.T) {
	e, err := as2.ParseEntity([]byte("no headers no blank line"))
	if err == nil {
		t.Error("an entity with no header/body separator reported no problem")
	}
	if string(e.Body) != "no headers no blank line" {
		t.Errorf("body = %q, want the bytes kept", e.Body)
	}
}

// ------------------------------------------------------------------- the MIC

func TestNormalizeMICAlg(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"sha1":                   {"sha1", true},
		"SHA1":                   {"sha1", true},
		"sha-1":                  {"sha1", true},
		"sha":                    {"sha1", true},
		"md5":                    {"md5", true},
		"sha256":                 {"sha256", true},
		"sha-256":                {"sha256", true}, // what openssl writes
		"sha2-256":               {"sha256", true}, // what some Java stacks write
		`"sha-512"`:              {"sha512", true},
		"2.16.840.1.101.3.4.2.1": {"sha256", true},
		"1.3.14.3.2.26":          {"sha1", true},
		"whirlpool":              {"", false},
		"":                       {"", false},
	}
	for in, want := range cases {
		got, ok := as2.NormalizeMICAlg(in)
		if ok != want.ok {
			t.Errorf("NormalizeMICAlg(%q) ok = %v, want %v", in, ok, want.ok)
			continue
		}
		if ok && got != want.want {
			t.Errorf("NormalizeMICAlg(%q) = %q, want %q", in, got, want.want)
		}
	}
}

func TestComputeMICMatchesOpenSSL(t *testing.T) {
	mics := referenceMICs(t)
	inner := fixture(t, "inner.mime")

	for alg, want := range map[string]string{
		"sha256": mics["inner_sha256"],
		"sha1":   mics["inner_sha1"],
		"md5":    mics["inner_md5"],
	} {
		got, err := as2.ComputeMIC(alg, as2.MICOverSignedContent, inner)
		if err != nil {
			t.Fatalf("ComputeMIC(%s): %v", alg, err)
		}
		if got.Digest != want {
			t.Errorf("%s digest = %q, want openssl's %q", alg, got.Digest, want)
		}
		if got.Header() != want+", "+alg {
			t.Errorf("header = %q, want RFC 4130's \"<digest>, <alg>\" form", got.Header())
		}
	}
}

// Canonicalization touches the headers and nothing else. Rewriting the body
// would move the digest away from the one the sender computed.
func TestCanonicalizeLeavesTheBodyAlone(t *testing.T) {
	in := []byte("Content-Type: text/plain\nX-Extra: 1\n\nline one\nline two\n")
	got := as2.Canonicalize(in)
	want := "Content-Type: text/plain\r\nX-Extra: 1\r\n\r\nline one\nline two\n"
	if string(got) != want {
		t.Errorf("Canonicalize = %q, want %q", got, want)
	}

	already := []byte("A: 1\r\n\r\nbody\r\nhere")
	if got := as2.Canonicalize(already); !bytes.Equal(got, already) {
		t.Errorf("Canonicalize changed already-canonical bytes: %q", got)
	}
}

func TestCanonicalizeAllNormalizesEverything(t *testing.T) {
	in := []byte("A: 1\n\nline one\nline two")
	if got := string(as2.CanonicalizeAll(in)); got != "A: 1\r\n\r\nline one\r\nline two" {
		t.Errorf("CanonicalizeAll = %q", got)
	}
}

// ------------------------------------------------------------ format sniffing

func TestDetectFormat(t *testing.T) {
	cases := []struct {
		name, contentType string
		body              string
		want              string
	}{
		{"x12 by content", "application/octet-stream", "ISA*00*          *00*", as2.FormatX12},
		{"edifact UNB", "application/octet-stream", "UNB+UNOA:1+SENDER", as2.FormatEDIFACT},
		{"edifact UNA", "text/plain", "UNA:+.? 'UNB+UNOA", as2.FormatEDIFACT},
		{"xml declaration", "application/octet-stream", `<?xml version="1.0"?><PO/>`, as2.FormatXML},
		{"xml by header", "application/xml", "<PurchaseOrder/>", as2.FormatXML},
		{"json", "application/json", `{"po":4711}`, as2.FormatJSON},
		{"json sniffed", "application/octet-stream", `{"po":4711}`, as2.FormatJSON},
		{"a brace that is not json", "application/octet-stream", "{not json at all", as2.FormatText},
		{"plain text", "text/plain", "just some text", as2.FormatText},
		{"binary", "application/octet-stream", "\x00\x01\x02\xff\xfe", as2.FormatBinary},
		{"empty", "", "", as2.FormatText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := as2.DetectFormat(tc.contentType, []byte(tc.body)); got != tc.want {
				t.Errorf("DetectFormat(%q, %q) = %q, want %q", tc.contentType, tc.body, got, tc.want)
			}
		})
	}
}

// The preview is shown in a list and indexed for search, so it has to be one
// short line of valid UTF-8 whatever arrived.
func TestMakePreview(t *testing.T) {
	long := strings.Repeat("ISA*00*          ", 200)
	got := as2.MakePreview([]byte(long), as2.FormatX12)
	if len([]rune(got)) > as2.PreviewLimit+1 {
		t.Errorf("preview is %d runes, want at most %d", len([]rune(got)), as2.PreviewLimit+1)
	}
	if strings.ContainsAny(got, "\r\n\t") {
		t.Errorf("preview carries line breaks: %q", got)
	}
	if as2.MakePreview([]byte{0, 1, 2}, as2.FormatBinary) != "" {
		t.Error("binary content produced a preview")
	}
	if got := as2.MakePreview([]byte("a\x01b\nc"), as2.FormatText); got != "ab c" {
		t.Errorf("preview = %q, want control characters dropped and breaks folded", got)
	}
}
