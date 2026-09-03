package server

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
)

func TestConfigDir(t *testing.T) {
	abs, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}

	for _, tc := range []struct {
		name, source, want string
	}{
		{"in-memory config has none", "", ""},
		{"absolute path", "/etc/tommy/tommy.toml", "/etc/tommy"},
		{"relative path", "conf/tommy.toml", "conf"},
		{"bare filename resolves to the working directory", "tommy.toml", abs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := configDir(tc.source); got != tc.want {
				t.Fatalf("configDir(%q) = %q, want %q", tc.source, got, tc.want)
			}
		})
	}
}

// TestDepsCarryConfigDir is the reason the field exists: a provider that
// generates something it wants back on the next run - an AS2 certificate, the
// planned --tls one - can only put it beside the config if it is told where
// the config was.
func TestDepsCarryConfigDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tommy.toml")
	if err := os.WriteFile(path, []byte("[ui]\nport = 0\n[ingress]\nport = 0\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.ApplyDefaults()

	spy := &depsSpy{}
	srv, err := New(Options{Config: cfg, Plugins: []plugin.Plugin{&spyPlugin{prov: spy}}})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })

	if spy.seen.ConfigDir != dir {
		t.Fatalf("provider saw ConfigDir %q, want %q", spy.seen.ConfigDir, dir)
	}
}

// TestDepsConfigDirEmptyForInMemoryConfig pins the other half: every CLI
// shortcut and every test builds its config in memory, and those must not be
// handed a directory that looks meaningful.
func TestDepsConfigDirEmptyForInMemoryConfig(t *testing.T) {
	cfg := config.Ephemeral()
	cfg.ApplyDefaults()

	spy := &depsSpy{}
	srv, err := New(Options{Config: cfg, Plugins: []plugin.Plugin{&spyPlugin{prov: spy}}})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })

	if spy.seen.ConfigDir != "" {
		t.Fatalf("provider saw ConfigDir %q, want empty", spy.seen.ConfigDir)
	}
}

// spyPlugin is the smallest plugin the registry accepts, carrying one provider
// that records the Deps the core handed it.
type spyPlugin struct{ prov *depsSpy }

func (p *spyPlugin) Name() string  { return "spy" }
func (p *spyPlugin) Title() string { return "Spy" }
func (p *spyPlugin) Description() string {
	return "A test-only plugin that records the dependencies the core hands its provider at registration time."
}
func (p *spyPlugin) Providers() []plugin.Provider        { return []plugin.Provider{p.prov} }
func (p *spyPlugin) Templates() fs.FS                    { return nil }
func (p *spyPlugin) RegisterAPI(plugin.Mux, plugin.Deps) {}
func (p *spyPlugin) RegisterUI(plugin.Mux, plugin.Deps)  {}

type depsSpy struct{ seen plugin.Deps }

func (d *depsSpy) Name() string   { return "spy" }
func (d *depsSpy) Plugin() string { return "spy" }
func (d *depsSpy) Description() string {
	return "Records the Deps it was registered with, so a test can assert on what the core hands a provider."
}

func (d *depsSpy) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{{
		Method:      "POST",
		Path:        "/spy",
		Description: "Accepts anything and records nothing; it exists so the provider has a route to declare.",
	}}
}

func (d *depsSpy) Snippets() []plugin.Snippet {
	return []plugin.Snippet{{
		Title: "Poke the spy",
		Lang:  "bash",
		Code:  `curl -X POST {{.IngressURL}}/spy`,
	}}
}

func (d *depsSpy) RegisterIngress(mux plugin.Mux, deps plugin.Deps) {
	d.seen = deps
	mux.HandleFunc("POST /spy", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}
