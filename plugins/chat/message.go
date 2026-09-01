// Package chat is tommy's chat content type: the canonical Message every chat
// provider converts into, the read-back API under /api/v1/chat/ and the
// channel-and-stream tab under /ui/chat/.
//
// Providers (Slack, Microsoft Teams, and whatever comes later) live in
// plugins/chat/providers/... and never import each other; all they share is the
// Message in this file.
//
// Two design points are load bearing:
//
//   - Structured content is kept verbatim. Slack Block Kit, a Teams MessageCard
//     and a Teams Adaptive Card are three different schemas, so the model keeps
//     the original JSON plus a Format discriminator rather than flattening all
//     three into one shape and losing fidelity. Whoever renders a card
//     dispatches on Format.
//   - Every message carries plain Text, even when the real payload was
//     structured. The text fallback is what makes a message readable before any
//     rich rendering exists, so capture never waits on rendering.
//
// Threads are a relation and the store has none, deliberately: the channel and
// thread index is derived from the flat event list at render time by thread.go.
package chat

import (
	"encoding/json"
	"hash/fnv"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/can3p/tommy/core/event"
)

// PluginName is the plugin name and the URL segment it is mounted under.
const PluginName = "chat"

// TypeMessage is the event.Type every captured chat message carries. A provider
// that grows another resource later adds a new type rather than overloading
// this one, and every read surface switches on the type instead of assuming it.
const TypeMessage = "chat.message"

// Format discriminates the schema of a piece of structured content. It is the
// value a card renderer dispatches on, and the reason the raw JSON can be kept
// verbatim without anybody having to guess what it is.
type Format string

// The structured-content schemas providers may attach. A provider that supports
// a new schema adds a constant here rather than inventing a string at the call
// site, so the renderer's switch stays exhaustive.
const (
	// FormatSlackBlocks is a Slack Block Kit "blocks" array, verbatim:
	// [{"type":"section","text":{"type":"mrkdwn","text":"…"}}, …].
	FormatSlackBlocks Format = "slack.blocks"
	// FormatSlackAttachments is Slack's legacy "attachments" array, verbatim:
	// [{"color":"#36a64f","fallback":"…","fields":[…]}, …].
	FormatSlackAttachments Format = "slack.attachments"
	// FormatTeamsMessageCard is an O365 connector MessageCard object, verbatim,
	// starting at the object carrying "@type":"MessageCard".
	FormatTeamsMessageCard Format = "msteams.messagecard"
	// FormatTeamsAdaptiveCard is an Adaptive Card object, verbatim, starting at
	// the card itself ("type":"AdaptiveCard") - not the Bot Framework envelope
	// that wraps it in {"type":"message","attachments":[{"content":{…}}]}.
	FormatTeamsAdaptiveCard Format = "msteams.adaptivecard"
)

// Formats lists every schema the plugin knows the name of, in a stable order.
// It exists so a UI can offer a filter and a renderer can assert it handles
// them all.
func Formats() []Format {
	return []Format{FormatSlackBlocks, FormatSlackAttachments, FormatTeamsMessageCard, FormatTeamsAdaptiveCard}
}

// Label is the human name of a schema, for a badge or an inspector title.
func (f Format) Label() string {
	switch f {
	case FormatSlackBlocks:
		return "Block Kit"
	case FormatSlackAttachments:
		return "Slack attachments"
	case FormatTeamsMessageCard:
		return "MessageCard"
	case FormatTeamsAdaptiveCard:
		return "Adaptive Card"
	case "":
		return "structured content"
	default:
		return string(f)
	}
}

// Known reports whether the format is one of the schemas declared above. An
// unknown format is still stored and still shown as JSON; it just has no
// bespoke renderer.
func (f Format) Known() bool {
	for _, known := range Formats() {
		if f == known {
			return true
		}
	}
	return false
}

// Content is one piece of structured content exactly as the provider received
// it, tagged with the schema it is written in.
//
// Data is the original JSON, untouched. It is never normalized into some
// common shape here: the three schemas do not have one, and flattening them
// would throw away the fidelity a card renderer needs.
type Content struct {
	Format Format          `json:"format"`
	Data   json.RawMessage `json:"data"`
}

// Empty reports whether the content carries no JSON worth showing.
func (c Content) Empty() bool {
	trimmed := strings.TrimSpace(string(c.Data))
	return trimmed == "" || trimmed == "null"
}

// Decode unmarshals the verbatim JSON into v. It is the entry point a renderer
// uses once it has dispatched on Format.
func (c Content) Decode(v any) error { return json.Unmarshal(c.Data, v) }

// Value returns the content as a generic Go value, for a renderer or an
// inspector that walks it without a schema of its own.
func (c Content) Value() any {
	if c.Empty() {
		return nil
	}
	var v any
	if err := json.Unmarshal(c.Data, &v); err != nil {
		return string(c.Data)
	}
	return v
}

// ChannelRef identifies where a message was posted. Slack gives a channel id
// ("C0123ABCD") or a name override ("#general"); a Teams incoming webhook has
// no channel id at all, so a provider uses the webhook target it was posted to.
// Either way ID is what groups the stream and Name is the label, when the wire
// format gave one.
type ChannelRef struct {
	// ID is the provider's own identifier for the destination. It is what
	// messages are grouped by, so a provider must put something stable here.
	ID string `json:"id"`
	// Name is the display name where one is available ("general", "Build
	// alerts"). Empty is fine; the UI falls back to the ID.
	Name string `json:"name,omitempty"`
}

// Display is the channel's label: its name, else its id, else a placeholder.
func (c ChannelRef) Display() string {
	if c.Name != "" {
		return c.Name
	}
	if c.ID != "" {
		return c.ID
	}
	return "(unknown channel)"
}

// Author is who the message came from as the receiving chat system would show
// it: an incoming webhook posts as a bot with a name and an icon, while a Web
// API call posts as the token's bot user.
type Author struct {
	// ID is the provider's user or bot id, when the payload carried one.
	ID string `json:"id,omitempty"`
	// Name is the display name: a username override, a bot name, a Teams
	// activity title.
	Name string `json:"name,omitempty"`
	// IconURL is the avatar or icon URL. It points at somebody else's server
	// and tommy never fetches it; the UI uses it as an <img> source only.
	IconURL string `json:"icon_url,omitempty"`
	// Bot says the message was posted by a bot or an incoming webhook rather
	// than a human, which is the normal case for everything tommy captures.
	Bot bool `json:"bot,omitempty"`
}

// Display is the author's label, falling back to the id and then a placeholder.
func (a Author) Display() string {
	if a.Name != "" {
		return a.Name
	}
	if a.ID != "" {
		return a.ID
	}
	return "(unknown)"
}

// Initials is the one or two letters an avatar placeholder shows when the
// author has no icon.
func (a Author) Initials() string {
	name := strings.TrimSpace(a.Display())
	name = strings.TrimLeft(name, "(@#")
	fields := strings.FieldsFunc(name, func(r rune) bool {
		return r == ' ' || r == '-' || r == '_' || r == '.'
	})
	var out []rune
	for _, f := range fields {
		r, size := utf8.DecodeRuneInString(f)
		if size == 0 || r == utf8.RuneError {
			continue
		}
		out = append(out, []rune(strings.ToUpper(string(r)))...)
		if len(out) == 2 {
			break
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

// Message is the chat plugin's canonical model: what every provider converts
// its wire format into, and what lands in event.Payload.
//
// Provider-specific metadata - the webhook path it arrived on, the team and bot
// ids in a Slack services URL, the bearer token that was presented, the Teams
// tenant guid - belongs in Event.Meta, not here. This struct only carries what
// any chat API has.
type Message struct {
	// Channel is where the message was posted.
	Channel ChannelRef `json:"channel"`
	// Author is who it was posted as.
	Author Author `json:"author"`

	// Text is the plain-text body, and it is always populated: Normalize
	// derives one from Contents when the payload carried only structured
	// content, because a readable line is what makes a message useful before
	// any card renderer exists. It is untrusted input - every surface escapes
	// it and none of them renders it as HTML.
	Text string `json:"text"`

	// Contents holds the structured payloads verbatim, in the order they were
	// supplied. A Slack message may carry both blocks and legacy attachments,
	// and a Teams message may carry several cards, so this is a slice.
	Contents []Content `json:"contents,omitempty"`

	// TS is the message's own identity in the provider's terms: a Slack
	// "1503435956.000247", or whatever id a provider minted for a webhook post
	// that has none. It may be empty, in which case the event id identifies the
	// message instead.
	TS string `json:"ts,omitempty"`

	// ThreadTS is the TS of the message this one replies to, empty for a
	// top-level message. Slack sends it as thread_ts. Threading is derived from
	// this at render time; nothing is stored as a relation.
	ThreadTS string `json:"thread_ts,omitempty"`
}

// Normalize fills in the derived and defaulted fields. Every provider calls it
// once it has finished converting a request, and the plugin calls it again on
// read-back, so a message is never displayed half-built.
func (m *Message) Normalize() {
	m.Channel.ID = strings.TrimSpace(m.Channel.ID)
	m.Channel.Name = strings.TrimSpace(m.Channel.Name)
	m.Author.ID = strings.TrimSpace(m.Author.ID)
	m.Author.Name = strings.TrimSpace(m.Author.Name)
	m.Author.IconURL = strings.TrimSpace(m.Author.IconURL)
	m.TS = strings.TrimSpace(m.TS)
	m.ThreadTS = strings.TrimSpace(m.ThreadTS)

	// A thread_ts equal to the message's own ts is Slack's way of saying "this
	// is the thread parent", not "this is a reply to itself".
	if m.ThreadTS != "" && m.ThreadTS == m.TS {
		m.ThreadTS = ""
	}

	kept := m.Contents[:0]
	for _, c := range m.Contents {
		if c.Empty() {
			continue
		}
		kept = append(kept, c)
	}
	m.Contents = kept
	if len(m.Contents) == 0 {
		m.Contents = nil
	}

	if strings.TrimSpace(m.Text) == "" {
		m.Text = FallbackText(m.Contents)
	}
}

// IsReply reports whether the message hangs under a parent.
func (m *Message) IsReply() bool { return m.ThreadTS != "" }

// HasContent reports whether the message carries structured content.
func (m *Message) HasContent() bool { return len(m.Contents) > 0 }

// Identity is how the message names itself, preferring the provider's own ts
// and falling back to the id of the event that carried it. Thread derivation
// matches a reply's ThreadTS against this.
func (m *Message) Identity(id event.ID) string {
	if m.TS != "" {
		return m.TS
	}
	return string(id)
}

// RootKey is the identity of the thread the message belongs to: its parent's
// ts for a reply, its own identity otherwise.
func (m *Message) RootKey(id event.ID) string {
	if m.ThreadTS != "" {
		return m.ThreadTS
	}
	return m.Identity(id)
}

// Preview is the one-line summary of a message for a list or a badge.
func (m *Message) Preview() string {
	text := singleLine(m.Text)
	if strings.TrimSpace(text) != "" {
		return truncateRunes(text, 120)
	}
	if m.HasContent() {
		return "(" + m.Contents[0].Format.Label() + ")"
	}
	return "(empty message)"
}

// Summary is the provider-agnostic listing data for this message, so the
// generic event view, the API and the chat tab all agree on what a message is
// called. Title carries the channel because that is what a chat event is
// filed under, per the core's own Summary contract.
func (m *Message) Summary() event.Summary {
	return event.Summary{
		From:    m.Author.Display(),
		To:      []string{m.Channel.Display()},
		Title:   m.Channel.Display(),
		Snippet: truncateRunes(singleLine(m.Text), 200),
	}
}

// NewEvent builds the event a provider appends for a message, with the plugin,
// type, summary and payload already filled in. The caller still sets Raw, and
// Meta for anything provider-specific.
func NewEvent(provider string, m *Message) *event.Event {
	m.Normalize()
	return &event.Event{
		Plugin:   PluginName,
		Provider: provider,
		Type:     TypeMessage,
		Summary:  m.Summary(),
		Payload:  m,
	}
}

// MessageOf returns the canonical message carried by an event, if it carries
// one. The in-memory store shares payloads, so the common path is a type
// assertion; the JSON fallback keeps every read surface honest for an event
// that came back through a serializing store.
func MessageOf(e *event.Event) (*Message, bool) {
	if e == nil || e.Payload == nil {
		return nil, false
	}
	var m *Message
	switch p := e.Payload.(type) {
	case *Message:
		if p == nil {
			return nil, false
		}
		clone := *p
		m = &clone
	case Message:
		clone := p
		m = &clone
	default:
		encoded, err := json.Marshal(e.Payload)
		if err != nil {
			return nil, false
		}
		var decoded Message
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return nil, false
		}
		if decoded.Channel.ID == "" && decoded.Text == "" && len(decoded.Contents) == 0 && decoded.Author.Name == "" {
			return nil, false
		}
		m = &decoded
	}
	m.Normalize()
	return m, true
}

// Captured pairs an event with the message decoded from it. It is what every
// derived view is built out of, because the timestamps and the provider name
// live on the event while the content lives on the message.
type Captured struct {
	Event   *event.Event
	Message *Message
}

// ID is the event id of the captured message.
func (c Captured) ID() event.ID {
	if c.Event == nil {
		return ""
	}
	return c.Event.ID
}

// Identity is how the message names itself, with the event id as the fallback.
func (c Captured) Identity() string { return c.Message.Identity(c.ID()) }

// RootKey is the identity of the thread this message belongs to.
func (c Captured) RootKey() string { return c.Message.RootKey(c.ID()) }

// Messages extracts every decodable chat message from a list of events, keeping
// the event alongside it. Events of another type - a provider's future reaction
// or edit, say - are skipped rather than guessed at.
func Messages(events []*event.Event) []Captured {
	out := make([]Captured, 0, len(events))
	for _, e := range events {
		if e == nil || e.Type != TypeMessage {
			continue
		}
		m, ok := MessageOf(e)
		if !ok {
			continue
		}
		out = append(out, Captured{Event: e, Message: m})
	}
	return out
}

// textKeys are the JSON object keys whose string values read as prose. Only
// values under these keys are harvested, which is what keeps "type":"mrkdwn"
// and an action_id out of a fallback line.
var textKeys = map[string]bool{
	"text":             true,
	"title":            true,
	"summary":          true,
	"value":            true,
	"fallback":         true,
	"pretext":          true,
	"alt_text":         true,
	"label":            true,
	"activitytitle":    true,
	"activitysubtitle": true,
	"activitytext":     true,
	"fallbacktext":     true,
	"speak":            true,
}

const (
	// fallbackMaxDepth stops a pathological payload from walking forever.
	fallbackMaxDepth = 12
	// fallbackMaxParts caps how many strings one fallback line is built from.
	fallbackMaxParts = 40
	// fallbackMaxRunes caps the length of a derived fallback.
	fallbackMaxRunes = 2000
)

// FallbackText derives a readable plain-text body from structured content, for
// a provider whose payload carried no text of its own - a bare Adaptive Card,
// or a Slack message that is only blocks.
//
// It is deliberately a text harvest and not a renderer: it walks the JSON and
// collects the string values that read as prose, so it works on any of the
// three schemas and on one nobody has written a renderer for yet. Providers may
// call it directly; Normalize calls it for them when Text is empty.
func FallbackText(contents []Content) string {
	var parts []string
	seen := map[string]bool{}
	for _, c := range contents {
		harvest(c.Value(), 0, &parts, seen)
	}
	if len(parts) == 0 {
		return ""
	}
	return truncateRunes(strings.Join(parts, "\n"), fallbackMaxRunes)
}

func harvest(v any, depth int, parts *[]string, seen map[string]bool) {
	if depth > fallbackMaxDepth || len(*parts) >= fallbackMaxParts {
		return
	}
	switch t := v.(type) {
	case map[string]any:
		for _, key := range sortedKeys(t) {
			child := t[key]
			if s, ok := child.(string); ok {
				if !textKeys[strings.ToLower(key)] {
					continue
				}
				s = strings.TrimSpace(s)
				if s == "" || seen[s] {
					continue
				}
				seen[s] = true
				*parts = append(*parts, s)
				if len(*parts) >= fallbackMaxParts {
					return
				}
				continue
			}
			harvest(child, depth+1, parts, seen)
		}
	case []any:
		for _, item := range t {
			harvest(item, depth+1, parts, seen)
			if len(*parts) >= fallbackMaxParts {
				return
			}
		}
	}
}

// sortedKeys keeps a fallback deterministic. Object key order is not preserved
// by encoding/json, so without this the same card could produce two different
// fallback lines.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// A tiny insertion sort: these maps are card nodes, not data sets.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// Slug turns an arbitrary provider identifier into a URL path segment that
// round-trips: an id that is already path-safe is used verbatim, and anything
// else is transliterated and given a short hash so two different ids can never
// collapse onto one key.
func Slug(s string) string {
	if s == "" {
		return "none"
	}
	if pathSafe(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 9)
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return b.String() + "-" + strconv.FormatUint(uint64(h.Sum32()), 16)
}

func pathSafe(s string) bool {
	if s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
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
