package push_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/push"
)

// fakeProvider is a test-only push provider. It exists because the FCM and APNs
// providers are the next waves and the plugin core still has to be proven end to
// end.
//
// It is deliberately two endpoints rather than one, and they are deliberately
// shaped like the two real ecosystems: one takes the device token in the path
// with the APNs headers beside it, the other takes an FCM v1 message envelope
// in the body. Half of what this plugin core is for is refusing to pretend
// those are the same request, and a fake with one generic endpoint would prove
// nothing about that.
//
// The conversions below are the smallest honest reading of each wire format.
// They are not the real providers - they ignore authentication, they return a
// made-up response, and they cover only the keys the model names - but they are
// the worked example of how the model is meant to be filled in, so keep them
// faithful.
type fakeProvider struct{}

func (fakeProvider) Name() string   { return "fake" }
func (fakeProvider) Plugin() string { return push.Name }

func (fakeProvider) Description() string {
	return "Test-only endpoints shaped like the two real push ecosystems - a device token in the path with " +
		"APNs headers, and an FCM v1 message envelope in the body - so the push plugin can be exercised " +
		"end to end before the real providers exist."
}

func (fakeProvider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{
		{
			Method:      "POST",
			Path:        "/fake-push/apns/{token}",
			Description: "Accept an APNs-shaped payload for the device token in the path and record it as a push.message event.",
		},
		{
			Method:      "POST",
			Path:        "/fake-push/fcm/{project}",
			Description: "Accept an FCM v1 send envelope and record its message as a push.message event.",
		},
	}
}

func (fakeProvider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{
		{
			Title: "Send a fake APNs alert",
			Lang:  "bash",
			Code: `curl -s -X POST {{.IngressURL}}/fake-push/apns/00fc13adff785122b4ad28809a3420982341241421348097878e577c991de8f0 \
  -H 'apns-topic: com.example.MyApp' \
  -H 'apns-push-type: alert' \
  -H 'apns-priority: 10' \
  -d '{"aps":{"alert":{"title":"Game Request","subtitle":"Five Card Draw","body":"Bob wants to play poker"},"badge":1,"sound":"default"},"gameID":"12345678"}'`,
		},
		{
			Title: "Send a fake FCM data-only message",
			Lang:  "bash",
			Code: `curl -s -X POST {{.IngressURL}}/fake-push/fcm/my-project \
  -d '{"message":{"topic":"weather","data":{"kind":"refresh"},"android":{"priority":"HIGH","ttl":"3600s","collapse_key":"weather"}}}'`,
		},
	}
}

func (p fakeProvider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()
	mux.HandleFunc("POST /fake-push/apns/{token}", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(w, r)
		if body == nil {
			return
		}
		m, err := apnsMessage(r.PathValue("token"), headerMap(r.Header), body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.record(w, r, d, m, body)
	})
	mux.HandleFunc("POST /fake-push/fcm/{project}", func(w http.ResponseWriter, r *http.Request) {
		body := readBody(w, r)
		if body == nil {
			return
		}
		m, err := fcmMessage(r.PathValue("project"), body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		p.record(w, r, d, m, body)
	})
}

func (p fakeProvider) record(w http.ResponseWriter, r *http.Request, d plugin.Deps, m *push.Message, body []byte) {
	ev := push.NewEvent("fake", m)
	ev.Raw.Method = r.Method
	ev.Raw.Path = r.URL.Path
	ev.Raw.Headers = r.Header.Clone()
	ev.Raw.Body = body
	ev.Raw.PeerAddr = r.RemoteAddr
	if err := d.Append(r.Context(), ev); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":       string(ev.ID),
		"kind":     string(m.Kind),
		"displays": m.Displays(),
	})
}

func readBody(w http.ResponseWriter, r *http.Request) []byte {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil
	}
	return body
}

func headerMap(h http.Header) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"apns-topic", "apns-push-type", "apns-priority", "apns-expiration", "apns-collapse-id", "apns-id"} {
		if v := h.Get(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// apnsMessage is the smallest honest reading of an APNs request.
//
// Three things here are the point of the exercise, and the APNs provider should
// do all three:
//
//   - The device token comes from the path, so Target.Source is "path".
//   - apns-topic is the app's bundle ID, so it lands in App, never in Target.
//   - content-available with no alert is a silent push even though it carries
//     no data, so Kind is set explicitly rather than left to be derived.
func apnsMessage(token string, h map[string]string, body []byte) (*push.Message, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}

	m := &push.Message{
		PushType: h["apns-push-type"],
		App:      h["apns-topic"],
		Target:   push.Target{Kind: push.TargetDevice, Value: token, Source: "path"},
		Payloads: []push.Payload{{Format: push.FormatAPNs, Data: json.RawMessage(body)}},
	}
	m.Delivery.SetPriority(h["apns-priority"])
	m.Delivery.CollapseKey = h["apns-collapse-id"]
	if raw, ok := h["apns-expiration"]; ok {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			m.Delivery.Expiry = push.ExpiresAt(n, raw)
		} else {
			m.Delivery.Expiry = &push.Expiry{Raw: raw}
		}
	}

	var aps struct {
		Alert            json.RawMessage `json:"alert"`
		Badge            *int            `json:"badge"`
		Sound            json.RawMessage `json:"sound"`
		Category         string          `json:"category"`
		ContentAvailable int             `json:"content-available"`
	}
	if raw, ok := payload["aps"]; ok {
		if err := json.Unmarshal(raw, &aps); err != nil {
			return nil, err
		}
	}

	alert := &push.Alert{Badge: aps.Badge, Category: aps.Category, Sound: apnsSound(aps.Sound)}
	if len(aps.Alert) > 0 {
		var text string
		if err := json.Unmarshal(aps.Alert, &text); err == nil {
			// aps.alert may be a bare string, in which case it is the body.
			alert.Body = text
		} else {
			var a struct {
				Title        string   `json:"title"`
				Subtitle     string   `json:"subtitle"`
				Body         string   `json:"body"`
				TitleLocKey  string   `json:"title-loc-key"`
				TitleLocArgs []string `json:"title-loc-args"`
				LocKey       string   `json:"loc-key"`
				LocArgs      []string `json:"loc-args"`
			}
			if err := json.Unmarshal(aps.Alert, &a); err != nil {
				return nil, err
			}
			alert.Title, alert.Subtitle, alert.Body = a.Title, a.Subtitle, a.Body
			if a.TitleLocKey != "" || a.LocKey != "" {
				alert.Localization = &push.Localization{
					TitleKey: a.TitleLocKey, TitleArgs: a.TitleLocArgs,
					BodyKey: a.LocKey, BodyArgs: a.LocArgs,
				}
			}
		}
	}
	m.Alert = alert

	// Custom keys are peers of aps, and they may hold any JSON, not just
	// strings - which is why Message.Data is raw JSON rather than a map of
	// strings.
	delete(payload, "aps")
	if len(payload) > 0 {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		m.Data = encoded
	}

	if aps.ContentAvailable == 1 && alert.Empty() {
		m.Kind = push.KindSilent
	}
	m.Normalize()
	return m, nil
}

func apnsSound(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var dict struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &dict)
	return dict.Name
}

// fcmMessage is the smallest honest reading of an FCM v1 send envelope.
//
// The point of the exercise here is targeting: exactly one of four mutually
// exclusive fields carries the address, and which one it was matters, so
// Target.Source records the field name rather than being dropped.
func fcmMessage(project string, body []byte) (*push.Message, error) {
	var envelope struct {
		Message struct {
			Token     string            `json:"token"`
			Fid       string            `json:"fid"`
			Topic     string            `json:"topic"`
			Condition string            `json:"condition"`
			Data      map[string]string `json:"data"`
			Android   *struct {
				CollapseKey  string `json:"collapse_key"`
				Priority     string `json:"priority"`
				TTL          string `json:"ttl"`
				Notification *struct {
					Title       string   `json:"title"`
					Body        string   `json:"body"`
					Image       string   `json:"image"`
					Sound       string   `json:"sound"`
					TitleLocKey string   `json:"title_loc_key"`
					BodyLocKey  string   `json:"body_loc_key"`
					BodyLocArgs []string `json:"body_loc_args"`
					Count       *int     `json:"notification_count"`
				} `json:"notification"`
			} `json:"android"`
			Notification *struct {
				Title string `json:"title"`
				Body  string `json:"body"`
				Image string `json:"image"`
			} `json:"notification"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	msg := envelope.Message

	m := &push.Message{App: project}
	switch {
	case msg.Token != "":
		m.Target = push.Target{Kind: push.TargetDevice, Value: msg.Token, Source: "token"}
	case msg.Fid != "":
		m.Target = push.Target{Kind: push.TargetDevice, Value: msg.Fid, Source: "fid"}
	case msg.Topic != "":
		m.Target = push.Target{Kind: push.TargetTopic, Value: msg.Topic, Source: "topic"}
	case msg.Condition != "":
		m.Target = push.Target{Kind: push.TargetCondition, Value: msg.Condition, Source: "condition"}
	}

	var raw struct {
		Message json.RawMessage `json:"message"`
	}
	_ = json.Unmarshal(body, &raw)
	m.Payloads = append(m.Payloads, push.Payload{Format: push.FormatFCM, Data: raw.Message})
	if sub := subObject(raw.Message, "android"); sub != nil {
		m.Payloads = append(m.Payloads, push.Payload{Format: push.FormatFCMAndroid, Data: sub})
	}
	if sub := subObject(raw.Message, "apns"); sub != nil {
		m.Payloads = append(m.Payloads, push.Payload{Format: push.FormatFCMApns, Data: sub})
	}

	if n := msg.Notification; n != nil {
		m.Alert = &push.Alert{Title: n.Title, Body: n.Body, Image: n.Image}
	}
	if a := msg.Android; a != nil {
		m.Delivery.SetPriority(a.Priority)
		m.Delivery.CollapseKey = a.CollapseKey
		if a.TTL != "" {
			m.Delivery.Expiry = push.ExpiresAfter(ttlSeconds(a.TTL), a.TTL)
		}
		if n := a.Notification; n != nil {
			if m.Alert == nil {
				m.Alert = &push.Alert{}
			}
			// An android.notification overrides the platform-independent one.
			overwrite(&m.Alert.Title, n.Title)
			overwrite(&m.Alert.Body, n.Body)
			overwrite(&m.Alert.Image, n.Image)
			overwrite(&m.Alert.Sound, n.Sound)
			if n.Count != nil {
				m.Alert.Badge = n.Count
			}
			if n.TitleLocKey != "" || n.BodyLocKey != "" {
				m.Alert.Localization = &push.Localization{
					TitleKey: n.TitleLocKey,
					BodyKey:  n.BodyLocKey, BodyArgs: n.BodyLocArgs,
				}
			}
		}
	}
	if len(msg.Data) > 0 {
		encoded, err := json.Marshal(msg.Data)
		if err != nil {
			return nil, err
		}
		m.Data = encoded
	}
	m.Normalize()
	return m, nil
}

func overwrite(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

func subObject(doc json.RawMessage, key string) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(doc, &obj); err != nil {
		return nil
	}
	return obj[key]
}

// ttlSeconds reads FCM's duration encoding: a number of seconds with an "s"
// suffix, possibly fractional, rounded down to the second.
func ttlSeconds(ttl string) int64 {
	n, err := strconv.ParseFloat(strings.TrimSuffix(ttl, "s"), 64)
	if err != nil {
		return -1
	}
	return int64(n)
}

// injectAPNs and injectFCM put a push straight into the store, which is how the
// plugin's own tests fill it: no transport in the way, so a failure is the
// plugin's.
func injectAPNs(t *testing.T, in *testutil.Instance, token string, headers map[string]string, body []byte) *event.Event {
	t.Helper()
	m, err := apnsMessage(token, headers, body)
	if err != nil {
		t.Fatalf("convert apns payload: %v", err)
	}
	return appendEvent(t, in, m, body)
}

func injectFCM(t *testing.T, in *testutil.Instance, project string, body []byte) *event.Event {
	t.Helper()
	m, err := fcmMessage(project, body)
	if err != nil {
		t.Fatalf("convert fcm envelope: %v", err)
	}
	return appendEvent(t, in, m, body)
}

func appendEvent(t *testing.T, in *testutil.Instance, m *push.Message, body []byte) *event.Event {
	t.Helper()
	ev := push.NewEvent("fake", m)
	ev.Raw.Method = http.MethodPost
	ev.Raw.Body = body
	if err := (plugin.Deps{Store: in.Store}).Append(context.Background(), ev); err != nil {
		t.Fatalf("append event: %v", err)
	}
	return ev
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// start boots a whole tommy with the push plugin and the fake provider.
func start(t *testing.T) *testutil.Instance {
	t.Helper()
	return testutil.Start(t, nil, push.New(fakeProvider{}))
}
