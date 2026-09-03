package http_test

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/can3p/tommy/plugins/as2"
)

// Every AS2 message in this package's tests is built by OpenSSL and posted over
// a real socket, and every MDN that comes back is handed to OpenSSL to verify.
//
// That is not belt-and-braces. There is no Go AS2 client in existence, so a test
// that built its messages with tommy's own code would only ever prove tommy
// agrees with itself - which is exactly the shape of test that let ftpserverlib
// silently corrupt every download in this project until somebody ran curl. An
// independent implementation of the crypto is the only thing that can say
// "usable", as opposed to "self-consistent".
//
// OpenSSL 3.6.1 is what these were written against. When it is missing the tests
// skip rather than pass, because a green run on a machine that could not check
// anything is worse than a red one.

// opensslBin and curlBin are the external tools, resolved once. Empty means the
// tests that need them skip.
var (
	opensslBin string
	curlBin    string
	// labDir holds the three key pairs every test shares: tommy (the receiver),
	// partner (the sender) and stranger (whose key nothing here holds, so a
	// message encrypted to it cannot be decrypted).
	labDir string
	labErr error
)

func TestMain(m *testing.M) {
	os.Exit(func() int {
		opensslBin = findOpenSSL()
		curlBin, _ = exec.LookPath("curl")
		if opensslBin != "" {
			dir, err := os.MkdirTemp("", "as2-http-lab-")
			if err != nil {
				labErr = err
			} else {
				defer func() { _ = os.RemoveAll(dir) }()
				if labErr = buildLab(dir); labErr == nil {
					labDir = dir
				}
			}
		}
		return m.Run()
	}())
}

// findOpenSSL prefers a build that actually has CMS. macOS ships LibreSSL as
// /usr/bin/openssl and Homebrew's real OpenSSL is not on PATH ahead of it, so
// PATH order alone picks the wrong one on the machine this was written on.
func findOpenSSL() string {
	if p := os.Getenv("OPENSSL"); p != "" {
		return p
	}
	for _, p := range []string{"/opt/homebrew/bin/openssl", "/usr/local/opt/openssl@3/bin/openssl"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, _ := exec.LookPath("openssl")
	return p
}

// buildLab generates the key pairs and then proves this OpenSSL can do CMS at
// all, so a build without it skips instead of failing every test with the same
// unhelpful message.
func buildLab(dir string) error {
	for _, who := range []string{"tommy", "partner", "stranger"} {
		cmd := exec.Command(opensslBin, "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-sha256",
			"-days", "365", "-subj", "/CN="+who+".example/O=tommy AS2 test",
			"-keyout", filepath.Join(dir, who+".key"), "-out", filepath.Join(dir, who+".crt"))
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("generate %s: %v: %s", who, err, out)
		}
	}
	probe := filepath.Join(dir, "probe.txt")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		return err
	}
	cmd := exec.Command(opensslBin, "cms", "-sign", "-in", probe, "-binary", "-outform", "SMIME",
		"-signer", filepath.Join(dir, "partner.crt"), "-inkey", filepath.Join(dir, "partner.key"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("openssl has no usable cms: %v: %s", err, out)
	}
	return nil
}

// requireOpenSSL skips cleanly when the toolchain is not here.
func requireOpenSSL(t *testing.T) {
	t.Helper()
	if opensslBin == "" {
		t.Skip("openssl not found; set OPENSSL to a build with CMS support")
	}
	if labDir == "" {
		t.Skipf("openssl is present but unusable: %v", labErr)
	}
}

func requireCurl(t *testing.T) {
	t.Helper()
	requireOpenSSL(t)
	if curlBin == "" {
		t.Skip("curl not found")
	}
}

// key is the path to one of the lab's files.
func key(name string) string { return filepath.Join(labDir, name) }

// openssl runs the tool with stdin and returns stdout, failing the test on a
// non-zero exit with whatever it wrote to stderr, which is where OpenSSL puts
// the only useful part of a CMS error.
func openssl(t *testing.T, stdin []byte, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(opensslBin, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("openssl %s: %v\n%s", strings.Join(args, " "), err, errBuf.String())
	}
	return out.Bytes()
}

// ediEntity is the MIME entity an AS2 sender wraps around an EDI document:
// CRLF headers, a CRLF separator, and the document with no trailing newline.
// It is built here rather than by OpenSSL because it carries no crypto - it is
// the plaintext both ends are arguing about.
func ediEntity(doc string) []byte {
	return []byte("Content-Type: application/edi-x12\r\n" +
		"Content-Transfer-Encoding: binary\r\n" +
		"Content-Disposition: attachment; filename=payload.edi\r\n" +
		"\r\n" + doc)
}

// sampleEDI is a small X12 850 carrying a token the corrupt-signature test
// flips, so the tampering lands in the signed content rather than in the
// signature structure - a broken signature and a broken message are different
// failures and only the second is interesting here.
const sampleEDI = "ISA*00*          *00*          *ZZ*PARTNER        *ZZ*TOMMY          *260903*1200*U*00401*000000001*0*P*>~" +
	"ST*850*0001~BEG*00*SA*PO-4711**20260903~PO1*1*10*EA*9.99**BP*WIDGET-1~CTT*1~SE*5*0001~IEA*1*000000001~"

// cmsSign returns OpenSSL's S/MIME multipart/signed output over entity, signed
// by the partner - the identity a test configures as the expected one.
func cmsSign(t *testing.T, entity []byte, md string) []byte {
	t.Helper()
	return cmsSignAs(t, entity, md, "partner")
}

// cmsSignAs signs as one of the lab's identities, so a test can send something
// signed by somebody other than the configured partner.
func cmsSignAs(t *testing.T, entity []byte, md, who string) []byte {
	t.Helper()
	return openssl(t, entity, "cms", "-sign", "-binary", "-outform", "SMIME", "-md", md,
		"-signer", key(who+".crt"), "-inkey", key(who+".key"))
}

// cmsEncryptBase64 encrypts to recipientCert and returns base64 as it goes on
// the wire. DER plus `openssl base64` rather than -outform SMIME on purpose:
// the SMIME form carries three MIME headers of its own, and in AS2 those belong
// in the HTTP request, not in the body. Feeding the SMIME output through
// `tail -n +2` - which strips only MIME-Version - leaves the other two inside
// the body, and the receiver then fails to base64-decode it. That is a real
// mistake with a confusing symptom, and it is why this helper exists.
func cmsEncryptBase64(t *testing.T, entity []byte, recipientCert string) []byte {
	t.Helper()
	der := openssl(t, entity, "cms", "-encrypt", "-aes-128-cbc", "-outform", "DER", recipientCert)
	return openssl(t, der, "base64")
}

// splitSMIME promotes an S/MIME message's own MIME headers to HTTP headers and
// returns the body, which is what an AS2 sender puts on the wire: there is no
// second header block inside an AS2 request body.
//
// The separator has to be looked for in both forms and the earlier one wins.
// OpenSSL writes this outer header block with bare LF while the part headers
// below it use CRLF, so a splitter that only knows about CRLFCRLF finds the
// wrong blank line - somewhere deep inside the multipart - and silently
// produces a body that is missing its first part.
func splitSMIME(t *testing.T, raw []byte) (http.Header, []byte) {
	t.Helper()
	sep, n := -1, 0
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		sep, n = i, 4
	}
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 && (sep < 0 || i < sep) {
		sep, n = i, 2
	}
	if sep < 0 {
		t.Fatalf("openssl output has no header/body separator:\n%s", firstBytes(raw, 200))
	}
	h := http.Header{}
	for _, line := range strings.Split(strings.ReplaceAll(string(raw[:sep]), "\r\n", "\n"), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		h.Set(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	return h, raw[sep+n:]
}

func firstBytes(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

// The issue codes asserted on in the failure tests. They are spelled out from
// the as2 package rather than as string literals so a rename is a compile
// error here rather than a test that silently stops checking anything.
const (
	issueIntegrityCheckFailed = as2.IssueIntegrityCheckFailed
	issueDecryptionFailed     = as2.IssueDecryptionFailed
	issueMalformedMIME        = as2.IssueMalformedMIME
	issueEmptyBody            = as2.IssueEmptyBody
)
