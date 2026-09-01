package clienthelp_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/can3p/tommy/clienthelp"
)

// recordingRoundTripper captures the request it was actually handed and
// returns a canned response, so tests can assert on the rewritten request
// without a real network hop.
type recordingRoundTripper struct {
	got *http.Request
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       http.NoBody,
		Header:     http.Header{},
	}, nil
}

func TestTransportRewritesSchemeHostPortPathQuery(t *testing.T) {
	rec := &recordingRoundTripper{}
	rt := clienthelp.TransportWith("http://127.0.0.1:8822", rec)

	req, err := http.NewRequest(http.MethodPost, "https://api.sendgrid.com/v3/mail/send?dry_run=true", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer SG.fake-key")

	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	got := rec.got
	if got == nil {
		t.Fatal("underlying transport never saw a request")
	}
	if got.URL.Scheme != "http" {
		t.Errorf("scheme = %q, want %q", got.URL.Scheme, "http")
	}
	if got.URL.Host != "127.0.0.1:8822" {
		t.Errorf("host = %q, want %q", got.URL.Host, "127.0.0.1:8822")
	}
	if got.Host != "127.0.0.1:8822" {
		t.Errorf("req.Host = %q, want %q", got.Host, "127.0.0.1:8822")
	}
	if got.URL.Path != "/v3/mail/send" {
		t.Errorf("path = %q, want unchanged %q", got.URL.Path, "/v3/mail/send")
	}
	if got.URL.RawQuery != "dry_run=true" {
		t.Errorf("query = %q, want unchanged %q", got.URL.RawQuery, "dry_run=true")
	}
	if got.Header.Get("Authorization") != "Bearer SG.fake-key" {
		t.Errorf("headers not preserved: %v", got.Header)
	}
}

func TestTransportDoesNotMutateOriginalRequest(t *testing.T) {
	rec := &recordingRoundTripper{}
	rt := clienthelp.TransportWith("http://127.0.0.1:8822", rec)

	req, err := http.NewRequest(http.MethodGet, "https://api.twilio.com/2010-04-01/Accounts/AC123/Messages.json", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	originalURL := *req.URL
	originalHost := req.Host // http.NewRequest sets this from the URL

	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	if req.URL.Scheme != originalURL.Scheme || req.URL.Host != originalURL.Host {
		t.Errorf("original request was mutated: got %+v, want unchanged %+v", req.URL, originalURL)
	}
	if req.Host != originalHost {
		t.Errorf("original request's Host field was mutated: got %q, want %q", req.Host, originalHost)
	}
	// The RoundTripper must have handed the underlying transport a distinct
	// *http.Request value, not the same pointer.
	if rec.got == req {
		t.Error("RoundTrip must clone the request, not pass the original through")
	}
}

func TestTransportInvalidBaseURLFailsAtRoundTrip(t *testing.T) {
	rec := &recordingRoundTripper{}
	// A control character makes url.Parse fail.
	rt := clienthelp.TransportWith("http://exa\x7fmple.com", rec)

	req, _ := http.NewRequest(http.MethodGet, "https://api.twilio.com/", nil)
	if _, err := rt.RoundTrip(req); err == nil {
		t.Fatal("expected an error for an unparsable base URL")
	}
	if rec.got != nil {
		t.Error("the underlying transport must not be called when the base URL is invalid")
	}
}

func TestTransportThroughRealHTTPServer(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client := &http.Client{Transport: clienthelp.Transport(srv.URL)}

	// Address a URL that looks like the real vendor endpoint; the transport
	// must redirect it to the httptest server regardless.
	req, err := http.NewRequest(http.MethodPost, "https://api.sendgrid.com/v3/mail/send?x=1", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer SG.fake-key")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	if gotPath != "/v3/mail/send" {
		t.Errorf("server saw path %q", gotPath)
	}
	if gotQuery != "x=1" {
		t.Errorf("server saw query %q", gotQuery)
	}
	if gotAuth != "Bearer SG.fake-key" {
		t.Errorf("server saw auth %q", gotAuth)
	}
}

func TestHTTPClientConvenience(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := clienthelp.HTTPClient(srv.URL)
	resp, err := client.Get("https://api.twilio.com/anything")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if !hit {
		t.Error("HTTPClient did not route the request to the fake server")
	}
}

// baseURLHostPort is a sanity check that TransportWith carries a base URL
// with an explicit port through unchanged, not just bare hosts.
func TestTransportPreservesBasePort(t *testing.T) {
	base, err := url.Parse("http://localhost:8822")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rec := &recordingRoundTripper{}
	rt := clienthelp.TransportWith(base.String(), rec)

	req, _ := http.NewRequest(http.MethodGet, "https://api.mailjet.com/v3.1/send", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if rec.got.URL.Host != "localhost:8822" {
		t.Errorf("host = %q, want %q", rec.got.URL.Host, "localhost:8822")
	}
}
