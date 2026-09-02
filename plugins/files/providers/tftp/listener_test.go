package tftp_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/files"
	"github.com/can3p/tommy/plugins/files/providers/tftp"
)

// startListener boots a whole tommy with this provider on an ephemeral port,
// so parallel test runs never collide, and returns the address it bound.
func startListener(t *testing.T, values map[string]any) (*testutil.Instance, string) {
	t.Helper()

	settings := map[string]any{"port": 0}
	for k, v := range values {
		settings[k] = v
	}
	prov := tftp.New()
	cfg := config.Ephemeral()
	cfg.SetProvider(files.PluginName, tftp.ProviderName, config.NewProviderConfig(settings))

	inst := testutil.Start(t, cfg, files.New(prov))
	addr, err := prov.Addr(5 * time.Second)
	if err != nil {
		t.Fatalf("listener never bound: %v", err)
	}
	return inst, addr
}

func waitForEvent(t *testing.T, inst *testutil.Instance, typ string) *event.Event {
	t.Helper()
	events := inst.WaitForEvents(1, store.Query{Plugin: files.PluginName, Type: typ}, 5*time.Second)
	return events[0]
}

// requireCurl skips the test when curl is not on PATH, the way the ftp
// provider's own curl test does.
func requireCurl(t *testing.T) string {
	t.Helper()
	curlPath, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not found on PATH")
	}
	return curlPath
}

// ---------------------------------------------------------------------------
// The real-client test: curl speaks tftp natively, and this is the one that
// matters most per docs/lessons.md - the best bug this project ever found
// (ftpserverlib silently corrupting downloads by defaulting to ASCII mode)
// was invisible to a mocked test and obvious the moment a real client fetched
// the file back. This round-trips a payload built specifically to expose a
// mode bug: CRLF pairs, a lone LF, a NUL byte and a high byte all survive
// netascii translation only if the transfer actually ran in octet mode.
// ---------------------------------------------------------------------------

func TestCurlUploadThenDownloadByteExact(t *testing.T) {
	curlPath := requireCurl(t)
	inst, addr := startListener(t, nil)

	dir := t.TempDir()
	localPath := filepath.Join(dir, "local.bin")
	downloadPath := filepath.Join(dir, "downloaded.bin")

	// Built to break a mode bug: "\r\n" and a lone "\n" would both be mangled
	// by netascii translation, and a NUL plus a high byte (0xFF) would break
	// any transfer that treats the payload as text at all. Repeated to push
	// the transfer past one 512-byte TFTP block, so block continuation is
	// exercised too, not just a single DATA packet.
	unit := []byte("line one\r\nline two\nNUL:\x00 high:\xff end\r\n")
	payload := bytes.Repeat(unit, 40) // well past 512 bytes
	if err := os.WriteFile(localPath, payload, 0o644); err != nil {
		t.Fatalf("write local fixture: %v", err)
	}

	uploadURL := "tftp://" + addr + "/upload/report.bin"
	upload := exec.Command(curlPath, "-sS", "-T", localPath, uploadURL)
	if out, err := upload.CombinedOutput(); err != nil {
		t.Fatalf("curl upload failed: %v\n%s", err, out)
	}

	uploadEv := waitForEvent(t, inst, files.EventUpload)
	op, ok := files.OpOf(uploadEv)
	if !ok || op.Path != "/upload/report.bin" || op.Size != int64(len(payload)) {
		t.Errorf("upload event = %+v, want path /upload/report.bin size %d", uploadEv.Payload, len(payload))
	}
	if uploadEv.Provider != tftp.ProviderName {
		t.Errorf("event provider = %q, want %q", uploadEv.Provider, tftp.ProviderName)
	}
	if uploadEv.Raw.Transport != "udp" {
		t.Errorf("Raw.Transport = %q, want udp", uploadEv.Raw.Transport)
	}
	if uploadEv.Raw.PeerAddr == "" {
		t.Error("Raw.PeerAddr is empty, want the client's UDP address recorded")
	}
	// curl's tftp:// URL path becomes the filename verbatim, leading slash
	// stripped - the untouched request per provider rule 4, not the VFS's
	// resolved, always-rooted path (which is what op.Path above asserts).
	if body := string(uploadEv.Raw.Body); !strings.Contains(body, "WRQ") || !strings.Contains(body, "upload/report.bin") {
		t.Errorf("Raw.Body = %q, want it to name the WRQ and the file", body)
	}

	download := exec.Command(curlPath, "-sS", "tftp://"+addr+"/upload/report.bin", "-o", downloadPath)
	if out, err := download.CombinedOutput(); err != nil {
		t.Fatalf("curl download failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(downloadPath)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded %d bytes, want %d bytes byte-identical to the upload (mode bug if this ever fails)", len(got), len(payload))
	}

	// Also visible over the plugin's own read-back API, proving the TFTP
	// upload and the HTTP surface share one tree with ftp and sftp.
	status, body := inst.GetBody(inst.API("/files/content/upload/report.bin"))
	if status != 200 {
		t.Fatalf("GET content: status %d", status)
	}
	if body != string(payload) {
		t.Error("content served over the API does not match the TFTP upload")
	}
}

// TestCurlLargerBlksize proves a client that negotiates a bigger block size
// than TFTP's 512-byte default still round-trips correctly - blksize is one
// of the options the task calls out as common enough to be worth supporting,
// and pin/tftp/v3 handles the negotiation, but only a real transfer proves
// the negotiated size was actually honored end to end.
func TestCurlLargerBlksize(t *testing.T) {
	curlPath := requireCurl(t)
	_, addr := startListener(t, nil)

	dir := t.TempDir()
	localPath := filepath.Join(dir, "big.bin")
	downloadPath := filepath.Join(dir, "big-downloaded.bin")

	payload := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64KB, several negotiated blocks
	if err := os.WriteFile(localPath, payload, 0o644); err != nil {
		t.Fatalf("write local fixture: %v", err)
	}

	uploadURL := "tftp://" + addr + "/big.bin"
	upload := exec.Command(curlPath, "-sS", "--tftp-blksize", "4096", "-T", localPath, uploadURL)
	if out, err := upload.CombinedOutput(); err != nil {
		t.Fatalf("curl upload (blksize 4096) failed: %v\n%s", err, out)
	}

	download := exec.Command(curlPath, "-sS", "--tftp-blksize", "4096", uploadURL, "-o", downloadPath)
	if out, err := download.CombinedOutput(); err != nil {
		t.Fatalf("curl download (blksize 4096) failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(downloadPath)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded %d bytes, want %d bytes matching the upload with blksize negotiated to 4096", len(got), len(payload))
	}
}

// TestCurlDownloadMissingFileFails proves a client asking for something that
// was never uploaded gets a real TFTP error rather than a hang or a silent
// empty file.
func TestCurlDownloadMissingFileFails(t *testing.T) {
	curlPath := requireCurl(t)
	_, addr := startListener(t, nil)

	cmd := exec.Command(curlPath, "-sS", "tftp://"+addr+"/nope.bin", "-o", filepath.Join(t.TempDir(), "out"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("curl download of a nonexistent file succeeded, want an error; output: %s", out)
	}
}

// TestListenStopsOnContextCancel proves the provider honors the lifecycle the
// core supervises it with - the same contract every other listener provider
// in this codebase proves for itself. Verifying it here matters more than
// usual: this is the first UDP listener provider, and pin/tftp/v3's Shutdown
// blocks on in-flight transfers, so a hung transfer would hang this test too.
func TestListenStopsOnContextCancel(t *testing.T) {
	prov := tftp.New()
	d := plugin.Deps{Config: config.NewProviderConfig(map[string]any{"port": 0})}.Normalize()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- prov.Listen(ctx, d) }()

	if _, err := prov.Addr(5 * time.Second); err != nil {
		t.Fatalf("listener never bound: %v", err)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Listen returned %v, want nil on a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not return after the context was canceled")
	}
}

// TestEphemeralPortNeverCollides proves port = 0 really does bind a free
// port rather than falling back to the package default - the exact mistake
// docs/lessons.md warns a listener provider can make silently.
func TestEphemeralPortNeverCollides(t *testing.T) {
	_, addr := startListener(t, nil)
	if strings.HasSuffix(addr, ":"+strconv.Itoa(tftp.DefaultPort)) {
		t.Fatalf("bound %s, which is the package default port - port 0 should have picked an ephemeral one", addr)
	}
}
