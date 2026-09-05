package sftp

import (
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/plugins/files"
)

// TestConformance is the gate every provider passes: real descriptions, at
// least one snippet that renders, and - since this one is a listener with no
// HTTP routes - no endpoints declared or mounted.
func TestConformance(t *testing.T) {
	plugintest.ConformanceProvider(t, New())
	plugintest.Conformance(t, files.New(New()))
}

func TestProviderIdentity(t *testing.T) {
	p := New()
	if p.Name() != ProviderName || p.Plugin() != files.PluginName {
		t.Errorf("identity = %s/%s, want files/sftp", p.Plugin(), p.Name())
	}
	if len(p.Endpoints()) != 0 {
		t.Errorf("Endpoints() = %+v, want none: this provider mounts no HTTP routes", p.Endpoints())
	}
	var (
		_ plugin.ListenerProvider    = p
		_ plugin.AddressableProvider = p
		_ files.VFSBinder            = p
	)
}

// TestBindVFSSharesTheTree pins the half of the contract that makes files
// uploaded over SFTP visible to every other surface.
func TestBindVFSSharesTheTree(t *testing.T) {
	p := New()
	plug := files.New(p)
	if p.tree() != plug.VFS() {
		t.Fatal("the provider is not writing into the plugin's shared VFS")
	}
}

// TestLoadConfig pins the difference between an absent port and port 0: the
// first means the conventional 2222, the second means "pick one for me", which
// is what makes parallel test runs possible.
func TestLoadConfig(t *testing.T) {
	empty := LoadConfig(config.NewProviderConfig(nil))
	if empty.Port != DefaultPort {
		t.Errorf("absent port = %d, want %d", empty.Port, DefaultPort)
	}
	if empty.Bind != DefaultBind || empty.ServerVersion != DefaultServerVersion {
		t.Errorf("defaults not applied: %+v", empty)
	}
	if empty.HostKeyPath == "" || !strings.HasSuffix(empty.HostKeyPath, DefaultHostKeyName) {
		t.Errorf("default host_key_path = %q, want one ending in %q", empty.HostKeyPath, DefaultHostKeyName)
	}
	if empty.HandshakeTimeout != DefaultHandshakeTimeout || empty.IdleTimeout != DefaultIdleTimeout {
		t.Errorf("timeouts = %v / %v", empty.HandshakeTimeout, empty.IdleTimeout)
	}
	if empty.MaxConnections != DefaultMaxConnections || empty.MaxAuthTries != DefaultMaxAuthTries {
		t.Errorf("limits = %d / %d", empty.MaxConnections, empty.MaxAuthTries)
	}
	if empty.RequiresAuth() {
		t.Error("RequiresAuth() is true with no credentials configured")
	}

	set := LoadConfig(config.NewProviderConfig(map[string]any{
		"port":              0,
		"bind":              "0.0.0.0",
		"host_key_path":     "/tmp/tommy/key",
		"authorized_keys":   "/tmp/tommy/authorized_keys",
		"username":          "any",
		"password":          "s3cret",
		"server_version":    "SSH-2.0-test",
		"handshake_timeout": 5,
		"idle_timeout":      7,
		"max_connections":   3,
		"max_auth_tries":    2,
	}))
	if set.Port != 0 || set.Bind != "0.0.0.0" || set.ListenAddr() != "0.0.0.0:0" {
		t.Errorf("config = %+v", set)
	}
	if set.HostKeyPath != "/tmp/tommy/key" || set.AuthorizedKeysPath != "/tmp/tommy/authorized_keys" {
		t.Errorf("paths = %q / %q", set.HostKeyPath, set.AuthorizedKeysPath)
	}
	if set.HandshakeTimeout != 5*time.Second || set.IdleTimeout != 7*time.Second {
		t.Errorf("timeouts = %v / %v", set.HandshakeTimeout, set.IdleTimeout)
	}
	if set.MaxConnections != 3 || set.MaxAuthTries != 2 || set.ServerVersion != "SSH-2.0-test" {
		t.Errorf("config = %+v", set)
	}
	if !set.RequiresAuth() {
		t.Error("RequiresAuth() is false even though credentials are pinned")
	}
}

// TestSnippetsCarryTheLivePort is the anti-regression for the failure mode
// snippets exist to avoid: a copied command that names a port nothing is
// listening on.
func TestSnippetsCarryTheLivePort(t *testing.T) {
	ctx := plugintest.SnippetCtx()
	ctx.SetAddr(files.PluginName, ProviderName, "localhost:31337")

	rendered, err := plugin.RenderSnippets(New().Snippets(), ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(rendered) == 0 {
		t.Fatal("no snippets")
	}
	var sawPort bool
	for _, s := range rendered {
		if strings.Contains(s.Code, "31337") {
			sawPort = true
		}
		if strings.Contains(s.Code, "2222") {
			t.Errorf("snippet %q hardcodes the default port:\n%s", s.Title, s.Code)
		}
	}
	if !sawPort {
		t.Errorf("no snippet used the live port: %+v", rendered)
	}

	// With nothing bound the snippets still have to render to something a
	// person can run - but the port comes from the provider's own report
	// (plugin.PortProvider), which is what the core fills the context with
	// when no listener has bound, rather than from a literal in the snippet.
	cfg := plugin.NewSnippetCtx("localhost", "127.0.0.1:8811", "127.0.0.1:8811", "127.0.0.1:8822")
	lp := New().ListenPort(plugin.ProviderConfig{})
	if lp.Port != DefaultPort {
		t.Fatalf("ListenPort() with no configuration = %d, want the package default %d", lp.Port, DefaultPort)
	}
	cfg.SetAddr(files.PluginName, ProviderName, net.JoinHostPort("localhost", strconv.Itoa(lp.Port)))
	cold, err := plugin.RenderSnippets(New().Snippets(), cfg)
	if err != nil {
		t.Fatalf("render with no listener: %v", err)
	}
	if !strings.Contains(cold[0].Code, "2222") {
		t.Errorf("with no listener bound the snippet should carry the reported port %d:\n%s", DefaultPort, cold[0].Code)
	}
}
