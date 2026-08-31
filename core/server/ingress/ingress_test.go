package ingress_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/server/ingress"
)

func ok(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }

func TestParsePattern(t *testing.T) {
	tests := []struct {
		in                      string
		method, host, path, key string
		wantErr                 string
	}{
		{in: "/v3.1/send", path: "/v3.1/send", key: "/v3.1/send"},
		{in: "POST /v3/mail/send", method: "POST", path: "/v3/mail/send", key: "/v3/mail/send"},
		{
			in: "POST /2010-04-01/Accounts/{sid}/Messages.json", method: "POST",
			path: "/2010-04-01/Accounts/{sid}/Messages.json",
			key:  "/2010-04-01/Accounts/{}/Messages.json",
		},
		{in: "GET /files/{path...}", method: "GET", path: "/files/{path...}", key: "/files/{...}"},
		{in: "GET /exact/{$}", method: "GET", path: "/exact/{$}", key: "/exact/{$}"},
		{in: "example.com/hook", host: "example.com", path: "/hook", key: "example.com/hook"},
		{in: "", wantErr: "empty pattern"},
		{in: "post /lower", wantErr: "invalid method"},
		{in: "GET", wantErr: "path must start with /"},
		{in: "GET ", wantErr: "path must start with /"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			p, err := ingress.ParsePattern(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if p.Method != tc.method || p.Host != tc.host || p.Path != tc.path {
				t.Errorf("got method=%q host=%q path=%q", p.Method, p.Host, p.Path)
			}
			if p.Key() != tc.key {
				t.Errorf("Key = %q, want %q", p.Key(), tc.key)
			}
		})
	}
}

func TestPatternConflicts(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", "POST /x", "POST /x", true},
		{"different methods on the same path are fine", "POST /x", "GET /x", false},
		{"a method-less pattern shadows every method", "/x", "POST /x", true},
		{"wildcard names do not matter", "GET /a/{id}", "GET /a/{sid}", true},
		{"different paths", "POST /a", "POST /b", false},
		{"trailing wildcards", "GET /f/{p...}", "GET /f/{q...}", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ingress.ParsePattern(tc.a)
			if err != nil {
				t.Fatal(err)
			}
			b, err := ingress.ParsePattern(tc.b)
			if err != nil {
				t.Fatal(err)
			}
			if got := a.Conflicts(b); got != tc.want {
				t.Errorf("%q vs %q = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			if got := b.Conflicts(a); got != tc.want {
				t.Errorf("conflict detection must be symmetric")
			}
		})
	}
}

func TestRegisterDetectsCollisions(t *testing.T) {
	in := ingress.New(nil)
	in.For("mail", "mailjet").HandleFunc("POST /v3.1/send", ok)
	in.For("mail", "sendgrid").HandleFunc("POST /v3.1/send", ok)

	err := in.Err()
	if err == nil {
		t.Fatal("two providers claiming the same route must fail at startup")
	}
	msg := err.Error()
	for _, want := range []string{"mail/mailjet", "mail/sendgrid", "/v3.1/send"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q must name %q", msg, want)
		}
	}
}

func TestRegisterAllowsDifferentMethodsOnOnePath(t *testing.T) {
	in := ingress.New(nil)
	in.For("sms", "twilio").HandleFunc("POST /2010-04-01/Accounts/{sid}/Messages.json", ok)
	in.For("sms", "twilio").HandleFunc("GET /2010-04-01/Accounts/{sid}/Messages.json", ok)
	if err := in.Err(); err != nil {
		t.Fatalf("a provider must be able to serve several methods on one path: %v", err)
	}
	if len(in.Routes()) != 2 {
		t.Errorf("routes = %d, want 2", len(in.Routes()))
	}
}

func TestRegisterRejectsReservedPrefixes(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		wantErr bool
	}{
		{"api v1 is core", "POST /api/v1/events", true},
		{"ui is core", "GET /ui/mail/", true},
		{"internal prefix is core", "GET /_tommy/x", true},
		// Slack's Web API genuinely lives under /api/, which is why only
		// /api/v1/ is reserved.
		{"slack chat.postMessage is fine", "POST /api/chat.postMessage", false},
		{"root would swallow everything", "/", true},
		{"catch-all would swallow everything", "GET /{path...}", true},
		{"a normal vendor path", "POST /v3/mail/send", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := ingress.New(nil)
			in.For("chat", "slack").HandleFunc(tc.pattern, ok)
			err := in.Err()
			if tc.wantErr && err == nil {
				t.Fatalf("%q should have been rejected", tc.pattern)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("%q should have been accepted: %v", tc.pattern, err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "chat/slack") {
				t.Errorf("error %q must name the claimant", err)
			}
		})
	}
}

func TestRegisterRejectsUnusablePatterns(t *testing.T) {
	in := ingress.New(nil)
	// net/http rejects this one; the ingress must turn the panic into an error
	// rather than take the process down.
	in.For("x", "y").HandleFunc("POST /a/{bad", ok)
	if in.Err() == nil {
		t.Fatal("an unusable pattern must be reported")
	}
}

func TestServesRegisteredRoutes(t *testing.T) {
	in := ingress.New(nil)
	in.For("sms", "twilio").HandleFunc("POST /2010-04-01/Accounts/{sid}/Messages.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.PathValue("sid")))
	})
	if err := in.Err(); err != nil {
		t.Fatalf("register: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/2010-04-01/Accounts/AC123/Messages.json", nil)
	in.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "AC123" {
		t.Errorf("status=%d body=%q", rec.Code, rec.Body.String())
	}

	if !in.Has("POST", "/2010-04-01/Accounts/x/Messages.json") {
		t.Error("Has must resolve a wildcard route")
	}
	if in.Has("POST", "/nope") {
		t.Error("Has must not resolve an unmounted route")
	}
}

func TestNotFoundListsWhatIsMounted(t *testing.T) {
	in := ingress.New(nil)
	in.For("mail", "mailjet").HandleFunc("POST /v3.1/send", ok)
	in.SetNotFound(ingress.NotFoundHandler(func() []plugin.PluginInfo {
		return []plugin.PluginInfo{{
			Name: "mail", Title: "Mail", Description: "Fakes mail vendor APIs.",
			Providers: []plugin.ProviderInfo{{
				Name:        "mailjet",
				Description: "Fakes the Mailjet send API.",
				Endpoints:   []plugin.Endpoint{{Method: "POST", Path: "/v3.1/send", Description: "Send."}},
			}},
		}}
	}))

	rec := httptest.NewRecorder()
	in.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v3.1/sned", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"/v3.1/sned", "mailjet", "POST", "/v3.1/send", "tommy providers"} {
		if !strings.Contains(body, want) {
			t.Errorf("404 body must mention %q; it is the most common misconfiguration\n%s", want, body)
		}
	}

	// A JSON client gets the same information in a shape it can read.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/nope", nil)
	req.Header.Set("Accept", "application/json")
	in.Handler().ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `"plugins"`) {
		t.Errorf("json 404 = %s", rec.Body.String())
	}

	// A matched route still works with the 404 handler installed.
	rec = httptest.NewRecorder()
	in.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v3.1/send", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("matched route returned %d", rec.Code)
	}
}
