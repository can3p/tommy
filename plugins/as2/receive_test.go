package as2_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/plugins/as2"
)

// The Receiver is the seam plugins/as2/providers/* codes against, so its
// options and its HTTP behavior are pinned here rather than left to the
// provider to discover.

func TestHandleWritesTheMDN(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	req := request(t, "signed.mime", signedReceipt("sha256"))

	httpReq := httptest.NewRequest(http.MethodPost, "/as2", strings.NewReader(string(req.Body)))
	httpReq.Header = req.Header
	rec := httptest.NewRecorder()
	r.Handle(rec, httpReq)

	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	// Some AS2 clients will not read a chunked MDN, so the length is explicit.
	if res.Header.Get("Content-Length") == "" {
		t.Error("no Content-Length on the MDN")
	}
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "multipart/signed;") {
		t.Errorf("Content-Type = %q", res.Header.Get("Content-Type"))
	}
	if res.Header.Get("AS2-From") != "TOMMY" {
		t.Errorf("AS2-From = %q, want the request's AS2-To", res.Header.Get("AS2-From"))
	}
}

func TestHandleRefusesAnOversizedBody(t *testing.T) {
	id := as2.NewIdentity()
	if err := id.Configure(as2.IdentityConfig{InMemory: true}); err != nil {
		t.Fatal(err)
	}
	r := as2.NewReceiver(id, plugintest.NewDeps(), as2.WithProvider("test"), as2.WithMaxBody(16))

	httpReq := httptest.NewRequest(http.MethodPost, "/as2", strings.NewReader(strings.Repeat("x", 64)))
	httpReq.Header.Set("Content-Type", "application/edi-x12")
	rec := httptest.NewRecorder()
	r.Handle(rec, httpReq)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

func TestReceiverOptionsReachTheEventAndTheMDN(t *testing.T) {
	id := as2.NewIdentity()
	if err := id.Configure(as2.IdentityConfig{InMemory: true}); err != nil {
		t.Fatal(err)
	}
	deps := plugintest.NewDeps()
	r := as2.NewReceiver(id, deps,
		as2.WithProvider("edge"),
		as2.WithReportingUA("tommy AS2 (edge)"),
		as2.WithMeta(map[string]any{"listener": "edge:8822"}),
	)
	if r.Identity() != id {
		t.Error("Identity() does not return the identity the receiver was built with")
	}

	res, err := r.Receive(context.Background(), request(t, "plain.mime", signedReceipt("sha256")))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if res.Event.Provider != "edge" {
		t.Errorf("event provider = %q, want edge", res.Event.Provider)
	}
	if res.Event.Meta["listener"] != "edge:8822" {
		t.Errorf("event meta = %+v, want the provider's own key", res.Event.Meta)
	}
	if !strings.Contains(string(res.Response.Body), "Reporting-UA: tommy AS2 (edge)") {
		t.Errorf("the MDN does not carry the configured Reporting-UA:\n%s", res.Response.Body)
	}
	// Rule 4: Raw is the untouched request, with the transport recorded.
	if res.Event.Raw.Transport != as2.Transport || res.Event.Raw.Method != http.MethodPost {
		t.Errorf("Raw = %+v, want the HTTP request recorded", res.Event.Raw)
	}
	if res.Event.Raw.Headers.Get("AS2-From") != "PARTNER" {
		t.Error("the request headers were not kept on Raw")
	}
}

func TestEmptyBodyIsReported(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	req := request(t, "plain.mime", signedReceipt("sha256"))
	req.Body = nil

	res := receive(t, r, req)
	if !res.Message.HasIssue(as2.IssueEmptyBody) {
		t.Fatalf("issues = %+v, want %s", res.Message.Issues, as2.IssueEmptyBody)
	}
	if res.Response.Status != http.StatusOK {
		t.Errorf("status = %d, want 200 with an error disposition", res.Response.Status)
	}
}

// RFC 4130 §6.2 requires both identifiers, but a receiver "MUST make no
// restrictions on the textual values" - so a missing one is a warning on a
// captured message, never a refusal.
func TestMissingIdentifiersAreWarningsNotRefusals(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	req := request(t, "plain.mime", signedReceipt("sha256"))
	req.Header.Del("AS2-From")
	req.Header.Del("Message-ID")

	res := receive(t, r, req)
	m := res.Message
	if !m.HasIssue(as2.IssueMissingIdentifier) || !m.HasIssue(as2.IssueMissingMessageID) {
		t.Fatalf("issues = %+v, want both identifier warnings", m.Issues)
	}
	if _, failed := m.FirstError(); failed {
		t.Errorf("a missing header became an error: %+v", m.Issues)
	}
	if m.Payload.Format != as2.FormatX12 {
		t.Error("the message was not captured")
	}
	// With no Message-ID there is nothing to correlate on, so the field is
	// omitted rather than invented.
	if strings.Contains(string(res.Response.Body), "Original-Message-ID:") {
		t.Error("the MDN invented an Original-Message-ID")
	}
	if got := m.Title(); got == "" {
		t.Error("a message with no identifiers has no title")
	}
}

func TestRouteAndTitleFallbacks(t *testing.T) {
	cases := []struct {
		from, to, subject, id string
		wantTitle             string
	}{
		{"A", "B", "PO", "<1>", "PO (A → B)"},
		{"A", "B", "", "<1>", "A → B"},
		{"", "B", "", "<1>", "(no AS2-From) → B"},
		{"A", "", "", "<1>", "A → (no AS2-To)"},
		{"", "", "", "<1>", "<1>"},
		{"", "", "", "", "AS2 message"},
	}
	for _, tc := range cases {
		m := &as2.Message{From: tc.from, To: tc.to, Subject: tc.subject, MessageID: tc.id}
		if got := m.Title(); got != tc.wantTitle {
			t.Errorf("Title() for %+v = %q, want %q", tc, got, tc.wantTitle)
		}
	}
}
