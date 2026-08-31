package testutil_test

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/server"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/core/testutil/fakeplugin"
)

func TestFakePluginConformance(t *testing.T) {
	// The registry-wide backstop I1 will run across the real plugins.
	plugintest.Conformance(t, fakeplugin.New())
}

// TestEndToEnd is the whole point of F1: a request to the ingress becomes an
// event that the API, the UI and the SSE stream all agree on.
func TestEndToEnd(t *testing.T) {
	in := testutil.Start(t, nil, fakeplugin.New())

	status, body := in.PostJSON(in.Ingress("/fake/v1/send"), map[string]string{
		"from": "alice@example.com",
		"to":   "bob@example.com",
		"text": "It works.",
	})
	if status != http.StatusCreated {
		t.Fatalf("ingress: status %d body %s", status, body)
	}

	events := in.WaitForEvents(1, store.Query{Plugin: "fake"}, 2*time.Second)
	e := events[0]
	if e.Provider != "echo" || e.Summary.From != "alice@example.com" {
		t.Fatalf("event = %+v", e)
	}

	// Read-back through the provider's own route, served from the store, so an
	// SDK that writes then fetches sees its own write.
	rstatus, rbody := in.GetBody(in.Ingress("/fake/v1/messages/" + string(e.ID)))
	if rstatus != http.StatusOK || !strings.Contains(rbody, "It works.") {
		t.Errorf("read back: status %d body %s", rstatus, rbody)
	}

	// The API sees it.
	astatus, abody := in.GetBody(in.API("/events?plugin=fake"))
	if astatus != http.StatusOK || !strings.Contains(abody, "It works.") {
		t.Errorf("api: status %d body %s", astatus, abody)
	}

	// So does the UI.
	ustatus, ubody := in.GetBody(in.UI("/fake/"))
	if ustatus != http.StatusOK || !strings.Contains(ubody, "It works.") {
		t.Errorf("ui: status %d", ustatus)
	}
}

func TestListenerProviderIsSupervised(t *testing.T) {
	fake := fakeplugin.New()
	var line *fakeplugin.LineProvider
	for _, p := range fake.Providers() {
		if lp, ok := p.(*fakeplugin.LineProvider); ok {
			line = lp
		}
	}
	if line == nil {
		t.Fatal("the fake plugin must ship a listener provider")
	}

	in := testutil.Start(t, nil, fake)

	addr, err := line.Addr(3 * time.Second)
	if err != nil {
		t.Fatalf("listener never bound: %v", err)
	}

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := fmt.Fprintln(conn, "hello over tcp"); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if !strings.HasPrefix(reply, "OK ") {
		t.Errorf("reply = %q", reply)
	}

	events := in.WaitForEvents(1, store.Query{Type: "fake.line"}, 2*time.Second)
	if events[0].Raw.Transport != "tcp" {
		t.Errorf("raw = %+v; a non-HTTP transport must still be captured", events[0].Raw)
	}
	if events[0].Summary.Title != "hello over tcp" {
		t.Errorf("summary = %+v", events[0].Summary)
	}
}

func TestShutdownReleasesEveryPort(t *testing.T) {
	fake := fakeplugin.New()
	srv, err := server.New(server.Options{Config: config.Ephemeral(), Plugins: []plugin.Plugin{fake}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	addrs := srv.Addrs()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	for name, addr := range map[string]string{"ui": addrs.UI, "ingress": addrs.Ingress} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("%s port %s was not released: %v", name, addr, err)
			continue
		}
		_ = ln.Close()
	}

	// Shutting down twice is a no-op, not a panic.
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Errorf("second shutdown: %v", err)
	}
}

func TestFreshStatePerInstance(t *testing.T) {
	a := testutil.Start(t, nil, fakeplugin.New())
	b := testutil.Start(t, nil, fakeplugin.New())

	if a.UIURL == b.UIURL {
		t.Fatalf("two instances bound the same UI port %q; ephemeral ports are what let tests run in parallel", a.UIURL)
	}

	if status, body := a.PostJSON(a.Ingress("/fake/v1/send"), map[string]string{"text": "only in a"}); status != http.StatusCreated {
		t.Fatalf("send: %d %s", status, body)
	}
	if got := len(a.Events(store.Query{})); got != 1 {
		t.Errorf("instance a has %d events", got)
	}
	if got := len(b.Events(store.Query{})); got != 0 {
		t.Errorf("instance b must have its own store, it has %d events", got)
	}
}

// collidingPlugin claims a route the fake plugin already owns.
type collidingPlugin struct{ *fakeplugin.Plugin }

func (collidingPlugin) Name() string  { return "collider" }
func (collidingPlugin) Title() string { return "Collider" }
func (collidingPlugin) Description() string {
	return "A plugin that deliberately claims a route another provider already owns."
}
func (collidingPlugin) Providers() []plugin.Provider { return []plugin.Provider{collidingProvider{}} }
func (collidingPlugin) Templates() fs.FS             { return nil }

type collidingProvider struct{}

func (collidingProvider) Name() string   { return "clash" }
func (collidingProvider) Plugin() string { return "collider" }
func (collidingProvider) Description() string {
	return "Claims the same ingress route as the fake echo provider, to prove startup fails loudly."
}
func (collidingProvider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{{Method: "POST", Path: "/fake/v1/send", Description: "The very same route."}}
}
func (collidingProvider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{{Title: "x", Lang: "bash", Code: "curl {{.IngressURL}}/fake/v1/send"}}
}
func (collidingProvider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	mux.HandleFunc("POST /fake/v1/send", func(w http.ResponseWriter, r *http.Request) {})
}

func TestIngressCollisionFailsStartupLoudly(t *testing.T) {
	_, err := server.New(server.Options{
		Config:  config.Ephemeral(),
		Plugins: []plugin.Plugin{fakeplugin.New(), collidingPlugin{fakeplugin.New()}},
	})
	if err == nil {
		t.Fatal("two providers claiming one route must stop the server from starting")
	}
	for _, want := range []string{"fake/echo", "collider/clash", "/fake/v1/send"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q:\n%v", want, err)
		}
	}
}

// undeclaredPlugin mounts a route it never declares.
type undeclaredProvider struct{ collidingProvider }

func (undeclaredProvider) Plugin() string { return "undeclared" }
func (undeclaredProvider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{{Method: "POST", Path: "/never/mounted", Description: "Declared but not mounted."}}
}
func (undeclaredProvider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	mux.HandleFunc("POST /somewhere/else", func(w http.ResponseWriter, r *http.Request) {})
}

type undeclaredPlugin struct{ collidingPlugin }

func (undeclaredPlugin) Name() string                 { return "undeclared" }
func (undeclaredPlugin) Providers() []plugin.Provider { return []plugin.Provider{undeclaredProvider{}} }

func TestUndeclaredEndpointFailsStartup(t *testing.T) {
	_, err := server.New(server.Options{
		Config:  config.Ephemeral(),
		Plugins: []plugin.Plugin{undeclaredPlugin{}},
	})
	if err == nil {
		t.Fatal("a declared endpoint that is never mounted must fail startup")
	}
	if !strings.Contains(err.Error(), "never mounts it") {
		t.Errorf("error = %v", err)
	}
}

func TestSharedAndSeparateListeners(t *testing.T) {
	t.Run("api shares the ui listener by default", func(t *testing.T) {
		in := testutil.Start(t, nil, fakeplugin.New())
		if hostPort(in.UIURL) != hostPort(in.APIURL) {
			t.Errorf("ui %q and api %q should share a listener", in.UIURL, in.APIURL)
		}
		if hostPort(in.UIURL) == hostPort(in.IngressURL) {
			t.Error("the ingress must get its own listener by default")
		}
	})

	t.Run("api can have its own port", func(t *testing.T) {
		cfg := config.Ephemeral()
		cfg.API.Port = config.Int(0)
		in := testutil.Start(t, cfg, fakeplugin.New())
		if hostPort(in.UIURL) == hostPort(in.APIURL) {
			t.Errorf("api %q should have its own listener", in.APIURL)
		}
		if status, _ := in.GetBody(in.API("/health")); status != http.StatusOK {
			t.Errorf("health on the dedicated API port = %d", status)
		}
	})

	t.Run("ingress can share the ui listener", func(t *testing.T) {
		port := freePort(t)
		cfg := &config.Config{
			UI:      config.ListenerConfig{Port: config.Int(port)},
			Ingress: config.ListenerConfig{Port: config.Int(port)},
		}
		in := testutil.Start(t, cfg, fakeplugin.New())

		if status, _ := in.GetBody(in.API("/health")); status != http.StatusOK {
			t.Errorf("health = %d", status)
		}
		if status, body := in.PostJSON(in.Ingress("/fake/v1/send"), map[string]string{"text": "shared"}); status != http.StatusCreated {
			t.Errorf("ingress on the shared listener: %d %s", status, body)
		}
		if status, _ := in.GetBody(in.UI("/")); status != http.StatusOK {
			t.Errorf("ui on the shared listener = %d", status)
		}
		// The core prefixes still win over the ingress catch-all.
		if status, _ := in.GetBody(in.UI("/static/app.css")); status != http.StatusOK {
			t.Errorf("static assets on the shared listener = %d", status)
		}
	})
}

func TestIngress404ListsProviders(t *testing.T) {
	in := testutil.Start(t, nil, fakeplugin.New())
	status, body := in.GetBody(in.Ingress("/nope"))
	if status != http.StatusNotFound {
		t.Fatalf("status = %d", status)
	}
	for _, want := range []string{"/fake/v1/send", "echo", "tommy providers"} {
		if !strings.Contains(body, want) {
			t.Errorf("404 body must mention %q:\n%s", want, body)
		}
	}
}

func TestConfigDrivesRetention(t *testing.T) {
	cfg := config.Ephemeral()
	cfg.Storage.Capacity = 2
	in := testutil.Start(t, cfg, fakeplugin.New())

	for i := range 5 {
		if status, body := in.PostJSON(in.Ingress("/fake/v1/send"), map[string]string{
			"text": fmt.Sprintf("msg %d", i),
		}); status != http.StatusCreated {
			t.Fatalf("send: %d %s", status, body)
		}
	}
	events := in.Events(store.Query{})
	if len(events) != 2 {
		t.Fatalf("retained %d events, want the configured capacity of 2", len(events))
	}
	if events[0].Summary.Snippet != "msg 4" {
		t.Errorf("newest retained = %q", events[0].Summary.Snippet)
	}
}

func hostPort(url string) string {
	s := strings.TrimPrefix(url, "http://")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}
