package as2_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/can3p/tommy/plugins/as2"
)

// sandboxHome points the OS config directory at a temporary one, so a test that
// is *checking* for stray writes cannot itself scribble in the user's home if it
// regresses.
func sandboxHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	return home
}

// filesUnder lists every regular file below root, for asserting that nothing
// was created.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	if err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			found = append(found, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return found
}

// TestConfigureGeneratesNothing is a regression test for a real defect: the
// identity used to generate and write a key pair inside Configure, which runs in
// a provider's RegisterIngress - so anything that merely built a server left a
// private key in the user's own config directory. plugintest.Conformance builds
// a server, which meant `make check` did it.
//
// Registration must create nothing. Only genuine use may.
func TestConfigureGeneratesNothing(t *testing.T) {
	home := sandboxHome(t)

	id := as2.NewIdentity()
	if err := id.Configure(as2.IdentityConfig{}); err != nil {
		t.Fatalf("configure: %v", err)
	}

	if got := filesUnder(t, home); len(got) != 0 {
		t.Fatalf("Configure created %v; it must create nothing", got)
	}
}

// TestFirstUseGeneratesAndPersists is the other half: deferring generation must
// not have turned it off. The certificate has to exist once something actually
// needs it, and it has to be written down, because a partner that imported it
// should not have to import it again after a restart.
func TestFirstUseGeneratesAndPersists(t *testing.T) {
	home := sandboxHome(t)

	id := as2.NewIdentity()
	if err := id.Configure(as2.IdentityConfig{}); err != nil {
		t.Fatalf("configure: %v", err)
	}

	cert, key, err := id.KeyPair()
	if err != nil {
		t.Fatalf("KeyPair on first use: %v", err)
	}
	if cert == nil || key == nil {
		t.Fatal("KeyPair returned no certificate or no key")
	}
	if len(filesUnder(t, home)) == 0 {
		t.Fatal("first use wrote nothing; a generated certificate must persist")
	}

	// A second identity over the same directory must find what the first wrote
	// rather than mint a second one, which is the whole point of persisting.
	again := as2.NewIdentity()
	if err := again.Configure(as2.IdentityConfig{}); err != nil {
		t.Fatalf("configure second: %v", err)
	}
	cert2, _, err := again.KeyPair()
	if err != nil {
		t.Fatalf("KeyPair second: %v", err)
	}
	if !cert.Equal(cert2) {
		t.Fatal("a restart minted a new certificate; the partner would have to re-import it")
	}
}

// TestConfigureReportsBadFilesImmediately pins the half that stays eager. A path
// that does not resolve is an operator's mistake, and they want to hear about it
// at startup rather than when the first message arrives.
func TestConfigureReportsBadFilesImmediately(t *testing.T) {
	sandboxHome(t)

	id := as2.NewIdentity()
	err := id.Configure(as2.IdentityConfig{
		CertFile: filepath.Join(t.TempDir(), "nope.pem"),
		KeyFile:  filepath.Join(t.TempDir(), "nope.key"),
	})
	if err == nil {
		t.Fatal("Configure accepted a certificate path that does not exist")
	}
}

// TestInMemoryWritesNothingEver is the mode for anyone who wants tommy to leave
// no files behind at all.
func TestInMemoryWritesNothingEver(t *testing.T) {
	home := sandboxHome(t)

	id := as2.NewIdentity()
	if err := id.Configure(as2.IdentityConfig{InMemory: true}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if _, _, err := id.KeyPair(); err != nil {
		t.Fatalf("KeyPair: %v", err)
	}
	if got := filesUnder(t, home); len(got) != 0 {
		t.Fatalf("in-memory identity wrote %v", got)
	}
}
