package http_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/as2"
	as2http "github.com/can3p/tommy/plugins/as2/providers/http"
)

// GET /as2/certificate is what gives the encrypted flow a cold start at all. A
// partner cannot encrypt anything until it holds this, and the alternative is
// telling somebody to find a PEM file inside a container.

func TestCertificateServedBeforeAnyMessage(t *testing.T) {
	in := start(t, nil)

	resp := in.Get(in.Ingress("/as2/certificate"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/x-pem-file" {
		t.Errorf("Content-Type = %q, want application/x-pem-file", got)
	}
	if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "filename=") {
		t.Errorf("Content-Disposition = %q, want a filename so a browser saves something usable", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}

	pem := readAll(t, resp)
	if !strings.HasPrefix(string(pem), "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("body is not a PEM certificate:\n%s", firstBytes(pem, 120))
	}
	// It is the configured certificate and not some other one: compare the
	// fingerprints OpenSSL computes for each, which is also the check a person
	// does by eye against what their AS2 software imported.
	if got, want := fingerprint(t, pem), fingerprintOf(t, key("tommy.crt")); got != want {
		t.Errorf("served certificate fingerprint = %s, configured one is %s", got, want)
	}

	// Nothing arrived, and nothing had to: this route works on an empty
	// instance, which is the entire point of it.
	if evs := in.Events(store.Query{Plugin: as2.Name}); len(evs) != 0 {
		t.Errorf("serving the certificate recorded %d events", len(evs))
	}
}

// TestCertificateDrivesColdStart is the whole exchange with nothing shared in
// advance: fetch the certificate over HTTP, encrypt to it with OpenSSL, post,
// and confirm the server could open what came back. If this passes, a partner
// who has only been given a URL can talk to tommy.
func TestCertificateDrivesColdStart(t *testing.T) {
	in := start(t, nil)

	resp := in.Get(in.Ingress("/as2/certificate"))
	pem := readAll(t, resp)
	_ = resp.Body.Close()

	certPath := filepath.Join(t.TempDir(), "fetched.pem")
	if err := os.WriteFile(certPath, pem, 0o600); err != nil {
		t.Fatalf("write fetched certificate: %v", err)
	}

	h := http.Header{}
	h.Set("Content-Type", "application/pkcs7-mime; smime-type=enveloped-data; name=smime.p7m")
	h.Set("Content-Transfer-Encoding", "base64")
	postResp, mdn := post(t, in, h, cmsEncryptBase64(t, ediEntity(sampleEDI), certPath))
	assertMDNEnvelope(t, postResp, mdn)

	m, _ := captured(t, in)
	if m.Security.Encryption == nil || !m.Security.Encryption.Decrypted {
		t.Fatalf("a message encrypted to the certificate this endpoint served did not decrypt: %+v",
			m.Security.Encryption)
	}
}

// TestCertificateUnavailable covers the 503 branch. A cert_file that is not
// there must not take the endpoint down: messages are still captured, they
// simply cannot be decrypted, and the operator is told which setting to fix.
func TestCertificateUnavailable(t *testing.T) {
	in := start(t, map[string]any{
		"cert_file": filepath.Join(t.TempDir(), "absent.crt"),
		"key_file":  filepath.Join(t.TempDir(), "absent.key"),
	})

	status, body := in.GetBody(in.Ingress("/as2/certificate"))
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503\n%s", status, body)
	}
	if !strings.Contains(body, "cert_file") {
		t.Errorf("the 503 body does not name the setting to fix:\n%s", body)
	}

	// The receiving endpoint is still up, which is the part that matters.
	h := http.Header{}
	h.Set("Content-Type", "application/edi-x12")
	resp, mdn := post(t, in, h, []byte(sampleEDI))
	assertMDNEnvelope(t, resp, mdn)
}

// TestGeneratedCertificateGoesToCertDir pins where a generated key pair lands.
// The failure this guards against is silent and unpleasant: a provider that
// ignored cert_dir would write into the real user config directory, including
// during `make check`.
func TestGeneratedCertificateGoesToCertDir(t *testing.T) {
	requireOpenSSL(t)
	isolateConfigDir(t)

	dir := t.TempDir()
	cfg := config.Ephemeral()
	cfg.SetProvider(as2.Name, as2http.Name, config.NewProviderConfig(map[string]any{
		"cert_dir":    dir,
		"common_name": "tommy-test.example",
	}))
	in := testutil.Start(t, cfg, as2.New(as2http.New()))

	// Booting a server must not have created anything. Generation is deferred
	// to first use precisely so that building a server - which every
	// conformance test does - writes no private key anywhere.
	for _, name := range []string{as2.CertFileName, as2.KeyFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s was written merely by starting the server", name)
		}
	}

	pem := readAll(t, in.Get(in.Ingress("/as2/certificate")))

	// Fetching it is a genuine use, so now it must exist - and be on disk, so a
	// partner that imported it does not have to import it again after a restart.
	for _, name := range []string{as2.CertFileName, as2.KeyFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not written to cert_dir after first use: %v", name, err)
		}
	}

	subject := string(opensslOn(t, pem, "x509", "-noout", "-subject"))
	if !strings.Contains(subject, "tommy-test.example") {
		t.Errorf("subject = %q, want the configured common_name", strings.TrimSpace(subject))
	}
}

// TestInMemoryIdentityWritesNothing is the setting for anyone who wants tommy
// to leave no files behind. The assertion that matters is the negative one, so
// the config directory is redirected somewhere empty and checked afterwards.
func TestInMemoryIdentityWritesNothing(t *testing.T) {
	requireOpenSSL(t)
	home := isolateConfigDir(t)

	cfg := config.Ephemeral()
	cfg.SetProvider(as2.Name, as2http.Name, config.NewProviderConfig(map[string]any{
		"in_memory": true,
	}))
	in := testutil.Start(t, cfg, as2.New(as2http.New()))

	pem := readAll(t, in.Get(in.Ingress("/as2/certificate")))
	if !strings.HasPrefix(string(pem), "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("no usable certificate was generated in memory")
	}
	var found []string
	_ = filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found = append(found, path)
		}
		return nil
	})
	if len(found) != 0 {
		t.Errorf("in_memory wrote files: %v", found)
	}
}

// isolateConfigDir points os.UserConfigDir at a temporary directory and returns
// it. Without this, any test that lets a certificate be generated writes into
// the real user's config directory - a side effect of `go test` that nobody
// would look for and that would survive the run.
func isolateConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)            // darwin: $HOME/Library/Application Support
	t.Setenv("XDG_CONFIG_HOME", dir) // linux
	t.Setenv("AppData", dir)         // windows
	return dir
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

// fingerprint is the SHA-256 of a PEM certificate as OpenSSL prints it.
func fingerprint(t *testing.T, pem []byte) string {
	t.Helper()
	return strings.TrimSpace(string(opensslOn(t, pem, "x509", "-noout", "-fingerprint", "-sha256")))
}

func fingerprintOf(t *testing.T, path string) string {
	t.Helper()
	pem, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return fingerprint(t, pem)
}

// opensslOn runs openssl over bytes on stdin.
func opensslOn(t *testing.T, stdin []byte, args ...string) []byte {
	t.Helper()
	return openssl(t, stdin, args...)
}
