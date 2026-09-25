package server

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/testutil/fakeplugin"
)

// statefulPlugin is a fakeplugin that keeps state, recording what it is bound to.
type statefulPlugin struct {
	*fakeplugin.Plugin
	bound   []plugin.Storage
	failure error
}

func (p *statefulPlugin) BindStorage(_ context.Context, st plugin.Storage) error {
	p.bound = append(p.bound, st)
	return p.failure
}

func storageConfig(t *testing.T, mutate func(*config.StorageConfig)) *config.Config {
	t.Helper()
	cfg := config.Ephemeral()
	cfg.Storage.Backend = config.StorageFilesystem
	cfg.Storage.Path = t.TempDir()
	mutate(&cfg.Storage)
	return cfg
}

func TestStatefulPluginIsBoundToItsOwnDirectory(t *testing.T) {
	p := &statefulPlugin{Plugin: fakeplugin.New(fakeplugin.WithName("vault"))}
	cfg := storageConfig(t, func(*config.StorageConfig) {})
	srv, err := New(Options{Config: cfg, Plugins: []plugin.Plugin{p}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.closeListeners()
	if len(p.bound) != 1 || p.bound[0].State == nil || p.bound[0].Blobs == nil {
		t.Fatalf("bound = %+v, want one persistent binding", p.bound)
	}
	if err := p.bound[0].State.Save(context.Background(), "snapshot", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Storage.Path, "plugins", "vault", "state", "snapshot")); err != nil {
		t.Fatalf("snapshot not under the plugin's own directory: %v", err)
	}
}

func TestMemoryScopeGetsNoState(t *testing.T) {
	p := &statefulPlugin{Plugin: fakeplugin.New(fakeplugin.WithName("vault"))}
	cfg := storageConfig(t, func(s *config.StorageConfig) {
		s.Plugins = map[string]config.StorageScopeConfig{"vault": {Backend: config.StorageMemory}}
	})
	srv, err := New(Options{Config: cfg, Plugins: []plugin.Plugin{p}})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.closeListeners()
	if len(p.bound) != 1 || p.bound[0].State != nil {
		t.Fatalf("a memory override must bind with no State, got %+v", p.bound)
	}
	if entries, _ := os.ReadDir(cfg.Storage.Path); len(entries) != 0 {
		t.Fatalf("a memory scope wrote to disk: %v", entries)
	}
}

// A restore that fails must stop startup before anything listens: a client
// must never reach a catalog that silently came back empty.
func TestRestoreFailureStopsStartupBeforeAnyListenerBinds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	p := &statefulPlugin{Plugin: fakeplugin.New(fakeplugin.WithName("vault")), failure: errors.New("corrupt snapshot")}
	cfg := storageConfig(t, func(*config.StorageConfig) {})
	cfg.UI.Port = config.Int(port)
	_, err = New(Options{Config: cfg, Plugins: []plugin.Plugin{p}})
	if err == nil || !strings.Contains(err.Error(), "plugin vault storage: corrupt snapshot") {
		t.Fatalf("New = %v, want the restore error", err)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("UI port was left bound after a failed restore: %v", err)
	}
	probe.Close()
}

func TestStorageOverridesThatCannotTakeEffectAreRefused(t *testing.T) {
	stateless := fakeplugin.New(fakeplugin.WithName("mail"))
	stateful := &statefulPlugin{Plugin: fakeplugin.New(fakeplugin.WithName("vault"))}
	for _, tc := range []struct {
		name  string
		scope map[string]config.StorageScopeConfig
		want  string
	}{
		{"unknown plugin", map[string]config.StorageScopeConfig{"nope": {Backend: config.StorageMemory}},
			`storage.plugins.nope: no plugin named "nope"`},
		{"plugin without state", map[string]config.StorageScopeConfig{"mail": {Backend: config.StorageFilesystem}},
			`storage.plugins.mail: plugin "mail" keeps no state beyond its captured events`},
		{"unknown provider", map[string]config.StorageScopeConfig{"vault": {Providers: map[string]config.StorageScopeConfig{"nope": {Backend: config.StorageMemory}}}},
			`storage.plugins.vault.providers.nope: plugin "vault" has no provider named "nope"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := storageConfig(t, func(s *config.StorageConfig) { s.Plugins = tc.scope })
			_, err := New(Options{Config: cfg, Plugins: []plugin.Plugin{stateless, stateful}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
