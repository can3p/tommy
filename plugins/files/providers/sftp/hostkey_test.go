package sftp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestHostKeySurvivesAcrossRuns is the single most important test in this
// package. A regenerated host key makes every client that has ever connected
// fail with REMOTE HOST IDENTIFICATION HAS CHANGED, and the person debugging it
// blames tommy rather than their known_hosts file.
func TestHostKeySurvivesAcrossRuns(t *testing.T) {
	// A nested directory that does not exist yet, because the real default is
	// under a config directory tommy may be the first thing to write to.
	path := filepath.Join(t.TempDir(), "config", "tommy", "sftp_host_ed25519")

	first, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if !first.Created {
		t.Error("first load did not report generating the key")
	}
	if first.Fingerprint() == "" || !strings.HasPrefix(first.Fingerprint(), "SHA256:") {
		t.Errorf("fingerprint = %q", first.Fingerprint())
	}
	if got := first.Signer.PublicKey().Type(); got != ssh.KeyAlgoED25519 {
		t.Errorf("key type = %q, want %q", got, ssh.KeyAlgoED25519)
	}

	second, err := LoadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if second.Created {
		t.Error("second load regenerated the key instead of reading it back")
	}
	if second.Fingerprint() != first.Fingerprint() {
		t.Fatalf("host key changed between runs: %s then %s", first.Fingerprint(), second.Fingerprint())
	}
	if second.AuthorizedKey() != first.AuthorizedKey() {
		t.Error("the public half changed between runs")
	}
}

// TestHostKeyFilePermissions: a private key readable by anyone else is a
// finding in every audit, and the warning helper is what surfaces one that
// already exists.
func TestHostKeyFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_key")
	if _, err := LoadOrCreateHostKey(path); err != nil {
		t.Fatalf("load: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("host key mode = %#o, want 0600", perm)
	}
	if warn := hostKeyPermsWarning(path); warn != "" {
		t.Errorf("unexpected warning for a 0600 key: %s", warn)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if warn := hostKeyPermsWarning(path); warn == "" {
		t.Error("no warning for a world-readable host key")
	}
}

// TestHostKeyCorruptFileIsAnError: quietly generating a replacement would be
// the same catastrophe as never persisting one, so an unreadable key stops the
// listener with a message naming the file to delete.
func TestHostKeyCorruptFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host_key")
	if err := os.WriteFile(path, []byte("this is not a private key\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadOrCreateHostKey(path)
	if err == nil {
		t.Fatal("a corrupt host key loaded without error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the file: %v", err)
	}
}

func TestHostKeyPathHelpers(t *testing.T) {
	if def := DefaultHostKeyPath(); !strings.HasSuffix(def, filepath.Join("tommy", DefaultHostKeyName)) {
		t.Errorf("DefaultHostKeyPath() = %q", def)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory in this environment")
	}
	if got, want := ExpandPath("~/x/key"), filepath.Join(home, "x", "key"); got != want {
		t.Errorf("ExpandPath(~/x/key) = %q, want %q", got, want)
	}
	if got := ExpandPath("relative/key"); got != "relative/key" {
		t.Errorf("ExpandPath left a relative path alone: %q", got)
	}
}

// TestLoadAuthorizedKeys covers the allowlist parser, including the two ways it
// is allowed to refuse: a path that is not there and a file with nothing in it.
func TestLoadAuthorizedKeys(t *testing.T) {
	if a, err := loadAuthorizedKeys(""); err != nil || a.enabled() {
		t.Errorf("an unset path should disable the allowlist: %v %v", a, err)
	}

	dir := t.TempDir()
	pub, _ := testKeyPair(t)
	path := filepath.Join(dir, "authorized_keys")
	body := "# a comment\n\n" + string(ssh.MarshalAuthorizedKey(pub))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	keys, err := loadAuthorizedKeys(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !keys.enabled() || !keys.has(pub) {
		t.Error("the configured key was not accepted")
	}
	other, _ := testKeyPair(t)
	if keys.has(other) {
		t.Error("an unlisted key was accepted")
	}

	if _, err := loadAuthorizedKeys(filepath.Join(dir, "nope")); err == nil {
		t.Error("a missing authorized_keys file loaded without error")
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("# nothing here\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := loadAuthorizedKeys(empty); err == nil {
		t.Error("an authorized_keys file with no keys loaded without error")
	}
}
