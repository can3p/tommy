package mailjet_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/mail"
	"github.com/can3p/tommy/plugins/mail/providers/mailjet"
)

// uuidPattern matches the RFC 4122 v4 shape the provider mints for
// MessageUUID and ErrorIdentifier.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// harness wires a fresh provider onto a fresh mux with deterministic
// dependencies (plugintest.NewDeps: fixed clock, counting ids, in-memory store
// and blob store), so each test gets its own isolated instance and generated
// MessageID sequences start predictably.
type harness struct {
	t    *testing.T
	deps plugin.Deps
	mux  *http.ServeMux
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWithConfig(t, nil)
}

// newHarnessWithConfig builds a harness whose provider section carries the
// given raw TOML-shaped values, for the auth-pinning tests.
func newHarnessWithConfig(t *testing.T, values map[string]any) *harness {
	t.Helper()
	h := &harness{
		t:    t,
		deps: plugintest.NewDeps(),
		mux:  http.NewServeMux(),
	}
	prov := mailjet.New()
	deps := h.deps
	if values != nil {
		deps = deps.WithConfig(configOf(values))
	}
	prov.RegisterIngress(h.mux, deps)
	return h
}

func (h *harness) send(body []byte, auth *[2]string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, mailjet.SendPath, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if auth != nil {
		req.SetBasicAuth(auth[0], auth[1])
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// events lists every mail event recorded so far, oldest first (request
// order), unlike the store's own newest-first listing.
func (h *harness) events(t *testing.T) []*event.Event {
	t.Helper()
	evs, err := h.deps.Store.List(context.Background(), store.Query{Plugin: mail.PluginName})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	out := make([]*event.Event, len(evs))
	for i, e := range evs {
		out[len(evs)-1-i] = e
	}
	return out
}

func messageOf(t *testing.T, e *event.Event) *mail.Message {
	t.Helper()
	msg, ok := mail.MessageOf(e)
	if !ok {
		t.Fatalf("event %s did not carry a mail.Message", e.ID)
	}
	return msg
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func TestConformance(t *testing.T) {
	plugintest.ConformanceProvider(t, mailjet.New())
}

func TestConformanceThroughThePlugin(t *testing.T) {
	plugintest.Conformance(t, mail.New(mailjet.New()))
}

func TestBasicSend(t *testing.T) {
	h := newHarness(t)
	rec := h.send(fixture(t, "basic.json"), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("content-type = %q", ct)
	}

	var resp struct {
		Messages []struct {
			Status   string `json:"Status"`
			CustomID string `json:"CustomID"`
			To       []struct {
				Email       string `json:"Email"`
				MessageUUID string `json:"MessageUUID"`
				MessageID   int64  `json:"MessageID"`
				MessageHref string `json:"MessageHref"`
			} `json:"To"`
			Cc  []any `json:"Cc"`
			Bcc []any `json:"Bcc"`
		} `json:"Messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Messages) != 1 {
		t.Fatalf("Messages = %+v, want 1 entry", resp.Messages)
	}
	m := resp.Messages[0]
	if m.Status != "success" {
		t.Fatalf("status = %q, body = %s", m.Status, rec.Body.String())
	}
	if m.CustomID != "" {
		t.Errorf("CustomID = %q, want empty when the request did not set one", m.CustomID)
	}
	if m.Cc == nil || m.Bcc == nil {
		t.Errorf("Cc/Bcc must be `[]`, not null: Cc=%v Bcc=%v", m.Cc, m.Bcc)
	}
	if len(m.To) != 1 {
		t.Fatalf("To = %+v, want one recipient", m.To)
	}
	to := m.To[0]
	if to.Email != "bob@example.com" {
		t.Errorf("To[0].Email = %q", to.Email)
	}
	if to.MessageID == 0 {
		t.Error("MessageID must be non-zero outside sandbox mode")
	}
	if !uuidPattern.MatchString(to.MessageUUID) {
		t.Errorf("MessageUUID = %q, want a v4 UUID shape", to.MessageUUID)
	}
	if to.MessageHref != "https://api.mailjet.com/v3/message/"+strconv.FormatInt(to.MessageID, 10) {
		t.Errorf("MessageHref = %q", to.MessageHref)
	}

	// The canonical Message must be exactly what was sent, nothing vendor
	// specific folded in.
	events := h.events(t)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	msg := messageOf(t, events[0])
	if msg.From.Email != "alice@example.com" || msg.From.Name != "Alice" {
		t.Errorf("From = %+v", msg.From)
	}
	if len(msg.To) != 1 || msg.To[0].Email != "bob@example.com" || msg.To[0].Name != "Bob" {
		t.Errorf("To = %+v", msg.To)
	}
	if msg.Subject != "Hello from tommy" || msg.Text != "It works." || msg.HTML != "<p>It <b>works</b>.</p>" {
		t.Errorf("message = %+v", msg)
	}
}

func TestReplyToBecomesASingleElementSlice(t *testing.T) {
	h := newHarness(t)
	rec := h.send(fixture(t, "reply_to_and_headers.json"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	events := h.events(t)
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	msg := messageOf(t, events[0])
	if len(msg.ReplyTo) != 1 || msg.ReplyTo[0].Email != "no-reply@example.com" || msg.ReplyTo[0].Name != "No Reply" {
		t.Fatalf("ReplyTo = %+v, want a one-element slice", msg.ReplyTo)
	}
	if len(msg.Cc) != 1 || msg.Cc[0].Email != "carol@example.com" {
		t.Errorf("Cc = %+v", msg.Cc)
	}
	if len(msg.Bcc) != 1 || msg.Bcc[0].Email != "dan@example.com" {
		t.Errorf("Bcc = %+v", msg.Bcc)
	}
	if got := msg.Headers.Get("X-Campaign"); got != "billing" {
		t.Errorf("headers = %+v", msg.Headers)
	}
	if got := msg.Headers.Get("X-Trace"); got != "abc123" {
		t.Errorf("headers = %+v", msg.Headers)
	}
}

// TestMetaCarriesEverythingVendorSpecific checks that CustomID, CustomCampaign,
// EventPayload and SandboxMode never leak into the canonical mail.Message and
// instead land in Event.Meta, per the mail plugin's own contract.
func TestMetaCarriesEverythingVendorSpecific(t *testing.T) {
	h := newHarness(t)
	rec := h.send(fixture(t, "reply_to_and_headers.json"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := h.events(t)
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	meta := events[0].Meta
	if meta["custom_id"] != "invoice-42" {
		t.Errorf("meta custom_id = %v", meta["custom_id"])
	}
	if meta["custom_campaign"] != "billing-2026" {
		t.Errorf("meta custom_campaign = %v", meta["custom_campaign"])
	}
	if meta["sandbox_mode"] != false {
		t.Errorf("meta sandbox_mode = %v", meta["sandbox_mode"])
	}
	payload, ok := meta["event_payload"].(map[string]any)
	if !ok || payload["order_id"] != float64(42) {
		t.Errorf("meta event_payload = %v", meta["event_payload"])
	}
	if _, ok := events[0].Payload.(*mail.Message); !ok {
		t.Fatalf("payload type = %T", events[0].Payload)
	}
}

func TestAttachmentsRoundTripThroughTheBlobStore(t *testing.T) {
	h := newHarness(t)
	rec := h.send(fixture(t, "attachments.json"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	events := h.events(t)
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	msg := messageOf(t, events[0])
	if len(msg.Attachments) != 2 {
		t.Fatalf("attachments = %+v", msg.Attachments)
	}
	att := msg.Attachments[0]
	if att.Inline {
		t.Errorf("Attachments[0] should not be inline: %+v", att)
	}
	if att.Filename != "invoice.csv" || att.ContentType != "text/csv" {
		t.Errorf("attachment = %+v", att)
	}
	if att.Blob.ID == "" || att.Size == 0 {
		t.Errorf("bytes were not stored in the blob store: %+v", att)
	}
	rc, ref, err := h.deps.Blobs.Open(context.Background(), att.Blob.ID)
	if err != nil {
		t.Fatalf("open blob: %v", err)
	}
	defer func() { _ = rc.Close() }()
	data, _ := io.ReadAll(rc)
	if string(data) != "invoice,42\n" {
		t.Errorf("blob content = %q", data)
	}
	if ref.ContentType != "text/csv" {
		t.Errorf("blob content type = %q", ref.ContentType)
	}

	inline := msg.Attachments[1]
	if !inline.Inline {
		t.Errorf("InlinedAttachments entry must be marked Inline: %+v", inline)
	}
	if inline.ContentID != "logo" {
		t.Errorf("ContentID = %q", inline.ContentID)
	}
	if inline.Blob.ID == "" {
		t.Error("inline attachment bytes were not stored")
	}
}

func TestFanOutOneEventPerMessage(t *testing.T) {
	h := newHarness(t)
	rec := h.send(fixture(t, "fanout.json"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Messages []map[string]any `json:"Messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2", len(resp.Messages))
	}

	events := h.events(t)
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 (one per Messages[] entry)", len(events))
	}
	first, second := messageOf(t, events[0]), messageOf(t, events[1])
	if first.Subject != "Fanned one" || second.Subject != "Fanned two" {
		t.Errorf("subjects = %q, %q", first.Subject, second.Subject)
	}
	if second.To[0].Email != "carol@example.com" || second.Cc[0].Email != "dan@example.com" {
		t.Errorf("second message recipients = %+v/%+v", second.To, second.Cc)
	}
	if events[0].Meta["fan_out_index"] != 0 || events[1].Meta["fan_out_index"] != 1 {
		t.Errorf("fan_out_index not recorded correctly: %v, %v", events[0].Meta["fan_out_index"], events[1].Meta["fan_out_index"])
	}
}

func TestSandboxModeZeroesGeneratedIDs(t *testing.T) {
	h := newHarness(t)
	rec := h.send(fixture(t, "sandbox.json"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	want := `{"Messages":[{"Status":"success","CustomID":"","To":[{"Email":"passenger1@mailjet.com","MessageUUID":"","MessageID":0,"MessageHref":"https://api.mailjet.com/v3/message/0"}],"Cc":[],"Bcc":[]}]}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("body = %s, want %s", rec.Body.String(), want)
	}

	events := h.events(t)
	if len(events) != 1 || events[0].Meta["sandbox_mode"] != true {
		t.Errorf("sandbox_mode must be recorded in meta: %+v", events)
	}
}

func TestAuthIsAcceptedByDefaultAndRecorded(t *testing.T) {
	h := newHarness(t)
	rec := h.send(fixture(t, "basic.json"), &[2]string{"whatever-key", "whatever-secret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := h.events(t)
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0].Meta["presented_api_key"] != "whatever-key" || events[0].Meta["presented_secret_key"] != "whatever-secret" {
		t.Errorf("meta = %+v, want the presented credentials recorded", events[0].Meta)
	}

	// No credentials at all is also accepted by default.
	h2 := newHarness(t)
	rec2 := h2.send(fixture(t, "basic.json"), nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("no-auth status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
}

func TestAuthPinnedRejectsAMismatch(t *testing.T) {
	h := newHarnessWithConfig(t, map[string]any{"api_key": "expected-key", "secret_key": "expected-secret"})

	// Wrong credentials.
	rec := h.send(fixture(t, "basic.json"), &[2]string{"wrong-key", "wrong-secret"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var errBody struct {
		ErrorIdentifier string `json:"ErrorIdentifier"`
		ErrorCode       string `json:"ErrorCode"`
		StatusCode      int    `json:"StatusCode"`
		ErrorMessage    string `json:"ErrorMessage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errBody.StatusCode != 401 || errBody.ErrorMessage == "" || !uuidPattern.MatchString(errBody.ErrorIdentifier) {
		t.Errorf("error body = %+v", errBody)
	}
	if events := h.events(t); len(events) != 0 {
		t.Errorf("a rejected auth attempt must not append an event, got %d", len(events))
	}

	// No credentials at all is also a mismatch.
	rec2 := h.send(fixture(t, "basic.json"), nil)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d", rec2.Code)
	}

	// Right credentials succeed.
	rec3 := h.send(fixture(t, "basic.json"), &[2]string{"expected-key", "expected-secret"})
	if rec3.Code != http.StatusOK {
		t.Fatalf("correct-auth status = %d, body = %s", rec3.Code, rec3.Body.String())
	}
}

func TestErrorShapes(t *testing.T) {
	tests := []struct {
		name       string
		fixture    string
		wantStatus int
		// global means a top-level {ErrorIdentifier,...} error rather than a
		// 200-shaped {Messages:[{Status:"error",...}]} envelope.
		global         bool
		wantCode       string
		wantRelatedTo  []string
		wantMsgSnippet string
	}{
		{
			name:           "missing Messages array",
			fixture:        "error_empty_messages.json",
			wantStatus:     http.StatusBadRequest,
			global:         true,
			wantCode:       "mj-0004",
			wantRelatedTo:  []string{"Messages"},
			wantMsgSnippet: "Messages",
		},
		{
			name:           "malformed json",
			fixture:        "error_malformed_json.json",
			wantStatus:     http.StatusBadRequest,
			global:         true,
			wantCode:       "mj-0002",
			wantMsgSnippet: "Malformed JSON",
		},
		{
			name:           "missing from",
			fixture:        "error_missing_from.json",
			wantStatus:     http.StatusOK,
			wantCode:       "mj-0004",
			wantRelatedTo:  []string{"From"},
			wantMsgSnippet: "From",
		},
		{
			name:           "no recipients",
			fixture:        "error_no_recipients.json",
			wantStatus:     http.StatusOK,
			wantCode:       "mj-0004",
			wantRelatedTo:  []string{"To", "Cc", "Bcc"},
			wantMsgSnippet: "recipient",
		},
		{
			name:           "no text or html part",
			fixture:        "error_no_body.json",
			wantStatus:     http.StatusOK,
			wantCode:       "send-0003",
			wantRelatedTo:  []string{"HTMLPart", "TextPart"},
			wantMsgSnippet: "HTMLPart",
		},
		{
			name:           "bad attachment base64",
			fixture:        "error_bad_base64.json",
			wantStatus:     http.StatusOK,
			wantCode:       "mj-0004",
			wantRelatedTo:  []string{"Attachments"},
			wantMsgSnippet: "base64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			rec := h.send(fixture(t, tt.fixture), nil)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.global {
				var body struct {
					ErrorIdentifier string   `json:"ErrorIdentifier"`
					ErrorCode       string   `json:"ErrorCode"`
					StatusCode      int      `json:"StatusCode"`
					ErrorMessage    string   `json:"ErrorMessage"`
					ErrorRelatedTo  []string `json:"ErrorRelatedTo"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if body.ErrorCode != tt.wantCode {
					t.Errorf("ErrorCode = %q, want %q", body.ErrorCode, tt.wantCode)
				}
				if body.StatusCode != tt.wantStatus {
					t.Errorf("StatusCode = %d", body.StatusCode)
				}
				if !uuidPattern.MatchString(body.ErrorIdentifier) {
					t.Errorf("ErrorIdentifier = %q", body.ErrorIdentifier)
				}
				if !strings.Contains(body.ErrorMessage, tt.wantMsgSnippet) {
					t.Errorf("ErrorMessage = %q, want it to mention %q", body.ErrorMessage, tt.wantMsgSnippet)
				}
				if tt.wantRelatedTo != nil && !equalStrings(body.ErrorRelatedTo, tt.wantRelatedTo) {
					t.Errorf("ErrorRelatedTo = %v, want %v", body.ErrorRelatedTo, tt.wantRelatedTo)
				}
				return
			}

			var body struct {
				Messages []struct {
					Status string `json:"Status"`
					Errors []struct {
						ErrorIdentifier string   `json:"ErrorIdentifier"`
						ErrorCode       string   `json:"ErrorCode"`
						StatusCode      int      `json:"StatusCode"`
						ErrorMessage    string   `json:"ErrorMessage"`
						ErrorRelatedTo  []string `json:"ErrorRelatedTo"`
					} `json:"Errors"`
				} `json:"Messages"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(body.Messages) != 1 || body.Messages[0].Status != "error" {
				t.Fatalf("Messages = %+v", body.Messages)
			}
			errs := body.Messages[0].Errors
			if len(errs) != 1 {
				t.Fatalf("Errors = %+v", errs)
			}
			e := errs[0]
			if e.ErrorCode != tt.wantCode {
				t.Errorf("ErrorCode = %q, want %q", e.ErrorCode, tt.wantCode)
			}
			if !uuidPattern.MatchString(e.ErrorIdentifier) {
				t.Errorf("ErrorIdentifier = %q", e.ErrorIdentifier)
			}
			if !strings.Contains(e.ErrorMessage, tt.wantMsgSnippet) {
				t.Errorf("ErrorMessage = %q, want it to mention %q", e.ErrorMessage, tt.wantMsgSnippet)
			}
			if !equalStrings(e.ErrorRelatedTo, tt.wantRelatedTo) {
				t.Errorf("ErrorRelatedTo = %v, want %v", e.ErrorRelatedTo, tt.wantRelatedTo)
			}

			// A per-message failure must not have appended anything to the store.
			if events := h.events(t); len(events) != 0 {
				t.Errorf("a failed message must not append an event, got %d", len(events))
			}
		})
	}
}

// TestPartialFailureInABatchIsStill200 proves the batch-level rule
// specifically: one bad message among good ones does not fail the request,
// does not stop the other message from being recorded, and both entries
// appear at their original positions.
func TestPartialFailureInABatchIsStill200(t *testing.T) {
	h := newHarness(t)
	rec := h.send(fixture(t, "mixed_success_and_error.json"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var body struct {
		Messages []struct {
			Status string `json:"Status"`
		} `json:"Messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Messages) != 2 {
		t.Fatalf("Messages = %+v", body.Messages)
	}
	if body.Messages[0].Status != "success" || body.Messages[1].Status != "error" {
		t.Errorf("statuses = %q, %q", body.Messages[0].Status, body.Messages[1].Status)
	}

	events := h.events(t)
	if len(events) != 1 {
		t.Fatalf("events = %d, want exactly the one good message recorded", len(events))
	}
}

// tinyBlobStore always reports the store is full, so
// TestOversizedAttachmentReportsCapacityExceeded can exercise the
// blob.ErrCapacityExceeded path deterministically rather than relying on the
// real memory store's byte accounting.
type tinyBlobStore struct{}

func (tinyBlobStore) Put(context.Context, io.Reader, blob.Ref) (blob.Ref, error) {
	return blob.Ref{}, blob.ErrCapacityExceeded
}
func (tinyBlobStore) Open(context.Context, string) (io.ReadSeekCloser, blob.Ref, error) {
	return nil, blob.Ref{}, blob.ErrNotFound
}
func (tinyBlobStore) Delete(context.Context, string) error { return blob.ErrNotFound }
func (tinyBlobStore) Stat(context.Context, string) (blob.Ref, error) {
	return blob.Ref{}, blob.ErrNotFound
}

func TestOversizedAttachmentReportsCapacityExceeded(t *testing.T) {
	deps := plugintest.NewDeps()
	deps.Blobs = tinyBlobStore{}
	mux := http.NewServeMux()
	mailjet.New().RegisterIngress(mux, deps)

	req := httptest.NewRequest(http.MethodPost, mailjet.SendPath, strings.NewReader(string(fixture(t, "attachments.json"))))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Messages []struct {
			Status string `json:"Status"`
			Errors []struct {
				ErrorCode  string `json:"ErrorCode"`
				StatusCode int    `json:"StatusCode"`
			} `json:"Errors"`
		} `json:"Messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Messages) != 1 || body.Messages[0].Status != "error" {
		t.Fatalf("Messages = %+v, body = %s", body.Messages, rec.Body.String())
	}
	if len(body.Messages[0].Errors) != 1 || body.Messages[0].Errors[0].StatusCode != http.StatusInsufficientStorage {
		t.Errorf("errors = %+v, want a capacity error", body.Messages[0].Errors)
	}
}

// TestEndToEndThroughARealServer exercises the provider the way a real SDK
// would: over the wire, through the shared ingress, into the real mail plugin
// API, proving read-your-own-write and that the provider composes with the
// rest of tommy rather than just with a bare mux.
func TestEndToEndThroughARealServer(t *testing.T) {
	in := testutil.Start(t, nil, mail.New(mailjet.New()))

	status, body := in.PostJSON(in.Ingress(mailjet.SendPath), json.RawMessage(fixture(t, "fanout.json")))
	if status != http.StatusOK {
		t.Fatalf("send status = %d: %s", status, body)
	}

	in.WaitForEvents(2, store.Query{Plugin: mail.PluginName, Provider: mailjet.ProviderName}, 2*time.Second)

	var views []mail.MessageView
	if status := in.GetJSON(in.API("/mail/messages"), &views); status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	if len(views) != 2 {
		t.Fatalf("expected the fan-out to produce two messages, got %d", len(views))
	}
	for _, v := range views {
		if v.Provider != mailjet.ProviderName {
			t.Errorf("provider = %q, want %q", v.Provider, mailjet.ProviderName)
		}
	}
}

// --- helpers ---

// configOf builds a plugin.ProviderConfig the way the TOML loader would, so
// tests exercise the exact same path a real config file takes.
func configOf(values map[string]any) plugin.ProviderConfig {
	return config.NewProviderConfig(values)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
