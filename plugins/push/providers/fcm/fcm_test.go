package fcm_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/plugins/push"
	"github.com/can3p/tommy/plugins/push/providers/fcm"
)

// --- conformance -------------------------------------------------------

func TestConformance(t *testing.T) {
	t.Parallel()
	plugintest.ConformanceProvider(t, fcm.New())
	plugintest.Conformance(t, push.New(fcm.New()))
}

// --- test harness --------------------------------------------------------

// harness wraps a bare httptest.Server mounting only the fcm provider, plus
// the Deps it was mounted with, so a test can drive it over real HTTP and
// inspect the store afterwards.
type harness struct {
	t  *testing.T
	ts *httptest.Server
	d  plugin.Deps
}

func newHarness(t *testing.T, cfg plugin.ProviderConfig) *harness {
	t.Helper()
	d := plugintest.NewDeps().WithConfig(cfg)
	mux := http.NewServeMux()
	fcm.New().RegisterIngress(mux, d)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &harness{t: t, ts: ts, d: d}
}

// send posts a raw body to the send route for the given project, with any
// extra headers.
func (h *harness) send(project string, body []byte, headers map[string]string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+"/v1/projects/"+project+"/messages:send", bytes.NewReader(body))
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

func (h *harness) events() []*event.Event {
	h.t.Helper()
	evs, err := h.d.Store.List(h.t.Context(), store.Query{Plugin: push.Name, Provider: fcm.ProviderName})
	if err != nil {
		h.t.Fatalf("list events: %v", err)
	}
	return evs
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

// --- success shape ---------------------------------------------------------

func TestSuccessResponseShape(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})
	resp := h.send("my-project", loadFixture(t, "notification.json"), map[string]string{"Authorization": "Bearer any-token"})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, readAll(t, resp.Body))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var body struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(readAll(t, resp.Body), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	evs := h.events()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	want := "projects/my-project/messages/0:" + string(evs[0].ID)
	if body.Name != want {
		t.Errorf("name = %q, want %q", body.Name, want)
	}
}

// --- table-driven message conversion ---------------------------------------

type wantMessage struct {
	kind         push.Kind
	displays     bool
	targetKind   push.TargetKind
	targetValue  string
	targetSource string
	app          string
	title        string
	body         string
	image        string
	sound        string
	badge        *int
	priority     push.Priority
	collapseKey  string
	ttlSeconds   int64
	dataJSON     string
	formats      []push.Format
}

func intp(n int) *int { return &n }

func TestMessageConversion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		project string
		fixture string
		want    wantMessage
	}{
		{
			name:    "notification to a topic",
			project: "my-project",
			fixture: "notification.json",
			want: wantMessage{
				kind: push.KindNotification, displays: true,
				targetKind: push.TargetTopic, targetValue: "weather", targetSource: "topic",
				app: "my-project", title: "Storm warning", body: "Batten down the hatches",
				image:   "https://example.com/storm.png",
				formats: []push.Format{push.FormatFCM},
			},
		},
		{
			name:    "data-only message is silent",
			project: "my-project",
			fixture: "data_only.json",
			want: wantMessage{
				kind: push.KindSilent, displays: false,
				targetKind: push.TargetDevice, targetValue: "cQAdeviceRegistrationTokenExample000111222333444555", targetSource: "token",
				app:      "my-project",
				dataJSON: `{"kind":"refresh","seq":"42"}`,
				formats:  []push.Format{push.FormatFCM},
			},
		},
		{
			name:    "fid targeting",
			project: "proj-2",
			fixture: "target_fid.json",
			want: wantMessage{
				kind: push.KindNotification, displays: true,
				targetKind: push.TargetDevice, targetValue: "fis-installation-id-example-000111222", targetSource: "fid",
				app: "proj-2", title: "Hello", body: "Addressed by Firebase Installation ID",
				formats: []push.Format{push.FormatFCM},
			},
		},
		{
			name:    "android/apns/webpush overrides land as separate payloads",
			project: "my-project",
			fixture: "overrides.json",
			want: wantMessage{
				kind: push.KindNotification, displays: true,
				targetKind: push.TargetDevice, targetValue: "cQAdeviceRegistrationTokenExample000111222333444555", targetSource: "token",
				app: "my-project",
				// android.notification overrides title and sets sound, but
				// leaves body alone since it did not set one itself.
				title: "Android title", body: "Base body", sound: "default",
				badge:       intp(3),
				priority:    push.PriorityHigh,
				collapseKey: "weather",
				ttlSeconds:  3600,
				formats: []push.Format{
					push.FormatFCM, push.FormatFCMAndroid, push.FormatFCMApns, push.FormatFCMWebpush,
				},
			},
		},
		{
			name:    "android.data overrides message.data",
			project: "my-project",
			fixture: "data_override.json",
			want: wantMessage{
				kind: push.KindSilent, displays: false,
				targetKind: push.TargetTopic, targetValue: "weather", targetSource: "topic",
				app:      "my-project",
				dataJSON: `{"a":"android-level","b":"extra"}`,
				formats:  []push.Format{push.FormatFCM, push.FormatFCMAndroid},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, plugin.ProviderConfig{})
			resp := h.send(c.project, loadFixture(t, c.fixture), nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, readAll(t, resp.Body))
			}
			resp.Body.Close()

			evs := h.events()
			if len(evs) != 1 {
				t.Fatalf("events = %d, want 1", len(evs))
			}
			m, ok := push.MessageOf(evs[0])
			if !ok {
				t.Fatalf("event carries no push message")
			}

			w := c.want
			if m.Kind != w.kind {
				t.Errorf("Kind = %q, want %q", m.Kind, w.kind)
			}
			if m.Displays() != w.displays {
				t.Errorf("Displays() = %v, want %v", m.Displays(), w.displays)
			}
			if m.Target.Kind != w.targetKind || m.Target.Value != w.targetValue || m.Target.Source != w.targetSource {
				t.Errorf("Target = %+v, want {%s %s %s}", m.Target, w.targetKind, w.targetValue, w.targetSource)
			}
			if m.App != w.app {
				t.Errorf("App = %q, want %q", m.App, w.app)
			}
			if w.title != "" || w.body != "" || w.image != "" || w.sound != "" || w.badge != nil {
				if m.Alert == nil {
					t.Fatalf("Alert = nil, want non-nil")
				}
				if m.Alert.Title != w.title {
					t.Errorf("Alert.Title = %q, want %q", m.Alert.Title, w.title)
				}
				if m.Alert.Body != w.body {
					t.Errorf("Alert.Body = %q, want %q", m.Alert.Body, w.body)
				}
				if m.Alert.Image != w.image {
					t.Errorf("Alert.Image = %q, want %q", m.Alert.Image, w.image)
				}
				if m.Alert.Sound != w.sound {
					t.Errorf("Alert.Sound = %q, want %q", m.Alert.Sound, w.sound)
				}
				if (m.Alert.Badge == nil) != (w.badge == nil) {
					t.Errorf("Alert.Badge = %v, want %v", m.Alert.Badge, w.badge)
				} else if w.badge != nil && *m.Alert.Badge != *w.badge {
					t.Errorf("Alert.Badge = %d, want %d", *m.Alert.Badge, *w.badge)
				}
			}
			if w.priority != "" && m.Delivery.Priority != w.priority {
				t.Errorf("Delivery.Priority = %q, want %q", m.Delivery.Priority, w.priority)
			}
			if w.collapseKey != "" && m.Delivery.CollapseKey != w.collapseKey {
				t.Errorf("Delivery.CollapseKey = %q, want %q", m.Delivery.CollapseKey, w.collapseKey)
			}
			if w.ttlSeconds != 0 {
				if m.Delivery.Expiry == nil || m.Delivery.Expiry.TTLSeconds == nil {
					t.Fatalf("Delivery.Expiry.TTLSeconds = nil, want %d", w.ttlSeconds)
				}
				if *m.Delivery.Expiry.TTLSeconds != w.ttlSeconds {
					t.Errorf("Delivery.Expiry.TTLSeconds = %d, want %d", *m.Delivery.Expiry.TTLSeconds, w.ttlSeconds)
				}
			}
			if w.dataJSON != "" {
				var got, want any
				if err := json.Unmarshal(m.Data, &got); err != nil {
					t.Fatalf("decode Data: %v", err)
				}
				if err := json.Unmarshal([]byte(w.dataJSON), &want); err != nil {
					t.Fatalf("decode want dataJSON: %v", err)
				}
				gj, _ := json.Marshal(got)
				wj, _ := json.Marshal(want)
				if string(gj) != string(wj) {
					t.Errorf("Data = %s, want %s", gj, wj)
				}
			}
			gotFormats := make([]push.Format, 0, len(m.Payloads))
			for _, p := range m.Payloads {
				gotFormats = append(gotFormats, p.Format)
			}
			if len(gotFormats) != len(w.formats) {
				t.Fatalf("Payload formats = %v, want %v", gotFormats, w.formats)
			}
			for i := range w.formats {
				if gotFormats[i] != w.formats[i] {
					t.Errorf("Payload[%d].Format = %q, want %q", i, gotFormats[i], w.formats[i])
				}
			}
		})
	}
}

// android.notification.notificationPriority must never end up as
// Delivery.Priority - it is display prominence, not delivery urgency.
func TestNotificationPriorityIsNotDeliveryPriority(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})
	resp := h.send("my-project", loadFixture(t, "overrides.json"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	evs := h.events()
	m, ok := push.MessageOf(evs[0])
	if !ok {
		t.Fatalf("event carries no push message")
	}
	// android.priority in the fixture is "HIGH" and
	// android.notification.notificationPriority is "PRIORITY_HIGH" - only the
	// former may end up normalized onto Delivery.Priority.
	if m.Delivery.Priority != push.PriorityHigh {
		t.Fatalf("Delivery.Priority = %q, want %q", m.Delivery.Priority, push.PriorityHigh)
	}
	if m.Delivery.PriorityRaw != "HIGH" {
		t.Errorf("Delivery.PriorityRaw = %q, want %q (not PRIORITY_HIGH)", m.Delivery.PriorityRaw, "HIGH")
	}
}

// --- validateOnly ------------------------------------------------------

func TestValidateOnlyIsRecordedNotSkipped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})
	resp := h.send("my-project", loadFixture(t, "validate_only.json"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, readAll(t, resp.Body))
	}
	resp.Body.Close()

	evs := h.events()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1 - validateOnly must still be captured", len(evs))
	}
	if v, _ := evs[0].Meta["validate_only"].(bool); !v {
		t.Errorf("Meta[validate_only] = %v, want true", evs[0].Meta["validate_only"])
	}
}

// --- auth ----------------------------------------------------------------

func TestAuthAcceptsAnyTokenByDefault(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})
	resp := h.send("my-project", loadFixture(t, "notification.json"), map[string]string{"Authorization": "Bearer whatever-anyone-typed"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	evs := h.events()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if got := evs[0].Meta["authorization"]; got != "Bearer whatever-anyone-typed" {
		t.Errorf("Meta[authorization] = %v, want recorded verbatim", got)
	}
}

func TestAuthRejectsMismatchWhenPinned(t *testing.T) {
	t.Parallel()
	cfg := config.NewProviderConfig(map[string]any{"bearer_token": "expected-token"})
	h := newHarness(t, cfg)

	resp := h.send("my-project", loadFixture(t, "notification.json"), map[string]string{"Authorization": "Bearer wrong-token"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(readAll(t, resp.Body), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Status != "UNAUTHENTICATED" {
		t.Errorf("error.status = %q, want UNAUTHENTICATED", body.Error.Status)
	}
	if body.Error.Code != 401 {
		t.Errorf("error.code = %d, want 401", body.Error.Code)
	}
	if len(h.events()) != 0 {
		t.Errorf("events = %d, want 0 - a rejected auth must not be recorded", len(h.events()))
	}
}

func TestAuthAcceptsMatchWhenPinned(t *testing.T) {
	t.Parallel()
	cfg := config.NewProviderConfig(map[string]any{"bearer_token": "expected-token"})
	h := newHarness(t, cfg)

	resp := h.send("my-project", loadFixture(t, "notification.json"), map[string]string{"Authorization": "Bearer expected-token"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// --- error shapes ------------------------------------------------------

func TestErrorShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		fixture       string
		wantStatus    int
		wantErrStatus string
	}{
		{"malformed json", "error_bad_json.json", http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"missing message", "error_missing_message.json", http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"no target specified", "error_no_target.json", http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"multiple targets specified", "error_multiple_targets.json", http.StatusBadRequest, "INVALID_ARGUMENT"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, plugin.ProviderConfig{})
			resp := h.send("my-project", loadFixture(t, c.fixture), nil)
			if resp.StatusCode != c.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, c.wantStatus, readAll(t, resp.Body))
			}
			var body struct {
				Error struct {
					Code    int    `json:"code"`
					Status  string `json:"status"`
					Message string `json:"message"`
					Details []struct {
						Type            string `json:"@type"`
						FieldViolations []struct {
							Field       string `json:"field"`
							Description string `json:"description"`
						} `json:"fieldViolations"`
					} `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(readAll(t, resp.Body), &body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if body.Error.Status != c.wantErrStatus {
				t.Errorf("error.status = %q, want %q", body.Error.Status, c.wantErrStatus)
			}
			if body.Error.Code != c.wantStatus {
				t.Errorf("error.code = %d, want %d", body.Error.Code, c.wantStatus)
			}
			if body.Error.Message == "" {
				t.Errorf("error.message is empty")
			}
			if len(h.events()) != 0 {
				t.Errorf("events = %d, want 0 - a rejected request must not be recorded", len(h.events()))
			}
		})
	}
}

// A field-violation detail (google.rpc.BadRequest) is present for a
// structured validation failure, matching the shape documented at
// https://firebase.google.com/docs/cloud-messaging/error-codes.
func TestErrorDetailsCarryFieldViolation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})
	resp := h.send("my-project", loadFixture(t, "error_no_target.json"), nil)
	defer resp.Body.Close()

	var body struct {
		Error struct {
			Details []struct {
				Type            string `json:"@type"`
				FieldViolations []struct {
					Field       string `json:"field"`
					Description string `json:"description"`
				} `json:"fieldViolations"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(readAll(t, resp.Body), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if len(body.Error.Details) != 1 {
		t.Fatalf("details = %d, want 1", len(body.Error.Details))
	}
	if body.Error.Details[0].Type != "type.googleapis.com/google.rpc.BadRequest" {
		t.Errorf("details[0].@type = %q, want google.rpc.BadRequest", body.Error.Details[0].Type)
	}
	if len(body.Error.Details[0].FieldViolations) == 0 {
		t.Errorf("fieldViolations is empty")
	}
}

// --- snake_case / camelCase duality -----------------------------------

// dualSpellingSnapshot is the part of a push.Message that must come out
// identical regardless of which spelling a client used - everything except
// Payloads, whose raw bytes are expected to differ (they are verbatim, and
// the two fixtures are not byte-identical inputs).
type dualSpellingSnapshot struct {
	Kind         push.Kind
	TargetKind   push.TargetKind
	TargetValue  string
	TargetSource string
	Title        string
	Body         string
	TitleLocKey  string
	TitleLocArgs []string
	BodyLocKey   string
	BodyLocArgs  []string
	Badge        int
	Priority     push.Priority
	CollapseKey  string
	TTLSeconds   int64
	PayloadCount int
}

func snapshotOf(t *testing.T, m *push.Message) dualSpellingSnapshot {
	t.Helper()
	s := dualSpellingSnapshot{
		Kind:         m.Kind,
		TargetKind:   m.Target.Kind,
		TargetValue:  m.Target.Value,
		TargetSource: m.Target.Source,
		Priority:     m.Delivery.Priority,
		CollapseKey:  m.Delivery.CollapseKey,
		PayloadCount: len(m.Payloads),
	}
	if m.Alert != nil {
		s.Title = m.Alert.Title
		s.Body = m.Alert.Body
		if m.Alert.Badge != nil {
			s.Badge = *m.Alert.Badge
		}
		if m.Alert.Localization != nil {
			s.TitleLocKey = m.Alert.Localization.TitleKey
			s.TitleLocArgs = m.Alert.Localization.TitleArgs
			s.BodyLocKey = m.Alert.Localization.BodyKey
			s.BodyLocArgs = m.Alert.Localization.BodyArgs
		}
	}
	if m.Delivery.Expiry != nil && m.Delivery.Expiry.TTLSeconds != nil {
		s.TTLSeconds = *m.Delivery.Expiry.TTLSeconds
	}
	return s
}

// TestDualSpellingProducesIdenticalMessage sends the same request twice, once
// with every dual-spellable field in lowerCamelCase and once in snake_case
// (collapseKey/collapse_key, titleLocKey/title_loc_key, titleLocArgs/
// title_loc_args, bodyLocKey/body_loc_key, bodyLocArgs/body_loc_args,
// notificationCount/notification_count, and the envelope's own
// validateOnly/validate_only), and checks the two decode to the same
// push.Message in every field the model exposes. Proto3 JSON explicitly
// accepts both spellings for a field
// (https://protobuf.dev/programming-guides/json/), so a real client sending
// the underscore form must not lose data tommy would otherwise show for the
// camelCase form.
func TestDualSpellingProducesIdenticalMessage(t *testing.T) {
	t.Parallel()

	hCamel := newHarness(t, plugin.ProviderConfig{})
	respCamel := hCamel.send("my-project", loadFixture(t, "dual_camel.json"), nil)
	if respCamel.StatusCode != http.StatusOK {
		t.Fatalf("camelCase status = %d, want 200; body: %s", respCamel.StatusCode, readAll(t, respCamel.Body))
	}
	respCamel.Body.Close()

	hSnake := newHarness(t, plugin.ProviderConfig{})
	respSnake := hSnake.send("my-project", loadFixture(t, "dual_snake.json"), nil)
	if respSnake.StatusCode != http.StatusOK {
		t.Fatalf("snake_case status = %d, want 200; body: %s", respSnake.StatusCode, readAll(t, respSnake.Body))
	}
	respSnake.Body.Close()

	camelEvs := hCamel.events()
	snakeEvs := hSnake.events()
	if len(camelEvs) != 1 || len(snakeEvs) != 1 {
		t.Fatalf("events = %d/%d, want 1/1", len(camelEvs), len(snakeEvs))
	}

	mCamel, ok := push.MessageOf(camelEvs[0])
	if !ok {
		t.Fatalf("camelCase event carries no push message")
	}
	mSnake, ok := push.MessageOf(snakeEvs[0])
	if !ok {
		t.Fatalf("snake_case event carries no push message")
	}

	gotCamel := snapshotOf(t, mCamel)
	gotSnake := snapshotOf(t, mSnake)
	if !reflect.DeepEqual(gotCamel, gotSnake) {
		t.Errorf("camelCase and snake_case decoded to different messages:\n  camelCase = %+v\n  snake_case = %+v", gotCamel, gotSnake)
	}

	// Sanity: the snapshot actually captured the fields this test exists to
	// check, rather than passing on two empty structs.
	want := dualSpellingSnapshot{
		Kind: push.KindNotification, TargetKind: push.TargetDevice,
		TargetValue: "device-token-dual-test", TargetSource: "token",
		Title: "Android title", Body: "Base body",
		TitleLocKey: "TITLE_KEY", TitleLocArgs: []string{"a", "b"},
		BodyLocKey: "BODY_KEY", BodyLocArgs: []string{"c", "d"},
		Badge: 5, Priority: push.PriorityHigh, CollapseKey: "weather",
		TTLSeconds: 3600, PayloadCount: 2,
	}
	if !reflect.DeepEqual(gotCamel, want) {
		t.Errorf("camelCase snapshot = %+v, want %+v", gotCamel, want)
	}

	if v, _ := camelEvs[0].Meta["validate_only"].(bool); !v {
		t.Errorf("camelCase Meta[validate_only] = %v, want true (validateOnly)", camelEvs[0].Meta["validate_only"])
	}
	if v, _ := snakeEvs[0].Meta["validate_only"].(bool); !v {
		t.Errorf("snake_case Meta[validate_only] = %v, want true (validate_only)", snakeEvs[0].Meta["validate_only"])
	}
}

// TestBothSpellingsConflictCamelCaseWins checks the documented tie-break in
// normalizeKeys: when a request sends both android.collapseKey and
// android.collapse_key with different values (a client should never do this,
// but tommy's job is to be honest about what it decided rather than to
// pretend the ambiguity cannot occur), the camelCase value - the discovery
// document's canonical spelling - is the one that lands on the model.
func TestBothSpellingsConflictCamelCaseWins(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})
	resp := h.send("my-project", loadFixture(t, "conflict_both_spellings.json"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, readAll(t, resp.Body))
	}
	resp.Body.Close()

	evs := h.events()
	m, ok := push.MessageOf(evs[0])
	if !ok {
		t.Fatalf("event carries no push message")
	}
	if m.Delivery.CollapseKey != "camel-wins" {
		t.Errorf("Delivery.CollapseKey = %q, want %q (camelCase must win over collapse_key)", m.Delivery.CollapseKey, "camel-wins")
	}
}

// TestDataKeysAreNeverRenamed checks that normalizeKeys' opaqueKeys
// exemption actually protects message.data: a caller's own snake_case data
// key ("my_custom_key") is application payload, not a proto field name, and
// must reach Message.Data exactly as sent rather than being rewritten to
// "myCustomKey" by the same mechanism that fixes up collapseKey.
func TestDataKeysAreNeverRenamed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})
	resp := h.send("my-project", loadFixture(t, "data_key_not_renamed.json"), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, readAll(t, resp.Body))
	}
	resp.Body.Close()

	evs := h.events()
	m, ok := push.MessageOf(evs[0])
	if !ok {
		t.Fatalf("event carries no push message")
	}
	var data map[string]string
	if err := json.Unmarshal(m.Data, &data); err != nil {
		t.Fatalf("decode Data: %v", err)
	}
	if data["my_custom_key"] != "value" {
		t.Errorf("Data[my_custom_key] = %q, want %q - a data key must never be renamed", data["my_custom_key"], "value")
	}
	if data["otherCamelKey"] != "value2" {
		t.Errorf("Data[otherCamelKey] = %q, want %q", data["otherCamelKey"], "value2")
	}
	if _, present := data["myCustomKey"]; present {
		t.Errorf("Data contains a renamed myCustomKey - opaqueKeys did not protect message.data")
	}
}
