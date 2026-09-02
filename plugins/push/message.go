// Package push is tommy's push-notification content type: the canonical
// Message every push provider converts into, the read-back API under
// /api/v1/push/ and the lock-screen tab under /ui/push/.
//
// Providers (FCM, APNs, and whatever comes later) live in
// plugins/push/providers/... and never import each other; all they share is the
// Message in this file.
//
// # Two ecosystems, one model, neither one's vocabulary
//
// APNs and FCM overlap in what they do and agree on almost nothing about how
// they say it. The model here is built from what the two genuinely share, named
// in neutral English, with every field's doc comment naming the wire field it
// comes from on each side. Where only one side has a concept, the field says so
// rather than being quietly dropped or renamed into the other's terms.
//
// Four design points are load bearing:
//
//   - Targeting is not "a recipient". APNs addresses one device, and the token
//     is in the request path (POST /3/device/{token}), not the body. FCM
//     addresses a device, an installation, a topic or a boolean condition over
//     topics, in the body, and a topic fans out to every subscriber. Target
//     keeps the kind and the wire location apart so a capture says what was
//     actually sent - see Target.
//   - Whether the device displays anything is the headline, not a detail. An
//     APNs push with content-available and no alert, and an FCM message with
//     only a data block, both display nothing at all, and "why did nothing show
//     up" is most of what anyone debugs here. Kind says it in one word instead
//     of leaving a reader to infer it from an absent title.
//   - The vendor's own payload is kept verbatim behind a Format discriminator,
//     exactly as the chat plugin keeps Block Kit and Adaptive Cards. That is
//     what lets a provider ship before the model understands every field: an
//     unmodeled key is still captured, still shown, still copyable.
//   - Everything in here is untrusted. A push title, body and data block are
//     written by whatever system is under test; every read surface interpolates
//     them as plain strings through html/template and none of them renders a
//     captured value as HTML.
//
// # A note on FCM field spellings
//
// FCM v1 is a proto3-backed Google API, so its JSON parser accepts both the
// canonical lowerCamelCase name and the original snake_case proto field name:
// collapseKey and collapse_key are the same field, as are clickAction and
// click_action, validateOnly and validate_only. The discovery document lists
// only the camelCase form because that is the canonical output name, not
// because the other is rejected - snake_case here is NOT the deprecated Legacy
// API. The comments below use whichever spelling reads more clearly; a provider
// must accept both, and the fcm provider normalises them before decoding.
//
// # What was checked against live vendor documentation
//
// The FCM shape is taken from the live discovery document served at
// https://fcm.googleapis.com/$discovery/rest?version=v1 and the reference page
// it backs. The APNs shape is taken from Apple's "Generating a remote
// notification" (the aps payload key reference) and "Sending notification
// requests to APNs" (the request headers). Both contradicted assumptions worth
// recording; the contradictions are noted at the fields they affect.
package push

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Name is the plugin name and the URL segment it is mounted under.
const Name = "push"

// EventType is the event.Type every captured push carries. A provider that
// grows another resource later - a token-registration call, a feedback poll -
// adds a new type rather than overloading this one, and every read surface
// switches on the type instead of assuming it.
const EventType = "push.message"

// Transport is the Raw.Transport a push provider records. Both ecosystems are
// HTTP: FCM is HTTP/1.1-compatible JSON, APNs is the same request over HTTP/2.
// The version is not part of this value, because Raw.Transport names the
// family and the ingress may serve either.
const Transport = "http"

// Kind says whether the receiving device will show the user anything by
// itself. It is the single most-debugged fact about a push and the reason this
// tab exists rather than the generic event view.
//
// It is deliberately about the effect, not about what the sender called it: an
// APNs push declared apns-push-type: alert that carries an empty aps dictionary
// still displays nothing, and saying so is the point. What the sender called it
// is kept separately in Message.PushType.
type Kind string

const (
	// KindNotification carries something the platform displays without the app
	// running: alert text, a badge count or a sound. Apple's own line is "to
	// interact with the user, include the alert, badge, or sound keys"; FCM's
	// equivalent is a notification block at any level (notification,
	// android.notification, apns.payload.aps.alert, webpush.notification).
	KindNotification Kind = "notification"
	// KindSilent displays nothing. The app is woken to handle the payload
	// itself: an APNs background push (content-available, no alert), or an FCM
	// data-only message. This is the case people spend an afternoon on.
	KindSilent Kind = "silent"
	// KindEmpty displays nothing and carries nothing either - no alert, no
	// data, no declared push type that would wake the app. It is almost always
	// a mistake, and it is named so that the tab can say so out loud rather
	// than rendering a blank card.
	KindEmpty Kind = "empty"
)

// Kinds lists every kind in a stable order, so a UI can offer a filter and a
// test can assert the switch is exhaustive.
func Kinds() []Kind { return []Kind{KindNotification, KindSilent, KindEmpty} }

// Displays reports whether the platform puts something in front of the user.
func (k Kind) Displays() bool { return k == KindNotification }

// Label is the human name of a kind, for a badge.
func (k Kind) Label() string {
	switch k {
	case KindNotification:
		return "notification"
	case KindSilent:
		return "silent"
	case KindEmpty:
		return "empty"
	default:
		return string(k)
	}
}

// Explain is the one sentence the tab shows about what the device does with
// this push. It exists here rather than in the template so that the API can
// hand back the same sentence and the two never drift.
func (k Kind) Explain() string {
	switch k {
	case KindNotification:
		return "The device displays this: it has alert text, a badge or a sound, so it appears without the app running."
	case KindSilent:
		return "The device displays nothing. This wakes the app in the background to handle the payload itself."
	case KindEmpty:
		return "The device displays nothing and the app receives nothing. There is no alert, no data and no push type that would wake the app - this is almost always a mistake."
	default:
		return ""
	}
}

// TargetKind says what a push is addressed to. It is not "a recipient": the two
// ecosystems address genuinely different things and one of them fans out.
type TargetKind string

const (
	// TargetDevice is one device or app installation, named by an opaque token
	// the platform minted. An APNs device token (64 hex characters in the
	// request path), an FCM registration token, or an FCM Firebase Installation
	// ID are all this kind; Target.Source keeps them apart.
	TargetDevice TargetKind = "device"
	// TargetTopic is a publish/subscribe topic: FCM delivers to every device
	// subscribed to it, so one captured message stands for an unknown number of
	// deliveries. APNs has no equivalent - see the warning on Target.Source
	// about apns-topic, which despite its name is not this.
	TargetTopic TargetKind = "topic"
	// TargetCondition is a boolean expression over topics, FCM's
	// "'foo' in topics && 'bar' in topics". FCM only.
	TargetCondition TargetKind = "condition"
)

// Fanout reports whether the target stands for more than one device. It is
// worth saying in the UI: a topic push that "did not arrive" may have arrived
// on nine devices out of ten.
func (t TargetKind) Fanout() bool { return t == TargetTopic || t == TargetCondition }

// Label is the human name of a target kind.
func (t TargetKind) Label() string {
	switch t {
	case TargetDevice:
		return "device"
	case TargetTopic:
		return "topic"
	case TargetCondition:
		return "condition"
	default:
		return string(t)
	}
}

// Target is what the push was addressed to.
//
// Source is the load-bearing half. The two ecosystems put the address in
// different places and call it different things, and collapsing that into one
// string is how a capture stops being evidence:
//
//   - APNs puts the device token in the request path, POST /3/device/{token},
//     and nothing in the body names it. An APNs provider records
//     Kind: TargetDevice, Source: "path".
//   - FCM puts it in the body as exactly one of four mutually exclusive fields.
//     Verified against the live discovery document rather than the plan, which
//     named three: "token" is now marked deprecated in favor of "fid", a
//     Firebase Installation ID, and both are still accepted. An FCM provider
//     records Source: "token", "fid", "topic" or "condition" - the field it
//     actually found.
//
// Beware apns-topic. Despite the name it is the app's bundle ID, not a
// pub/sub topic, and it must never become a TargetTopic. It belongs in
// Message.App.
type Target struct {
	Kind TargetKind `json:"kind"`
	// Value is the address exactly as it was sent: the token, the topic name,
	// or the condition expression. It is untrusted text like everything else
	// here and is never parsed for meaning.
	Value string `json:"value"`
	// Source is the wire location the value was read from - "path" for APNs,
	// and "token", "fid", "topic" or "condition" for FCM. Empty when a provider
	// has nothing more specific to say.
	Source string `json:"source,omitempty"`
}

// Empty reports whether the message named no target at all.
func (t Target) Empty() bool { return strings.TrimSpace(t.Value) == "" }

// Display is the target's label for a list or a badge, with long opaque tokens
// shortened in the middle: an FCM registration token runs past 150 characters
// and would push everything else off the row, while its head and tail are what
// somebody actually compares against the token their app logged.
func (t Target) Display() string {
	v := strings.TrimSpace(t.Value)
	if v == "" {
		return "(no target)"
	}
	if t.Kind != TargetDevice {
		return v
	}
	return Shorten(v, 24)
}

// Shorten elides the middle of a long opaque identifier, keeping enough of each
// end to compare against. n is the number of characters kept in total.
func Shorten(s string, n int) string {
	if n < 8 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	head := n / 2
	tail := n - head
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

// Localization is a title or body that names a key in the app's own string
// resources rather than carrying the text.
//
// Both ecosystems have it, under different spellings: APNs uses title-loc-key /
// title-loc-args and loc-key / loc-args inside the alert dictionary (plus
// subtitle-loc-key / subtitle-loc-args, which have no FCM counterpart and stay
// in the verbatim payload), and FCM uses title_loc_key / title_loc_args and
// body_loc_key / body_loc_args on android.notification.
//
// It is modeled rather than left to the verbatim payload because a
// notification that shows a raw resource key on the device - or shows nothing,
// because the key is missing from the bundle - is a common enough failure that
// the tab should be able to say "this text is a key, not a message".
type Localization struct {
	TitleKey  string   `json:"title_key,omitempty"`
	TitleArgs []string `json:"title_args,omitempty"`
	BodyKey   string   `json:"body_key,omitempty"`
	BodyArgs  []string `json:"body_args,omitempty"`
}

// Empty reports whether any localization key was set.
func (l *Localization) Empty() bool {
	return l == nil || (l.TitleKey == "" && l.BodyKey == "")
}

// Alert is what the platform displays. A nil Alert means the push carries no
// display payload at all, which is exactly what makes it silent.
//
// Every field names the wire field it comes from on each side. Where a field
// exists on only one side that is stated, because a provider author reaching
// for the nearest-looking neighbor on the other side is how a model gets
// quietly wrong.
type Alert struct {
	// Title is the notification's title. APNs aps.alert.title; FCM
	// notification.title, overridden by android.notification.title.
	Title string `json:"title,omitempty"`
	// Subtitle is the second line above the body. APNs aps.alert.subtitle
	// only - FCM has no subtitle on any of its notification blocks, so an FCM
	// provider leaves this empty rather than promoting something else into it.
	Subtitle string `json:"subtitle,omitempty"`
	// Body is the message text. APNs aps.alert.body, or the whole aps.alert
	// when it was sent as a bare string; FCM notification.body, overridden by
	// android.notification.body.
	Body string `json:"body,omitempty"`
	// Image is the URL of an image to display. FCM notification.image,
	// android.notification.image and apns.fcm_options.image; APNs itself has no
	// image key, since rich media there arrives through a notification service
	// extension fetching a URL from a custom key.
	//
	// It points at somebody else's server. Tommy never fetches it and the tab
	// shows it as text rather than as an <img> or an href, so a captured URL
	// cannot make a browser reach out to whatever the sender named.
	Image string `json:"image,omitempty"`
	// Badge is the number to show on the app icon. APNs aps.badge; FCM
	// android.notification.notification_count, which Google documents as "may
	// be displayed as a badge count for launchers that support badging".
	//
	// It is a pointer because zero is meaningful and different from unset:
	// Apple documents badge 0 as "remove the current badge", so a push whose
	// only job is clearing the badge must not look like one that never
	// mentioned it.
	Badge *int `json:"badge,omitempty"`
	// Sound is the sound to play: a filename bundled with the app, or
	// "default" for the system sound. APNs aps.sound; FCM
	// android.notification.sound.
	//
	// APNs also accepts a dictionary here for critical alerts
	// ({"critical":1,"name":…,"volume":…}); a provider puts its name in this
	// field and leaves the critical flag and volume to the verbatim payload,
	// because FCM has no counterpart and inventing one would be this model
	// letting Apple's vocabulary win.
	Sound string `json:"sound,omitempty"`
	// Category is the notification type the app registered, which decides
	// which action buttons appear. APNs aps.category only.
	//
	// FCM's nearest-looking neighbors are different concepts and must not be
	// mapped here: android.notification.click_action names the activity a tap
	// opens, and channel_id names the Android notification channel. Both stay
	// in the verbatim payload.
	Category string `json:"category,omitempty"`
	// Localization is set when the text is a resource key rather than the text.
	Localization *Localization `json:"localization,omitempty"`
}

// Empty reports whether the alert would put nothing in front of the user. An
// alert that carries only a localization key is not empty: it displays
// something, even if only the key.
func (a *Alert) Empty() bool {
	if a == nil {
		return true
	}
	return a.Title == "" && a.Subtitle == "" && a.Body == "" &&
		a.Badge == nil && a.Sound == "" && a.Localization.Empty()
}

// HasBanner reports whether the alert has text to put on a lock screen. A push
// that sets only a badge or a sound still counts as a notification - the user
// notices it - but there is no banner to draw, and the tab says which it is.
func (a *Alert) HasBanner() bool {
	if a == nil {
		return false
	}
	return a.Title != "" || a.Subtitle != "" || a.Body != "" || !a.Localization.Empty()
}

// Priority is the delivery urgency, normalized onto three levels.
//
// Normalizing here is a deliberate choice, and so is keeping the original.
// The three ecosystems disagree on both the spelling and the number of levels:
// APNs sends apns-priority 10, 5 or 1; FCM sends android.priority HIGH or
// NORMAL; webpush sends an RFC 8030 Urgency of very-low, low, normal or high.
// A single badge that means the same thing whichever provider captured the
// message is worth having, and a list cannot show three vocabularies. But the
// exact value is what an APNs debugger is actually looking at - "did this go
// out at 5 or at 10" is the question - so Delivery.PriorityRaw keeps whatever
// the sender wrote and nothing is thrown away.
//
// Do not confuse this with android.notification.notification_priority
// (PRIORITY_MIN … PRIORITY_MAX). That is how loudly the notification is
// displayed once it arrives, not how urgently it is delivered, and it belongs
// in the verbatim payload.
type Priority string

const (
	PriorityHigh   Priority = "high"
	PriorityNormal Priority = "normal"
	PriorityLow    Priority = "low"
)

// Priorities lists the levels from most to least urgent.
func Priorities() []Priority { return []Priority{PriorityHigh, PriorityNormal, PriorityLow} }

// PriorityOf maps a vendor's own spelling onto the three levels above. It lives
// here rather than in each provider so that two providers cannot disagree about
// what "5" means.
//
// Recognized: APNs "10", "5" and "1"; FCM "HIGH" and "NORMAL" (the discovery
// document's enum, though the field description says the values are spelled
// "high" and "normal", so both cases are accepted); webpush "high", "normal",
// "low" and "very-low". Anything else returns false, and the caller records the
// raw value without a level rather than guessing.
func PriorityOf(raw string) (Priority, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "10", "high":
		return PriorityHigh, true
	case "5", "normal":
		return PriorityNormal, true
	case "1", "low", "very-low":
		return PriorityLow, true
	default:
		return "", false
	}
}

// Expiry is how long the platform may keep trying to deliver.
//
// The two ecosystems state this in incompatible terms and the model keeps both
// rather than converting between them:
//
//   - APNs sends apns-expiration, an absolute UNIX epoch in seconds (UTC).
//   - FCM sends android.ttl, a relative duration encoded as a string ending in
//     "s" ("3600s", "3.000000001s"), rounded down to the second, capped at four
//     weeks and defaulting to four weeks; webpush sends a TTL header in seconds.
//
// Converting a duration into a deadline needs a "since when", and the honest
// answer - when the sender sent it - is not something a catcher knows. So a
// provider fills in whichever one the wire gave it, and Deadline computes the
// other on demand from the time the message was captured.
//
// The one thing both do say identically is the zero case: apns-expiration 0 and
// ttl "0s" both mean "try once now, do not store". That is a shared meaning
// neither vocabulary owns, so it gets a name.
//
// A nil *Expiry means the sender set nothing and the platform default applies -
// up to 30 days of APNs storage, four weeks of FCM storage.
type Expiry struct {
	// Immediate is the "deliver now or drop it" case.
	Immediate bool `json:"immediate,omitempty"`
	// TTLSeconds is a relative lifetime, as FCM states it.
	TTLSeconds *int64 `json:"ttl_seconds,omitempty"`
	// At is an absolute deadline, as APNs states it.
	At *time.Time `json:"at,omitempty"`
	// Raw is exactly what the sender wrote, so a malformed value is still
	// visible rather than silently becoming nil.
	Raw string `json:"raw,omitempty"`
}

// ExpiresAfter builds the relative form, from an FCM ttl or a webpush TTL
// header. Zero seconds means deliver-now-or-drop; a negative value is recorded
// as raw only, since it cannot mean anything.
func ExpiresAfter(seconds int64, raw string) *Expiry {
	e := &Expiry{Raw: raw}
	switch {
	case seconds == 0:
		e.Immediate = true
	case seconds > 0:
		e.TTLSeconds = &seconds
	}
	return e
}

// ExpiresAt builds the absolute form, from an APNs apns-expiration header. Zero
// means deliver-now-or-drop, which is why this cannot simply be a *time.Time:
// Apple's zero is a sentinel, not 1 January 1970.
func ExpiresAt(unix int64, raw string) *Expiry {
	e := &Expiry{Raw: raw}
	if unix <= 0 {
		e.Immediate = true
		return e
	}
	t := time.Unix(unix, 0).UTC()
	e.At = &t
	return e
}

// Deadline resolves the expiry into an absolute time, given when the message
// was captured. It reports false for an immediate or unusable expiry, which the
// caller should describe rather than draw on a clock.
func (e *Expiry) Deadline(receivedAt time.Time) (time.Time, bool) {
	if e == nil || e.Immediate {
		return time.Time{}, false
	}
	if e.At != nil {
		return *e.At, true
	}
	if e.TTLSeconds != nil {
		return receivedAt.Add(time.Duration(*e.TTLSeconds) * time.Second), true
	}
	return time.Time{}, false
}

// Describe is the one line the tab and the API show for an expiry.
func (e *Expiry) Describe() string {
	switch {
	case e == nil:
		return ""
	case e.Immediate:
		return "deliver immediately or drop"
	case e.At != nil:
		return "expires at " + e.At.Format(time.RFC3339)
	case e.TTLSeconds != nil:
		return "expires " + (time.Duration(*e.TTLSeconds) * time.Second).String() + " after sending"
	case e.Raw != "":
		return "unusable expiry: " + e.Raw
	default:
		return ""
	}
}

// Delivery is how the platform should carry the message, as opposed to what it
// should show.
type Delivery struct {
	// Priority is the normalized urgency; see Priority.
	Priority Priority `json:"priority,omitempty"`
	// PriorityRaw is the value the sender actually wrote ("10", "HIGH",
	// "very-low"), kept because the normalization above is lossy.
	PriorityRaw string `json:"priority_raw,omitempty"`
	// Expiry is how long delivery may be retried.
	Expiry *Expiry `json:"expiry,omitempty"`
	// CollapseKey groups messages that supersede one another: when several are
	// waiting, only the last is delivered. APNs sends apns-collapse-id (at most
	// 64 bytes); FCM sends android.collapse_key (at most four distinct keys
	// live at once).
	//
	// This is the one place the two vocabularies genuinely describe the same
	// mechanism, so it is normalized into one field. Note that the plan grouped
	// "category/collapse key" as though they were two names for one thing; they
	// are not related at all - see Alert.Category.
	CollapseKey string `json:"collapse_key,omitempty"`
}

// SetPriority records a vendor's own priority spelling, normalizing it where it
// is recognized and keeping it either way.
func (d *Delivery) SetPriority(raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	d.PriorityRaw = raw
	if p, ok := PriorityOf(raw); ok {
		d.Priority = p
	}
}

// Empty reports whether the sender said nothing about delivery.
func (d Delivery) Empty() bool {
	return d.Priority == "" && d.PriorityRaw == "" && d.Expiry == nil && d.CollapseKey == ""
}

// Format discriminates the schema of a verbatim payload. It is what an
// inspector dispatches on, and the reason a provider can capture a request in
// full before the model understands every key in it.
type Format string

const (
	// FormatAPNs is an APNs notification payload exactly as posted: the aps
	// dictionary plus whatever custom keys sit beside it.
	FormatAPNs Format = "apns.payload"
	// FormatFCM is the FCM HTTP v1 "message" object exactly as posted - the
	// value of the "message" key in the request body, not the request envelope.
	FormatFCM Format = "fcm.v1.message"
	// FormatFCMAndroid is an FCM AndroidConfig block, verbatim.
	FormatFCMAndroid Format = "fcm.v1.android"
	// FormatFCMApns is an FCM ApnsConfig block, verbatim. It is worth lifting
	// out of the message it arrived in: it carries a whole APNs payload plus
	// APNs headers, and a message that displays on Android but not on iOS is
	// usually explained by what is in here.
	FormatFCMApns Format = "fcm.v1.apns"
	// FormatFCMWebpush is an FCM WebpushConfig block, verbatim.
	FormatFCMWebpush Format = "fcm.v1.webpush"
)

// Formats lists every schema the plugin knows the name of, in a stable order.
func Formats() []Format {
	return []Format{FormatAPNs, FormatFCM, FormatFCMAndroid, FormatFCMApns, FormatFCMWebpush}
}

// Label is the human name of a schema, for an inspector title or a badge.
func (f Format) Label() string {
	switch f {
	case FormatAPNs:
		return "APNs payload"
	case FormatFCM:
		return "FCM message"
	case FormatFCMAndroid:
		return "FCM android override"
	case FormatFCMApns:
		return "FCM apns override"
	case FormatFCMWebpush:
		return "FCM webpush override"
	case "":
		return "payload"
	default:
		return string(f)
	}
}

// Known reports whether the format is one of the schemas declared above. An
// unknown format is still stored and still shown as JSON; it just has no name.
func (f Format) Known() bool {
	for _, known := range Formats() {
		if f == known {
			return true
		}
	}
	return false
}

// Payload is one piece of the request kept exactly as it arrived, tagged with
// the schema it is written in.
//
// Data is never normalized into some common shape: the two ecosystems do not
// have one, and flattening them would throw away the fidelity that makes a
// capture worth reading.
type Payload struct {
	Format Format          `json:"format"`
	Data   json.RawMessage `json:"data"`
}

// Empty reports whether the payload carries no JSON worth showing.
func (p Payload) Empty() bool { return emptyJSON(p.Data) }

// Decode unmarshals the verbatim JSON into v, once a caller has dispatched on
// Format.
func (p Payload) Decode(v any) error { return json.Unmarshal(p.Data, v) }

// Value returns the payload as a generic Go value, for an inspector that walks
// it without a schema of its own.
func (p Payload) Value() any { return jsonValue(p.Data) }

// Message is the push plugin's canonical model: what every provider converts
// its wire format into, and what lands in event.Payload.
//
// Provider-specific metadata - the bearer token that was presented, the JWT
// claims, the request headers, the Firebase project in the URL - belongs in
// Event.Meta, not here. This struct only carries what a push is.
type Message struct {
	// Kind says whether the device displays anything. Normalize derives it when
	// a provider leaves it empty; see DeriveKind for the rule and for the one
	// case a provider must set it itself.
	Kind Kind `json:"kind"`
	// PushType is what the sender declared this push to be, in its own words.
	//
	// APNs sends apns-push-type, and the live documentation lists eleven valid
	// values: alert, background, controls, complication, fileprovider,
	// liveactivity, location, mdm, pushtotalk, voip and widgets. It is recorded
	// verbatim rather than as an enum, because a value Apple adds next year
	// should show up in a capture rather than being dropped, and because a
	// misspelled one is exactly the sort of thing somebody is here to see.
	//
	// FCM has no equivalent field at all, so an FCM provider leaves this empty.
	PushType string `json:"push_type,omitempty"`
	// App identifies the application this push is for: an APNs apns-topic,
	// which despite its name is the app's bundle ID (optionally with a suffix
	// such as .voip), or the Firebase project from an FCM request path
	// (/v1/projects/{project}/messages:send).
	//
	// It is separate from Target on purpose. Reading apns-topic as a pub/sub
	// topic is the single easiest way to get this model wrong.
	App string `json:"app,omitempty"`
	// Target is what the push was addressed to.
	Target Target `json:"target"`
	// Alert is what gets displayed, or nil when nothing does.
	Alert *Alert `json:"alert,omitempty"`
	// Data is the payload handed to the app, as a JSON object, verbatim.
	//
	// It is raw JSON rather than a map because the two ecosystems disagree
	// about the value type: FCM's data block is documented as key/value pairs
	// that "must be UTF-8 encoded" - strings - while APNs custom keys may be
	// any primitive, including nested dictionaries and arrays. Flattening
	// Apple's into strings would lose structure, and forcing Google's into
	// arbitrary JSON would suggest a freedom it does not have.
	//
	// For APNs this is every top-level key of the payload except "aps",
	// collected into one object. For FCM it is the data block, with a
	// platform-level override (android.data, webpush.data) preferred over
	// message.data when both are present, because that is what FCM does.
	Data json.RawMessage `json:"data,omitempty"`
	// Delivery is how the platform should carry it.
	Delivery Delivery `json:"delivery"`
	// Payloads holds the request verbatim, tagged by schema. The first entry is
	// the vendor's own body; a provider appends the per-platform blocks it
	// found inside it so a reader sees them called out rather than having to
	// dig. See Format.
	Payloads []Payload `json:"payloads,omitempty"`
}

// Normalize fills in the derived and defaulted fields. Every provider calls it
// once it has finished converting a request, and the plugin calls it again on
// read-back, so a message is never displayed half-built.
func (m *Message) Normalize() {
	m.App = strings.TrimSpace(m.App)
	m.PushType = strings.TrimSpace(m.PushType)
	m.Target.Value = strings.TrimSpace(m.Target.Value)
	m.Target.Source = strings.TrimSpace(m.Target.Source)
	if m.Target.Kind == "" && m.Target.Value != "" {
		m.Target.Kind = TargetDevice
	}

	if m.Alert != nil {
		m.Alert.Title = strings.TrimSpace(m.Alert.Title)
		m.Alert.Subtitle = strings.TrimSpace(m.Alert.Subtitle)
		m.Alert.Body = strings.TrimSpace(m.Alert.Body)
		m.Alert.Image = strings.TrimSpace(m.Alert.Image)
		m.Alert.Sound = strings.TrimSpace(m.Alert.Sound)
		m.Alert.Category = strings.TrimSpace(m.Alert.Category)
		if m.Alert.Localization.Empty() {
			m.Alert.Localization = nil
		}
		if m.Alert.Empty() {
			m.Alert = nil
		}
	}

	if emptyJSON(m.Data) {
		m.Data = nil
	}

	kept := m.Payloads[:0]
	for _, p := range m.Payloads {
		if p.Empty() {
			continue
		}
		kept = append(kept, p)
	}
	m.Payloads = kept
	if len(m.Payloads) == 0 {
		m.Payloads = nil
	}

	if m.Kind == "" {
		m.Kind = m.DeriveKind()
	}
}

// DeriveKind works out whether the device displays anything, from the payload
// alone. Normalize calls it for any message whose provider left Kind empty.
//
// The rule:
//
//   - A non-empty Alert displays. That follows Apple's own line that alert,
//     badge and sound are the keys that "interact with the user", and FCM's
//     that a notification block is what the platform renders.
//   - Otherwise, a message carrying data, or declaring a push type other than
//     "alert", is silent: the app is woken and handles it.
//   - Otherwise it is empty, and says so.
//
// There is one case a provider must set Kind itself rather than rely on this:
// an APNs background push that carries content-available: 1 with no custom
// keys at all. That is a legitimate "wake up and go fetch" and it has no alert
// and no data, so the rule above would call it empty. An APNs provider that
// sees content-available - or apns-push-type: background - sets KindSilent
// explicitly. FCM has no such case; it will not accept a message with neither
// a notification nor data.
func (m *Message) DeriveKind() Kind {
	if !m.Alert.Empty() {
		return KindNotification
	}
	if m.HasData() {
		return KindSilent
	}
	if m.PushType != "" && !strings.EqualFold(m.PushType, "alert") {
		return KindSilent
	}
	return KindEmpty
}

// Displays reports whether the device puts something in front of the user.
func (m *Message) Displays() bool { return m.Kind.Displays() }

// HasData reports whether the app-directed payload carries anything.
func (m *Message) HasData() bool { return len(m.DataKeys()) > 0 }

// DataKeys lists the keys of the app-directed payload, sorted, for a badge or a
// summary line. A Data that is not a JSON object yields no keys.
func (m *Message) DataKeys() []string {
	if emptyJSON(m.Data) {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(m.Data, &obj); err != nil {
		return nil
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// DataValue returns the app-directed payload as a generic Go value, for the
// JSON inspector.
func (m *Message) DataValue() any { return jsonValue(m.Data) }

// Payload returns the verbatim payload written in the given schema.
func (m *Message) Payload(f Format) (Payload, bool) {
	for _, p := range m.Payloads {
		if p.Format == f {
			return p, true
		}
	}
	return Payload{}, false
}

// Title is how the message names itself: the alert title where there is one,
// then the body, then a description of what kind of push it is. It is never
// empty, because a list row with no label is unusable.
func (m *Message) Title() string {
	if m.Alert != nil {
		if m.Alert.Title != "" {
			return singleLine(m.Alert.Title)
		}
		if m.Alert.Localization != nil && m.Alert.Localization.TitleKey != "" {
			return singleLine(m.Alert.Localization.TitleKey)
		}
		if m.Alert.Body != "" {
			return truncateRunes(singleLine(m.Alert.Body), 80)
		}
	}
	switch m.Kind {
	case KindSilent:
		return "(silent push)"
	case KindEmpty:
		return "(empty push)"
	default:
		return "(no alert text)"
	}
}

// Preview is the one-line summary for a list or a badge. For a silent push it
// names the data keys, because those are the only thing there is to see and
// they are what somebody searches for.
func (m *Message) Preview() string {
	if m.Alert != nil {
		parts := make([]string, 0, 3)
		for _, s := range []string{m.Alert.Subtitle, m.Alert.Body} {
			if s != "" {
				parts = append(parts, singleLine(s))
			}
		}
		if len(parts) > 0 {
			return truncateRunes(strings.Join(parts, " — "), 160)
		}
		if l := m.Alert.Localization; l != nil && l.BodyKey != "" {
			return truncateRunes(singleLine(l.BodyKey), 160)
		}
	}
	if keys := m.DataKeys(); len(keys) > 0 {
		return truncateRunes("data: "+strings.Join(keys, ", "), 160)
	}
	if m.Alert != nil {
		return badgeAndSound(m.Alert)
	}
	return "no alert, no data"
}

// badgeAndSound describes an alert that has no text - one that only bumps the
// badge or plays a sound. Saying so beats an empty preview row.
func badgeAndSound(a *Alert) string {
	var parts []string
	if a.Badge != nil {
		if *a.Badge == 0 {
			parts = append(parts, "clears the badge")
		} else {
			parts = append(parts, "badge "+strconv.Itoa(*a.Badge))
		}
	}
	if a.Sound != "" {
		parts = append(parts, "sound "+a.Sound)
	}
	if len(parts) == 0 {
		return "no alert text"
	}
	return strings.Join(parts, ", ")
}

// emptyJSON reports whether raw JSON carries nothing worth showing.
func emptyJSON(b json.RawMessage) bool {
	t := strings.TrimSpace(string(b))
	return t == "" || t == "null" || t == "{}" || t == "[]"
}

// jsonValue decodes raw JSON into a generic Go value, falling back to the text
// itself so that an unparseable capture is still visible.
func jsonValue(b json.RawMessage) any {
	if emptyJSON(b) {
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	return v
}

func singleLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.TrimSpace(s)
}

func truncateRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
