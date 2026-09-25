package http_test

import (
	"io"
	stdhttp "net/http"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/s3"
	s3http "github.com/can3p/tommy/plugins/s3/providers/http"
)

// bootS3 starts a whole tommy with only the s3 plugin, persisting to dir.
func bootS3(t *testing.T, dir string, buckets ...string) (*testutil.Instance, *s3.Plugin, string) {
	t.Helper()
	cfg := config.Ephemeral()
	cfg.Storage.Backend = config.StorageFilesystem
	cfg.Storage.Path = dir
	values := map[string]any{"port": 0, "bind": "127.0.0.1"}
	if len(buckets) > 0 {
		values["buckets"] = buckets
	}
	cfg.SetProvider(s3.PluginName, s3http.ProviderName, config.NewProviderConfig(values))
	provider := s3http.New()
	p := s3.New(provider)
	inst := testutil.Start(t, cfg, p)
	addr, err := provider.Addr(3 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return inst, p, "http://" + addr
}

func do(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, err := stdhttp.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := stdhttp.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// The issue's scenario: an app stores uploads in S3 and keeps the keys in its
// database, then the stack restarts. Buckets, objects and their metadata come
// back; the events recording how they got there deliberately do not.
func TestBucketsAndObjectsSurviveARestartButEventsDoNot(t *testing.T) {
	dir := t.TempDir()
	first, _, url := bootS3(t, dir)
	if code, body := do(t, "PUT", url+"/media", ""); code != 200 {
		t.Fatalf("create bucket = %d %s", code, body)
	}
	req, _ := stdhttp.NewRequest("PUT", url+"/media/avatars/1.png", strings.NewReader("png bytes"))
	req.Header.Set("Content-Type", "image/png")
	req.Header.Set("X-Amz-Meta-Owner", "user-1")
	resp, err := stdhttp.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("put object = %v %v", resp, err)
	}
	resp.Body.Close()
	if n := len(first.Events(store.Query{Plugin: s3.PluginName})); n != 2 {
		t.Fatalf("%d s3 events before restart, want 2", n)
	}
	first.Stop()

	second, p, url := bootS3(t, dir)
	code, body := do(t, "GET", url+"/media/avatars/1.png", "")
	if code != 200 || body != "png bytes" {
		t.Fatalf("GET after restart = %d %q", code, body)
	}
	obj, err := p.Store().HeadObject("media", "avatars/1.png")
	if err != nil {
		t.Fatal(err)
	}
	if obj.Headers.ContentType != "image/png" || obj.Metadata["owner"] != "user-1" {
		t.Fatalf("metadata after restart = %+v / %+v", obj.Headers, obj.Metadata)
	}
	if events := second.Events(store.Query{Plugin: s3.PluginName}); len(events) != 0 {
		t.Fatalf("%d s3 events after restart, want none: event history is not persisted", len(events))
	}

	// The core blob route still serves a persisted object's bytes, which is
	// what a captured event's download link points at.
	code, body = second.GetBody(second.API("/blobs/" + obj.Blob.ID))
	if code != 200 || body != "png bytes" {
		t.Fatalf("GET /blobs/%s after restart = %d %q", obj.Blob.ID, code, body)
	}
}

// Startup buckets (#41) and persistence compose: a configured bucket is
// created once, its contents survive, and one deleted at runtime is recreated
// by the next start.
func TestConfiguredBucketsComposeWithPersistence(t *testing.T) {
	dir := t.TempDir()
	first, _, url := bootS3(t, dir, "media", "exports")
	if code, _ := do(t, "PUT", url+"/media/kept", "still here"); code != 200 {
		t.Fatalf("put = %d", code)
	}
	if code, _ := do(t, "DELETE", url+"/exports", ""); code != 204 {
		t.Fatalf("delete bucket = %d", code)
	}
	first.Stop()

	_, p, url := bootS3(t, dir, "media", "exports")
	if code, body := do(t, "GET", url+"/media/kept", ""); code != 200 || body != "still here" {
		t.Fatalf("object in a configured bucket after restart = %d %q", code, body)
	}
	if _, err := p.Store().HeadBucket("exports"); err != nil {
		t.Fatalf("configured bucket deleted at runtime was not recreated: %v", err)
	}
}
