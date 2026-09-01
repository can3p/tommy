package twilio_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/plugins/sms"
	"github.com/can3p/tommy/plugins/sms/providers/twilio"
)

// apiResource mirrors the provider's private wire struct field for field, so
// tests can assert on the exact shape a real client would decode without
// reaching into the package's internals.
type apiResource struct {
	AccountSid          string            `json:"account_sid"`
	APIVersion          string            `json:"api_version"`
	Body                string            `json:"body"`
	DateCreated         string            `json:"date_created"`
	DateSent            string            `json:"date_sent"`
	DateUpdated         string            `json:"date_updated"`
	Direction           string            `json:"direction"`
	ErrorCode           *int              `json:"error_code"`
	ErrorMessage        *string           `json:"error_message"`
	From                *string           `json:"from"`
	MessagingServiceSid *string           `json:"messaging_service_sid"`
	NumMedia            string            `json:"num_media"`
	NumSegments         string            `json:"num_segments"`
	Price               *string           `json:"price"`
	PriceUnit           *string           `json:"price_unit"`
	Sid                 string            `json:"sid"`
	Status              string            `json:"status"`
	SubresourceURIs     map[string]string `json:"subresource_uris"`
	To                  string            `json:"to"`
	URI                 string            `json:"uri"`
}

// apiError mirrors Twilio's REST error envelope.
type apiError struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	MoreInfo string `json:"more_info"`
	Status   int    `json:"status"`
}

func strPtr(s string) *string { return &s }

// accountSid is the fixed {AccountSid} path segment every test uses.
const accountSid = "ACaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// fixedDate is plugintest.NewDeps()'s clock (2024-01-01 12:00:00 UTC),
// formatted the way Twilio formats date_created/date_sent/date_updated.
const fixedDate = "Mon, 01 Jan 2024 12:00:00 +0000"

// createURL is the ingress path this provider mounts its create route on.
const createURL = "/2010-04-01/Accounts/" + accountSid + "/Messages.json"

// newProvider mounts a fresh Twilio provider on a plain ServeMux, backed by
// deterministic deps (fixed clock, counting ids), so every fresh case gets
// sid "SM.../MMtest-id-001" for its first message.
func newProvider(t *testing.T) (*http.ServeMux, plugin.Deps) {
	t.Helper()
	d := plugintest.NewDeps()
	mux := http.NewServeMux()
	twilio.New().RegisterIngress(mux, d)
	return mux, d
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return bytes.TrimRight(b, "\n")
}

func postForm(t *testing.T, mux *http.ServeMux, body []byte, basicUser, basicPass string, hasAuth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, createURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if hasAuth {
		req.SetBasicAuth(basicUser, basicPass)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestCreate covers form decoding, the exact 201 resource shape (including the
// RFC 1123 date layout and null-vs-empty handling), repeated MediaUrl and
// MessagingServiceSid standing in for From, and asserts the canonical
// sms.Message the plugin actually stored.
func TestCreate(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		wantSid  string
		wantBody apiResource
		wantMsg  func() *sms.Message
	}{
		{
			name:    "basic sms with From",
			fixture: "create_basic.form",
			wantSid: "SMtest-id-001",
			wantBody: apiResource{
				AccountSid:          accountSid,
				APIVersion:          "2010-04-01",
				Body:                "It works.",
				DateCreated:         fixedDate,
				DateSent:            fixedDate,
				DateUpdated:         fixedDate,
				Direction:           "outbound-api",
				ErrorCode:           nil,
				ErrorMessage:        nil,
				From:                strPtr("+15557122661"),
				MessagingServiceSid: nil,
				NumMedia:            "0",
				NumSegments:         "1",
				Price:               nil,
				PriceUnit:           nil,
				Sid:                 "SMtest-id-001",
				Status:              "queued",
				SubresourceURIs: map[string]string{
					"media":    "/2010-04-01/Accounts/" + accountSid + "/Messages/SMtest-id-001/Media.json",
					"feedback": "/2010-04-01/Accounts/" + accountSid + "/Messages/SMtest-id-001/Feedback.json",
				},
				To:  "+15558675310",
				URI: "/2010-04-01/Accounts/" + accountSid + "/Messages/SMtest-id-001.json",
			},
			wantMsg: func() *sms.Message {
				m := &sms.Message{From: "+15557122661", To: "+15558675310", Body: "It works."}
				m.Normalize()
				return m
			},
		},
		{
			name:    "mms with MessagingServiceSid and two media",
			fixture: "create_mms.form",
			wantSid: "MMtest-id-001",
			wantBody: apiResource{
				AccountSid:          accountSid,
				APIVersion:          "2010-04-01",
				Body:                "Two pictures.",
				DateCreated:         fixedDate,
				DateSent:            fixedDate,
				DateUpdated:         fixedDate,
				Direction:           "outbound-api",
				ErrorCode:           nil,
				ErrorMessage:        nil,
				From:                nil,
				MessagingServiceSid: strPtr("MGxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"),
				NumMedia:            "2",
				NumSegments:         "1",
				Price:               nil,
				PriceUnit:           nil,
				Sid:                 "MMtest-id-001",
				Status:              "queued",
				SubresourceURIs: map[string]string{
					"media":    "/2010-04-01/Accounts/" + accountSid + "/Messages/MMtest-id-001/Media.json",
					"feedback": "/2010-04-01/Accounts/" + accountSid + "/Messages/MMtest-id-001/Feedback.json",
				},
				To:  "+15558675310",
				URI: "/2010-04-01/Accounts/" + accountSid + "/Messages/MMtest-id-001.json",
			},
			wantMsg: func() *sms.Message {
				m := &sms.Message{
					To:               "+15558675310",
					MessagingService: "MGxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
					Body:             "Two pictures.",
					Media: []sms.Media{
						{URL: "https://example.com/cat.png"},
						{URL: "https://example.com/dog.png"},
					},
				}
				m.Normalize()
				return m
			},
		},
		{
			name:    "unicode body forces UCS-2",
			fixture: "create_unicode.form",
			wantSid: "SMtest-id-001",
			wantBody: apiResource{
				AccountSid:          accountSid,
				APIVersion:          "2010-04-01",
				Body:                "Hi \U0001F600",
				DateCreated:         fixedDate,
				DateSent:            fixedDate,
				DateUpdated:         fixedDate,
				Direction:           "outbound-api",
				ErrorCode:           nil,
				ErrorMessage:        nil,
				From:                strPtr("+15557122661"),
				MessagingServiceSid: nil,
				NumMedia:            "0",
				NumSegments:         "1",
				Price:               nil,
				PriceUnit:           nil,
				Sid:                 "SMtest-id-001",
				Status:              "queued",
				SubresourceURIs: map[string]string{
					"media":    "/2010-04-01/Accounts/" + accountSid + "/Messages/SMtest-id-001/Media.json",
					"feedback": "/2010-04-01/Accounts/" + accountSid + "/Messages/SMtest-id-001/Feedback.json",
				},
				To:  "+15558675310",
				URI: "/2010-04-01/Accounts/" + accountSid + "/Messages/SMtest-id-001.json",
			},
			wantMsg: func() *sms.Message {
				m := &sms.Message{From: "+15557122661", To: "+15558675310", Body: "Hi \U0001F600"}
				m.Normalize()
				return m
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, d := newProvider(t)
			rec := postForm(t, mux, readFixture(t, tt.fixture), "", "", false)

			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}

			var got apiResource
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode response: %v, body=%s", err, rec.Body.String())
			}
			// The want.wantMsg's Segments (computed below from the same
			// wantMsg helper) decides NumSegments, so verify it agrees with
			// what CountSegments would say independently.
			wantMsg := tt.wantMsg()
			if got := tt.wantBody.NumSegments; got != itoa(wantMsg.Segments.Count) {
				t.Fatalf("test fixture bug: wantBody.NumSegments=%s but wantMsg.Segments.Count=%d", got, wantMsg.Segments.Count)
			}

			if got.Sid != tt.wantSid {
				t.Errorf("sid = %q, want %q", got.Sid, tt.wantSid)
			}
			if !resourceEqual(got, tt.wantBody) {
				t.Errorf("response body mismatch:\n got  = %+v\n want = %+v", dump(got), dump(tt.wantBody))
			}

			// The event the provider actually stored: verify the canonical
			// Message (never the Twilio-specific Meta) matches exactly.
			events, err := d.Store.List(t.Context(), store.Query{Plugin: sms.Name, Provider: twilio.Name})
			if err != nil || len(events) != 1 {
				t.Fatalf("store.List: %v, %d events", err, len(events))
			}
			m, ok := sms.MessageOf(events[0])
			if !ok {
				t.Fatalf("event carries no sms.Message")
			}
			if !messageEqual(m, wantMsg) {
				t.Errorf("stored message mismatch:\n got  = %+v\n want = %+v", m, wantMsg)
			}
			if events[0].Type != sms.EventType {
				t.Errorf("event.Type = %q, want %q", events[0].Type, sms.EventType)
			}
			if events[0].Raw.Transport != "http" || events[0].Raw.Method != "POST" || !events[0].Raw.Text {
				t.Errorf("Raw not populated as expected: %+v", events[0].Raw)
			}
			if !bytes.Equal(events[0].Raw.Body, readFixture(t, tt.fixture)) {
				t.Errorf("Raw.Body does not carry the exact request body")
			}
		})
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func resourceEqual(got, want apiResource) bool {
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	return bytes.Equal(g, w)
}

func dump(v apiResource) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func messageEqual(got, want *sms.Message) bool {
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	return bytes.Equal(g, w)
}

// TestCreateExactBody pins the exact byte-for-byte response of the simplest
// case, so a change to field order or whitespace is caught even if the
// structural comparison above would tolerate it.
func TestCreateExactBody(t *testing.T) {
	mux, _ := newProvider(t)
	rec := postForm(t, mux, readFixture(t, "create_basic.form"), "", "", false)

	want := `{"account_sid":"` + accountSid + `","api_version":"2010-04-01","body":"It works.",` +
		`"date_created":"` + fixedDate + `","date_sent":"` + fixedDate + `","date_updated":"` + fixedDate + `",` +
		`"direction":"outbound-api","error_code":null,"error_message":null,"from":"+15557122661",` +
		`"messaging_service_sid":null,"num_media":"0","num_segments":"1","price":null,"price_unit":null,` +
		`"sid":"SMtest-id-001","status":"queued","subresource_uris":{` +
		`"feedback":"/2010-04-01/Accounts/` + accountSid + `/Messages/SMtest-id-001/Feedback.json",` +
		`"media":"/2010-04-01/Accounts/` + accountSid + `/Messages/SMtest-id-001/Media.json"},` +
		`"to":"+15558675310","uri":"/2010-04-01/Accounts/` + accountSid + `/Messages/SMtest-id-001.json"}` + "\n"

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != want {
		t.Errorf("exact body mismatch:\n got  = %s\n want = %s", rec.Body.String(), want)
	}
}

// TestCreateErrors covers the Twilio error shapes for a missing/invalid To, a
// missing sender and a missing body-or-media.
func TestCreateErrors(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		wantCode int
	}{
		{name: "missing To", fixture: "create_missing_to.form", wantCode: 21604},
		{name: "invalid To", fixture: "create_invalid_to.form", wantCode: 21211},
		{name: "missing sender", fixture: "create_missing_sender.form", wantCode: 21603},
		{name: "missing body and media", fixture: "create_missing_body.form", wantCode: 21602},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, d := newProvider(t)
			rec := postForm(t, mux, readFixture(t, tt.fixture), "", "", false)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var got apiError
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode error body: %v, body=%s", err, rec.Body.String())
			}
			if got.Code != tt.wantCode {
				t.Errorf("code = %d, want %d", got.Code, tt.wantCode)
			}
			if got.Status != http.StatusBadRequest {
				t.Errorf("status field = %d, want 400", got.Status)
			}
			wantMoreInfo := "https://www.twilio.com/docs/errors/" + itoa(tt.wantCode)
			if got.MoreInfo != wantMoreInfo {
				t.Errorf("more_info = %q, want %q", got.MoreInfo, wantMoreInfo)
			}
			if got.Message == "" {
				t.Errorf("message is empty")
			}

			// A rejected create must never append an event.
			events, err := d.Store.List(t.Context(), store.Query{Plugin: sms.Name, Provider: twilio.Name})
			if err != nil {
				t.Fatalf("store.List: %v", err)
			}
			if len(events) != 0 {
				t.Errorf("rejected create appended %d events, want 0", len(events))
			}
		})
	}
}

// TestAuth covers the "accept anything by default, record what was
// presented" rule and the pinned-credentials rejection path, using Twilio's
// real invalid-credentials error shape.
func TestAuth(t *testing.T) {
	t.Run("no credentials presented is accepted by default", func(t *testing.T) {
		mux, d := newProvider(t)
		rec := postForm(t, mux, readFixture(t, "create_basic.form"), "", "", false)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		events, _ := d.Store.List(t.Context(), store.Query{Plugin: sms.Name, Provider: twilio.Name})
		if len(events) != 1 {
			t.Fatalf("got %d events", len(events))
		}
		auth, ok := events[0].Meta["basic_auth"].(map[string]any)
		if !ok {
			t.Fatalf("Meta[basic_auth] missing or wrong type: %+v", events[0].Meta)
		}
		if auth["presented"] != false {
			t.Errorf("basic_auth.presented = %v, want false", auth["presented"])
		}
	})

	t.Run("any credentials presented are accepted by default and recorded", func(t *testing.T) {
		mux, d := newProvider(t)
		rec := postForm(t, mux, readFixture(t, "create_basic.form"), "ACwhatever", "shh", true)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		events, _ := d.Store.List(t.Context(), store.Query{Plugin: sms.Name, Provider: twilio.Name})
		auth := events[0].Meta["basic_auth"].(map[string]any)
		if auth["presented"] != true || auth["account_sid"] != "ACwhatever" || auth["auth_token"] != "shh" {
			t.Errorf("basic_auth = %+v", auth)
		}
	})

	t.Run("pinned credentials reject a mismatch with Twilio's real error", func(t *testing.T) {
		d := plugintest.NewDeps().WithConfig(mustProviderConfig(t, map[string]any{
			"account_sid": "ACpinnedxxxxxxxxxxxxxxxxxxxxxxxxxx",
			"auth_token":  "correct-token",
		}))
		mux := http.NewServeMux()
		twilio.New().RegisterIngress(mux, d)

		rec := postForm(t, mux, readFixture(t, "create_basic.form"), "ACpinnedxxxxxxxxxxxxxxxxxxxxxxxxxx", "wrong-token", true)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var got apiError
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Code != 20003 || got.Status != 401 {
			t.Errorf("got %+v, want code 20003 status 401", got)
		}
		if got.MoreInfo != "https://www.twilio.com/docs/errors/20003" {
			t.Errorf("more_info = %q", got.MoreInfo)
		}

		events, _ := d.Store.List(t.Context(), store.Query{Plugin: sms.Name, Provider: twilio.Name})
		if len(events) != 0 {
			t.Errorf("a rejected auth must not append an event, got %d", len(events))
		}
	})

	t.Run("pinned credentials accept the matching pair", func(t *testing.T) {
		d := plugintest.NewDeps().WithConfig(mustProviderConfig(t, map[string]any{
			"account_sid": "ACpinnedxxxxxxxxxxxxxxxxxxxxxxxxxx",
			"auth_token":  "correct-token",
		}))
		mux := http.NewServeMux()
		twilio.New().RegisterIngress(mux, d)

		rec := postForm(t, mux, readFixture(t, "create_basic.form"), "ACpinnedxxxxxxxxxxxxxxxxxxxxxxxxxx", "correct-token", true)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	})
}

func mustProviderConfig(t *testing.T, values map[string]any) plugin.ProviderConfig {
	t.Helper()
	return config.NewProviderConfig(values)
}
