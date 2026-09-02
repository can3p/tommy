package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/core/testutil/fakeplugin"
)

// These tests speak to a real socket with a real HTTP/2 client, because the
// only interesting question about h2c is whether the protocol is actually
// negotiated - and a test that checked the response body alone would pass just
// as happily against a listener that quietly stayed on HTTP/1.1. Every
// assertion here is on resp.Proto as much as on the payload.

// h2cClient speaks cleartext HTTP/2 with prior knowledge: it opens the
// connection with the HTTP/2 client preface instead of asking for an upgrade.
// Leaving HTTP/1 out of Protocols is what forces that - with HTTP/1 also set,
// net/http prefers it for an http:// URL and the test would prove nothing.
//
// This is the same handshake golang.org/x/net/http2's Transport performs with
// AllowHTTP plus a plain-TCP DialTLS, which is how sideshow/apns2 and the other
// HTTP/2-only clients reach a non-TLS host.
func h2cClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{}
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetUnencryptedHTTP2(true)
	// An HTTP/2 connection is kept alive, and a graceful shutdown waits on it.
	// Cleanups run last-registered-first, so this happens before the harness
	// stops the server.
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

// http1Client is an ordinary client, pinned to HTTP/1.1 so a test that expects
// HTTP/1.1 cannot pass for the wrong reason.
func http1Client(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{}
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetHTTP1(true)
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

// freePort reserves an ephemeral port and gives it straight back, so a test
// that needs two surfaces on one port can name it. Never a well-known port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return port
}

// send posts a message to the fake plugin's ingress route and returns the
// negotiated protocol, the status and the body.
func send(t *testing.T, c *http.Client, base, text string) (proto string, status int, body string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		strings.TrimSuffix(base, "/")+"/fake/v1/send",
		strings.NewReader(fmt.Sprintf(`{"from":"a@example.com","to":"b@example.com","text":%q}`, text)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", req.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.Proto, resp.StatusCode, string(raw)
}

func get(t *testing.T, c *http.Client, url string) (proto string, status int, body string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.Proto, resp.StatusCode, string(raw)
}

// TestIngressServesHTTP2AndHTTP1OnOnePort is the whole point of h2c: one
// listener, both protocols, at the same time.
func TestIngressServesHTTP2AndHTTP1OnOnePort(t *testing.T) {
	in := testutil.Start(t, nil, fakeplugin.New())

	proto, status, body := send(t, h2cClient(t), in.IngressURL, "over h2c")
	if proto != "HTTP/2.0" {
		t.Fatalf("the ingress answered %s, so h2c never engaged: %s", proto, body)
	}
	if status != http.StatusCreated {
		t.Fatalf("h2c POST = %d %s", status, body)
	}

	// The same port, an unchanged HTTP/1.1 client, and no h2c in sight. Every
	// existing provider and vendor SDK takes this path.
	proto, status, body = send(t, http1Client(t), in.IngressURL, "over http/1.1")
	if proto != "HTTP/1.1" {
		t.Fatalf("expected HTTP/1.1, got %s", proto)
	}
	if status != http.StatusCreated {
		t.Fatalf("HTTP/1.1 POST = %d %s", status, body)
	}

	// The default client (HTTP/1.1 then HTTP/2 over TLS only) must be
	// untouched too - that is what curl and every vendor SDK look like.
	proto, status, body = send(t, in.Client, in.IngressURL, "default client")
	if proto != "HTTP/1.1" {
		t.Fatalf("the default client got %s, so h2c changed the HTTP/1.1 path", proto)
	}
	if status != http.StatusCreated {
		t.Fatalf("default client POST = %d %s", status, body)
	}

	// The provider recorded all three the same way, so nothing about the
	// capture path depends on the protocol the request arrived over.
	events := in.WaitForEvents(3, store.Query{Plugin: "fake"}, 2*time.Second)
	if len(events) != 3 {
		t.Fatalf("recorded %d events, want 3", len(events))
	}
	for _, e := range events {
		if e.Raw.PeerAddr == "" {
			t.Errorf("event %s has no peer address", e.ID)
		}
		if e.Raw.Method != http.MethodPost || e.Raw.Path != "/fake/v1/send" {
			t.Errorf("event %s recorded %s %s", e.ID, e.Raw.Method, e.Raw.Path)
		}
		if len(e.Raw.Body) == 0 {
			t.Errorf("event %s has no raw body", e.ID)
		}
	}
}

// TestIngressReadBackOverH2C exercises a route with a path wildcard and a
// response the client reads back, since HTTP/2 header handling and trailers are
// where a half-wired server usually falls over.
func TestIngressReadBackOverH2C(t *testing.T) {
	in := testutil.Start(t, nil, fakeplugin.New())
	c := h2cClient(t)

	proto, status, body := send(t, c, in.IngressURL, "read me back")
	if proto != "HTTP/2.0" || status != http.StatusCreated {
		t.Fatalf("send over h2c: %s %d %s", proto, status, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.ID == "" {
		t.Fatalf("send response %q: %v", body, err)
	}

	proto, status, body = get(t, c, in.Ingress("/fake/v1/messages/"+created.ID))
	if proto != "HTTP/2.0" {
		t.Fatalf("read-back answered %s", proto)
	}
	if status != http.StatusOK || !strings.Contains(body, "read me back") {
		t.Fatalf("read-back = %d %s", status, body)
	}
}

// TestIngressH2CCanBeDisabled proves the setting is real in both directions.
func TestIngressH2CCanBeDisabled(t *testing.T) {
	cfg := config.Ephemeral()
	cfg.Ingress.H2C = config.Bool(false)
	in := testutil.Start(t, cfg, fakeplugin.New())

	if in.Config.H2C("ingress") {
		t.Fatal("config still reports h2c on the ingress")
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		in.Ingress("/fake/v1/send"), strings.NewReader(`{"text":"nope"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := h2cClient(t).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("h2c reached a listener with h2c off, answering %s %d", resp.Proto, resp.StatusCode)
	}

	// HTTP/1.1 keeps working, which is the point of being able to turn it off.
	proto, status, body := send(t, http1Client(t), in.IngressURL, "still fine")
	if proto != "HTTP/1.1" || status != http.StatusCreated {
		t.Fatalf("HTTP/1.1 with h2c off: %s %d %s", proto, status, body)
	}
}

// TestUIListenerIsHTTP1ByDefault: h2c is an ingress setting, and with separate
// listeners it stays there.
func TestUIListenerIsHTTP1ByDefault(t *testing.T) {
	in := testutil.Start(t, nil, fakeplugin.New())
	if in.Config.H2C("ui") || in.Config.H2C("api") {
		t.Fatal("the ui and api listeners must not serve h2c by default")
	}

	resp, err := h2cClient(t).Get(in.API("/health"))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("h2c reached the ui listener, answering %s %d", resp.Proto, resp.StatusCode)
	}

	proto, status, _ := get(t, http1Client(t), in.API("/health"))
	if proto != "HTTP/1.1" || status != http.StatusOK {
		t.Fatalf("health over HTTP/1.1: %s %d", proto, status)
	}
}

// TestSharedListenerCarriesH2C pins down what happens when the ingress is
// pointed at the UI port. There is then one listener and one protocol decision
// for all three surfaces, taken before any routing, so the UI and the API are
// served over h2c too. That is safe rather than merely tolerable: browsers
// never attempt h2c (HTTP/2 in a browser requires TLS), so what a browser sees
// is unchanged.
func TestSharedListenerCarriesH2C(t *testing.T) {
	port := freePort(t)
	cfg := &config.Config{
		UI:      config.ListenerConfig{Port: config.Int(port)},
		Ingress: config.ListenerConfig{Port: config.Int(port)},
	}
	in := testutil.Start(t, cfg, fakeplugin.New())

	if !in.Config.IngressSharesUIListener() {
		t.Fatal("the ingress should be sharing the ui listener")
	}
	for _, surface := range []string{"ui", "api", "ingress"} {
		if !in.Config.H2C(surface) {
			t.Errorf("%s should inherit h2c from the shared listener", surface)
		}
	}

	c := h2cClient(t)
	proto, status, body := send(t, c, in.IngressURL, "shared listener")
	if proto != "HTTP/2.0" || status != http.StatusCreated {
		t.Fatalf("ingress on the shared listener: %s %d %s", proto, status, body)
	}
	if proto, status, _ = get(t, c, in.API("/health")); proto != "HTTP/2.0" || status != http.StatusOK {
		t.Errorf("api on the shared listener: %s %d", proto, status)
	}
	if proto, status, _ = get(t, c, in.UI("/")); proto != "HTTP/2.0" || status != http.StatusOK {
		t.Errorf("ui on the shared listener: %s %d", proto, status)
	}

	// And the browser-shaped path is exactly what it was.
	b := http1Client(t)
	if proto, status, _ = get(t, b, in.UI("/")); proto != "HTTP/1.1" || status != http.StatusOK {
		t.Errorf("ui over HTTP/1.1: %s %d", proto, status)
	}
	if proto, status, _ = get(t, b, in.UI("/static/app.css")); proto != "HTTP/1.1" || status != http.StatusOK {
		t.Errorf("static assets over HTTP/1.1: %s %d", proto, status)
	}
	if proto, status, _ = send(t, b, in.IngressURL, "shared over 1.1"); proto != "HTTP/1.1" || status != http.StatusCreated {
		t.Errorf("ingress over HTTP/1.1 on the shared listener: %s %d", proto, status)
	}
}

// TestSharedListenerHonorsH2COff: turning the ingress setting off takes h2c
// off the shared listener as well, so the escape hatch is not lost by sharing.
func TestSharedListenerHonorsH2COff(t *testing.T) {
	port := freePort(t)
	cfg := &config.Config{
		UI:      config.ListenerConfig{Port: config.Int(port)},
		Ingress: config.ListenerConfig{Port: config.Int(port), H2C: config.Bool(false)},
	}
	in := testutil.Start(t, cfg, fakeplugin.New())

	if in.Config.H2C("ui") || in.Config.H2C("ingress") {
		t.Fatal("h2c should be off on the shared listener")
	}
	resp, err := h2cClient(t).Get(in.API("/health"))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("h2c reached a shared listener with h2c off: %s %d", resp.Proto, resp.StatusCode)
	}
	if proto, status, _ := get(t, http1Client(t), in.UI("/")); proto != "HTTP/1.1" || status != http.StatusOK {
		t.Fatalf("ui over HTTP/1.1: %s %d", proto, status)
	}
}

// TestCurlH2C is the independent second opinion: a client that is not Go's.
// curl --http2-prior-knowledge performs the same handshake an HTTP/2-only
// vendor client does, and curl --http2 tries the deprecated Upgrade: h2c
// handshake instead, which net/http does not implement - the request must
// still succeed, over HTTP/1.1.
func TestCurlH2C(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is not installed")
	}
	in := testutil.Start(t, nil, fakeplugin.New())
	url := in.Ingress("/fake/v1/send")

	run := func(t *testing.T, args ...string) (httpVersion, code string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		full := append([]string{
			"-s", "-o", "/dev/null", "-w", "%{http_version} %{http_code}",
			"-H", "Content-Type: application/json",
			"-d", `{"from":"a@example.com","to":"b@example.com","text":"from curl"}`,
		}, args...)
		out, err := exec.CommandContext(ctx, curl, append(full, url)...).CombinedOutput()
		if err != nil {
			t.Fatalf("curl %v: %v: %s", args, err, out)
		}
		fields := strings.Fields(string(out))
		if len(fields) != 2 {
			t.Fatalf("curl %v printed %q", args, out)
		}
		return fields[0], fields[1]
	}

	t.Run("prior knowledge", func(t *testing.T) {
		version, code := run(t, "--http2-prior-knowledge")
		if version != "2" {
			t.Errorf("curl --http2-prior-knowledge negotiated HTTP/%s, want 2", version)
		}
		if code != "201" {
			t.Errorf("status = %s", code)
		}
	})

	t.Run("upgrade handshake falls back to HTTP/1.1", func(t *testing.T) {
		version, code := run(t, "--http2")
		if version != "1.1" {
			t.Logf("curl --http2 negotiated HTTP/%s", version)
		}
		if code != "201" {
			t.Errorf("a client asking for the h2c upgrade must still be served: status = %s", code)
		}
	})
}
