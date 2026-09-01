package e2e_test

import (
	"bufio"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// tommyProcess is a running tommy subprocess, with the listener URLs parsed
// out of the startup banner cmd/serve.go and cmd/mail.go both print.
type tommyProcess struct {
	cmd        *exec.Cmd
	UIURL      string
	APIURL     string
	IngressURL string
}

var urlLineRE = regexp.MustCompile(`^(ui|api|ingress)\s+(\S+)$`)

// startTommyProcess execs the real tommy binary with args, waits for it to
// print its three listener URLs and for the API to answer, and registers
// cleanup that stops it gracefully. Running the actual compiled CLI, rather
// than calling into cmd's unexported helpers, is what proves the CLI-flags
// path and the config-file path share one bootstrap: both go through
// exactly the code a user would run.
func startTommyProcess(t *testing.T, args ...string) *tommyProcess {
	t.Helper()

	cmd := exec.Command(tommyBinPath, args...)
	cmd.Env = append(os.Environ(), "TOMMY_NO_UPDATE_CHECK=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s %v: %v", tommyBinPath, args, err)
	}
	p := &tommyProcess{cmd: cmd}
	t.Cleanup(func() { p.stop(t) })

	lines := make(chan string)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()

	urls := map[string]string{}
	timeout := time.After(5 * time.Second)
	for len(urls) < 3 {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("%s %v exited before printing its listener URLs (got %v)", tommyBinPath, args, urls)
			}
			if m := urlLineRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				urls[m[1]] = m[2]
			}
		case <-timeout:
			t.Fatalf("%s %v did not print its listener URLs in time (got %v)", tommyBinPath, args, urls)
		}
	}
	// Keep draining stdout so the child is never blocked writing further
	// lines (the plugin summary and the "run tommy providers" hint follow).
	go func() {
		for line := range lines {
			_ = line
		}
	}()

	p.UIURL, p.APIURL, p.IngressURL = urls["ui"], urls["api"], urls["ingress"]
	waitAPIHealthy(t, p.APIURL)
	return p
}

func (p *tommyProcess) stop(t *testing.T) {
	if p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Logf("tommy process did not exit after SIGTERM, killing")
		_ = p.cmd.Process.Kill()
		<-done
	}
}

func waitAPIHealthy(t *testing.T, apiURL string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(apiURL + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("tommy api at %s never became healthy: %v", apiURL, lastErr)
}

func writeTempConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tommy.toml")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

// TestSinglePluginCLIMatchesConfigFile is the CLI-vs-TOML half of I1's brief:
// `tommy mail --enabled-providers mailjet` and `tommy serve --config` with an
// equivalent [plugins.mail] section must behave identically, because both
// build the same config.Config and run the same core/server bootstrap. It
// execs the real binary both ways and asserts on the externally observable
// behavior: mailjet works, sendgrid is unreachable, the sms plugin does not
// exist at all, and the event store agrees.
func TestSinglePluginCLIMatchesConfigFile(t *testing.T) {
	cli := startTommyProcess(t, "mail", "--ui-port", "0", "--api-port", "0", "--in-port", "0", "--enabled-providers", "mailjet")

	cfgPath := writeTempConfig(t, `
default_enabled = false

[ui]
port = 0
[api]
port = 0
[ingress]
port = 0

[plugins.mail]
enabled = true

[plugins.mail.providers.mailjet]
enabled = true
`)
	fromConfig := startTommyProcess(t, "serve", "--config", cfgPath)

	t.Run("cli", func(t *testing.T) { assertOnlyMailjetRuns(t, cli) })
	t.Run("config-file", func(t *testing.T) { assertOnlyMailjetRuns(t, fromConfig) })
}

// assertOnlyMailjetRuns is run against both processes in
// TestSinglePluginCLIMatchesConfigFile and must see identical behavior from
// each.
func assertOnlyMailjetRuns(t *testing.T, p *tommyProcess) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, p.IngressURL+"/v3.1/send", strings.NewReader(
		`{"Messages":[{"From":{"Email":"a@example.com"},"To":[{"Email":"b@example.com"}],"Subject":"hi","TextPart":"it works"}]}`))
	if err != nil {
		t.Fatalf("build mailjet request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("any", "any")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mailjet send: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mailjet send: status %d body %s", resp.StatusCode, body)
	}

	sgResp, err := http.Post(p.IngressURL+"/v3/mail/send", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("sendgrid probe: %v", err)
	}
	_ = sgResp.Body.Close()
	if sgResp.StatusCode != http.StatusNotFound {
		t.Errorf("sendgrid should be disabled, got status %d", sgResp.StatusCode)
	}

	smsResp, err := http.Get(p.APIURL + "/sms/messages")
	if err != nil {
		t.Fatalf("sms probe: %v", err)
	}
	_ = smsResp.Body.Close()
	if smsResp.StatusCode != http.StatusNotFound {
		t.Errorf("sms plugin should not be running at all, got status %d", smsResp.StatusCode)
	}

	evResp, err := http.Get(p.APIURL + "/events?plugin=mail")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	evBody, _ := io.ReadAll(evResp.Body)
	_ = evResp.Body.Close()
	if !strings.Contains(string(evBody), `"provider":"mailjet"`) {
		t.Errorf("expected a mailjet event, got %s", evBody)
	}
	if strings.Contains(string(evBody), `"provider":"sendgrid"`) || strings.Contains(string(evBody), `"provider":"smtp"`) {
		t.Errorf("expected only mailjet events, got %s", evBody)
	}
}
