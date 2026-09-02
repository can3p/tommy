package fcm

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode"

	"github.com/can3p/tommy/plugins/push"
)

// sendMessageRequest is the SendMessageRequest resource verified against the
// live discovery document (https://fcm.googleapis.com/$discovery/rest?version=v1):
// {"message": Message, "validateOnly": bool}. Message is kept raw so the
// push.FormatFCM payload this provider records is the request's own bytes,
// never a re-marshaled copy.
type sendMessageRequest struct {
	Message      json.RawMessage `json:"message"`
	ValidateOnly bool            `json:"validateOnly"`
}

// wireMessage is the Message resource's addressing, notification and data
// fields, typed. android/apns/webpush stay json.RawMessage here so they can
// both be captured verbatim as their own push.Payload entries and parsed
// further, from those same bytes, for the fields the canonical model needs.
//
// Every field name below is lowerCamelCase, the spelling used as the field's
// canonical name in the discovery document's schema.properties keys (fetched
// and parsed programmatically, not summarized - see the package doc
// comment). FCM v1 is a proto3-backed API, and the proto3 JSON mapping spec
// (https://protobuf.dev/programming-guides/json/) is explicit that "parsers
// accept both the lowerCamelCase name ... and the original proto field
// name" - so a real client may legitimately send either this camelCase
// spelling or the underscore_separated one. normalizeKeys is what makes that
// true here: every raw object is passed through it before being decoded into
// one of these structs, so a struct only ever needs to declare the camelCase
// tag and still matches both.
type wireMessage struct {
	Token        string            `json:"token"`
	Fid          string            `json:"fid"`
	Topic        string            `json:"topic"`
	Condition    string            `json:"condition"`
	Data         map[string]string `json:"data"`
	Notification *wireNotification `json:"notification"`
	Android      json.RawMessage   `json:"android"`
	Apns         json.RawMessage   `json:"apns"`
	Webpush      json.RawMessage   `json:"webpush"`
}

// wireNotification is the Notification resource: the platform-independent
// template, present at any of notification / android.notification /
// webpush.notification / apns.payload.aps.alert - this is the first of those.
type wireNotification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Image string `json:"image"`
}

// wireAndroidConfig is AndroidConfig, typed for the fields the canonical
// model reads out of it. Everything else (restrictedPackageName,
// bandwidthConstrainedOk, directBootOk, fcmOptions, ...) stays in the
// verbatim push.FormatFCMAndroid payload only.
type wireAndroidConfig struct {
	CollapseKey  string                   `json:"collapseKey"`
	Priority     string                   `json:"priority"`
	TTL          string                   `json:"ttl"`
	Data         map[string]string        `json:"data"`
	Notification *wireAndroidNotification `json:"notification"`
}

// wireAndroidNotification is AndroidNotification, typed for the fields that
// override the platform-independent Notification. click_action and
// channel_id are deliberately absent from push.Alert - they stay in the
// verbatim payload, see push.Alert.Category's doc comment - and
// notificationPriority is deliberately not read into push.Delivery: it is
// display prominence, not delivery urgency.
type wireAndroidNotification struct {
	Title             string   `json:"title"`
	Body              string   `json:"body"`
	Image             string   `json:"image"`
	Sound             string   `json:"sound"`
	TitleLocKey       string   `json:"titleLocKey"`
	TitleLocArgs      []string `json:"titleLocArgs"`
	BodyLocKey        string   `json:"bodyLocKey"`
	BodyLocArgs       []string `json:"bodyLocArgs"`
	NotificationCount *int     `json:"notificationCount"`
}

// wireWebpushConfig is WebpushConfig, typed only for the one field the
// canonical model can use: a data override. notification and headers carry
// no counterpart on push.Alert (see message.go's doc comment on Alert.Subtitle
// and the push README's "What live vendor documentation contradicted") and
// stay in the verbatim payload only.
type wireWebpushConfig struct {
	Data map[string]string `json:"data"`
}

// subObject returns the raw bytes of one top-level key of a JSON object,
// exactly as sent, or nil when the key is absent, null, or the object itself
// cannot be parsed as an object at all - which the caller has already
// validated by this point, but a defensive nil is cheaper than a panic.
//
// This looks the key up by its single canonical spelling only ("android",
// "apns", "webpush", ...): every key this provider ever passes here is one
// word, so proto3's snake_case/camelCase duality never applies to it and no
// normalization is needed to find it. Contrast normalizeKeys, which is what
// handles duality for the multi-word field names living *inside* these
// objects.
func subObject(doc json.RawMessage, key string) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(doc, &obj); err != nil {
		return nil
	}
	raw, ok := obj[key]
	if !ok || emptyRaw(raw) {
		return nil
	}
	return raw
}

func emptyRaw(b json.RawMessage) bool {
	t := bytes.TrimSpace(b)
	return len(t) == 0 || string(t) == "null"
}

// opaqueKeys names the object keys whose *value* is never proto-JSON-mapped,
// so normalizeKeys must never rename anything inside it:
//
//   - "data" (message.data, android.data, webpush.data) is the caller's own
//     arbitrary string map. Renaming one of its keys - a caller's own
//     "my_custom_key" - would silently corrupt exactly the payload this
//     project exists to show verbatim.
//   - "headers" (apns.headers, webpush.headers) holds real HTTP/APNs header
//     names, not proto field names.
//   - "payload" (apns.payload) is the aps dictionary plus whatever custom
//     keys the caller put beside it - again the caller's own data, not FCM's
//     schema.
//   - "message" is the SendMessageRequest's own Message value. It is
//     opaque *at that one call site* so that decoding the envelope for
//     validateOnly/validate_only never touches the bytes that become the
//     push.FormatFCM payload; buildFromRequest normalizes a copy of it
//     separately, on its own, when it needs wireMessage's typed fields. No
//     object anywhere in this schema has a field actually named "message",
//     so listing it here is harmless everywhere else normalizeKeys is used.
var opaqueKeys = map[string]bool{
	"data":    true,
	"headers": true,
	"payload": true,
	"message": true,
}

// normalizeKeys rewrites every snake_case object key it finds - other than
// inside an opaqueKeys value - to the lowerCamelCase spelling the wire
// structs above declare, so that json.Unmarshal into one of them matches a
// request using either spelling. See wireMessage's doc comment for why both
// are legitimate FCM v1 input, not "the deprecated Legacy API versus v1".
//
// It is a single, general tree-walk rather than a per-field lookup table:
// any field added to any wire struct above is covered automatically, which
// is the point - nobody adding a field later has to remember to also teach
// a normalizer about it.
//
// It never round-trips a value through interface{}: at every level it
// decodes only into map[string]json.RawMessage / []json.RawMessage, so a
// value it does not need to rename (a string, a number, an already-camelCase
// object, anything under an opaque key) is spliced back byte-for-byte
// unchanged rather than reformatted by a generic Marshal. That is what lets
// buildFromRequest normalize a *copy* of the request for typed decoding while
// the original bytes still go untouched into every push.Payload - rule 4
// ("Always populate Raw with the untouched request") and the push core's own
// "Payloads holds the request verbatim" apply just as much to a normalized
// copy's source bytes as to the top-level request body.
//
// When both spellings of one field are present in the same object, the
// camelCase entry wins and the snake_case one is dropped - camelCase is the
// discovery document's canonical name, so that is the one this provider
// prefers when a request is (accidentally or deliberately) ambiguous. See
// TestBothSpellingsConflictCamelCaseWins.
func normalizeKeys(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw
	}
	switch trimmed[0] {
	case '{':
		return normalizeObject(raw)
	case '[':
		return normalizeArray(raw)
	default:
		return raw
	}
}

func normalizeObject(raw json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}

	out := make(map[string]json.RawMessage, len(obj))
	// Any key that is already unambiguous (no underscore - either genuinely
	// camelCase, or a single word with no dual spelling at all) copies
	// straight across.
	for k, v := range obj {
		if !strings.Contains(k, "_") {
			out[k] = v
		}
	}
	// A snake_case key fills its camelCase slot only when that slot is not
	// already taken - the canonical spelling wins on a conflict.
	for k, v := range obj {
		if !strings.Contains(k, "_") {
			continue
		}
		camel := snakeToCamel(k)
		if _, exists := out[camel]; !exists {
			out[camel] = v
		}
	}

	for k, v := range out {
		if opaqueKeys[k] {
			continue
		}
		out[k] = normalizeKeys(v)
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return encoded
}

func normalizeArray(raw json.RawMessage) json.RawMessage {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return raw
	}
	for i, v := range arr {
		arr[i] = normalizeKeys(v)
	}
	encoded, err := json.Marshal(arr)
	if err != nil {
		return raw
	}
	return encoded
}

// snakeToCamel converts one snake_case key to lowerCamelCase using the same
// mechanical rule proto3 JSON name generation uses: each "_x" becomes "X",
// the underscore dropped. A key with no underscore is unaffected by this
// function's caller (normalizeObject only calls it for keys containing "_").
func snakeToCamel(s string) string {
	var b strings.Builder
	upperNext := false
	for _, r := range s {
		if r == '_' {
			upperNext = true
			continue
		}
		if upperNext {
			b.WriteRune(unicode.ToUpper(r))
			upperNext = false
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// overwrite replaces *dst with v only when v is non-empty, the merge rule
// AndroidNotification uses over the platform-independent Notification: an
// override block only overrides the keys it actually sets.
func overwrite(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

// ttlSeconds reads AndroidConfig.ttl's google-duration encoding: a number of
// seconds with an "s" suffix, possibly fractional, rounded down to the
// second. An unparseable value returns -1, which push.ExpiresAfter treats as
// neither zero nor positive, so it lands in Expiry.Raw only rather than being
// silently dropped.
func ttlSeconds(ttl string) int64 {
	n, err := strconv.ParseFloat(strings.TrimSuffix(ttl, "s"), 64)
	if err != nil {
		return -1
	}
	return int64(n)
}

// buildMessage converts one already-validated Message envelope into tommy's
// canonical push.Message. project is the Firebase project taken from the
// request path. raw is the request's own "message" bytes, untouched, kept as
// the first Payload entry; msg is the same bytes already unmarshaled by the
// caller (from a normalized copy - see buildFromRequest).
//
// The point of this conversion, per the push core's own worked example
// (plugins/push/fake_test.go's fcmMessage) and README: targeting records
// which of the four mutually exclusive fields was actually used, the
// android/apns/webpush blocks are lifted out as their own verbatim Payload
// entries rather than merged away, and android.notification's fields
// override the platform-independent notification's only where it sets them.
func buildMessage(project string, raw json.RawMessage, msg wireMessage) *push.Message {
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

	// raw is the untouched "message" bytes - exactly what the caller sent,
	// dual-spelled keys included - so the FormatFCM payload is never a
	// normalized copy.
	m.Payloads = append(m.Payloads, push.Payload{Format: push.FormatFCM, Data: raw})

	var android *wireAndroidConfig
	if sub := subObject(raw, "android"); sub != nil {
		// Likewise: the payload holds sub exactly as sent; only the copy fed
		// to json.Unmarshal below is normalized.
		m.Payloads = append(m.Payloads, push.Payload{Format: push.FormatFCMAndroid, Data: sub})
		android = &wireAndroidConfig{}
		if err := json.Unmarshal(normalizeKeys(sub), android); err != nil {
			android = nil
		}
	}
	if sub := subObject(raw, "apns"); sub != nil {
		m.Payloads = append(m.Payloads, push.Payload{Format: push.FormatFCMApns, Data: sub})
	}
	var webpush *wireWebpushConfig
	if sub := subObject(raw, "webpush"); sub != nil {
		m.Payloads = append(m.Payloads, push.Payload{Format: push.FormatFCMWebpush, Data: sub})
		webpush = &wireWebpushConfig{}
		if err := json.Unmarshal(normalizeKeys(sub), webpush); err != nil {
			webpush = nil
		}
	}

	if n := msg.Notification; n != nil {
		m.Alert = &push.Alert{Title: n.Title, Body: n.Body, Image: n.Image}
	}
	if android != nil {
		m.Delivery.SetPriority(android.Priority)
		m.Delivery.CollapseKey = android.CollapseKey
		if android.TTL != "" {
			m.Delivery.Expiry = push.ExpiresAfter(ttlSeconds(android.TTL), android.TTL)
		}
		if n := android.Notification; n != nil {
			if m.Alert == nil {
				m.Alert = &push.Alert{}
			}
			overwrite(&m.Alert.Title, n.Title)
			overwrite(&m.Alert.Body, n.Body)
			overwrite(&m.Alert.Image, n.Image)
			overwrite(&m.Alert.Sound, n.Sound)
			if n.NotificationCount != nil {
				m.Alert.Badge = n.NotificationCount
			}
			if n.TitleLocKey != "" || n.BodyLocKey != "" {
				m.Alert.Localization = &push.Localization{
					TitleKey: n.TitleLocKey, TitleArgs: n.TitleLocArgs,
					BodyKey: n.BodyLocKey, BodyArgs: n.BodyLocArgs,
				}
			}
		}
	}

	// Data: message.data, overridden by a platform-specific block when one is
	// present - see Message.Data's doc comment ("a platform-level override
	// (android.data, webpush.data) preferred over message.data when both are
	// present, because that is what FCM does"). android wins when both an
	// android and a webpush override set data, since that is the more common
	// shape (a webpush-only override on an otherwise Android-first payload).
	data := msg.Data
	if android != nil && len(android.Data) > 0 {
		data = android.Data
	} else if webpush != nil && len(webpush.Data) > 0 {
		data = webpush.Data
	}
	if len(data) > 0 {
		if encoded, err := json.Marshal(data); err == nil {
			m.Data = encoded
		}
	}

	return m
}
