package http_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/plugins/as2"
	as2http "github.com/can3p/tommy/plugins/as2/providers/http"
)

// TestSnippetsActuallyRun renders this provider's snippets against a live
// instance and executes them.
//
// Snippets are the product here - they are what the how-to-test panel, the 404
// body and `tommy providers` all hand a newcomer - and a snippet that looks
// right is worth nothing. Conformance already proves they parse and render;
// this proves they work, which is a different claim and the one that matters.
//
// It has already earned its place: the first version of the second snippet was
// adapted from the plugin README's example, which pipes `openssl cms -encrypt
// -outform SMIME` through `tail -n +2`. That strips MIME-Version and leaves
// Content-Disposition, Content-Type and Content-Transfer-Encoding sitting
// inside the request body, where they are not base64 - so the receiver reports
// "illegal base64 data at input byte 7" and never decrypts anything. Running
// the snippet is the only thing that finds that.
func TestSnippetsActuallyRun(t *testing.T) {
	requireCurl(t)
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}

	prov := as2http.New()
	in := start(t, nil)
	ctx := in.Server.SnippetCtx()

	snippets := prov.Snippets()
	if len(snippets) < 2 {
		t.Fatalf("got %d snippets, want the cold-start one and the OpenSSL exchange", len(snippets))
	}

	wants := [][]string{
		// The cold start: no keys, no setup, a real unsigned MDN back.
		{"HTTP/1.1 200 OK", "Original-Message-ID: <1@partner.example>", "Disposition: automatic-action"},
		// The full exchange, ending in OpenSSL's own verdict on the receipt.
		{"Verification successful", "Received-Content-MIC:", "processed"},
	}

	for i, s := range snippets {
		t.Run(s.Title, func(t *testing.T) {
			code, err := s.Render(ctx)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			out := runShell(t, code)
			for _, want := range wants[i] {
				if !strings.Contains(out, want) {
					t.Errorf("snippet output is missing %q:\n%s", want, out)
				}
			}
		})
	}

	// Both snippets posted a message, and both were captured.
	if evs := in.WaitForEvents(2, store.Query{Plugin: as2.Name}, 5*time.Second); len(evs) != 2 {
		t.Errorf("the snippets produced %d captured messages, want 2", len(evs))
	}
}

// runShell runs a snippet the way a person would: /bin/sh, in a scratch
// directory, with nothing carried over between snippets.
//
// The one accommodation is PATH. The snippets say `openssl`, and on macOS that
// resolves to the system LibreSSL rather than to a build the test suite has
// checked, so the OpenSSL these tests found is put in front of it. A user
// running the snippet by hand uses whatever their `openssl` is, which is the
// right thing for a snippet to assume.
func runShell(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "snippet.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write snippet: %v", err)
	}
	cmd := exec.Command("/bin/sh", path)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(opensslBin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("snippet exited %v:\n%s", err, out.String())
	}
	return out.String()
}

// TestREADMECarriesTheSnippets keeps the two copies in step.
//
// The checklist asks for the snippets to appear in the README, and a README
// that has drifted from the code is worse than one that never had them: the
// version somebody reads is the one that no longer works. Rendering against a
// fixed localhost:8822 is what the README is written for; the live surfaces
// render against the real ports.
func TestREADMECarriesTheSnippets(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	ctx := plugin.NewSnippetCtx("localhost", "localhost:8811", "localhost:8811", "localhost:8822")
	for _, s := range as2http.New().Snippets() {
		code, err := s.Render(ctx)
		if err != nil {
			t.Fatalf("render %q: %v", s.Title, err)
		}
		if !strings.Contains(string(readme), code) {
			t.Errorf("README.md does not carry the %q snippet as rendered:\n%s", s.Title, code)
		}
	}
}
