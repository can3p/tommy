package push_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/testutil"
)

type envelope struct {
	ID       string         `json:"id"`
	Provider string         `json:"provider"`
	Type     string         `json:"type"`
	Title    string         `json:"title"`
	Preview  string         `json:"preview"`
	Displays bool           `json:"displays"`
	Explain  string         `json:"explain"`
	RawURL   string         `json:"raw_url"`
	Meta     map[string]any `json:"meta"`
	Message  struct {
		Kind     string `json:"kind"`
		PushType string `json:"push_type"`
		App      string `json:"app"`
		Target   struct {
			Kind   string `json:"kind"`
			Value  string `json:"value"`
			Source string `json:"source"`
		} `json:"target"`
		Alert *struct {
			Title string `json:"title"`
			Badge *int   `json:"badge"`
		} `json:"alert"`
		Data     json.RawMessage `json:"data"`
		Delivery struct {
			Priority    string `json:"priority"`
			PriorityRaw string `json:"priority_raw"`
			CollapseKey string `json:"collapse_key"`
			Expiry      *struct {
				Immediate  bool   `json:"immediate"`
				TTLSeconds *int64 `json:"ttl_seconds"`
			} `json:"expiry"`
		} `json:"delivery"`
		Payloads []struct {
			Format string          `json:"format"`
			Data   json.RawMessage `json:"data"`
		} `json:"payloads"`
	} `json:"message"`
}

func listMessages(t *testing.T, in *testutil.Instance, query string) []envelope {
	t.Helper()
	var out []envelope
	url := in.API("/push/messages")
	if query != "" {
		url += "?" + query
	}
	if status := in.GetJSON(url, &out); status != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, status)
	}
	return out
}

func seed(t *testing.T, in *testutil.Instance) {
	t.Helper()
	injectAPNs(t, in, "00fc13adff78", apnsAlertHeaders, fixture(t, "apns_alert.json"))
	injectAPNs(t, in, "00fc13adff78",
		map[string]string{"apns-push-type": "background", "apns-priority": "5", "apns-topic": "com.example.MyApp"},
		fixture(t, "apns_silent.json"))
	injectFCM(t, in, "my-project", fixture(t, "fcm_topic_data.json"))
}

func TestAPIListAndGet(t *testing.T) {
	in := start(t)
	seed(t, in)

	all := listMessages(t, in, "")
	if len(all) != 3 {
		t.Fatalf("listed %d messages, want 3", len(all))
	}
	// Newest first, per the store's ordering.
	if all[0].Message.Target.Kind != "topic" {
		t.Errorf("first message = %+v, want the FCM topic push", all[0].Message.Target)
	}
	for _, e := range all {
		if e.Type != "push.message" || e.Provider != "fake" || e.RawURL == "" || e.Title == "" {
			t.Errorf("envelope is missing correlation fields: %+v", e)
		}
		if e.Explain == "" {
			t.Error("every envelope explains what the device does; that is the plugin's whole point")
		}
	}

	one := all[0]
	var got envelope
	if status := in.GetJSON(in.API("/push/messages/"+one.ID), &got); status != http.StatusOK {
		t.Fatalf("get status = %d", status)
	}
	if got.ID != one.ID || got.Message.Target.Value != "weather" {
		t.Errorf("get returned %+v", got)
	}

	var missing map[string]string
	if status := in.GetJSON(in.API("/push/messages/nope"), &missing); status != http.StatusNotFound {
		t.Errorf("unknown id status = %d, want 404", status)
	}
}

// The filter this plugin exists for: show me everything that displayed nothing.
func TestAPIFilters(t *testing.T) {
	in := start(t)
	seed(t, in)

	cases := []struct {
		query string
		want  int
	}{
		{"displays=false", 2},
		{"displays=true", 1},
		{"kind=silent", 2},
		{"kind=notification", 1},
		{"target_kind=device", 2},
		{"target_kind=topic", 1},
		{"target=weather", 1},
		{"target=00fc13", 2},
		{"app=com.example.MyApp", 2},
		{"app=my-project", 1},
		{"push_type=background", 1},
		{"priority=normal", 2},
		{"priority=10", 1},
		{"data_key=gameID", 1},
		{"data_key=region", 1},
		{"data_key=nope", 0},
		{"kind=notification&displays=false", 0},
	}
	for _, tt := range cases {
		if got := listMessages(t, in, tt.query); len(got) != tt.want {
			t.Errorf("?%s returned %d messages, want %d", tt.query, len(got), tt.want)
		}
	}

	// Paging is applied after the push filters, so a limit never counts
	// messages the caller excluded.
	if got := listMessages(t, in, "displays=false&limit=1"); len(got) != 1 {
		t.Errorf("limit=1 returned %d", len(got))
	}
	if got := listMessages(t, in, "displays=false&offset=2"); len(got) != 0 {
		t.Errorf("offset past the end returned %d", len(got))
	}
}

// The canonical model survives the API, including the parts a provider author
// will be checking their own output against.
func TestAPICarriesTheWholeModel(t *testing.T) {
	in := start(t)
	ev := injectAPNs(t, in, "00fc13adff78", apnsAlertHeaders, fixture(t, "apns_alert.json"))

	var got envelope
	if status := in.GetJSON(in.API("/push/messages/"+string(ev.ID)), &got); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	m := got.Message
	if m.Kind != "notification" || m.PushType != "alert" || m.App != "com.example.MyApp" {
		t.Errorf("message = %+v", m)
	}
	if m.Target.Source != "path" {
		t.Errorf("target source = %q; APNs reads the token out of the path", m.Target.Source)
	}
	if m.Alert == nil || m.Alert.Badge == nil || *m.Alert.Badge != 3 {
		t.Errorf("alert = %+v", m.Alert)
	}
	if m.Delivery.Priority != "high" || m.Delivery.PriorityRaw != "10" {
		t.Errorf("delivery = %+v, want both the level and the raw value", m.Delivery)
	}
	if len(m.Payloads) != 1 || m.Payloads[0].Format != "apns.payload" {
		t.Errorf("payloads = %+v", m.Payloads)
	}
	if !strings.Contains(string(m.Payloads[0].Data), "interruption-level") {
		t.Error("a key the model does not name must still survive verbatim")
	}
	if got.Meta["target_source"] != "path" || got.Meta["displays"] != true {
		t.Errorf("meta = %v", got.Meta)
	}
}

// The raw route serves the request exactly as it arrived, and never as
// something a browser might decide to render.
func TestAPIRaw(t *testing.T) {
	in := start(t)
	ev := injectAPNs(t, in, "00fc13adff78", apnsAlertHeaders, fixture(t, "apns_hostile.json"))

	resp := in.Get(in.API("/push/messages/" + string(ev.ID) + "/raw"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type = %q, want text/plain", ct)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("the raw payload must be served with nosniff")
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != string(fixture(t, "apns_hostile.json")) {
		t.Error("the raw route did not return the bytes exactly as they arrived")
	}
}

func TestAPIDelete(t *testing.T) {
	in := start(t)
	seed(t, in)

	all := listMessages(t, in, "")
	req, err := http.NewRequest(http.MethodDelete, in.API("/push/messages/"+all[0].ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := in.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	if got := listMessages(t, in, ""); len(got) != 2 {
		t.Errorf("after deleting one, %d remain", len(got))
	}

	req, err = http.NewRequest(http.MethodDelete, in.API("/push/messages"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp = in.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear status = %d", resp.StatusCode)
	}
	if got := listMessages(t, in, ""); len(got) != 0 {
		t.Errorf("after clearing, %d remain", len(got))
	}
}
