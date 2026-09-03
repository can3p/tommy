package http_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/can3p/tommy/plugins/as2"
)

// TestCurlDrivesTheExchange posts with curl rather than Go's HTTP client.
//
// Every other test here already builds its message bytes with OpenSSL, so the
// crypto is independently checked. What this adds is the transport: Go's client
// and Go's server agree about a great many things by construction - chunking,
// header casing, connection reuse, expect/continue - and a fake vendor endpoint
// is only useful if a client that shares none of that code can talk to it. The
// ftpserverlib bug in this project's history was exactly this shape: invisible
// to a mocked driver, obvious the first time curl fetched a file.
func TestCurlDrivesTheExchange(t *testing.T) {
	requireCurl(t)
	in := start(t, nil)

	dir := t.TempDir()
	msg := filepath.Join(dir, "message.p7m")
	if err := os.WriteFile(msg, cmsEncryptBase64(t, cmsSign(t, ediEntity(sampleEDI), "sha256"), key("tommy.crt")), 0o600); err != nil {
		t.Fatalf("write message: %v", err)
	}
	head := filepath.Join(dir, "mdn.head")
	body := filepath.Join(dir, "mdn.body")

	out, err := exec.Command(curlBin, "-s", "--fail-with-body",
		"-o", body, "-D", head, "--data-binary", "@"+msg,
		"-H", "AS2-From: PARTNER",
		"-H", "AS2-To: TOMMY",
		"-H", "AS2-Version: 1.1",
		"-H", "Message-ID: "+messageID,
		"-H", "Content-Type: application/pkcs7-mime; smime-type=enveloped-data; name=smime.p7m",
		"-H", "Content-Transfer-Encoding: base64",
		"-H", "Disposition-Notification-To: as2@partner.example",
		"-H", "Disposition-Notification-Options: signed-receipt-protocol=optional,pkcs7-signature; signed-receipt-micalg=optional,sha256",
		in.Ingress("/as2")).CombinedOutput()
	if err != nil {
		t.Fatalf("curl: %v\n%s", err, out)
	}

	headers, err := os.ReadFile(head)
	if err != nil {
		t.Fatalf("read response headers: %v", err)
	}
	if !strings.HasPrefix(string(headers), "HTTP/1.1 200 OK") {
		t.Errorf("status line = %q", strings.SplitN(string(headers), "\r\n", 2)[0])
	}
	if !strings.Contains(string(headers), "Content-Type: multipart/signed") {
		t.Errorf("a signed receipt was requested but the reply is not multipart/signed:\n%s", headers)
	}

	// Reassemble and verify, exactly as the shipped snippet tells a user to.
	ct := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(headers), "\r\n", "\n"), "\n") {
		if v, ok := strings.CutPrefix(line, "Content-Type: "); ok {
			ct = v
		}
	}
	mdn, err := os.ReadFile(body)
	if err != nil {
		t.Fatalf("read MDN: %v", err)
	}
	mime := filepath.Join(dir, "mdn.mime")
	if err := os.WriteFile(mime, append([]byte("Content-Type: "+ct+"\r\n\r\n"), mdn...), 0o600); err != nil {
		t.Fatalf("write MDN entity: %v", err)
	}
	report := openssl(t, nil, "smime", "-verify", "-in", mime, "-CAfile", key("tommy.crt"), "-purpose", "any")

	m, _ := captured(t, in)
	if !m.Security.Signed || !m.Security.Encrypted {
		t.Errorf("security = %q, want signed and encrypted", m.Security.Summary())
	}
	for _, want := range []string{
		"Original-Message-ID: " + messageID,
		"Received-Content-MIC: " + m.MIC.Header(),
		as2.DispositionProcessed,
	} {
		if !strings.Contains(string(report), want) {
			t.Errorf("verified MDN is missing %q:\n%s", want, report)
		}
	}
}
