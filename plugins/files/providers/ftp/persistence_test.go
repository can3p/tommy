package ftp_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/files"
	"github.com/can3p/tommy/plugins/files/providers/ftp"
)

// startPersistent boots tommy with the files tree on the filesystem backend
// rooted at dir, and an FTP listener on an ephemeral port.
func startPersistent(t *testing.T, dir string) (*testutil.Instance, string) {
	t.Helper()
	prov := ftp.New()
	cfg := config.Ephemeral()
	cfg.Storage.Backend = config.StorageFilesystem
	cfg.Storage.Path = dir
	cfg.SetProvider(files.PluginName, ftp.ProviderName, config.NewProviderConfig(map[string]any{"port": 0}))
	inst := testutil.Start(t, cfg, files.New(prov))
	addr, err := prov.Addr(5 * time.Second)
	if err != nil {
		t.Fatalf("listener never bound: %v", err)
	}
	return inst, addr
}

// Transfers are kept to a minimum: ftpserverlib binds each passive data
// listener to the wildcard address, so on a machine where another process
// holds the same port on 127.0.0.1 a data connection can land elsewhere - a
// hazard of the ftp provider's tests in general, not of persistence.
//
// What an FTP client uploaded is still there after tommy restarts over the
// same storage path - listed, downloadable byte-for-byte, and served by the
// API - while the events that announced it are gone, because captured events
// are ephemeral whatever storage is configured.
func TestTreeSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("persisted-over-ftp-"), 500)

	first, addr := startPersistent(t, dir)
	c := dial(t, addr)
	c.login("any", "any")
	c.cmd("MKD /outbox", 257)
	c.stor("/outbox/report.csv", payload)
	c.stor("/outbox/draft.txt", []byte("draft"))
	c.cmd("RNFR /outbox/draft.txt", 350)
	c.cmd("RNTO /outbox/final.txt", 250)
	c.cmd("MKD /scratch", 257)
	c.cmd("RMD /scratch", 250)
	c.cmd("QUIT", 221)
	c.close()
	if len(first.Events(store.Query{Plugin: files.PluginName})) == 0 {
		t.Fatal("the first run recorded no events")
	}
	first.Stop()

	second, addr := startPersistent(t, dir)
	if got := second.Events(store.Query{Plugin: files.PluginName}); len(got) != 0 {
		t.Errorf("events survived the restart: %d, want none", len(got))
	}

	c = dial(t, addr)
	defer c.close()
	c.login("any", "any")
	if got := c.retr("/outbox/report.csv"); !bytes.Equal(got, payload) {
		t.Fatalf("downloaded %d bytes after restart, want the %d uploaded", len(got), len(payload))
	}
	if got := c.retr("/outbox/final.txt"); string(got) != "draft" {
		t.Errorf("renamed file after restart = %q", got)
	}
	listing := c.list("NLST", "/outbox")
	if !strings.Contains(listing, "final.txt") || strings.Contains(listing, "draft.txt") {
		t.Errorf("NLST /outbox after restart = %q", listing)
	}
	c.cmd("CWD /scratch", 550)

	status, body := second.GetBody(second.API("/files/content/outbox/report.csv"))
	if status != 200 || body != string(payload) {
		t.Errorf("API content after restart: status %d, %d bytes", status, len(body))
	}
}
