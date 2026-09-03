package as2_test

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/can3p/tommy/plugins/as2"
)

// No test may write to the user's real config directory, so every case here
// either passes an explicit directory or asks for the in-memory mode.

func TestIdentityUsesConfiguredFiles(t *testing.T) {
	id := as2.NewIdentity()
	if err := id.Configure(as2.IdentityConfig{
		CertFile: filepath.Join(testdataDir, "tommy.crt"),
		KeyFile:  filepath.Join(testdataDir, "tommy.key"),
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	info := id.Info()
	if info.Source != as2.SourceFiles || !info.Ready {
		t.Fatalf("Info() = %+v, want a ready identity loaded from files", info)
	}
	if !strings.Contains(info.Certificate.Subject, "tommy.local") {
		t.Errorf("subject = %q, want the configured certificate", info.Certificate.Subject)
	}
	if len(strings.Split(info.Certificate.Fingerprint, ":")) != 32 {
		t.Errorf("fingerprint = %q, want 32 colon-separated hex bytes", info.Certificate.Fingerprint)
	}
}

func TestIdentityRejectsAMismatchedPair(t *testing.T) {
	id := as2.NewIdentity()
	err := id.Configure(as2.IdentityConfig{
		CertFile: filepath.Join(testdataDir, "tommy.crt"),
		KeyFile:  filepath.Join(testdataDir, "partner.key"),
	})
	if err == nil {
		t.Fatal("a certificate and an unrelated key were accepted as a pair")
	}
	if !strings.Contains(err.Error(), "not the private key") {
		t.Errorf("error = %v, want it to say the key does not match", err)
	}
}

func TestIdentityRequiresBothPaths(t *testing.T) {
	id := as2.NewIdentity()
	err := id.Configure(as2.IdentityConfig{CertFile: filepath.Join(testdataDir, "tommy.crt")})
	if err == nil || !strings.Contains(err.Error(), "together") {
		t.Fatalf("error = %v, want a complaint that both paths are required", err)
	}
}

// The whole point of persisting: a partner imports the certificate once, not
// after every restart.
func TestGeneratedIdentityPersistsAndIsReused(t *testing.T) {
	// A directory tommy creates itself, so the permission assertion below is
	// about tommy's behavior rather than about whatever mode the test harness
	// happened to leave on a directory it made.
	dir := filepath.Join(t.TempDir(), "tommy", "as2")

	first := as2.NewIdentity()
	if err := first.Configure(as2.IdentityConfig{Dir: dir}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	info := first.Info()
	if info.Source != as2.SourcePersisted || !info.Ready {
		t.Fatalf("Info() = %+v, want a generated, persisted identity", info)
	}

	certPath := filepath.Join(dir, as2.CertFileName)
	keyPath := filepath.Join(dir, as2.KeyFileName)
	for _, p := range []string{certPath, keyPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was not written: %v", p, err)
		}
	}

	// A private key must not be world-readable.
	st, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %04o, want 0600", perm)
	}
	if dst, err := os.Stat(dir); err == nil && dst.Mode().Perm()&0o077 != 0 {
		t.Errorf("directory mode = %04o, want no group or other access", dst.Mode().Perm())
	}

	second := as2.NewIdentity()
	if err := second.Configure(as2.IdentityConfig{Dir: dir}); err != nil {
		t.Fatalf("second configure: %v", err)
	}
	if got, want := second.Info().Certificate.Fingerprint, info.Certificate.Fingerprint; got != want {
		t.Errorf("a restart produced a different certificate: %q vs %q; the partner would have to re-import", got, want)
	}
}

// Deps.ConfigDir is what "beside the config file" means, and it must beat the
// OS config directory - which no test may touch.
func TestConfigDirIsPreferredOverTheOSLocation(t *testing.T) {
	dir := t.TempDir()
	id := as2.NewIdentity()
	if err := id.Configure(as2.IdentityConfig{ConfigDir: dir}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if got := id.Info().CertPath; got != filepath.Join(dir, as2.CertFileName) {
		t.Errorf("cert path = %q, want it beside the config file in %q", got, dir)
	}
}

// An explicit Dir beats ConfigDir.
func TestExplicitDirWins(t *testing.T) {
	explicit, config := t.TempDir(), t.TempDir()
	id := as2.NewIdentity()
	if err := id.Configure(as2.IdentityConfig{Dir: explicit, ConfigDir: config}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if got := id.Info().CertPath; !strings.HasPrefix(got, explicit) {
		t.Errorf("cert path = %q, want it under the explicitly configured %q", got, explicit)
	}
	if entries, _ := os.ReadDir(config); len(entries) != 0 {
		t.Errorf("the config directory was written to anyway: %v", entries)
	}
}

func TestInMemoryIdentityWritesNothing(t *testing.T) {
	dir := t.TempDir()
	id := as2.NewIdentity()
	if err := id.Configure(as2.IdentityConfig{InMemory: true, Dir: dir}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	info := id.Info()
	if info.Source != as2.SourceMemory || !info.Ready {
		t.Fatalf("Info() = %+v, want a ready in-memory identity", info)
	}
	if info.CertPath != "" {
		t.Errorf("CertPath = %q, want nothing persisted", info.CertPath)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("in-memory mode wrote %v", entries)
	}
}

// A generated certificate has to do both AS2 jobs, and it has to last long
// enough that nobody re-imports it.
func TestGeneratedCertificateIsUsableForAS2(t *testing.T) {
	id := as2.NewIdentity()
	if err := id.Configure(as2.IdentityConfig{InMemory: true, CommonName: "tommy-under-test"}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	cert, key, err := id.KeyPair()
	if err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	if cert.Subject.CommonName != "tommy-under-test" {
		t.Errorf("common name = %q", cert.Subject.CommonName)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		t.Error("the generated key does not match the generated certificate")
	}
	if bits := key.N.BitLen(); bits < 2048 {
		t.Errorf("key is %d bits, want at least 2048", bits)
	}
	if cert.NotAfter.Sub(cert.NotBefore) < 9*365*24*60*60*1e9 {
		t.Errorf("validity is %s, want about ten years so a partner imports it once",
			cert.NotAfter.Sub(cert.NotBefore))
	}
	if cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Error("the certificate cannot sign, so it cannot sign an MDN")
	}
	if cert.KeyUsage&x509.KeyUsageKeyEncipherment == 0 {
		t.Error("the certificate cannot be a key-encipherment recipient, so nothing can be encrypted to it")
	}
	if len(id.CertificatePEM()) == 0 {
		t.Error("no PEM to hand a partner")
	}
}

func TestPartnerCertificateIsLoaded(t *testing.T) {
	id := as2.NewIdentity()
	if err := id.Configure(as2.IdentityConfig{
		InMemory:        true,
		PartnerCertFile: filepath.Join(testdataDir, "partner.crt"),
	}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	partner := id.Partner()
	if partner == nil {
		t.Fatal("no partner certificate loaded")
	}
	if !strings.Contains(partner.Subject.String(), "partner.example") {
		t.Errorf("partner subject = %q", partner.Subject)
	}
}

// A bad partner certificate must not stop tommy answering: it is a
// configuration mistake in the check, not in the ability to capture.
func TestBadPartnerCertificateDoesNotDisableTheIdentity(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "not-a-cert.pem")
	if err := os.WriteFile(bad, []byte("this is not PEM\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := as2.NewIdentity()
	err := id.Configure(as2.IdentityConfig{InMemory: true, PartnerCertFile: bad})
	if err == nil {
		t.Fatal("the unreadable partner certificate was accepted silently")
	}
	if _, _, keyErr := id.KeyPair(); keyErr != nil {
		t.Errorf("the key pair is unusable because of a bad partner file: %v", keyErr)
	}
	if id.Partner() != nil {
		t.Error("an unparseable file became the partner certificate")
	}
}

func TestEncryptedKeyIsRefusedWithAnExplanation(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(keyPath, []byte(
		"-----BEGIN ENCRYPTED PRIVATE KEY-----\nMIIB\n-----END ENCRYPTED PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := as2.NewIdentity()
	err := id.Configure(as2.IdentityConfig{
		CertFile: filepath.Join(testdataDir, "tommy.crt"),
		KeyFile:  keyPath,
	})
	if err == nil {
		t.Fatal("an encrypted key was accepted")
	}
	if !strings.Contains(err.Error(), "openssl pkey") {
		t.Errorf("error = %v, want it to say how to decrypt the key", err)
	}
}
