// Package testutil boots a whole tommy in-process on ephemeral ports, so tests
// exercise the real bootstrap instead of a hand-assembled subset.
//
// Every instance gets a fresh store and blob store, resolved URLs, and is shut
// down through t.Cleanup, so tests never collide and never leak listeners.
package testutil

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/server"
	"github.com/can3p/tommy/core/store"
)

// Instance is a running tommy plus the handles a test needs.
type Instance struct {
	TB     testing.TB
	Server *server.Server

	Store    store.Store
	Blobs    blob.BlobStore
	Registry *plugin.Registry
	Config   *config.Config

	// URLs are absolute and carry the ports actually bound.
	UIURL      string // http://127.0.0.1:PORT/ui/
	APIURL     string // http://127.0.0.1:PORT/api/v1
	IngressURL string // http://127.0.0.1:PORT

	Client *http.Client
}

// Start boots tommy with the given config and plugins, and registers cleanup.
// A nil config means ephemeral ports and defaults for everything else.
func Start(t testing.TB, cfg *config.Config, plugins ...plugin.Plugin) *Instance {
	t.Helper()

	ephemeral := cfg == nil
	if cfg == nil {
		cfg = config.Ephemeral()
	}
	if ephemeral {
		ephemeralListeners(cfg, plugins)
	}
	cfg.ApplyDefaults()

	level := slog.LevelError
	if os.Getenv("TOMMY_TEST_DEBUG") != "" {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	srv, err := server.New(server.Options{
		Config:          cfg,
		Plugins:         plugins,
		Logger:          logger,
		Version:         "test",
		ShutdownTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("testutil: start tommy: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("testutil: start tommy: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("testutil: shutdown: %v", err)
		}
	})

	uiURL, apiURL, ingressURL := srv.URLs()
	return &Instance{
		TB:         t,
		Server:     srv,
		Store:      srv.Store(),
		Blobs:      srv.Blobs(),
		Registry:   srv.Registry(),
		Config:     cfg,
		UIURL:      uiURL,
		APIURL:     apiURL,
		IngressURL: ingressURL,
		Client:     &http.Client{Timeout: 10 * time.Second},
	}
}

// Get performs a GET and returns the response. The body must be closed.
func (i *Instance) Get(url string) *http.Response {
	i.TB.Helper()
	resp, err := i.Client.Get(url)
	if err != nil {
		i.TB.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// GetBody performs a GET and returns status and body.
func (i *Instance) GetBody(url string) (int, string) {
	i.TB.Helper()
	resp := i.Get(url)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		i.TB.Fatalf("GET %s: read body: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

// GetJSON performs a GET and decodes the body into out.
func (i *Instance) GetJSON(url string, out any) int {
	i.TB.Helper()
	status, body := i.GetBody(url)
	if out != nil {
		if err := json.Unmarshal([]byte(body), out); err != nil {
			i.TB.Fatalf("GET %s: decode %q: %v", url, truncate(body, 300), err)
		}
	}
	return status
}

// PostJSON posts a JSON body and returns status and response body.
func (i *Instance) PostJSON(url string, payload any) (int, string) {
	i.TB.Helper()
	var body io.Reader
	switch p := payload.(type) {
	case nil:
	case string:
		body = strings.NewReader(p)
	case []byte:
		body = strings.NewReader(string(p))
	default:
		encoded, err := json.Marshal(p)
		if err != nil {
			i.TB.Fatalf("POST %s: encode payload: %v", url, err)
		}
		body = strings.NewReader(string(encoded))
	}
	resp, err := i.Client.Post(url, "application/json", body)
	if err != nil {
		i.TB.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		i.TB.Fatalf("POST %s: read body: %v", url, err)
	}
	return resp.StatusCode, string(out)
}

// Do sends a request built by the caller.
func (i *Instance) Do(req *http.Request) *http.Response {
	i.TB.Helper()
	resp, err := i.Client.Do(req)
	if err != nil {
		i.TB.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	return resp
}

// Events lists everything captured so far, newest first.
func (i *Instance) Events(q store.Query) []*event.Event {
	i.TB.Helper()
	events, err := i.Store.List(context.Background(), q)
	if err != nil {
		i.TB.Fatalf("list events: %v", err)
	}
	return events
}

// WaitForEvents waits until at least n events match q, and returns them.
// Providers append from their own goroutines, so polling is the honest way to
// wait rather than sleeping and hoping.
func (i *Instance) WaitForEvents(n int, q store.Query, timeout time.Duration) []*event.Event {
	i.TB.Helper()
	deadline := time.Now().Add(timeout)
	for {
		events := i.Events(q)
		if len(events) >= n {
			return events
		}
		if time.Now().After(deadline) {
			i.TB.Fatalf("timed out after %s waiting for %d events matching %+v, got %d", timeout, n, q, len(events))
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// URL joins a path onto a base URL.
func URL(base, path string) string {
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
}

// API returns an absolute URL under /api/v1.
func (i *Instance) API(path string) string { return URL(i.APIURL, path) }

// UI returns an absolute URL under /ui.
func (i *Instance) UI(path string) string { return URL(i.UIURL, path) }

// Ingress returns an absolute ingress URL.
func (i *Instance) Ingress(path string) string { return URL(i.IngressURL, path) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ephemeralListeners pins every listener provider to port 0.
//
// config.Ephemeral only zeroes the three core listeners. A listener provider
// reads its own port from its own config section, and falls back to its
// package default when that section says nothing - so a test that asked for an
// ephemeral server still got, say, SMTP on the well-known 1025. That collides
// with a real mail catcher on a developer's machine, or with another test
// binary, and fails only sometimes and only on some machines. An explicit port
// in the caller's config is left alone.
func ephemeralListeners(cfg *config.Config, plugins []plugin.Plugin) {
	for _, p := range plugins {
		for _, prov := range p.Providers() {
			if _, ok := prov.(plugin.ListenerProvider); !ok {
				continue
			}
			pc := cfg.Provider(p.Name(), prov.Name())
			if _, set := pc.Get("port"); set {
				continue
			}
			values := pc.Values()
			if values == nil {
				values = map[string]any{}
			}
			values["port"] = 0
			cfg.SetProvider(p.Name(), prov.Name(), config.NewProviderConfig(values))
		}
	}
}
