package apns

import (
	"encoding/json"

	"github.com/can3p/tommy/plugins/push"
)

// apsDictionary is the Apple-defined half of a notification payload: the
// value of the "aps" key. Every field below is from Table 1 of "Generating a
// remote notification", fetched live rather than recalled.
//
// Only the keys the canonical push.Message has a home for are named. The rest
// of Table 1 - thread-id, mutable-content, target-content-id,
// interruption-level, relevance-score, filter-criteria and the whole Live
// Activity set (stale-date, content-state, timestamp, event, dismissal-date,
// attributes-type, attributes) - stays in the verbatim payload, which is
// exactly what push.Payload exists for: an unmodeled key is still captured,
// still shown and still copyable. The few of those worth filtering a capture
// by are additionally lifted into Event.Meta; see apns.go.
type apsDictionary struct {
	// Alert is "Dictionary (or String)": a bare string is the body text.
	Alert json.RawMessage `json:"alert"`
	// Badge is a pointer because 0 is meaningful - "Specify 0 to remove the
	// current badge" - and must not look like an absent badge.
	Badge *int `json:"badge"`
	// Sound is a string for a regular notification and a dictionary for a
	// critical alert.
	Sound             json.RawMessage `json:"sound"`
	Category          string          `json:"category"`
	ThreadID          string          `json:"thread-id"`
	ContentAvailable  int             `json:"content-available"`
	MutableContent    int             `json:"mutable-content"`
	InterruptionLevel string          `json:"interruption-level"`
}

// alertDictionary is Table 2: the keys inside "alert".
//
// subtitle-loc-key and subtitle-loc-args are read but have nowhere to go:
// push.Localization models a title and a body key because those are what FCM
// also has, and the push plugin core is explicit that Apple-only keys stay in
// the verbatim payload rather than bending the shared model. They are named
// here so a future reader sees the omission is deliberate.
type alertDictionary struct {
	Title           string   `json:"title"`
	Subtitle        string   `json:"subtitle"`
	Body            string   `json:"body"`
	LaunchImage     string   `json:"launch-image"`
	TitleLocKey     string   `json:"title-loc-key"`
	TitleLocArgs    []string `json:"title-loc-args"`
	SubtitleLocKey  string   `json:"subtitle-loc-key"`
	SubtitleLocArgs []string `json:"subtitle-loc-args"`
	LocKey          string   `json:"loc-key"`
	LocArgs         []string `json:"loc-args"`
}

// soundDictionary is Table 3: the critical-alert form of "sound". Only name
// reaches the model; critical and volume have no FCM counterpart and stay in
// the verbatim payload, which is the push core's own instruction on
// Alert.Sound.
type soundDictionary struct {
	Critical int     `json:"critical"`
	Name     string  `json:"name"`
	Volume   float64 `json:"volume"`
}

// converted is one request turned into the canonical model, plus the few aps
// facts that belong in Event.Meta rather than in push.Message.
type converted struct {
	Message *push.Message
	// PayloadError is set when the body was not JSON at all. The push is
	// still captured; see buildMessage.
	PayloadError string
	// Aps carries the modeled-nowhere aps keys worth filtering by.
	Aps map[string]any
}

// buildMessage converts one APNs request into the canonical push.Message.
//
// It never fails. A body that is not JSON is recorded as-is with the reason
// noted, because Apple documents no reason string for a malformed payload and
// inventing one would be making up wire format - see README.md.
func buildMessage(token string, h headers, body []byte) converted {
	m := &push.Message{
		// The device token comes from the request path and nothing in the
		// body names it, which is exactly what Target.Source is for.
		Target: push.Target{Kind: push.TargetDevice, Value: token, Source: "path"},
	}
	h.applyTo(m)

	out := converted{Message: m, Aps: map[string]any{}}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		out.PayloadError = err.Error()
		if json.Valid(body) {
			// Valid JSON that is not an object - a bare string or array.
			// Keep it verbatim; it is still displayable and still copyable.
			m.Payloads = append(m.Payloads, push.Payload{Format: push.FormatAPNs, Data: json.RawMessage(body)})
		}
		m.Normalize()
		return out
	}
	m.Payloads = append(m.Payloads, push.Payload{Format: push.FormatAPNs, Data: json.RawMessage(body)})

	var aps apsDictionary
	if raw, ok := payload["aps"]; ok {
		if err := json.Unmarshal(raw, &aps); err != nil {
			// An aps key holding something other than a dictionary. The
			// request is still captured whole; only the modeled reading of
			// it is unavailable.
			out.PayloadError = "aps: " + err.Error()
		}
	}

	alert := &push.Alert{Badge: aps.Badge, Category: aps.Category, Sound: soundName(aps.Sound)}
	applyAlert(alert, aps.Alert)
	m.Alert = alert

	// Custom keys are peers of the aps dictionary, and Apple allows any
	// primitive type there - "dictionary, array, string, number, or Boolean"
	// - which is why push.Message.Data is raw JSON and not a string map.
	delete(payload, "aps")
	if len(payload) > 0 {
		if encoded, err := json.Marshal(payload); err == nil {
			m.Data = encoded
		}
	}

	// The one case the push core says a provider must decide itself: a
	// background push carrying content-available and nothing else has no
	// alert and no data, so DeriveKind would call it empty. It is silent.
	if aps.ContentAvailable == 1 && alert.Empty() {
		m.Kind = push.KindSilent
	}

	if aps.ContentAvailable != 0 {
		out.Aps["content_available"] = aps.ContentAvailable
	}
	if aps.MutableContent != 0 {
		out.Aps["mutable_content"] = aps.MutableContent
	}
	if aps.ThreadID != "" {
		out.Aps["thread_id"] = aps.ThreadID
	}
	if aps.InterruptionLevel != "" {
		out.Aps["interruption_level"] = aps.InterruptionLevel
	}
	if critical(aps.Sound) {
		out.Aps["critical_sound"] = true
	}

	m.Normalize()
	return out
}

// applyAlert reads the alert key, which is a dictionary or a bare string.
func applyAlert(alert *push.Alert, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		// "If you specify a string, the alert displays your string as the
		// body text."
		alert.Body = text
		return
	}
	var a alertDictionary
	if err := json.Unmarshal(raw, &a); err != nil {
		return
	}
	alert.Title, alert.Subtitle, alert.Body = a.Title, a.Subtitle, a.Body
	if a.TitleLocKey != "" || a.LocKey != "" {
		alert.Localization = &push.Localization{
			TitleKey: a.TitleLocKey, TitleArgs: a.TitleLocArgs,
			BodyKey: a.LocKey, BodyArgs: a.LocArgs,
		}
	}
}

// soundName reads the sound key in either of its two documented forms.
func soundName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var d soundDictionary
	if err := json.Unmarshal(raw, &d); err != nil {
		return ""
	}
	return d.Name
}

// critical reports whether the sound key is a critical-alert dictionary.
func critical(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var d soundDictionary
	if err := json.Unmarshal(raw, &d); err != nil {
		return false
	}
	return d.Critical == 1
}
