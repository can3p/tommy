package push_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/plugins/push"
)

func intp(n int) *int { return &n }

// The fixtures are read through the fake provider's conversions, which are the
// worked example of how a real provider fills the model in. Asserting the
// canonical model this produces is what pins the contract the FCM and APNs
// providers will code against.
func TestModelFromFixtures(t *testing.T) {
	tests := []struct {
		name  string
		build func(t *testing.T) *push.Message
		want  func(t *testing.T, m *push.Message)
	}{
		{
			name:  "apns alert",
			build: func(t *testing.T) *push.Message { return apns(t, "apns_alert.json", apnsAlertHeaders) },
			want: func(t *testing.T, m *push.Message) {
				if m.Kind != push.KindNotification {
					t.Errorf("kind = %q, want notification", m.Kind)
				}
				// apns-topic is the bundle ID, not a pub/sub topic. Getting
				// this wrong is the easiest way to break the model.
				if m.App != "com.example.MyApp" {
					t.Errorf("app = %q, want the bundle id from apns-topic", m.App)
				}
				if m.Target.Kind != push.TargetDevice {
					t.Errorf("target kind = %q, want device", m.Target.Kind)
				}
				if m.Target.Source != "path" {
					t.Errorf("target source = %q, want path", m.Target.Source)
				}
				if m.Target.Kind.Fanout() {
					t.Error("a device target must not be marked as fanning out")
				}
				if m.Alert.Title != "Game Request" || m.Alert.Subtitle != "Five Card Draw" ||
					m.Alert.Body != "Bob wants to play poker" {
					t.Errorf("alert = %+v", m.Alert)
				}
				if m.Alert.Badge == nil || *m.Alert.Badge != 3 {
					t.Errorf("badge = %v, want 3", m.Alert.Badge)
				}
				if m.Alert.Sound != "bingbong.aiff" || m.Alert.Category != "GAME_INVITATION" {
					t.Errorf("sound/category = %q/%q", m.Alert.Sound, m.Alert.Category)
				}
				if m.Delivery.Priority != push.PriorityHigh || m.Delivery.PriorityRaw != "10" {
					t.Errorf("priority = %q raw %q, want high/10", m.Delivery.Priority, m.Delivery.PriorityRaw)
				}
				if m.Delivery.CollapseKey != "poker" {
					t.Errorf("collapse key = %q", m.Delivery.CollapseKey)
				}
				// Custom keys are peers of aps and may be any JSON, so a
				// number array has to survive.
				keys := m.DataKeys()
				if len(keys) != 2 || keys[0] != "gameID" || keys[1] != "seats" {
					t.Errorf("data keys = %v, want gameID and seats", keys)
				}
				if !strings.Contains(string(m.Data), `[1,2,3]`) {
					t.Errorf("data = %s, want the array kept as an array", m.Data)
				}
				if _, ok := m.Payload(push.FormatAPNs); !ok {
					t.Error("the verbatim apns payload was not kept")
				}
			},
		},
		{
			name: "apns silent",
			build: func(t *testing.T) *push.Message {
				return apns(t, "apns_silent.json", map[string]string{"apns-push-type": "background", "apns-priority": "5"})
			},
			want: func(t *testing.T, m *push.Message) {
				// content-available with no alert and no custom keys: nothing
				// displays, nothing is carried, and it is still not "empty".
				if m.Kind != push.KindSilent {
					t.Errorf("kind = %q, want silent", m.Kind)
				}
				if m.Displays() {
					t.Error("a background push must not report as displaying")
				}
				if m.Alert != nil {
					t.Errorf("alert = %+v, want nil", m.Alert)
				}
				if m.HasData() {
					t.Error("this payload carries no custom keys")
				}
				if m.Delivery.Priority != push.PriorityNormal || m.Delivery.PriorityRaw != "5" {
					t.Errorf("priority = %q raw %q, want normal/5", m.Delivery.Priority, m.Delivery.PriorityRaw)
				}
				if m.PushType != "background" {
					t.Errorf("push type = %q", m.PushType)
				}
			},
		},
		{
			name:  "apns badge only",
			build: func(t *testing.T) *push.Message { return apns(t, "apns_badge_only.json", nil) },
			want: func(t *testing.T, m *push.Message) {
				// Apple's own line is that alert, badge and sound are the keys
				// that interact with the user, so this displays - but it has no
				// banner, and the tab has to be able to say which.
				if m.Kind != push.KindNotification {
					t.Errorf("kind = %q, want notification", m.Kind)
				}
				if m.Alert.HasBanner() {
					t.Error("a badge-and-sound push has no banner text")
				}
				if m.Alert.Badge == nil || *m.Alert.Badge != 0 {
					t.Fatalf("badge = %v, want a present zero", m.Alert.Badge)
				}
				if got := m.Preview(); !strings.Contains(got, "clears the badge") {
					t.Errorf("preview = %q, want badge 0 described as clearing", got)
				}
			},
		},
		{
			name:  "apns empty",
			build: func(t *testing.T) *push.Message { return apns(t, "apns_empty.json", nil) },
			want: func(t *testing.T, m *push.Message) {
				if m.Kind != push.KindEmpty {
					t.Errorf("kind = %q, want empty", m.Kind)
				}
				if m.Title() != "(empty push)" {
					t.Errorf("title = %q", m.Title())
				}
			},
		},
		{
			name:  "apns localized",
			build: func(t *testing.T) *push.Message { return apns(t, "apns_localized.json", nil) },
			want: func(t *testing.T, m *push.Message) {
				// A localization key is display payload: it renders as
				// something on the device, even if only the key itself.
				if m.Kind != push.KindNotification {
					t.Errorf("kind = %q, want notification", m.Kind)
				}
				l := m.Alert.Localization
				if l == nil || l.BodyKey != "GAME_PLAY_REQUEST_FORMAT" || l.TitleKey != "GAME_TITLE" {
					t.Fatalf("localization = %+v", l)
				}
				if len(l.BodyArgs) != 2 || l.BodyArgs[0] != "Shelly" {
					t.Errorf("body args = %v", l.BodyArgs)
				}
				if m.Title() != "GAME_TITLE" {
					t.Errorf("title = %q, want the title key", m.Title())
				}
			},
		},
		{
			name:  "apns critical sound dictionary",
			build: func(t *testing.T) *push.Message { return apns(t, "apns_critical_sound.json", nil) },
			want: func(t *testing.T, m *push.Message) {
				// A bare-string aps.alert is the body.
				if m.Alert.Body != "Pressure vessel over limit" || m.Alert.Title != "" {
					t.Errorf("alert = %+v, want a bare-string alert read as the body", m.Alert)
				}
				// The dictionary form contributes its name; the critical flag
				// and volume stay in the verbatim payload on purpose.
				if m.Alert.Sound != "klaxon.caf" {
					t.Errorf("sound = %q", m.Alert.Sound)
				}
				p, ok := m.Payload(push.FormatAPNs)
				if !ok || !strings.Contains(string(p.Data), `"critical"`) {
					t.Error("the critical flag must survive in the verbatim payload")
				}
			},
		},
		{
			name:  "fcm notification to a token",
			build: func(t *testing.T) *push.Message { return fcm(t, "fcm_notification.json") },
			want: func(t *testing.T, m *push.Message) {
				if m.Kind != push.KindNotification {
					t.Errorf("kind = %q, want notification", m.Kind)
				}
				if m.App != "my-project" {
					t.Errorf("app = %q, want the project from the path", m.App)
				}
				if m.Target.Kind != push.TargetDevice || m.Target.Source != "token" {
					t.Errorf("target = %+v, want a device read from the token field", m.Target)
				}
				if m.Alert.Title != "Breakfast is ready" || m.Alert.Image == "" {
					t.Errorf("alert = %+v", m.Alert)
				}
				// android.notification overrides the platform-independent one.
				if m.Alert.Sound != "default" {
					t.Errorf("sound = %q, want the android override", m.Alert.Sound)
				}
				if m.Alert.Badge == nil || *m.Alert.Badge != 2 {
					t.Errorf("badge = %v, want notification_count 2", m.Alert.Badge)
				}
				if m.Delivery.Priority != push.PriorityHigh || m.Delivery.PriorityRaw != "HIGH" {
					t.Errorf("priority = %q raw %q", m.Delivery.Priority, m.Delivery.PriorityRaw)
				}
				e := m.Delivery.Expiry
				if e == nil || e.TTLSeconds == nil || *e.TTLSeconds != 3600 {
					t.Fatalf("expiry = %+v, want a 3600s ttl", e)
				}
				if e.At != nil {
					t.Error("FCM states a duration; nothing may invent an absolute deadline for it")
				}
				// The per-platform blocks are lifted out so a reader sees them.
				for _, f := range []push.Format{push.FormatFCM, push.FormatFCMAndroid, push.FormatFCMApns} {
					if _, ok := m.Payload(f); !ok {
						t.Errorf("no verbatim payload for %s", f)
					}
				}
				// click_action and channel_id are not Category. They stay in
				// the verbatim payload.
				if m.Alert.Category != "" {
					t.Errorf("category = %q, want empty: FCM has no aps.category", m.Alert.Category)
				}
			},
		},
		{
			name:  "fcm data-only to a topic",
			build: func(t *testing.T) *push.Message { return fcm(t, "fcm_topic_data.json") },
			want: func(t *testing.T, m *push.Message) {
				if m.Kind != push.KindSilent {
					t.Errorf("kind = %q, want silent", m.Kind)
				}
				if m.Target.Kind != push.TargetTopic || m.Target.Value != "weather" {
					t.Errorf("target = %+v", m.Target)
				}
				if !m.Target.Kind.Fanout() {
					t.Error("a topic must be marked as fanning out")
				}
				// ttl "0s" and apns-expiration 0 mean the same thing, and the
				// model gives that shared meaning a name.
				e := m.Delivery.Expiry
				if e == nil || !e.Immediate || e.TTLSeconds != nil {
					t.Errorf("expiry = %+v, want immediate", e)
				}
				if keys := m.DataKeys(); len(keys) != 2 {
					t.Errorf("data keys = %v", keys)
				}
				if got := m.Preview(); !strings.Contains(got, "kind") {
					t.Errorf("preview = %q, want the data keys named", got)
				}
			},
		},
		{
			name:  "fcm condition",
			build: func(t *testing.T) *push.Message { return fcm(t, "fcm_condition.json") },
			want: func(t *testing.T, m *push.Message) {
				if m.Target.Kind != push.TargetCondition || m.Target.Source != "condition" {
					t.Errorf("target = %+v", m.Target)
				}
				if !m.Target.Kind.Fanout() {
					t.Error("a condition fans out")
				}
				// A condition is an expression and must be shown whole, not
				// shortened like an opaque token.
				if m.Target.Display() != m.Target.Value {
					t.Errorf("display = %q, want the whole expression", m.Target.Display())
				}
			},
		},
		{
			name:  "fcm installation id",
			build: func(t *testing.T) *push.Message { return fcm(t, "fcm_fid.json") },
			want: func(t *testing.T, m *push.Message) {
				// The live discovery document deprecates "token" in favor of
				// "fid". Both are a single installation, and Source is what
				// keeps them apart.
				if m.Target.Kind != push.TargetDevice || m.Target.Source != "fid" {
					t.Errorf("target = %+v, want a device read from fid", m.Target)
				}
				if m.Kind != push.KindSilent {
					t.Errorf("kind = %q, want silent", m.Kind)
				}
			},
		},
		{
			name:  "fcm localized",
			build: func(t *testing.T) *push.Message { return fcm(t, "fcm_localized.json") },
			want: func(t *testing.T, m *push.Message) {
				if m.Kind != push.KindNotification {
					t.Errorf("kind = %q, want notification", m.Kind)
				}
				l := m.Alert.Localization
				if l == nil || l.TitleKey != "news_title" || l.BodyKey != "news_body" {
					t.Fatalf("localization = %+v", l)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.build(t)
			tt.want(t, m)
			// Whatever a provider produced must survive a round trip through
			// JSON, because that is what a serializing store would do to it.
			encoded, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back push.Message
			if err := json.Unmarshal(encoded, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			back.Normalize()
			if back.Kind != m.Kind || back.Target != m.Target || back.App != m.App {
				t.Errorf("round trip lost something: %+v vs %+v", back, m)
			}
		})
	}
}

var apnsAlertHeaders = map[string]string{
	"apns-topic":       "com.example.MyApp",
	"apns-push-type":   "alert",
	"apns-priority":    "10",
	"apns-collapse-id": "poker",
}

func apns(t *testing.T, name string, headers map[string]string) *push.Message {
	t.Helper()
	m, err := apnsMessage("00fc13adff785122b4ad28809a3420982341241421348097878e577c991de8f0", headers, fixture(t, name))
	if err != nil {
		t.Fatalf("convert %s: %v", name, err)
	}
	return m
}

func fcm(t *testing.T, name string) *push.Message {
	t.Helper()
	m, err := fcmMessage("my-project", fixture(t, name))
	if err != nil {
		t.Fatalf("convert %s: %v", name, err)
	}
	return m
}

// The three ecosystems spell urgency differently and count a different number
// of levels. The mapping lives in one place so two providers cannot disagree
// about what "5" means.
func TestPriorityOf(t *testing.T) {
	for raw, want := range map[string]push.Priority{
		"10":       push.PriorityHigh,
		"5":        push.PriorityNormal,
		"1":        push.PriorityLow,
		"HIGH":     push.PriorityHigh,
		"high":     push.PriorityHigh,
		"NORMAL":   push.PriorityNormal,
		"normal":   push.PriorityNormal,
		"low":      push.PriorityLow,
		"very-low": push.PriorityLow,
	} {
		got, ok := push.PriorityOf(raw)
		if !ok || got != want {
			t.Errorf("PriorityOf(%q) = %q,%v; want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "urgent", "11", "PRIORITY_MAX"} {
		if _, ok := push.PriorityOf(raw); ok {
			t.Errorf("PriorityOf(%q) claimed to recognize it", raw)
		}
	}
	// An unrecognized value is still recorded rather than dropped, because it
	// is exactly what somebody is here to see.
	var d push.Delivery
	d.SetPriority("PRIORITY_MAX")
	if d.PriorityRaw != "PRIORITY_MAX" || d.Priority != "" {
		t.Errorf("delivery = %+v, want the raw value kept without a level", d)
	}
}

// APNs states an absolute deadline and FCM a relative lifetime. Neither is
// converted into the other, and both spell "try once, don't store" as zero.
func TestExpiry(t *testing.T) {
	received := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	ttl := push.ExpiresAfter(3600, "3600s")
	if ttl.At != nil || ttl.TTLSeconds == nil || ttl.Immediate {
		t.Fatalf("ExpiresAfter = %+v", ttl)
	}
	deadline, ok := ttl.Deadline(received)
	if !ok || !deadline.Equal(received.Add(time.Hour)) {
		t.Errorf("deadline = %v,%v", deadline, ok)
	}

	at := push.ExpiresAt(received.Add(2*time.Hour).Unix(), "1757246400")
	if at.At == nil || at.TTLSeconds != nil {
		t.Fatalf("ExpiresAt = %+v", at)
	}
	if d, ok := at.Deadline(received); !ok || !d.Equal(received.Add(2*time.Hour)) {
		t.Errorf("deadline = %v,%v", d, ok)
	}

	// Apple's zero is a sentinel, not 1 January 1970 - which is why Expiry
	// cannot simply be a *time.Time.
	for _, e := range []*push.Expiry{push.ExpiresAt(0, "0"), push.ExpiresAfter(0, "0s")} {
		if !e.Immediate || e.At != nil || e.TTLSeconds != nil {
			t.Errorf("zero expiry = %+v, want immediate", e)
		}
		if _, ok := e.Deadline(received); ok {
			t.Error("an immediate expiry has no deadline to draw on a clock")
		}
		if got := e.Describe(); got != "deliver immediately or drop" {
			t.Errorf("describe = %q", got)
		}
	}

	if got := (*push.Expiry)(nil).Describe(); got != "" {
		t.Errorf("nil expiry describes as %q, want empty", got)
	}
}

func TestKindDerivation(t *testing.T) {
	tests := []struct {
		name string
		m    push.Message
		want push.Kind
	}{
		{"title only", push.Message{Alert: &push.Alert{Title: "hi"}}, push.KindNotification},
		{"badge only", push.Message{Alert: &push.Alert{Badge: intp(1)}}, push.KindNotification},
		{"sound only", push.Message{Alert: &push.Alert{Sound: "default"}}, push.KindNotification},
		{"loc key only", push.Message{Alert: &push.Alert{Localization: &push.Localization{BodyKey: "K"}}}, push.KindNotification},
		{"data only", push.Message{Data: json.RawMessage(`{"a":"b"}`)}, push.KindSilent},
		{"declared push type", push.Message{PushType: "voip"}, push.KindSilent},
		{"declared alert with nothing in it", push.Message{PushType: "alert"}, push.KindEmpty},
		{"nothing at all", push.Message{}, push.KindEmpty},
		{"empty alert struct", push.Message{Alert: &push.Alert{}}, push.KindEmpty},
		{"empty data object", push.Message{Data: json.RawMessage(`{}`)}, push.KindEmpty},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.m
			m.Normalize()
			if m.Kind != tt.want {
				t.Errorf("kind = %q, want %q", m.Kind, tt.want)
			}
			if m.Kind.Explain() == "" {
				t.Error("every kind must explain itself; the tab and the API both show it")
			}
		})
	}

	// A provider that knows better wins: an APNs background push with
	// content-available and nothing else would otherwise derive as empty.
	m := push.Message{Kind: push.KindSilent}
	m.Normalize()
	if m.Kind != push.KindSilent {
		t.Errorf("Normalize overwrote a kind the provider set: %q", m.Kind)
	}
}

// Normalize must leave a message in the same state whether it is called once by
// a provider or again on read-back.
func TestNormalizeIsIdempotent(t *testing.T) {
	m := apns(t, "apns_alert.json", apnsAlertHeaders)
	first, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	m.Normalize()
	m.Normalize()
	second, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("Normalize is not idempotent:\n%s\n%s", first, second)
	}
}

func TestTargetDisplay(t *testing.T) {
	long := strings.Repeat("a", 160)
	device := push.Target{Kind: push.TargetDevice, Value: long}
	got := device.Display()
	if got == long || len([]rune(got)) > 30 {
		t.Errorf("a long device token was not shortened: %q", got)
	}
	if !strings.HasPrefix(got, "aaaa") || !strings.HasSuffix(got, "aaaa") {
		t.Errorf("shortening must keep both ends: %q", got)
	}
	topic := push.Target{Kind: push.TargetTopic, Value: "weather"}
	if topic.Display() != "weather" {
		t.Errorf("topic display = %q", topic.Display())
	}
	if (push.Target{}).Display() != "(no target)" {
		t.Error("an unaddressed push still needs a label")
	}
}

func TestFormatsAndKinds(t *testing.T) {
	for _, f := range push.Formats() {
		if !f.Known() || f.Label() == "" {
			t.Errorf("format %q is listed but has no label", f)
		}
	}
	if push.Format("apns.v99").Known() {
		t.Error("an unknown format must not claim to be known")
	}
	if got := push.Format("apns.v99").Label(); got != "apns.v99" {
		t.Errorf("an unknown format labels itself %q; it is still stored and still shown", got)
	}
	for _, k := range push.Kinds() {
		if k.Label() == "" || k.Explain() == "" {
			t.Errorf("kind %q is incompletely described", k)
		}
	}
}
