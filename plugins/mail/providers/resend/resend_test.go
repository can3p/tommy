package resend_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/mail"
	"github.com/can3p/tommy/plugins/mail/providers/resend"
)

// --- conformance -----------------------------------------------------------

func TestConformance(t *testing.T) {
	t.Parallel()
	plugintest.ConformanceProvider(t, resend.New())
	plugintest.Conformance(t, mail.New(resend.New()))
}

// --- harness ---------------------------------------------------------------

// harness mounts only the resend provider on a bare httptest.Server, with a
// deterministic id generator so the exact response body - which carries a
// minted UUID - can be asserted rather than merely pattern-matched.
type harness struct {
	t  *testing.T
	ts *httptest.Server
	d  plugin.Deps
}

// idFor is the email id the provider mints for the n-th event of a harness,
// counting from 1. It mirrors the encoding in id.go, spelled out here so the
// test would notice a silent change to it.
func idFor(n int) string {
	e := fmt.Sprintf("%024x", n)
	return e[0:8] + "-" + e[8:12] + "-4" + e[12:15] + "-a" + e[15:18] + "-" + e[18:24] + "facade"
}

func newHarness(t *testing.T, cfg plugin.ProviderConfig) *harness {
	t.Helper()
	d := plugintest.NewDeps().WithConfig(cfg)
	var n int
	d.NewID = func() string {
		n++
		return fmt.Sprintf("%024x", n)
	}
	mux := http.NewServeMux()
	resend.New().RegisterIngress(mux, d)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &harness{t: t, ts: ts, d: d}
}

func (h *harness) post(path string, body []byte, headers map[string]string) (*http.Response, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+path, bytes.NewReader(body))
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
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return resp, data
}

func (h *harness) send(body []byte, headers map[string]string) (*http.Response, []byte) {
	return h.post(resend.SendPath, body, headers)
}

func (h *harness) get(id string) (*http.Response, []byte) {
	h.t.Helper()
	resp, err := h.ts.Client().Get(h.ts.URL + "/emails/" + id)
	if err != nil {
		h.t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return resp, data
}

func (h *harness) events() []*event.Event {
	h.t.Helper()
	evs, err := h.d.Store.List(h.t.Context(), store.Query{Plugin: mail.PluginName, Provider: resend.ProviderName})
	if err != nil {
		h.t.Fatalf("list events: %v", err)
	}
	return evs
}

// messages returns the canonical messages this harness captured, oldest first,
// so a fixture's expectations line up with the order it was sent in.
func (h *harness) messages() []*mail.Message {
	h.t.Helper()
	evs := h.events()
	out := make([]*mail.Message, 0, len(evs))
	for i := len(evs) - 1; i >= 0; i-- {
		m, ok := mail.MessageOf(evs[i])
		if !ok {
			h.t.Fatalf("event %s carries no mail.Message", evs[i].ID)
		}
		out = append(out, m)
	}
	return out
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func addr(name, email string) mail.Address { return mail.Address{Name: name, Email: email} }

func addrsEq(a, b []mail.Address) bool {
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

// --- the success contract --------------------------------------------------

// TestSendResponseContract pins the exact wire response of POST /emails: 200,
// application/json, and a body of nothing but the minted id. Resend answers
// 200 here, not 201 or 202.
func TestSendResponseContract(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})

	resp, body := h.send(loadFixture(t, "send_basic.json"),
		map[string]string{"Authorization": "Bearer re_fake_key"})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	want := `{"id":"` + idFor(1) + `"}` + "\n"
	if string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// --- request -> canonical model, over the golden fixtures ------------------

func TestSendTranslatesToCanonicalMessage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		fixture string
		want    mail.Message
		// check runs extra assertions that do not fit the Message comparison.
		check func(t *testing.T, h *harness, ev *event.Event, m *mail.Message)
	}{
		{
			name:    "basic",
			fixture: "send_basic.json",
			want: mail.Message{
				From:    addr("Acme", "alice@example.com"),
				To:      []mail.Address{addr("", "bob@example.com")},
				Subject: "Hello from tommy",
				HTML:    "<p>It <b>works</b>.</p>",
				Text:    "It works.",
			},
		},
		{
			// The union's string spelling: every recipient field a bare
			// string, which is what the REST reference documents first.
			name:    "recipients as bare strings",
			fixture: "send_string_recipients.json",
			want: mail.Message{
				From:    addr("", "alice@example.com"),
				To:      []mail.Address{addr("", "bob@example.com")},
				Cc:      []mail.Address{addr("", "carol@example.com")},
				Bcc:     []mail.Address{addr("", "dave@example.com")},
				ReplyTo: []mail.Address{addr("Support", "support@example.com")},
				Subject: "Everything is a bare string",
				Text:    "one address per field",
			},
		},
		{
			// The union's array spelling, including a display name inside an
			// array element.
			name:    "recipients as arrays",
			fixture: "send_array_recipients.json",
			want: mail.Message{
				From:    addr("Acme", "alice@example.com"),
				To:      []mail.Address{addr("", "bob@example.com"), addr("Bobby", "bobby@example.com")},
				Cc:      []mail.Address{addr("", "carol@example.com")},
				Bcc:     []mail.Address{addr("", "dave@example.com"), addr("", "erin@example.com")},
				ReplyTo: []mail.Address{addr("", "support@example.com"), addr("", "help@example.com")},
				Subject: "Everything is an array",
				Text:    "several addresses per field",
			},
		},
		{
			// Exactly what resend-go marshals: arrays for to/cc/bcc, a bare
			// string for reply_to, headers as a flat map, and an attachment
			// whose content is a JSON array of byte values rather than base64.
			name:    "resend-go wire shape",
			fixture: "send_go_sdk.json",
			want: mail.Message{
				From:    addr("Acme", "alice@example.com"),
				To:      []mail.Address{addr("", "bob@example.com")},
				Cc:      []mail.Address{addr("", "carol@example.com")},
				Bcc:     []mail.Address{addr("", "dave@example.com")},
				ReplyTo: []mail.Address{addr("", "support@example.com")},
				Subject: "What resend-go puts on the wire",
				HTML:    "<p>hi</p>",
				Text:    "hi",
			},
			check: func(t *testing.T, h *harness, ev *event.Event, m *mail.Message) {
				if got := m.Headers.Get("X-Entity-Ref-ID"); got != "order-9001" {
					t.Errorf("header X-Entity-Ref-ID = %q, want order-9001", got)
				}
				if len(m.Attachments) != 1 {
					t.Fatalf("attachments = %d, want 1", len(m.Attachments))
				}
				if got := blobBytes(t, h, m.Attachments[0]); got != "Hello" {
					t.Errorf("attachment bytes = %q, want Hello", got)
				}
				if got := ev.Meta["scheduled_at"]; got != "2026-08-05T11:52:01.858Z" {
					t.Errorf("meta scheduled_at = %v", got)
				}
				if got := ev.Meta["topic_id"]; got != "3d5aa1cd-1234-4f00-bc7f-1d0dd3a0e1aa" {
					t.Errorf("meta topic_id = %v", got)
				}
				if ev.Meta["tags"] == nil {
					t.Error("meta tags missing")
				}
			},
		},
		{
			name:    "template with no body",
			fixture: "send_template.json",
			want: mail.Message{
				From: addr("Acme", "alice@example.com"),
				To:   []mail.Address{addr("", "bob@example.com")},
			},
			check: func(t *testing.T, _ *harness, ev *event.Event, _ *mail.Message) {
				if ev.Meta["template"] == nil {
					t.Error("meta template missing: a template send must still record what was asked for")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, plugin.ProviderConfig{})

			resp, body := h.send(loadFixture(t, tc.fixture), nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
			}

			evs := h.events()
			if len(evs) != 1 {
				t.Fatalf("events = %d, want 1", len(evs))
			}
			m, ok := mail.MessageOf(evs[0])
			if !ok {
				t.Fatal("event carries no mail.Message")
			}

			if m.From != tc.want.From {
				t.Errorf("from = %+v, want %+v", m.From, tc.want.From)
			}
			if !addrsEq(m.To, tc.want.To) {
				t.Errorf("to = %+v, want %+v", m.To, tc.want.To)
			}
			if !addrsEq(m.Cc, tc.want.Cc) {
				t.Errorf("cc = %+v, want %+v", m.Cc, tc.want.Cc)
			}
			if !addrsEq(m.Bcc, tc.want.Bcc) {
				t.Errorf("bcc = %+v, want %+v", m.Bcc, tc.want.Bcc)
			}
			if !addrsEq(m.ReplyTo, tc.want.ReplyTo) {
				t.Errorf("reply_to = %+v, want %+v", m.ReplyTo, tc.want.ReplyTo)
			}
			if m.Subject != tc.want.Subject {
				t.Errorf("subject = %q, want %q", m.Subject, tc.want.Subject)
			}
			if m.HTML != tc.want.HTML {
				t.Errorf("html = %q, want %q", m.HTML, tc.want.HTML)
			}
			if m.Text != tc.want.Text {
				t.Errorf("text = %q, want %q", m.Text, tc.want.Text)
			}
			if evs[0].Raw.Method != http.MethodPost || evs[0].Raw.Path != resend.SendPath || len(evs[0].Raw.Body) == 0 {
				t.Errorf("Raw not populated: %+v", evs[0].Raw)
			}
			if tc.check != nil {
				tc.check(t, h, evs[0], m)
			}
		})
	}
}

// blobBytes reads an attachment back out of the blob store.
func blobBytes(t *testing.T, h *harness, a mail.Attachment) string {
	t.Helper()
	rc, _, err := h.d.Blobs.Open(t.Context(), a.Blob.ID)
	if err != nil {
		t.Fatalf("blob open %s: %v", a.Blob.ID, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	return string(data)
}

// --- attachments -----------------------------------------------------------

// TestAttachmentSpellings covers all three ways `content` reaches the wire and
// the `path` form tommy refuses to fetch.
func TestAttachmentSpellings(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})

	resp, body := h.send(loadFixture(t, "send_attachments.json"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
	}

	m := h.messages()[0]
	if len(m.Attachments) != 3 {
		t.Fatalf("attachments = %d, want 3 (the fourth names a URL and must not be stored)", len(m.Attachments))
	}
	if got := blobBytes(t, h, m.Attachments[0]); got != "Hello" {
		t.Errorf("base64 attachment = %q, want Hello", got)
	}
	if got := blobBytes(t, h, m.Attachments[1]); got != "Hi" {
		t.Errorf("Buffer attachment = %q, want Hi", got)
	}
	if !m.Attachments[2].Inline || m.Attachments[2].ContentID != "logo" {
		t.Errorf("content_id attachment should be inline with cid logo, got %+v", m.Attachments[2])
	}

	// The `path` attachment is recorded and never fetched: tommy makes no
	// outbound requests at all.
	remote, ok := h.events()[0].Meta["remote_attachments"]
	if !ok {
		t.Fatal("meta remote_attachments missing")
	}
	encoded, err := json.Marshal(remote)
	if err != nil {
		t.Fatalf("marshal remote attachments: %v", err)
	}
	if !strings.Contains(string(encoded), "https://example.com/invoice.pdf") {
		t.Errorf("remote attachment URL not recorded: %s", encoded)
	}
	for _, a := range m.Attachments {
		if a.Filename == "invoice.pdf" {
			t.Error("a path attachment was turned into a blob; tommy must never fetch it")
		}
	}
}

// --- batch -----------------------------------------------------------------

// TestBatchFansOut checks the index-aligned response and that each element
// becomes its own event, which is the mail plugin's "one delivered message,
// not one API request" rule.
func TestBatchFansOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})

	resp, body := h.post(resend.BatchPath, loadFixture(t, "batch.json"),
		map[string]string{"Idempotency-Key": "order-9001"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
	}

	want := `{"data":[{"id":"` + idFor(1) + `"},{"id":"` + idFor(2) + `"}]}` + "\n"
	if string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}

	msgs := h.messages()
	if len(msgs) != 2 {
		t.Fatalf("events = %d, want 2", len(msgs))
	}
	if msgs[0].Subject != "First" || msgs[0].Text != "one" {
		t.Errorf("first message = %+v", msgs[0])
	}
	if msgs[1].Subject != "Second" || msgs[1].HTML != "<p>two</p>" {
		t.Errorf("second message = %+v", msgs[1])
	}
	if !addrsEq(msgs[1].To, []mail.Address{addr("", "carol@example.com"), addr("", "dave@example.com")}) {
		t.Errorf("second message to = %+v", msgs[1].To)
	}

	// The idempotency key is recorded on every fanned-out event and acted on
	// by none of them.
	for _, ev := range h.events() {
		if ev.Meta["idempotency_key"] != "order-9001" {
			t.Errorf("event %s did not record the idempotency key: %v", ev.ID, ev.Meta)
		}
	}
}

// TestBatchTooLarge rejects a batch past the documented 100.
func TestBatchTooLarge(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})

	one := `{"from":"alice@example.com","to":"bob@example.com","subject":"s","text":"t"}`
	body := "[" + strings.Repeat(one+",", resend.MaxBatch) + one + "]"

	resp, out := h.post(resend.BatchPath, []byte(body), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", resp.StatusCode, out)
	}
	if len(h.events()) != 0 {
		t.Errorf("a rejected batch appended %d events", len(h.events()))
	}
}

// TestBatchIsAllOrNothing proves the strict default: one bad element and
// nothing at all is captured.
func TestBatchIsAllOrNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})

	body := `[{"from":"alice@example.com","to":"bob@example.com","subject":"ok","text":"t"},` +
		`{"from":"alice@example.com","subject":"no recipient","text":"t"}]`

	resp, out := h.post(resend.BatchPath, []byte(body), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", resp.StatusCode, out)
	}
	if len(h.events()) != 0 {
		t.Errorf("half a batch was stored: %d events", len(h.events()))
	}
}

// --- errors ----------------------------------------------------------------

// TestErrorResponses drives every error this fake produces and asserts the
// exact status and body, since {name, message, statusCode} is the shape both
// official SDKs decode.
func TestErrorResponses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		fixture    string
		rawBody    string
		headers    map[string]string
		cfg        plugin.ProviderConfig
		wantStatus int
		wantBody   string
	}{
		{
			name:       "missing from",
			fixture:    "error_missing_from.json",
			wantStatus: 422,
			wantBody:   `{"name":"missing_required_field","message":"Missing ` + "`from`" + ` field.","statusCode":422}`,
		},
		{
			name:       "missing to",
			fixture:    "error_missing_to.json",
			wantStatus: 422,
			wantBody:   `{"name":"missing_required_field","message":"Missing ` + "`to`" + ` field.","statusCode":422}`,
		},
		{
			name:       "missing subject",
			fixture:    "error_missing_subject.json",
			wantStatus: 422,
			wantBody:   `{"name":"missing_required_field","message":"Missing ` + "`subject`" + ` field.","statusCode":422}`,
		},
		{
			name:       "unparseable from",
			fixture:    "error_bad_from.json",
			wantStatus: 422,
			wantBody: `{"name":"invalid_parameter","message":"Invalid ` + "`from`" +
				` field. The email address needs to follow the ` + "`email@example.com`" +
				` or ` + "`Name <email@example.com>`" + ` format.","statusCode":422}`,
		},
		{
			name:       "unparseable to",
			fixture:    "error_bad_to.json",
			wantStatus: 422,
			wantBody: `{"name":"invalid_parameter","message":"Invalid ` + "`to`" +
				` field. The email address needs to follow the ` + "`email@example.com`" +
				` or ` + "`Name <email@example.com>`" + ` format.","statusCode":422}`,
		},
		{
			name:       "attachment with neither content nor path",
			fixture:    "error_bad_attachment.json",
			wantStatus: 422,
			wantBody:   `{"name":"invalid_attachment","message":"Attachment must have either a ` + "`content`" + ` or ` + "`path`" + `.","statusCode":422}`,
		},
		{
			name:       "template combined with html",
			fixture:    "error_template_conflict.json",
			wantStatus: 422,
			wantBody:   `{"name":"validation_error","message":"The ` + "`template`" + ` field cannot be combined with ` + "`html`" + ` or ` + "`text`" + `.","statusCode":422}`,
		},
		{
			name:       "malformed json",
			rawBody:    `{"from": `,
			wantStatus: 400,
			wantBody:   `{"name":"validation_error","message":"An error was found with one or more fields in the request.","statusCode":400}`,
		},
		{
			name:       "idempotency key too long",
			fixture:    "send_basic.json",
			headers:    map[string]string{"Idempotency-Key": strings.Repeat("k", resend.MaxIdempotencyKey+1)},
			wantStatus: 400,
			wantBody:   `{"name":"invalid_idempotency_key","message":"Idempotency keys, if present, must have between 1 and 256 characters.","statusCode":400}`,
		},
		{
			name:       "pinned key, none presented",
			fixture:    "send_basic.json",
			cfg:        config.NewProviderConfig(map[string]any{"api_key": "re_expected"}),
			wantStatus: 401,
			wantBody:   `{"name":"missing_api_key","message":"Missing API key in the authorization header.","statusCode":401}`,
		},
		{
			name:       "pinned key, wrong one presented",
			fixture:    "send_basic.json",
			headers:    map[string]string{"Authorization": "Bearer re_wrong"},
			cfg:        config.NewProviderConfig(map[string]any{"api_key": "re_expected"}),
			wantStatus: 401,
			wantBody:   `{"name":"validation_error","message":"API key is invalid","statusCode":401}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, tc.cfg)

			body := []byte(tc.rawBody)
			if tc.fixture != "" {
				body = loadFixture(t, tc.fixture)
			}
			resp, got := h.send(body, tc.headers)

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			if strings.TrimSpace(string(got)) != tc.wantBody {
				t.Errorf("body  = %s\nwant  = %s", strings.TrimSpace(string(got)), tc.wantBody)
			}
			if len(h.events()) != 0 {
				t.Errorf("a rejected request appended %d events", len(h.events()))
			}
		})
	}
}

// TestAuthIsRecordedAndAcceptedByDefault is rule 1: no config, no rejection,
// and whatever was presented is on the event.
func TestAuthIsRecordedAndAcceptedByDefault(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})

	resp, body := h.send(loadFixture(t, "send_basic.json"),
		map[string]string{"Authorization": "Bearer re_whatever_you_like"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
	}
	if got := h.events()[0].Meta["authorization"]; got != "Bearer re_whatever_you_like" {
		t.Errorf("meta authorization = %v", got)
	}
}

// TestPinnedKeyAccepted is the other half: the matching key still works.
func TestPinnedKeyAccepted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, config.NewProviderConfig(map[string]any{"api_key": "re_expected"}))

	resp, body := h.send(loadFixture(t, "send_basic.json"),
		map[string]string{"Authorization": "Bearer re_expected"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
	}
}

// --- read-back -------------------------------------------------------------

// TestRetrieveServesFromTheStore pins the exact retrieve body. It also proves
// the id encoding round-trips: the only thing connecting the send response to
// this fetch is emailIDFor being reversible.
func TestRetrieveServesFromTheStore(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})

	_, sent := h.send(loadFixture(t, "send_go_sdk.json"), nil)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(sent, &created); err != nil {
		t.Fatalf("decode send response: %v", err)
	}

	resp, body := h.get(created.ID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, body)
	}

	// plugintest's clock is fixed at 2024-01-01T12:00:00Z, so the timestamp
	// is asserted verbatim - including the space separator and "+00" offset
	// the live reference's own example uses instead of RFC 3339.
	want := `{"object":"email",` +
		`"id":"` + created.ID + `",` +
		`"message_id":"<` + created.ID + `@example.com>",` +
		`"to":["bob@example.com"],` +
		`"from":"Acme <alice@example.com>",` +
		`"created_at":"2024-01-01 12:00:00.000000+00",` +
		`"subject":"What resend-go puts on the wire",` +
		`"html":"<p>hi</p>",` +
		`"text":"hi",` +
		`"bcc":["dave@example.com"],` +
		`"cc":["carol@example.com"],` +
		`"reply_to":["support@example.com"],` +
		`"last_event":"delivered",` +
		`"scheduled_at":"2026-08-05T11:52:01.858Z",` +
		`"tags":[{"name":"category","value":"confirm_email"}]}`
	if strings.TrimSpace(string(body)) != want {
		t.Errorf("body = %s\nwant = %s", strings.TrimSpace(string(body)), want)
	}
}

// TestRetrieveNullsAbsentBodies checks that a field the sender left out comes
// back null rather than "", which is how the live example renders it.
func TestRetrieveNullsAbsentBodies(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})

	body := `{"from":"alice@example.com","to":"bob@example.com","subject":"text only","text":"hi"}`
	_, sent := h.send([]byte(body), nil)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(sent, &created); err != nil {
		t.Fatalf("decode send response: %v", err)
	}

	_, got := h.get(created.ID)
	var res map[string]any
	if err := json.Unmarshal(got, &res); err != nil {
		t.Fatalf("decode retrieve response: %v", err)
	}
	if res["html"] != nil {
		t.Errorf("html = %v, want null", res["html"])
	}
	if res["text"] != "hi" {
		t.Errorf("text = %v, want hi", res["text"])
	}
	if res["scheduled_at"] != nil {
		t.Errorf("scheduled_at = %v, want null", res["scheduled_at"])
	}
	if tags, ok := res["tags"].([]any); !ok || len(tags) != 0 {
		t.Errorf("tags = %v, want []", res["tags"])
	}
}

// TestRetrieveLastEventIsConfigurable covers the one knob GET has.
func TestRetrieveLastEventIsConfigurable(t *testing.T) {
	t.Parallel()
	h := newHarness(t, config.NewProviderConfig(map[string]any{"last_event": "bounced"}))

	_, sent := h.send(loadFixture(t, "send_basic.json"), nil)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(sent, &created); err != nil {
		t.Fatalf("decode send response: %v", err)
	}
	_, got := h.get(created.ID)
	var res map[string]any
	if err := json.Unmarshal(got, &res); err != nil {
		t.Fatalf("decode retrieve response: %v", err)
	}
	if res["last_event"] != "bounced" {
		t.Errorf("last_event = %v, want bounced", res["last_event"])
	}
}

// TestRetrieveErrors separates "not here" from "not an id".
func TestRetrieveErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})

	cases := []struct {
		name       string
		id         string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "well-formed uuid nobody minted",
			id:         "49a3999c-0ce1-4ea6-ab68-afcd6dc2e794",
			wantStatus: 404,
			wantBody:   `{"name":"not_found","message":"Email not found","statusCode":404}`,
		},
		{
			name:       "our own encoding, but no such event",
			id:         idFor(4242),
			wantStatus: 404,
			wantBody:   `{"name":"not_found","message":"Email not found","statusCode":404}`,
		},
		{
			name:       "not a uuid at all",
			id:         "definitely-not-a-uuid",
			wantStatus: 422,
			wantBody:   `{"name":"invalid_parameter","message":"The ` + "`id`" + ` must be a valid UUID.","statusCode":422}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := h.get(tc.id)
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", resp.StatusCode, tc.wantStatus, body)
			}
			if strings.TrimSpace(string(body)) != tc.wantBody {
				t.Errorf("body = %s\nwant = %s", strings.TrimSpace(string(body)), tc.wantBody)
			}
		})
	}
}

// --- full stack ------------------------------------------------------------

// TestEndToEndThroughTheRealServer boots the whole server, with the real id
// generator, and drives send -> retrieve the way an SDK would. The unit tests
// pin ids to a counter; this one proves the encoding survives an actual
// 24-hex event id.
func TestEndToEndThroughTheRealServer(t *testing.T) {
	in := testutil.Start(t, nil, mail.New(resend.New()))

	req, err := http.NewRequest(http.MethodPost, in.Ingress(resend.SendPath),
		bytes.NewReader(loadFixture(t, "send_basic.json")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer re_live")
	resp := in.Do(req)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("send status = %d, body = %s", resp.StatusCode, body)
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode send response: %v", err)
	}
	if len(created.ID) != 36 || !strings.HasSuffix(created.ID, "facade") {
		t.Fatalf("id %q is not the UUID shape a Resend client expects", created.ID)
	}

	status, fetched := in.GetBody(in.Ingress("/emails/" + created.ID))
	if status != http.StatusOK {
		t.Fatalf("retrieve status = %d, body = %s", status, fetched)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(fetched), &res); err != nil {
		t.Fatalf("decode retrieve response: %v", err)
	}
	if res["id"] != created.ID {
		t.Errorf("retrieved id = %v, want %v", res["id"], created.ID)
	}
	if res["subject"] != "Hello from tommy" {
		t.Errorf("retrieved subject = %v", res["subject"])
	}
	if res["object"] != "email" {
		t.Errorf("retrieved object = %v, want email", res["object"])
	}

	// The mail plugin's own API sees the same message, which is the point of
	// capturing it at all.
	status, listed := in.GetBody(in.API("/mail/messages?provider=" + resend.ProviderName))
	if status != http.StatusOK {
		t.Fatalf("api list status = %d, body = %s", status, listed)
	}
	if !strings.Contains(listed, "Hello from tommy") {
		t.Errorf("mail API did not list the captured message: %s", listed)
	}
}
