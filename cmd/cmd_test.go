package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/can3p/tommy/plugins/all"
)

func resetFlags(t *testing.T) {
	t.Helper()
	before := serveFlags
	t.Cleanup(func() { serveFlags = before })
	serveFlags.configPath = ""
	serveFlags.uiPort = -1
	serveFlags.apiPort = -1
	serveFlags.ingressPort = -1
	serveFlags.bind = ""
	serveFlags.host = ""
	serveFlags.logLevel = "info"
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tommy.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	resetFlags(t)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if *cfg.UI.Port != 8811 || *cfg.Ingress.Port != 8822 {
		t.Errorf("ports = %d/%d", *cfg.UI.Port, *cfg.Ingress.Port)
	}
	if !cfg.APISharesUIListener() {
		t.Error("the API should share the UI listener by default")
	}
}

func TestLoadConfigFromTOML(t *testing.T) {
	resetFlags(t)
	serveFlags.configPath = writeConfig(t, "[ui]\nport = 9101\n[ingress]\nport = 9102\n[storage]\ncapacity = 7\n")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if *cfg.UI.Port != 9101 || *cfg.Ingress.Port != 9102 || cfg.Storage.Capacity != 7 {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.Source == "" {
		t.Error("the config should remember where it came from")
	}
}

func TestFlagsOverrideTheConfigFile(t *testing.T) {
	resetFlags(t)
	serveFlags.configPath = writeConfig(t, "[ui]\nport = 9101\n[ingress]\nport = 9102\n")
	serveFlags.uiPort = 7000
	serveFlags.ingressPort = 7001
	serveFlags.host = "tommy.test"

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if *cfg.UI.Port != 7000 || *cfg.Ingress.Port != 7001 {
		t.Errorf("flags did not win: %d/%d", *cfg.UI.Port, *cfg.Ingress.Port)
	}
	if cfg.Host != "tommy.test" {
		t.Errorf("host = %q", cfg.Host)
	}
}

func TestLoadConfigReportsBadInput(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		resetFlags(t)
		serveFlags.configPath = filepath.Join(t.TempDir(), "nope.toml")
		if _, err := loadConfig(); err == nil {
			t.Fatal("expected an error for a missing config file")
		}
	})

	t.Run("invalid config", func(t *testing.T) {
		resetFlags(t)
		serveFlags.configPath = writeConfig(t, "[ui]\nport = 70000\n")
		_, err := loadConfig()
		if err == nil || !strings.Contains(err.Error(), "out of range") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("port collision from flags", func(t *testing.T) {
		resetFlags(t)
		serveFlags.uiPort = 9000
		serveFlags.apiPort = 9001
		serveFlags.ingressPort = 9001
		if _, err := loadConfig(); err == nil {
			t.Fatal("expected a port collision to be reported")
		}
	})
}

func TestNewLogger(t *testing.T) {
	resetFlags(t)
	if _, err := newLogger("debug"); err != nil {
		t.Errorf("debug: %v", err)
	}
	if _, err := newLogger("shout"); err == nil {
		t.Error("an unknown level must be rejected with a helpful message")
	}
}

// The command describes whatever this binary was compiled with, so the test
// asserts the shape of the output rather than a fixed roster that changes every
// time a plugin is added.
func TestProvidersCommandListsCompiledPlugins(t *testing.T) {
	resetFlags(t)
	var out bytes.Buffer
	providersCmd.SetOut(&out)
	t.Cleanup(func() { providersCmd.SetOut(nil) })

	if err := providersCmd.RunE(providersCmd, nil); err != nil {
		t.Fatalf("providers: %v", err)
	}

	got := out.String()
	if len(all.Plugins()) == 0 {
		if !strings.Contains(got, "No plugins are enabled") {
			t.Errorf("with nothing compiled in, output = %q", got)
		}
		return
	}
	for _, p := range all.Plugins() {
		if !strings.Contains(got, p.Name()) {
			t.Errorf("output does not mention plugin %q: %q", p.Name(), got)
		}
		if !strings.Contains(got, p.Description()) {
			t.Errorf("output does not carry %q's description, which is the point of the command", p.Name())
		}
	}
}

func TestProvidersCommandJSON(t *testing.T) {
	resetFlags(t)
	var out bytes.Buffer
	providersCmd.SetOut(&out)
	providersFlags.asJSON = true
	t.Cleanup(func() {
		providersCmd.SetOut(nil)
		providersFlags.asJSON = false
	})

	if err := providersCmd.RunE(providersCmd, nil); err != nil {
		t.Fatalf("providers: %v", err)
	}
	var got []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if len(got) != len(all.Plugins()) {
		t.Fatalf("described %d plugins, want the %d compiled in", len(got), len(all.Plugins()))
	}
	for _, p := range got {
		if p.Name == "" || p.Description == "" {
			t.Errorf("plugin %+v is missing a name or description", p)
		}
	}
}

func TestProvidersCommandUnknownName(t *testing.T) {
	resetFlags(t)
	providersCmd.SetOut(&bytes.Buffer{})
	t.Cleanup(func() { providersCmd.SetOut(nil) })

	err := providersCmd.RunE(providersCmd, []string{"nosuchplugin"})
	if err == nil || !strings.Contains(err.Error(), "nosuchplugin") {
		t.Fatalf("err = %v", err)
	}
}

func TestCommandsAreRegistered(t *testing.T) {
	want := map[string]bool{
		"serve": false, "providers": false,
		"mail": false, "sms": false, "files": false, "chat": false,
	}
	for _, c := range rootCmd.Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("command %q is not registered on the root command", name)
		}
	}
}

// runProviders runs the command the way a user would and returns what it
// printed. It starts nothing: `tommy providers` must never bind a listener,
// which is also why this test can name well-known ports safely.
func runProviders(t *testing.T, asJSON bool, configPath string, args ...string) string {
	t.Helper()
	resetFlags(t)
	serveFlags.configPath = configPath
	var out bytes.Buffer
	providersCmd.SetOut(&out)
	providersFlags.asJSON = asJSON
	t.Cleanup(func() {
		providersCmd.SetOut(nil)
		providersFlags.asJSON = false
	})
	if err := providersCmd.RunE(providersCmd, args); err != nil {
		t.Fatalf("providers: %v", err)
	}
	return out.String()
}

type listedProvider struct {
	Name     string `json:"name"`
	Listener bool   `json:"listener"`
	Addr     string `json:"addr"`
	Port     int    `json:"port"`
	Network  string `json:"network"`
}

type listedPlugin struct {
	Name      string           `json:"name"`
	Providers []listedProvider `json:"providers"`
}

func listedProviders(t *testing.T, body string) []listedPlugin {
	t.Helper()
	var got []listedPlugin
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, body)
	}
	return got
}

// TestProvidersReportsListenerPorts is the reason ListenPort exists: with no
// configuration at all, the listing has to say which ports a default tommy
// would bind. It used to report none, because it only knew a port the config
// named explicitly, and every default lived in a package constant instead.
func TestProvidersReportsListenerPorts(t *testing.T) {
	var listeners int
	for _, p := range listedProviders(t, runProviders(t, true, "")) {
		for _, prov := range p.Providers {
			if !prov.Listener {
				if prov.Port != 0 {
					t.Errorf("%s/%s is path-routed onto the shared ingress but reports port %d", p.Name, prov.Name, prov.Port)
				}
				continue
			}
			listeners++
			if prov.Port == 0 {
				t.Errorf("%s/%s reports no port; the default lives in a constant nothing can read", p.Name, prov.Name)
			}
			if prov.Network != "tcp" && prov.Network != "udp" {
				t.Errorf("%s/%s reports network %q", p.Name, prov.Name, prov.Network)
			}
			if prov.Addr == "" {
				t.Errorf("%s/%s reports no address for a snippet to render against", p.Name, prov.Name)
			}
		}
	}
	if listeners == 0 {
		t.Skip("this build has no listener providers compiled in")
	}
}

// TestProvidersHonoursAConfiguredPort proves the listing reports the resolved
// value rather than the constant, in both directions.
func TestProvidersHonoursAConfiguredPort(t *testing.T) {
	if !hasProvider(t, "mail", "smtp") {
		t.Skip("this build has no mail/smtp provider")
	}
	path := writeConfig(t, "[plugins.mail.providers.smtp]\nport = 9999\n")

	var found bool
	for _, p := range listedProviders(t, runProviders(t, true, path, "mail/smtp")) {
		for _, prov := range p.Providers {
			found = true
			if prov.Port != 9999 {
				t.Errorf("mail/smtp reports port %d, want the configured 9999", prov.Port)
			}
			if !strings.HasSuffix(prov.Addr, ":9999") {
				t.Errorf("mail/smtp reports addr %q, want the configured port", prov.Addr)
			}
		}
	}
	if !found {
		t.Fatal("mail/smtp was not listed")
	}

	// port = 0 is the ephemeral case: nothing can know it without binding, and
	// the listing must not invent the package default.
	ephemeral := writeConfig(t, "[plugins.mail.providers.smtp]\nport = 0\n")
	for _, p := range listedProviders(t, runProviders(t, true, ephemeral, "mail/smtp")) {
		for _, prov := range p.Providers {
			if prov.Port != 0 || prov.Addr != "" {
				t.Errorf("mail/smtp reports %+v for an ephemeral port; only a bound listener knows it", prov)
			}
		}
	}
}

// TestProvidersPrintsListenerPortsForHumans: the human form is what most people
// read, and it printed no port at all for a default run.
func TestProvidersPrintsListenerPortsForHumans(t *testing.T) {
	if !hasProvider(t, "mail", "smtp") {
		t.Skip("this build has no mail/smtp provider")
	}
	got := runProviders(t, false, "", "mail/smtp")
	if !strings.Contains(got, "own tcp listener on localhost:1025") {
		t.Errorf("output does not say where the listener would bind:\n%s", got)
	}
}

func hasProvider(t *testing.T, plugin, provider string) bool {
	t.Helper()
	for _, p := range all.Plugins() {
		if p.Name() != plugin {
			continue
		}
		for _, prov := range p.Providers() {
			if prov.Name() == provider {
				return true
			}
		}
	}
	return false
}
