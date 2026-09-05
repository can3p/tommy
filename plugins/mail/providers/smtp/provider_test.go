package smtp

import (
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/plugins/mail"
)

// TestConformance is the gate every provider passes: real descriptions, at
// least one snippet that renders, and - since this one is a listener with no
// HTTP routes - no endpoints declared or mounted.
func TestConformance(t *testing.T) {
	plugintest.ConformanceProvider(t, New())
	plugintest.Conformance(t, mail.New(New()))
}

func TestProviderIdentity(t *testing.T) {
	p := New()
	if p.Name() != "smtp" || p.Plugin() != mail.PluginName {
		t.Errorf("identity = %s/%s", p.Plugin(), p.Name())
	}
	if len(p.Endpoints()) != 0 {
		t.Errorf("Endpoints() = %+v, want none: this provider mounts no HTTP routes", p.Endpoints())
	}
	var _ plugin.ListenerProvider = p
}

// TestLoadConfig pins the difference between an absent port and port 0: the
// first means "the conventional 1025", the second means "pick one for me",
// which is what makes parallel test runs possible.
func TestLoadConfig(t *testing.T) {
	empty := LoadConfig(config.NewProviderConfig(nil))
	if empty.Port != DefaultPort {
		t.Errorf("absent port = %d, want %d", empty.Port, DefaultPort)
	}
	if empty.Bind != DefaultBind || empty.MaxMessageBytes != DefaultMaxMessageBytes ||
		empty.MaxRecipients != DefaultMaxRecipients || empty.MaxLineLength != DefaultMaxLineLength {
		t.Errorf("defaults not applied: %+v", empty)
	}
	if empty.RequiresAuth() {
		t.Error("RequiresAuth() is true with no credentials configured")
	}

	set := LoadConfig(config.NewProviderConfig(map[string]any{
		"port":              0,
		"bind":              "0.0.0.0",
		"domain":            "mail.test",
		"max_message_bytes": 4096,
		"max_recipients":    3,
		"max_line_length":   900,
		"read_timeout":      5,
		"write_timeout":     7,
		"username":          "u",
	}))
	if set.Port != 0 || set.Bind != "0.0.0.0" || set.Domain != "mail.test" {
		t.Errorf("config = %+v", set)
	}
	if set.MaxMessageBytes != 4096 || set.MaxRecipients != 3 || set.MaxLineLength != 900 {
		t.Errorf("limits = %+v", set)
	}
	if set.ReadTimeout != 5*time.Second || set.WriteTimeout != 7*time.Second {
		t.Errorf("timeouts = %v / %v", set.ReadTimeout, set.WriteTimeout)
	}
	if !set.RequiresAuth() {
		t.Error("RequiresAuth() is false even though a username is pinned")
	}
	if set.ListenAddr() != "0.0.0.0:0" {
		t.Errorf("ListenAddr() = %q", set.ListenAddr())
	}
}

// TestSnippetsCarryTheLiveAddress proves a copied snippet points at the port
// this instance actually bound, and - when nothing is bound - at the port this
// provider reports it would bind, which is what the core fills the context
// with. Nothing in the snippet names a port of its own.
func TestSnippetsCarryTheLiveAddress(t *testing.T) {
	live := plugin.SnippetCtx{Host: "example.test"}
	live.SetAddr(mail.PluginName, ProviderName, "example.test:2500")

	for _, s := range New().Snippets() {
		out, err := s.Render(live)
		if err != nil {
			t.Fatalf("render %q: %v", s.Title, err)
		}
		if !strings.Contains(out, "2500") {
			t.Errorf("snippet %q does not carry the live port:\n%s", s.Title, out)
		}
		if strings.Contains(out, "1025") {
			t.Errorf("snippet %q used the default port even though a live one was known:\n%s", s.Title, out)
		}
	}

	// With no listener running the core asks the provider where it would
	// bind (plugin.PortProvider) and publishes that, so the snippet still
	// carries a usable address without a literal port in its template.
	lp := New().ListenPort(plugin.ProviderConfig{})
	if lp.Port != DefaultPort || lp.Network != "tcp" {
		t.Fatalf("ListenPort() with no configuration = %+v, want port %d over tcp", lp, DefaultPort)
	}
	cold := plugin.SnippetCtx{Host: "localhost"}
	cold.SetAddr(mail.PluginName, ProviderName, net.JoinHostPort("localhost", strconv.Itoa(lp.Port)))
	for _, s := range New().Snippets() {
		out, err := s.Render(cold)
		if err != nil {
			t.Fatalf("render %q: %v", s.Title, err)
		}
		if !strings.Contains(out, "1025") {
			t.Errorf("snippet %q has no address at all when nothing is bound:\n%s", s.Title, out)
		}
	}
}

// TestCurlSnippetActuallyWorks executes the bash snippet against a live
// listener. A snippet nobody has run is documentation; one the tests run is a
// promise.
func TestCurlSnippetActuallyWorks(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	inst, addr := startListener(t, nil)

	code := renderSnippet(t, "Send a message with curl", addr)
	run(t, "bash", []string{"-c", code})

	events := inst.WaitForEvents(1, store.Query{Plugin: mail.PluginName}, 10*time.Second)
	msg, _ := mail.MessageOf(events[0])
	if msg.Subject != "Hello from tommy" {
		t.Errorf("subject = %q, want the one the snippet sends", msg.Subject)
	}
}

// TestPythonSnippetActuallyWorks does the same for the multipart snippet, which
// is the one that exercises MIME end to end.
func TestPythonSnippetActuallyWorks(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	inst, addr := startListener(t, nil)

	// socket.getfqdn() is what smtplib uses for its EHLO name, and on a machine
	// whose reverse DNS is slow it can take half a minute. That is the test
	// host's problem, not the snippet's, so it is stubbed out here rather than
	// hardcoded into the snippet everyone copies.
	code := renderSnippet(t, "Send a multipart message with Python", addr)
	preamble := "import socket\nsocket.getfqdn = lambda *a: 'snippet.test'\n"
	run(t, python, []string{"-c", preamble + code})

	events := inst.WaitForEvents(1, store.Query{Plugin: mail.PluginName}, 10*time.Second)
	msg, _ := mail.MessageOf(events[0])
	if msg.Text == "" || msg.HTML == "" {
		t.Errorf("both bodies should have been parsed: text %q html %q", msg.Text, msg.HTML)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].Filename != "invoice.csv" {
		t.Errorf("attachments = %+v, want invoice.csv", msg.Attachments)
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
	ctx.SetAddr(mail.PluginName, ProviderName, addr)

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

func run(t *testing.T, name string, args []string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", name, err, out)
	}
}
