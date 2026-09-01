package sftp

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/plugins/files"
)

// TestSftpSnippetActuallyWorks runs the copy-paste snippet with the real
// OpenSSH client against a live listener. A snippet nobody has run is
// documentation; one the tests run is a promise - and it is the only check that
// the handshake, the "none" authentication that keeps the command free of a
// password prompt, and the subsystem request all work for the client people
// actually have.
func TestSftpSnippetActuallyWorks(t *testing.T) {
	if _, err := exec.LookPath("sftp"); err != nil {
		t.Skip("the openssh sftp client is not installed")
	}
	inst, _, addr := startSFTP(t, nil)

	code := renderSnippet(t, "Upload a file with the sftp client", addr)
	runIn(t, t.TempDir(), code)

	inst.WaitForEvents(2, store.Query{Plugin: files.PluginName}, 10*time.Second)
	status, body := inst.GetBody(inst.API("/files/content/upload/local.txt"))
	if status != 200 || strings.TrimSpace(body) != "it works" {
		t.Errorf("GET the file the snippet uploaded: status %d body %q", status, body)
	}
}

// TestScpSnippetActuallyWorks covers the other client people reach for. Modern
// OpenSSH scp speaks SFTP under the hood, so this is the same server path
// driven by a different binary.
func TestScpSnippetActuallyWorks(t *testing.T) {
	if _, err := exec.LookPath("scp"); err != nil {
		t.Skip("the openssh scp client is not installed")
	}
	inst, _, addr := startSFTP(t, nil)

	dir := t.TempDir()
	if err := os.WriteFile(dir+"/local.txt", []byte("it works\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	runIn(t, dir, renderSnippet(t, "Upload with scp (OpenSSH 9 speaks SFTP under the hood)", addr))

	inst.WaitForEvents(1, store.Query{Plugin: files.PluginName, Type: files.EventUpload}, 10*time.Second)
	status, body := inst.GetBody(inst.API("/files/content/local.txt"))
	if status != 200 || strings.TrimSpace(body) != "it works" {
		t.Errorf("GET the file scp uploaded: status %d body %q", status, body)
	}
}

// renderSnippet renders one snippet by title against a live listener address.
func renderSnippet(t *testing.T, title, addr string) string {
	t.Helper()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	ctx := plugin.SnippetCtx{Host: host}
	ctx.SetAddr(files.PluginName, ProviderName, addr)

	for _, s := range New().Snippets() {
		if s.Title != title {
			continue
		}
		code, err := s.Render(ctx)
		if err != nil {
			t.Fatalf("render %q: %v", title, err)
		}
		return code
	}
	t.Fatalf("no snippet titled %q", title)
	return ""
}

// runIn executes a bash snippet in its own directory, so the files it creates
// never land in the source tree.
func runIn(t *testing.T, dir, code string) {
	t.Helper()
	cmd := exec.Command("bash", "-c", code)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	done := make(chan struct{})
	timer := time.AfterFunc(30*time.Second, func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	defer func() {
		timer.Stop()
		close(done)
	}()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("snippet failed: %v\n%s\n--- snippet ---\n%s", err, out, code)
	}
}
