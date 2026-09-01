package sendgrid_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	blobmem "github.com/can3p/tommy/core/blob/memory"
	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/store"
	storemem "github.com/can3p/tommy/core/store/memory"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/mail"
	"github.com/can3p/tommy/plugins/mail/providers/sendgrid"
)

// --- conformance -----------------------------------------------------------

func TestConformance(t *testing.T) {
	t.Parallel()
	plugintest.ConformanceProvider(t, sendgrid.New())
	plugintest.Conformance(t, mail.New(sendgrid.New()))
}

// --- test harness ------------------------------------------------------

// harness wraps a bare httptest.Server mounting only the sendgrid provider,
// plus the Deps it was mounted with, so a test can both drive it over real
// HTTP and inspect the store/blobs directly afterwards.
type harness struct {
	t  *testing.T
	ts *httptest.Server
	d  plugin.Deps
}

func newHarness(t *testing.T, cfg plugin.ProviderConfig) *harness {
	t.Helper()
	d := plugintest.NewDeps().WithConfig(cfg)
	mux := http.NewServeMux()
	sendgrid.New().RegisterIngress(mux, d)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &harness{t: t, ts: ts, d: d}
}

// send posts a raw body to /v3/mail/send with the given extra headers.
func (h *harness) send(body []byte, headers map[string]string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+sendgrid.SendPath, bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		h.t.Fatalf("do request: %v", err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// events lists every event the mail plugin has captured from this provider.
func (h *harness) events() []*event.Event {
	h.t.Helper()
	evs, err := h.d.Store.List(h.t.Context(), store.Query{Plugin: mail.PluginName, Provider: sendgrid.ProviderName})
	if err != nil {
		h.t.Fatalf("list events: %v", err)
	}
	return evs
}

// eventByIndex finds the fanned-out event carrying a given
// personalization_index in Meta, so assertions do not depend on store order.
func (h *harness) eventByIndex(index int) *event.Event {
	h.t.Helper()
	for _, e := range h.events() {
		if idx, ok := e.Meta["personalization_index"].(int); ok && idx == index {
			return e
		}
	}
	h.t.Fatalf("no event with personalization_index=%d among %d events", index, len(h.events()))
	return nil
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return data
}

func addrEq(a, b mail.Address) bool { return a.Name == b.Name && a.Email == b.Email }

func addrsEq(a, b []mail.Address) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !addrEq(a[i], b[i]) {
			return false
		}
	}
	return true
}

// --- the 202/empty-body/X-Message-Id contract -------------------------

func TestBasicSendResponseContract(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})
	fixture := loadFixture(t, "basic.json")

	resp := h.send(fixture, map[string]string{"Authorization": "Bearer SG.test-key"})

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	body := readAll(t, resp.Body)
	if len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}
	msgID := resp.Header.Get("X-Message-Id")
	if msgID == "" {
		t.Fatal("X-Message-Id header is missing")
	}

	evs := h.events()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	ev := evs[0]

	msg, ok := mail.MessageOf(ev)
	if !ok {
		t.Fatal("event carries no canonical message")
	}
	if !addrEq(msg.From, mail.Address{Name: "Alice", Email: "alice@example.com"}) {
		t.Errorf("From = %+v", msg.From)
	}
	if !addrsEq(msg.To, []mail.Address{{Name: "Bob", Email: "bob@example.com"}}) {
		t.Errorf("To = %+v", msg.To)
	}
	if msg.Subject != "Hi Bob" {
		t.Errorf("Subject = %q, want personalization override %q", msg.Subject, "Hi Bob")
	}
	if msg.Text != "Hello in text." {
		t.Errorf("Text = %q", msg.Text)
	}
	if msg.HTML != "<p>Hello in <b>html</b>.</p>" {
		t.Errorf("HTML = %q", msg.HTML)
	}

	if ev.Meta["message_id"] != msgID {
		t.Errorf("Meta[message_id] = %v, want %q (the response header)", ev.Meta["message_id"], msgID)
	}
	if ev.Meta["authorization"] != "Bearer SG.test-key" {
		t.Errorf("Meta[authorization] = %v", ev.Meta["authorization"])
	}
	if ev.Meta["batch_id"] != "batch-abc" {
		t.Errorf("Meta[batch_id] = %v", ev.Meta["batch_id"])
	}
	if ev.Meta["ip_pool_name"] != "transactional" {
		t.Errorf("Meta[ip_pool_name] = %v", ev.Meta["ip_pool_name"])
	}
	cats, _ := ev.Meta["categories"].([]string)
	if len(cats) != 2 || cats[0] != "welcome" || cats[1] != "onboarding" {
		t.Errorf("Meta[categories] = %v", ev.Meta["categories"])
	}
	args, _ := ev.Meta["custom_args"].(map[string]any)
	if args["user_id"] != "42" {
		t.Errorf("Meta[custom_args] = %v", ev.Meta["custom_args"])
	}

	if ev.Raw.Transport != "http" || ev.Raw.Method != http.MethodPost || ev.Raw.Path != sendgrid.SendPath {
		t.Errorf("Raw = %+v", ev.Raw)
	}
	if !ev.Raw.Text {
		t.Error("Raw.Text should be true for a JSON body")
	}
	if !bytes.Equal(ev.Raw.Body, fixture) {
		t.Errorf("Raw.Body does not match the request body sent")
	}
}

// --- fan-out and subject/header/custom_args/send_at override precedence --

func TestFanOutOverridePrecedence(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})
	fixture := loadFixture(t, "fanout_override.json")

	resp := h.send(fixture, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", resp.StatusCode, readAll(t, resp.Body))
	}
	_ = readAll(t, resp.Body)

	if got := len(h.events()); got != 2 {
		t.Fatalf("got %d events, want 2 (one per personalization)", got)
	}

	// Personalization 0 (Carol): overrides subject, headers, custom_args and
	// send_at; cc/bcc are personalization-only.
	carolEv := h.eventByIndex(0)
	carolMsg, ok := mail.MessageOf(carolEv)
	if !ok {
		t.Fatal("carol: no canonical message")
	}
	if !addrsEq(carolMsg.To, []mail.Address{{Name: "Carol", Email: "carol@example.com"}}) {
		t.Errorf("carol To = %+v", carolMsg.To)
	}
	if !addrsEq(carolMsg.Cc, []mail.Address{{Email: "cc1@example.com"}}) {
		t.Errorf("carol Cc = %+v", carolMsg.Cc)
	}
	if !addrsEq(carolMsg.Bcc, []mail.Address{{Email: "bcc1@example.com"}}) {
		t.Errorf("carol Bcc = %+v", carolMsg.Bcc)
	}
	if carolMsg.Subject != "Carol's subject" {
		t.Errorf("carol Subject = %q, want personalization override", carolMsg.Subject)
	}
	if got := carolMsg.Headers.Get("X-Campaign"); got != "carol-campaign" {
		t.Errorf("carol X-Campaign = %q, want personalization override", got)
	}
	if got := carolMsg.Headers.Get("X-Mailer"); got != "tommy" {
		t.Errorf("carol X-Mailer = %q, want inherited top-level value", got)
	}
	carolArgs, _ := carolEv.Meta["custom_args"].(map[string]any)
	if carolArgs["segment"] != "carol-segment" {
		t.Errorf("carol custom_args[segment] = %v, want personalization override", carolArgs["segment"])
	}
	if carolArgs["shared"] != "yes" {
		t.Errorf("carol custom_args[shared] = %v, want inherited top-level value", carolArgs["shared"])
	}
	if carolEv.Meta["send_at"] != int64(1700000100) && carolEv.Meta["send_at"] != float64(1700000100) {
		// JSON numbers decode to float64 by default; the handler stores the
		// *int64 field directly, so this should be int64, but tolerate both.
		t.Errorf("carol send_at = %v (%T), want personalization override 1700000100", carolEv.Meta["send_at"], carolEv.Meta["send_at"])
	}

	// Personalization 1 (Dave): nothing overridden, everything inherited.
	daveEv := h.eventByIndex(1)
	daveMsg, ok := mail.MessageOf(daveEv)
	if !ok {
		t.Fatal("dave: no canonical message")
	}
	if !addrsEq(daveMsg.To, []mail.Address{{Name: "Dave", Email: "dave@example.com"}}) {
		t.Errorf("dave To = %+v", daveMsg.To)
	}
	if len(daveMsg.Cc) != 0 || len(daveMsg.Bcc) != 0 {
		t.Errorf("dave should have no cc/bcc, got cc=%+v bcc=%+v", daveMsg.Cc, daveMsg.Bcc)
	}
	if daveMsg.Subject != "Top level subject" {
		t.Errorf("dave Subject = %q, want inherited top-level subject", daveMsg.Subject)
	}
	if got := daveMsg.Headers.Get("X-Campaign"); got != "top-campaign" {
		t.Errorf("dave X-Campaign = %q, want inherited top-level value", got)
	}
	daveArgs, _ := daveEv.Meta["custom_args"].(map[string]any)
	if daveArgs["segment"] != "top-segment" || daveArgs["shared"] != "yes" {
		t.Errorf("dave custom_args = %v, want inherited top-level values", daveArgs)
	}
	if daveEv.Meta["send_at"] != int64(1700000000) {
		t.Errorf("dave send_at = %v, want inherited top-level 1700000000", daveEv.Meta["send_at"])
	}
}

// --- attachments round-tripping through the blob store ------------------

func TestAttachmentsRoundTripThroughBlobStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})
	fixture := loadFixture(t, "attachments.json")

	resp := h.send(fixture, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", resp.StatusCode, readAll(t, resp.Body))
	}
	_ = readAll(t, resp.Body)

	evs := h.events()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	msg, ok := mail.MessageOf(evs[0])
	if !ok {
		t.Fatal("no canonical message")
	}
	if len(msg.Attachments) != 2 {
		t.Fatalf("got %d attachments, want 2", len(msg.Attachments))
	}

	note := msg.Attachments[0]
	wantNote := []byte("hello attachment body")
	if note.Filename != "note.txt" || note.ContentType != "text/plain" || note.Inline {
		t.Errorf("note attachment = %+v", note)
	}
	if note.Size != int64(len(wantNote)) {
		t.Errorf("note.Size = %d, want %d", note.Size, len(wantNote))
	}
	assertBlobContent(t, h.d, note.Blob.ID, wantNote)

	logo := msg.Attachments[1]
	wantLogo := []byte("<html><body>inline img here</body></html>")
	if logo.Filename != "logo.html" || logo.ContentType != "text/html" || !logo.Inline {
		t.Errorf("logo attachment = %+v", logo)
	}
	if logo.ContentID != "logo" {
		t.Errorf("logo.ContentID = %q, want angle brackets trimmed to %q", logo.ContentID, "logo")
	}
	if logo.Size != int64(len(wantLogo)) {
		t.Errorf("logo.Size = %d, want %d", logo.Size, len(wantLogo))
	}
	assertBlobContent(t, h.d, logo.Blob.ID, wantLogo)

	// AttachmentByContentID is how the HTML body's cid: reference resolves.
	if _, ok := msg.AttachmentByContentID("cid:logo"); !ok {
		t.Error("AttachmentByContentID(\"cid:logo\") did not find the inline attachment")
	}
}

func assertBlobContent(t *testing.T, d plugin.Deps, id string, want []byte) {
	t.Helper()
	rc, _, err := d.Blobs.Open(t.Context(), id)
	if err != nil {
		t.Fatalf("open blob %s: %v", id, err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob %s: %v", id, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("blob %s content = %q, want %q", id, got, want)
	}
}

// --- reply_to vs reply_to_list -------------------------------------------

func TestReplyToMapsOntoReplyToSlice(t *testing.T) {
	t.Parallel()

	t.Run("single reply_to", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, plugin.ProviderConfig{})
		resp := h.send(loadFixture(t, "reply_to_single.json"), nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, body %s", resp.StatusCode, readAll(t, resp.Body))
		}
		_ = readAll(t, resp.Body)
		msg, _ := mail.MessageOf(h.events()[0])
		if !addrsEq(msg.ReplyTo, []mail.Address{{Name: "Support", Email: "support@example.com"}}) {
			t.Errorf("ReplyTo = %+v", msg.ReplyTo)
		}
	})

	t.Run("reply_to_list", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, plugin.ProviderConfig{})
		resp := h.send(loadFixture(t, "reply_to_list.json"), nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, body %s", resp.StatusCode, readAll(t, resp.Body))
		}
		_ = readAll(t, resp.Body)
		msg, _ := mail.MessageOf(h.events()[0])
		want := []mail.Address{
			{Name: "Support One", Email: "support1@example.com"},
			{Name: "Support Two", Email: "support2@example.com"},
		}
		if !addrsEq(msg.ReplyTo, want) {
			t.Errorf("ReplyTo = %+v, want %+v", msg.ReplyTo, want)
		}
	})

	t.Run("both set is rejected", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, plugin.ProviderConfig{})
		resp := h.send(loadFixture(t, "error_reply_to_conflict.json"), nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		assertErrorField(t, resp, "reply_to_list")
		if len(h.events()) != 0 {
			t.Error("a rejected request must not append any event")
		}
	})
}

// --- auth capture: accept anything by default ----------------------------

func TestAuthCaptureAcceptsAnythingByDefault(t *testing.T) {
	t.Parallel()

	t.Run("no authorization header at all", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, plugin.ProviderConfig{})
		resp := h.send(loadFixture(t, "basic.json"), nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, body %s", resp.StatusCode, readAll(t, resp.Body))
		}
		_ = readAll(t, resp.Body)
		ev := h.events()[0]
		if _, ok := ev.Meta["authorization"]; ok {
			t.Errorf("Meta[authorization] = %v, want absent when nothing was presented", ev.Meta["authorization"])
		}
	})

	t.Run("arbitrary bearer token is captured, not validated", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, plugin.ProviderConfig{})
		resp := h.send(loadFixture(t, "basic.json"), map[string]string{"Authorization": "Bearer SG.anything-at-all"})
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, body %s", resp.StatusCode, readAll(t, resp.Body))
		}
		_ = readAll(t, resp.Body)
		ev := h.events()[0]
		if ev.Meta["authorization"] != "Bearer SG.anything-at-all" {
			t.Errorf("Meta[authorization] = %v", ev.Meta["authorization"])
		}
	})
}

// --- auth pinning: reject a mismatch with SendGrid's real error shape ----

func TestAuthPinnedKeyRejectsMismatch(t *testing.T) {
	t.Parallel()
	cfg := config.NewProviderConfig(map[string]any{"api_key": "SG.real-secret"})
	h := newHarness(t, cfg)
	fixture := loadFixture(t, "basic.json")

	t.Run("missing header", func(t *testing.T) {
		resp := h.send(fixture, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		assertErrorField(t, resp, "")
	})

	t.Run("wrong key", func(t *testing.T) {
		resp := h.send(fixture, map[string]string{"Authorization": "Bearer SG.wrong-key"})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		assertErrorField(t, resp, "")
	})

	if len(h.events()) != 0 {
		t.Fatal("no event should have been appended for a rejected request")
	}

	t.Run("correct key is accepted", func(t *testing.T) {
		resp := h.send(fixture, map[string]string{"Authorization": "Bearer SG.real-secret"})
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, body %s", resp.StatusCode, readAll(t, resp.Body))
		}
		_ = readAll(t, resp.Body)
	})

	if len(h.events()) != 1 {
		t.Fatalf("got %d events after the accepted request, want 1", len(h.events()))
	}
	if got := h.events()[0].Meta["authorization"]; got != "Bearer SG.real-secret" {
		t.Errorf("Meta[authorization] = %v", got)
	}
}

// --- error shapes ----------------------------------------------------------

// wireErrorBody mirrors the unexported errorBody type, for decoding responses
// in black-box tests.
type wireErrorBody struct {
	Errors []struct {
		Message string  `json:"message"`
		Field   *string `json:"field"`
	} `json:"errors"`
}

func assertErrorField(t *testing.T, resp *http.Response, wantField string) wireErrorBody {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("error Content-Type = %q, want application/json", ct)
	}
	body := readAll(t, resp.Body)
	var out wireErrorBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode error body %s: %v", body, err)
	}
	if len(out.Errors) == 0 {
		t.Fatalf("errors body has no entries: %s", body)
	}
	if wantField != "" {
		found := false
		for _, e := range out.Errors {
			if e.Field != nil && *e.Field == wantField {
				found = true
			}
		}
		if !found {
			t.Errorf("no error entry has field %q: %+v", wantField, out.Errors)
		}
	}
	return out
}

func TestErrorShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		fixture    string
		wantStatus int
		wantField  string
	}{
		{"missing from", "error_missing_from.json", http.StatusBadRequest, "from.email"},
		{"missing personalizations", "error_missing_personalizations.json", http.StatusBadRequest, "personalizations"},
		{"bad attachment base64", "error_bad_attachment_base64.json", http.StatusBadRequest, "attachments.0.content"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, plugin.ProviderConfig{})
			resp := h.send(loadFixture(t, tt.fixture), nil)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			assertErrorField(t, resp, tt.wantField)
			if len(h.events()) != 0 {
				t.Error("a rejected request must not append any event")
			}
		})
	}

	t.Run("invalid json body", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, plugin.ProviderConfig{})
		resp := h.send([]byte("{not json"), nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		assertErrorField(t, resp, "")
	})
}

// --- attachment bytes over the blob store's capacity ----------------------

func TestAttachmentOverBlobCapacity(t *testing.T) {
	t.Parallel()
	clock := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	var n int
	d := plugin.Deps{
		Store: storemem.New(100),
		Blobs: blobmem.New(4), // smaller than either attachment in the fixture
		Now:   func() time.Time { return clock },
		NewID: func() string { n++; return "cap-test-id" },
	}.Normalize()

	mux := http.NewServeMux()
	sendgrid.New().RegisterIngress(mux, d)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	req, err := http.NewRequest(http.MethodPost, ts.URL+sendgrid.SendPath, bytes.NewReader(loadFixture(t, "attachments.json")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	assertErrorField(t, resp, "attachments.0")

	evs, err := d.Store.List(t.Context(), store.Query{Plugin: mail.PluginName})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(evs) != 0 {
		t.Error("a request that fails mid-attachment must not leave a partial event behind")
	}
}

// --- end to end through the full mail plugin and a real ingress ----------

func TestEndToEndThroughMailPlugin(t *testing.T) {
	t.Parallel()
	in := testutil.Start(t, nil, mail.New(sendgrid.New()))

	req, err := http.NewRequest(http.MethodPost, in.Ingress(sendgrid.SendPath), bytes.NewReader(loadFixture(t, "basic.json")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer SG.e2e-key")
	resp := in.Do(req)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", resp.StatusCode, readAll(t, resp.Body))
	}
	if len(readAll(t, resp.Body)) != 0 {
		t.Error("body should be empty")
	}
	if resp.Header.Get("X-Message-Id") == "" {
		t.Error("X-Message-Id header is missing")
	}

	events := in.WaitForEvents(1, store.Query{Plugin: mail.PluginName, Provider: sendgrid.ProviderName}, 2*time.Second)
	if events[0].Summary.Title != "Hi Bob" {
		t.Errorf("summary title = %q", events[0].Summary.Title)
	}

	// The mail plugin's own read-back API sees it too.
	status, body := in.GetBody(in.API("/mail/messages"))
	if status != http.StatusOK {
		t.Fatalf("GET /mail/messages: status %d", status)
	}
	if !bytes.Contains([]byte(body), []byte("Hi Bob")) {
		t.Errorf("GET /mail/messages did not include the sent subject: %s", body)
	}
}

// base64 sanity check: confirms the fixtures decode to what the assertions
// above expect, so a fixture typo fails loudly here rather than as a subtle
// mismatch elsewhere.
func TestFixtureAttachmentsDecodeAsExpected(t *testing.T) {
	t.Parallel()
	var req struct {
		Attachments []struct {
			Content string `json:"content"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(loadFixture(t, "attachments.json"), &req); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if len(req.Attachments) != 2 {
		t.Fatalf("fixture has %d attachments, want 2", len(req.Attachments))
	}
	got, err := base64.StdEncoding.DecodeString(req.Attachments[0].Content)
	if err != nil {
		t.Fatalf("decode attachment 0: %v", err)
	}
	if string(got) != "hello attachment body" {
		t.Fatalf("attachment 0 decodes to %q", got)
	}
}
