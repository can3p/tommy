package apns_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/plugins/push"
	"github.com/can3p/tommy/plugins/push/providers/apns"
)

// deviceToken is the token Apple's own documentation uses in its examples.
const deviceToken = "00fc13adff785122b4ad28809a3420982341241421348097878e577c991de8f0"

// --- conformance -----------------------------------------------------------

func TestConformance(t *testing.T) {
	t.Parallel()
	plugintest.ConformanceProvider(t, apns.New())
	plugintest.Conformance(t, push.New(apns.New()))
}

// --- harness ---------------------------------------------------------------

// harness mounts only the apns provider on a bare httptest server, so a test
// drives it over real HTTP and can read the store afterwards. HTTP/2 is not
// what httptest gives by default; h2_test.go covers that separately, and this
// harness deliberately exercises the HTTP/1.1 path the provider still serves
// (and warns about) so the downgrade behavior is pinned too.
type harness struct {
	t  *testing.T
	ts *httptest.Server
	d  plugin.Deps
}

func newHarness(t *testing.T, cfg plugin.ProviderConfig) *harness {
	t.Helper()
	d := plugintest.NewDeps().WithConfig(cfg)
	mux := http.NewServeMux()
	apns.New().RegisterIngress(mux, d)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &harness{t: t, ts: ts, d: d}
}

// post sends a body to the device path with the given headers.
func (h *harness) post(path string, body []byte, headers map[string]string) *http.Response {
	h.t.Helper()
	return h.do(http.MethodPost, path, body, headers)
}

func (h *harness) do(method, path string, body []byte, headers map[string]string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.ts.URL+path, bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
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
	evs, err := h.d.Store.List(h.t.Context(), store.Query{Plugin: push.Name, Provider: apns.ProviderName})
	if err != nil {
		h.t.Fatalf("list events: %v", err)
	}
	return evs
}

// only returns the single captured event, failing when there is not exactly one.
func (h *harness) only() *event.Event {
	h.t.Helper()
	evs := h.events()
	if len(evs) != 1 {
		h.t.Fatalf("captured %d events, want exactly 1", len(evs))
	}
	return evs[0]
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

// messageOf decodes the push.Message a captured event carries.
func messageOf(t *testing.T, ev *event.Event) *push.Message {
	t.Helper()
	m, ok := push.MessageOf(ev)
	if !ok {
		t.Fatalf("event payload is not a push.Message: %#v", ev.Payload)
	}
	return m
}

// alertHeaders is a well-formed header set for an ordinary alert push.
func alertHeaders() map[string]string {
	return map[string]string{
		"apns-topic":     "com.example.MyApp",
		"apns-push-type": "alert",
		"apns-priority":  "10",
	}
}

// --- the success shape -----------------------------------------------------

// Apple documents success as 200 with an empty body and an apns-id header.
// A body of any kind would be wrong, so its absence is asserted rather than
// assumed.
func TestSuccessIsEmptyBodyWithAPNsID(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})
	resp := h.post("/3/device/"+deviceToken, loadFixture(t, "alert.json"), alertHeaders())

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, readAll(t, resp.Body))
	}
	if body := readAll(t, resp.Body); len(body) != 0 {
		t.Errorf("success body = %q, want empty", body)
	}
	if got := resp.Header.Get("apns-id"); got == "" {
		t.Error("no apns-id header on a successful response")
	}
	// Development-environment only, and a local cleartext catcher is nothing
	// else, so it must be present and must correlate with the stored event.
	ev := h.only()
	if got := resp.Header.Get("apns-unique-id"); got != string(ev.ID) {
		t.Errorf("apns-unique-id = %q, want the event id %q", got, ev.ID)
	}
}

// A client that supplies its own apns-id must get that exact id back, since
// correlating a push with a log line is the whole point of sending one.
func TestSuppliedAPNsIDIsEchoed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})
	const id = "eabeae54-14a8-11e5-b60b-1697f925ec7b"
	hdr := alertHeaders()
	hdr["apns-id"] = id

	resp := h.post("/3/device/"+deviceToken, loadFixture(t, "alert.json"), hdr)
	if got := resp.Header.Get("apns-id"); got != id {
		t.Errorf("apns-id = %q, want the supplied %q", got, id)
	}
	if supplied, _ := h.only().Meta["apns_id_supplied"].(bool); !supplied {
		t.Error("apns_id_supplied is not recorded as true for a client-supplied id")
	}
}

// --- message conversion ----------------------------------------------------

func TestMessageConversion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fixture string
		headers map[string]string
		check   func(t *testing.T, m *push.Message, ev *event.Event)
	}{
		{
			name:    "alert push",
			fixture: "alert.json",
			headers: alertHeaders(),
			check: func(t *testing.T, m *push.Message, _ *event.Event) {
				if m.Kind != push.KindNotification {
					t.Errorf("Kind = %q, want %q", m.Kind, push.KindNotification)
				}
				if !m.Displays() {
					t.Error("an alert push must report that it displays")
				}
				if m.Alert == nil {
					t.Fatal("Alert is nil for an alert push")
				}
				if m.Alert.Title != "Game Request" {
					t.Errorf("title = %q", m.Alert.Title)
				}
				if m.Alert.Subtitle != "Five Card Draw" {
					t.Errorf("subtitle = %q", m.Alert.Subtitle)
				}
				if m.Alert.Body != "Bob wants to play poker" {
					t.Errorf("body = %q", m.Alert.Body)
				}
			},
		},
		{
			name:    "the device token comes from the path, not the body",
			fixture: "alert.json",
			headers: alertHeaders(),
			check: func(t *testing.T, m *push.Message, _ *event.Event) {
				if m.Target.Kind != push.TargetDevice {
					t.Errorf("Target.Kind = %q, want %q", m.Target.Kind, push.TargetDevice)
				}
				if m.Target.Value != deviceToken {
					t.Errorf("Target.Value = %q, want the path token", m.Target.Value)
				}
				if m.Target.Source == "" {
					t.Error("Target.Source is empty; the wire location must be recorded")
				}
			},
		},
		{
			name:    "apns-topic is the app bundle id, never the target",
			fixture: "alert.json",
			headers: alertHeaders(),
			check: func(t *testing.T, m *push.Message, _ *event.Event) {
				if m.App != "com.example.MyApp" {
					t.Errorf("App = %q, want the apns-topic bundle id", m.App)
				}
				if m.Target.Kind == push.TargetTopic {
					t.Error("apns-topic was modeled as a pub/sub topic; it is a bundle id")
				}
			},
		},
		{
			// The trap the core's author flagged hardest: no alert and no
			// custom keys would otherwise derive as KindEmpty.
			name:    "content-available with nothing else is silent, not empty",
			fixture: "silent.json",
			headers: map[string]string{"apns-topic": "com.example.MyApp", "apns-push-type": "background", "apns-priority": "5"},
			check: func(t *testing.T, m *push.Message, _ *event.Event) {
				if m.Kind != push.KindSilent {
					t.Errorf("Kind = %q, want %q - a bare content-available must not derive as empty",
						m.Kind, push.KindSilent)
				}
				if m.Displays() {
					t.Error("a background push must not report that it displays")
				}
				if m.Alert != nil {
					t.Errorf("Alert = %#v, want nil for a silent push", m.Alert)
				}
			},
		},
		{
			name:    "content-available with custom keys is still silent",
			fixture: "silent_with_data.json",
			headers: map[string]string{"apns-topic": "com.example.MyApp", "apns-push-type": "background"},
			check: func(t *testing.T, m *push.Message, _ *event.Event) {
				if m.Kind != push.KindSilent {
					t.Errorf("Kind = %q, want %q", m.Kind, push.KindSilent)
				}
				if !m.HasData() {
					t.Error("custom keys were not recorded as data")
				}
			},
		},
		{
			// Apple allows alert to be a plain string as well as a dictionary.
			name:    "a string alert is still an alert",
			fixture: "alert_string.json",
			headers: alertHeaders(),
			check: func(t *testing.T, m *push.Message, _ *event.Event) {
				if m.Kind != push.KindNotification {
					t.Errorf("Kind = %q, want %q", m.Kind, push.KindNotification)
				}
				if m.Alert == nil || m.Alert.Body != "A plain string alert" {
					t.Errorf("Alert = %#v, want the string lifted into the body", m.Alert)
				}
			},
		},
		{
			name:    "a localized alert is a notification even with no literal text",
			fixture: "localized.json",
			headers: alertHeaders(),
			check: func(t *testing.T, m *push.Message, _ *event.Event) {
				if m.Kind != push.KindNotification {
					t.Errorf("Kind = %q, want %q; loc-key alone still displays", m.Kind, push.KindNotification)
				}
			},
		},
		{
			name:    "every apns header is recorded verbatim",
			fixture: "alert.json",
			headers: map[string]string{
				"apns-topic":       "com.example.MyApp",
				"apns-push-type":   "alert",
				"apns-priority":    "5",
				"apns-collapse-id": "poker-invite",
				"apns-expiration":  "0",
			},
			check: func(t *testing.T, m *push.Message, ev *event.Event) {
				all, ok := ev.Meta["apns_headers"].(map[string]string)
				if !ok {
					t.Fatalf("apns_headers meta = %#v, want a map", ev.Meta["apns_headers"])
				}
				for _, want := range []string{"apns-topic", "apns-push-type", "apns-priority", "apns-collapse-id"} {
					if all[want] == "" {
						t.Errorf("header %q was not recorded in apns_headers", want)
					}
				}
				if m.Delivery.CollapseKey != "poker-invite" {
					t.Errorf("CollapseKey = %q, want poker-invite", m.Delivery.CollapseKey)
				}
				// apns-expiration 0 is a sentinel meaning "try once", not 1970.
				if m.Delivery.Expiry == nil {
					t.Fatal("Expiry is nil though apns-expiration was sent")
				}
				if !m.Delivery.Expiry.Immediate {
					t.Error("apns-expiration: 0 must be recorded as Immediate, not as the epoch")
				}
			},
		},
		{
			name:    "the raw body is kept byte for byte",
			fixture: "alert.json",
			headers: alertHeaders(),
			check: func(t *testing.T, _ *push.Message, ev *event.Event) {
				want := loadFixture(t, "alert.json")
				if !bytes.Equal(ev.Raw.Body, want) {
					t.Errorf("Raw.Body was not preserved verbatim:\n got %s\nwant %s", ev.Raw.Body, want)
				}
				if ev.Raw.Method != http.MethodPost {
					t.Errorf("Raw.Method = %q", ev.Raw.Method)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, plugin.ProviderConfig{})
			resp := h.post("/3/device/"+deviceToken, loadFixture(t, tc.fixture), tc.headers)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, readAll(t, resp.Body))
			}
			ev := h.only()
			tc.check(t, messageOf(t, ev), ev)
		})
	}
}

// --- the provider token ----------------------------------------------------

// The claims are the entire point: a wrong key id is a common real mistake and
// seeing it is why anyone points a client at a catcher. Nothing is verified.
func TestJWTClaimsAreRecordedNeverVerified(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})
	// header {"alg":"ES256","kid":"8YL3G3RRX7"}, claims {"iss":"C86NV9JX3D","iat":1756800000},
	// and a signature that is not a signature at all.
	const token = "bearer eyJhbGciOiJFUzI1NiIsImtpZCI6IjhZTDNHM1JSWDcifQ." +
		"eyJpc3MiOiJDODZOVjlKWDNEIiwiaWF0IjoxNzU2ODAwMDAwfQ.not-a-real-signature"

	hdr := alertHeaders()
	hdr["authorization"] = token
	resp := h.post("/3/device/"+deviceToken, loadFixture(t, "alert.json"), hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 - an unverifiable signature must still be accepted", resp.StatusCode)
	}

	ev := h.only()
	claims, ok := ev.Meta["jwt"]
	if !ok {
		t.Fatal("no jwt claims recorded")
	}
	blob, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	for _, want := range []string{"8YL3G3RRX7", "C86NV9JX3D"} {
		if !bytes.Contains(blob, []byte(want)) {
			t.Errorf("claim %q missing from recorded jwt: %s", want, blob)
		}
	}
	if ev.Meta["authorization"] != token {
		t.Errorf("the presented authorization header was not recorded verbatim")
	}
}

// A malformed token must be captured, not rejected: a fake that 401s is
// useless, and a broken token is exactly what someone is trying to see.
func TestMalformedJWTIsAcceptedAndRecorded(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})
	hdr := alertHeaders()
	hdr["authorization"] = "bearer this.is.not-base64-at-all"

	resp := h.post("/3/device/"+deviceToken, loadFixture(t, "alert.json"), hdr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a malformed provider token", resp.StatusCode)
	}
	if got := h.only().Meta["authorization"]; got != "bearer this.is.not-base64-at-all" {
		t.Errorf("authorization = %v, want it recorded verbatim", got)
	}
}

// --- errors this provider is willing to produce ----------------------------

func TestErrorShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		path       string
		body       []byte
		headers    map[string]string
		wantStatus int
		wantReason string
	}{
		{
			name:       "no device token in the path",
			method:     http.MethodPost,
			path:       "/3/device/",
			body:       []byte(`{"aps":{"alert":"hi"}}`),
			headers:    alertHeaders(),
			wantStatus: http.StatusBadRequest,
			wantReason: "MissingDeviceToken",
		},
		{
			name:       "extra path segments after the token",
			method:     http.MethodPost,
			path:       "/3/device/" + deviceToken + "/extra",
			body:       []byte(`{"aps":{"alert":"hi"}}`),
			headers:    alertHeaders(),
			wantStatus: http.StatusNotFound,
			wantReason: "BadPath",
		},
		{
			name:       "a method other than POST",
			method:     http.MethodGet,
			path:       "/3/device/" + deviceToken,
			headers:    alertHeaders(),
			wantStatus: http.StatusMethodNotAllowed,
			wantReason: "MethodNotAllowed",
		},
		{
			name:       "an empty payload",
			method:     http.MethodPost,
			path:       "/3/device/" + deviceToken,
			body:       nil,
			headers:    alertHeaders(),
			wantStatus: http.StatusBadRequest,
			wantReason: "PayloadEmpty",
		},
		{
			name:       "no apns-topic",
			method:     http.MethodPost,
			path:       "/3/device/" + deviceToken,
			body:       []byte(`{"aps":{"alert":"hi"}}`),
			headers:    map[string]string{"apns-push-type": "alert"},
			wantStatus: http.StatusBadRequest,
			wantReason: "MissingTopic",
		},
		{
			name:       "an unknown apns-push-type",
			method:     http.MethodPost,
			path:       "/3/device/" + deviceToken,
			body:       []byte(`{"aps":{"alert":"hi"}}`),
			headers:    map[string]string{"apns-topic": "com.example.MyApp", "apns-push-type": "telepathy"},
			wantStatus: http.StatusBadRequest,
			wantReason: "InvalidPushType",
		},
		{
			name:   "a non-numeric apns-priority",
			method: http.MethodPost,
			path:   "/3/device/" + deviceToken,
			body:   []byte(`{"aps":{"alert":"hi"}}`),
			headers: map[string]string{
				"apns-topic": "com.example.MyApp", "apns-push-type": "alert", "apns-priority": "urgent",
			},
			wantStatus: http.StatusBadRequest,
			wantReason: "BadPriority",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, plugin.ProviderConfig{})
			resp := h.do(tc.method, tc.path, tc.body, tc.headers)

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			var body struct {
				Reason    string `json:"reason"`
				Timestamp *int64 `json:"timestamp"`
			}
			raw := readAll(t, resp.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("error body is not JSON: %v (%s)", err, raw)
			}
			if body.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", body.Reason, tc.wantReason)
			}
			// Apple documents timestamp on 410 only, and this provider never
			// produces a 410, so it must never appear.
			if body.Timestamp != nil {
				t.Errorf("timestamp present on a %d response; Apple sends it only on 410", resp.StatusCode)
			}
			if len(h.events()) != 0 {
				t.Errorf("a rejected request was still captured as an event")
			}
		})
	}
}

// --- pinning, the one path that rejects ------------------------------------

func TestPinnedTopicAndKeyID(t *testing.T) {
	t.Parallel()

	t.Run("a mismatched topic gets TopicDisallowed", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, config.NewProviderConfig(map[string]any{"topic": "com.example.Pinned"}))
		resp := h.post("/3/device/"+deviceToken, loadFixture(t, "alert.json"), alertHeaders())
		assertReason(t, resp, http.StatusBadRequest, "TopicDisallowed")
	})

	t.Run("the pinned topic is accepted", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, config.NewProviderConfig(map[string]any{"topic": "com.example.MyApp"}))
		resp := h.post("/3/device/"+deviceToken, loadFixture(t, "alert.json"), alertHeaders())
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200 for the pinned topic", resp.StatusCode)
		}
	})

	t.Run("a mismatched key id gets InvalidProviderToken", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, config.NewProviderConfig(map[string]any{"key_id": "AAAAAAAAAA"}))
		hdr := alertHeaders()
		hdr["authorization"] = "bearer eyJhbGciOiJFUzI1NiIsImtpZCI6IjhZTDNHM1JSWDcifQ." +
			"eyJpc3MiOiJDODZOVjlKWDNEIiwiaWF0IjoxNzU2ODAwMDAwfQ.sig"
		resp := h.post("/3/device/"+deviceToken, loadFixture(t, "alert.json"), hdr)
		assertReason(t, resp, http.StatusForbidden, "InvalidProviderToken")
	})

	t.Run("nothing pinned accepts anything", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, plugin.ProviderConfig{})
		hdr := alertHeaders()
		hdr["apns-topic"] = "com.whatever.At.All"
		resp := h.post("/3/device/"+deviceToken, loadFixture(t, "alert.json"), hdr)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200 when nothing is pinned", resp.StatusCode)
		}
	})
}

func assertReason(t *testing.T, resp *http.Response, wantStatus int, wantReason string) {
	t.Helper()
	if resp.StatusCode != wantStatus {
		t.Errorf("status = %d, want %d", resp.StatusCode, wantStatus)
	}
	var body struct {
		Reason string `json:"reason"`
	}
	raw := readAll(t, resp.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, raw)
	}
	if body.Reason != wantReason {
		t.Errorf("reason = %q, want %q", body.Reason, wantReason)
	}
}

// --- a payload that is not JSON --------------------------------------------

// A body that does not parse is still captured. Losing the capture would hide
// the very mistake the user is chasing; the parse failure is recorded instead.
func TestUnparseablePayloadIsStillCaptured(t *testing.T) {
	t.Parallel()
	h := newHarness(t, plugin.ProviderConfig{})
	body := loadFixture(t, "not_json.json")
	resp := h.post("/3/device/"+deviceToken, body, alertHeaders())

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; a malformed payload must still be captured", resp.StatusCode)
	}
	ev := h.only()
	if ev.Meta["payload_error"] == nil {
		t.Error("no payload_error recorded for a body that does not parse")
	}
	if !bytes.Equal(ev.Raw.Body, body) {
		t.Error("the unparseable body was not kept verbatim")
	}
}
