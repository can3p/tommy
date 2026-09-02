package e2e_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
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

// providerInfo mirrors the fields of core/plugin.ProviderInfo this file
// needs off GET /api/v1/plugins: which providers are actually running, and
// (for a ListenerProvider like ftp or sftp, whose own port is exactly what
// CLI-1 makes reachable from the command line) the address it bound.
type providerInfo struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

type pluginInfo struct {
	Name      string         `json:"name"`
	Providers []providerInfo `json:"providers"`
}

func fetchPlugins(t *testing.T, apiURL string) []pluginInfo {
	t.Helper()
	resp, err := http.Get(apiURL + "/plugins")
	if err != nil {
		t.Fatalf("get /plugins: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /plugins: %v", err)
	}
	var plugins []pluginInfo
	if err := json.Unmarshal(body, &plugins); err != nil {
		t.Fatalf("/plugins is not valid JSON: %v\n%s", err, body)
	}
	return plugins
}

// dialAndReadGreeting dials addr over TCP and returns whatever it wrote
// first, proving a listener provider's own port - the harder half of CLI-1 -
// is really reachable and not just present in the config.
func dialAndReadGreeting(t *testing.T, addr string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read greeting from %s: %v", addr, err)
	}
	return string(buf[:n])
}

// TestFilesPluginCLIMatchesConfigFile is the files half of CLI-1: files
// shipped with no subcommand before this task, so this both proves `tommy
// files` exists and behaves like `tommy serve --config` with an equivalent
// [plugins.files] section, and exercises the new --ftp-port/--sftp-port
// flags that make each listener provider's own port reachable from the
// command line - the gap CLI-1 was written to close. Every port is 0
// (ephemeral); ftp's and sftp's real default ports (2121, 2222) are never
// bound in a test - see CLAUDE.md's testing conventions.
func TestFilesPluginCLIMatchesConfigFile(t *testing.T) {
	cli := startTommyProcess(t, "files",
		"--ui-port", "0", "--api-port", "0", "--in-port", "0",
		"--ftp-port", "0",
		"--enabled-providers", "ftp")

	cfgPath := writeTempConfig(t, `
default_enabled = false

[ui]
port = 0
[api]
port = 0
[ingress]
port = 0

[plugins.files]
enabled = true

[plugins.files.providers.ftp]
enabled = true
port = 0
`)
	fromConfig := startTommyProcess(t, "serve", "--config", cfgPath)

	t.Run("cli", func(t *testing.T) { assertOnlyFTPRuns(t, cli) })
	t.Run("config-file", func(t *testing.T) { assertOnlyFTPRuns(t, fromConfig) })
}

// assertOnlyFTPRuns is run against both processes in
// TestFilesPluginCLIMatchesConfigFile and must see identical behavior from
// each: only the files plugin is running, only ftp among its providers, and
// that provider's own listener actually answers FTP on the address the API
// reports for it.
func assertOnlyFTPRuns(t *testing.T, p *tommyProcess) {
	t.Helper()

	plugins := fetchPlugins(t, p.APIURL)
	var files *pluginInfo
	for i := range plugins {
		if plugins[i].Name == "files" {
			files = &plugins[i]
		} else {
			t.Errorf("only the files plugin should be running, found %q too", plugins[i].Name)
		}
	}
	if files == nil {
		t.Fatal("the files plugin is not running")
	}
	if len(files.Providers) != 1 || files.Providers[0].Name != "ftp" {
		t.Fatalf("files providers = %+v, want only ftp", files.Providers)
	}
	addr := files.Providers[0].Addr
	if addr == "" {
		t.Fatal("ftp provider reported no bound address")
	}

	greeting := dialAndReadGreeting(t, addr)
	if !strings.HasPrefix(greeting, "220") {
		t.Errorf("ftp greeting at %s = %q, want a 220 banner", addr, greeting)
	}

	smsResp, err := http.Get(p.APIURL + "/sms/messages")
	if err != nil {
		t.Fatalf("sms probe: %v", err)
	}
	_ = smsResp.Body.Close()
	if smsResp.StatusCode != http.StatusNotFound {
		t.Errorf("sms plugin should not be running at all, got status %d", smsResp.StatusCode)
	}
}

// TestChatPluginCLIMatchesConfigFile is the chat half of CLI-1: chat shipped
// with no subcommand before this task either. It proves `tommy chat` builds
// the same config an equivalent TOML file would and, crucially, that it
// wires the same rich Block Kit renderer plugins/all/all.go installs -
// posting Block Kit blocks through the CLI shortcut must render exactly as
// it does through tommy serve, not fall back to the plain-text form a
// missing renderer would produce.
func TestChatPluginCLIMatchesConfigFile(t *testing.T) {
	cli := startTommyProcess(t, "chat", "--ui-port", "0", "--api-port", "0", "--in-port", "0",
		"--enabled-providers", "slack")

	cfgPath := writeTempConfig(t, `
default_enabled = false

[ui]
port = 0
[api]
port = 0
[ingress]
port = 0

[plugins.chat]
enabled = true

[plugins.chat.providers.slack]
enabled = true
`)
	fromConfig := startTommyProcess(t, "serve", "--config", cfgPath)

	t.Run("cli", func(t *testing.T) { assertOnlySlackRuns(t, cli) })
	t.Run("config-file", func(t *testing.T) { assertOnlySlackRuns(t, fromConfig) })
}

// assertOnlySlackRuns is run against both processes in
// TestChatPluginCLIMatchesConfigFile and must see identical behavior from
// each: slack's webhook works and renders its Block Kit blocks as markup,
// msteams's webhook is unreachable, and the mail plugin does not exist at
// all.
func assertOnlySlackRuns(t *testing.T, p *tommyProcess) {
	t.Helper()

	slackResp, err := http.Post(
		p.IngressURL+"/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX",
		"application/json",
		strings.NewReader(`{"channel":"#general","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"*It works.*"}}]}`))
	if err != nil {
		t.Fatalf("slack webhook: %v", err)
	}
	slackBody, _ := io.ReadAll(slackResp.Body)
	_ = slackResp.Body.Close()
	if slackResp.StatusCode != http.StatusOK || string(slackBody) != "ok" {
		t.Fatalf("slack webhook: status %d body %q", slackResp.StatusCode, slackBody)
	}

	msResp, err := http.Post(
		p.IngressURL+"/webhookb2/11111111-1111-1111-1111-111111111111@22222222-2222-2222-2222-222222222222/IncomingWebhook/33333333333333333333333333333333/44444444-4444-4444-4444-444444444444",
		"application/json", strings.NewReader(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("msteams probe: %v", err)
	}
	_ = msResp.Body.Close()
	if msResp.StatusCode != http.StatusNotFound {
		t.Errorf("msteams should be disabled, got status %d", msResp.StatusCode)
	}

	mailResp, err := http.Get(p.APIURL + "/mail/messages")
	if err != nil {
		t.Fatalf("mail probe: %v", err)
	}
	_ = mailResp.Body.Close()
	if mailResp.StatusCode != http.StatusNotFound {
		t.Errorf("mail plugin should not be running at all, got status %d", mailResp.StatusCode)
	}

	evResp, err := http.Get(p.APIURL + "/events?plugin=chat")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	evBody, _ := io.ReadAll(evResp.Body)
	_ = evResp.Body.Close()
	if !strings.Contains(string(evBody), `"provider":"slack"`) {
		t.Errorf("expected a slack event, got %s", evBody)
	}
	if strings.Contains(string(evBody), `"provider":"msteams"`) {
		t.Errorf("expected only slack events, got %s", evBody)
	}
}

// TestMailjetCredentialFlagsPinTheCheck is the HTTP-provider half of gap 1 in
// CLI-1's follow-up: --mailjet-api-key/--mailjet-secret-key must behave
// exactly like --smtp-username/--smtp-password - unset accepts anything, set
// it turns on Mailjet's real check, and a mismatch gets Mailjet's real 401
// (mj-0015). Run against the real binary so the CLI flag path is proven all
// the way through provider config, not just through singlePluginConfig.
func TestMailjetCredentialFlagsPinTheCheck(t *testing.T) {
	p := startTommyProcess(t, "mail",
		"--ui-port", "0", "--api-port", "0", "--in-port", "0",
		"--enabled-providers", "mailjet",
		"--mailjet-api-key", "right-key",
		"--mailjet-secret-key", "right-secret")

	send := func(user, pass string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, p.IngressURL+"/v3.1/send", strings.NewReader(
			`{"Messages":[{"From":{"Email":"a@example.com"},"To":[{"Email":"b@example.com"}],"Subject":"hi","TextPart":"it works"}]}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(user, pass)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		return resp
	}

	wrong := send("wrong-key", "wrong-secret")
	wrongBody, _ := io.ReadAll(wrong.Body)
	_ = wrong.Body.Close()
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Errorf("mismatched credentials: status %d body %s, want 401", wrong.StatusCode, wrongBody)
	}
	if !strings.Contains(string(wrongBody), "mj-0015") {
		t.Errorf("mismatched credentials body = %s, want Mailjet's real mj-0015 error", wrongBody)
	}

	right := send("right-key", "right-secret")
	rightBody, _ := io.ReadAll(right.Body)
	_ = right.Body.Close()
	if right.StatusCode != http.StatusOK {
		t.Errorf("matching credentials: status %d body %s, want 200", right.StatusCode, rightBody)
	}
}
