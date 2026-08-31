package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestProvidersCommandWithNoPlugins(t *testing.T) {
	resetFlags(t)
	var out bytes.Buffer
	providersCmd.SetOut(&out)
	t.Cleanup(func() { providersCmd.SetOut(nil) })

	if err := providersCmd.RunE(providersCmd, nil); err != nil {
		t.Fatalf("providers: %v", err)
	}
	if !strings.Contains(out.String(), "No plugins are enabled") {
		t.Errorf("output = %q", out.String())
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
	if strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("json output = %q", out.String())
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
	want := map[string]bool{"serve": false, "providers": false}
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
