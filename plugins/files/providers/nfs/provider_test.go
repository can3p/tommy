package nfs_test

import (
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/files"
	"github.com/can3p/tommy/plugins/files/providers/nfs"
)

// TestConformance proves the provider on its own satisfies the
// discoverability contract: a real description, snippets that render, and no
// dangling endpoint declaration - a ListenerProvider mounts no HTTP route.
func TestConformance(t *testing.T) {
	plugintest.ConformanceProvider(t, nfs.New())
}

// TestPluginConformance proves the same thing through the files plugin, the
// way it is actually wired up.
func TestPluginConformance(t *testing.T) {
	plugintest.Conformance(t, files.New(nfs.New()))
}

// TestBootsViaTestutil is the shape every provider must work from: files.New
// wrapping the real provider, started through the shared harness on an
// ephemeral port.
func TestBootsViaTestutil(t *testing.T) {
	prov := nfs.New()
	inst := testutil.Start(t, nil, files.New(prov))

	addr, err := prov.Addr(5 * time.Second)
	if err != nil {
		t.Fatalf("listener never bound: %v", err)
	}
	if addr == "" {
		t.Fatal("Addr() returned an empty address")
	}
	if _, ok := inst.Registry.Plugin(files.PluginName); !ok {
		t.Fatal("files plugin not registered")
	}
}

// TestSnippetsCarryTheMountCommand is the one discoverability check worth
// spelling out for this provider. Mounting is the least obvious part of NFS
// and a user who cannot mount the share has nothing, so the rendered snippets
// must carry a complete command - including port= and mountport=, which are
// both required precisely because tommy runs no portmapper.
func TestSnippetsCarryTheMountCommand(t *testing.T) {
	ctx := plugintest.SnippetCtx()
	ctx.SetAddr(files.PluginName, nfs.ProviderName, "127.0.0.1:34567")

	var rendered []string
	for _, s := range nfs.New().Snippets() {
		out, err := s.Render(ctx)
		if err != nil {
			t.Fatalf("snippet %q did not render: %v", s.Title, err)
		}
		rendered = append(rendered, out)
	}
	all := strings.Join(rendered, "\n")

	for _, want := range []string{
		"mount -t nfs",
		"port=34567",
		"mountport=34567",
		"nfsvers=3", // Linux nfs(5)
		"vers=3",    // macOS mount_nfs(8)
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("no snippet mentions %q:\n%s", want, all)
		}
	}
	// The live port must win over the package default everywhere.
	if strings.Contains(all, "2049") {
		t.Fatalf("a snippet hardcoded the default port instead of the bound one:\n%s", all)
	}
}

// TestLoadConfigDefaults pins the two settings that decide where the listener
// lands and how many handles it will hold.
func TestLoadConfigDefaults(t *testing.T) {
	cfg := nfs.LoadConfig(config.NewProviderConfig(nil))
	if cfg.Port != nfs.DefaultPort {
		t.Fatalf("default port = %d, want %d", cfg.Port, nfs.DefaultPort)
	}
	if cfg.Bind != nfs.DefaultBind {
		t.Fatalf("default bind = %q, want %q", cfg.Bind, nfs.DefaultBind)
	}
	if cfg.HandleCache != nfs.DefaultHandleCache {
		t.Fatalf("default handle cache = %d, want %d", cfg.HandleCache, nfs.DefaultHandleCache)
	}

	// An explicit zero port means ephemeral and must survive; a silly handle
	// cache is raised to something a directory listing can work with, since
	// go-nfs hands back HandleLimit()/2 entries at a time.
	cfg = nfs.LoadConfig(config.NewProviderConfig(map[string]any{
		"port":         0,
		"handle_cache": 1,
		"bind":         "0.0.0.0",
	}))
	if cfg.Port != 0 {
		t.Fatalf("explicit port 0 became %d", cfg.Port)
	}
	if cfg.Bind != "0.0.0.0" {
		t.Fatalf("bind = %q", cfg.Bind)
	}
	if cfg.HandleCache < 64 {
		t.Fatalf("handle cache = %d, want it raised to a usable floor", cfg.HandleCache)
	}
	if got := cfg.ListenAddr(); got != "0.0.0.0:0" {
		t.Fatalf("ListenAddr = %q", got)
	}
}
