package apns_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/push"
	"github.com/can3p/tommy/plugins/push/providers/apns"
)

// APNs is an HTTP/2-only API: Apple retired the binary protocol in 2021 and
// never offered an HTTP/1.1 form of the provider API. A test that only proves
// HTTP/1.1 works has not tested this provider at all, so everything here
// drives the real ingress over cleartext HTTP/2 and asserts resp.Proto - a
// silent downgrade to HTTP/1.1 would otherwise pass as success, since the
// provider deliberately still answers an HTTP/1.1 request.
//
// The client is net/http's own, with Protocols.SetUnencryptedHTTP2, which
// speaks prior-knowledge h2c - the one handshake the ingress serves, and what
// every HTTP/2-only vendor client performs. It is also what keeps this test
// free of golang.org/x/net/http2, whose h2c package is deprecated.

// h2cClient returns a client that speaks prior-knowledge cleartext HTTP/2 and
// nothing else, so a server that cannot do h2c fails rather than downgrades.
func h2cClient() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: protocols}}
}

// http1Client returns a client pinned to HTTP/1.1.
func http1Client() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	return &http.Client{Transport: &http.Transport{Protocols: protocols}}
}

// startIngress boots a real tommy with the push plugin and the apns provider
// on ephemeral ports, and returns the instance.
func startIngress(t *testing.T) *testutil.Instance {
	t.Helper()
	return testutil.Start(t, nil, push.New(apns.New()))
}

func apnsPost(t *testing.T, c *http.Client, url string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("apns-topic", "com.example.MyApp")
	req.Header.Set("apns-push-type", "alert")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// The headline case: a push delivered over real HTTP/2 to the shared ingress.
func TestPushOverRealHTTP2(t *testing.T) {
	in := startIngress(t)
	url := in.IngressURL + "/3/device/" + deviceToken

	resp := apnsPost(t, h2cClient(), url, loadFixture(t, "alert.json"))

	// Assert the protocol first: without this the rest of the test would pass
	// just as well over HTTP/1.1 and prove nothing about APNs.
	if resp.ProtoMajor != 2 {
		t.Fatalf("response arrived over %s, want HTTP/2 - the ingress did not serve h2c", resp.Proto)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, readAll(t, resp.Body))
	}
	if body := readAll(t, resp.Body); len(body) != 0 {
		t.Errorf("success body = %q, want empty", body)
	}
	if resp.Header.Get("apns-id") == "" {
		t.Error("no apns-id header on the HTTP/2 response")
	}

	evs := in.Events(store.Query{Plugin: push.Name, Provider: apns.ProviderName})
	if len(evs) != 1 {
		t.Fatalf("captured %d events, want 1", len(evs))
	}
	m := messageOf(t, evs[0])
	if m.Kind != push.KindNotification {
		t.Errorf("Kind = %q, want %q", m.Kind, push.KindNotification)
	}
	if m.Target.Value != deviceToken {
		t.Errorf("Target.Value = %q, want the path token", m.Target.Value)
	}
	// The protocol the request actually arrived over is recorded, since "did
	// my client really negotiate HTTP/2" is a question people have.
	if got, _ := evs[0].Meta["http_version"].(string); !strings.HasPrefix(got, "HTTP/2") {
		t.Errorf("meta http_version = %q, want an HTTP/2 value", got)
	}
	if evs[0].Meta["warning"] != nil {
		t.Errorf("a genuine HTTP/2 request was flagged as downgraded: %v", evs[0].Meta["warning"])
	}
}

// A silent push over HTTP/2, so the KindSilent trap is pinned on the transport
// the provider actually runs on rather than only in the unit tests.
func TestSilentPushOverRealHTTP2(t *testing.T) {
	in := startIngress(t)
	url := in.IngressURL + "/3/device/" + deviceToken

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url,
		bytes.NewReader(loadFixture(t, "silent.json")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("apns-topic", "com.example.MyApp")
	req.Header.Set("apns-push-type", "background")
	req.Header.Set("apns-priority", "5")

	resp, err := h2cClient().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.ProtoMajor != 2 {
		t.Fatalf("response arrived over %s, want HTTP/2", resp.Proto)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	evs := in.Events(store.Query{Plugin: push.Name, Provider: apns.ProviderName})
	if len(evs) != 1 {
		t.Fatalf("captured %d events, want 1", len(evs))
	}
	if m := messageOf(t, evs[0]); m.Kind != push.KindSilent {
		t.Errorf("Kind = %q, want %q over HTTP/2 too", m.Kind, push.KindSilent)
	}
}

// An error is answered over HTTP/2 as well, with its JSON body intact - a
// client that decodes {"reason":...} must get one whatever the protocol.
func TestErrorOverRealHTTP2(t *testing.T) {
	in := startIngress(t)
	url := in.IngressURL + "/3/device/" + deviceToken

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url,
		bytes.NewReader([]byte(`{"aps":{"alert":"hi"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("apns-push-type", "alert") // no apns-topic
	resp, err := h2cClient().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.ProtoMajor != 2 {
		t.Fatalf("response arrived over %s, want HTTP/2", resp.Proto)
	}
	assertReason(t, resp, http.StatusBadRequest, "MissingTopic")
}

// The provider still serves an HTTP/1.1 request rather than dropping the
// capture, but says so three ways. This is what a user meets after
// --h2c=false, or with a client that tried the Upgrade: h2c handshake RFC 9113
// removed, and the point is that they are told rather than left guessing.
func TestHTTP1RequestIsCapturedAndFlagged(t *testing.T) {
	in := startIngress(t)
	url := in.IngressURL + "/3/device/" + deviceToken

	resp := apnsPost(t, http1Client(), url, loadFixture(t, "alert.json"))

	if resp.ProtoMajor != 1 {
		t.Fatalf("response arrived over %s, want HTTP/1.1 for this case", resp.Proto)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 - the capture must not be lost over a transport detail", resp.StatusCode)
	}
	if w := resp.Header.Get("tommy-warning"); w == "" {
		t.Error("no tommy-warning header on an HTTP/1.1 APNs request")
	} else if !strings.Contains(w, "HTTP/2") {
		t.Errorf("tommy-warning = %q, want it to mention HTTP/2", w)
	}

	evs := in.Events(store.Query{Plugin: push.Name, Provider: apns.ProviderName})
	if len(evs) != 1 {
		t.Fatalf("captured %d events, want 1", len(evs))
	}
	if evs[0].Meta["warning"] == nil {
		t.Error("the downgrade was not recorded on the event")
	}
}

// With h2c switched off the ingress is HTTP/1.1 only, so an APNs client cannot
// connect at all. That is the configuration this provider warns about, and the
// warning is only honest if the failure is real.
func TestH2CDisabledRefusesHTTP2(t *testing.T) {
	cfg := config.Ephemeral()
	cfg.Ingress.H2C = config.Bool(false)
	in := testutil.Start(t, cfg, push.New(apns.New()))
	url := in.IngressURL + "/3/device/" + deviceToken

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url,
		bytes.NewReader(loadFixture(t, "alert.json")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("apns-topic", "com.example.MyApp")
	req.Header.Set("apns-push-type", "alert")

	if resp, err := h2cClient().Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("an h2c request succeeded over %s though [ingress] h2c is false", resp.Proto)
	}

	// HTTP/1.1 still works, which is exactly why the provider warns instead of
	// refusing: the capture is available, just not over the real protocol.
	resp := apnsPost(t, http1Client(), url, loadFixture(t, "alert.json"))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HTTP/1.1 status = %d, want 200", resp.StatusCode)
	}
}
