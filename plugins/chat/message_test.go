package chat_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/plugins/chat"
)

const slackBlocks = `[
  {"type":"header","text":{"type":"plain_text","text":"Deploy finished"}},
  {"type":"section","text":{"type":"mrkdwn","text":"*api* is now on v1.4.2"}},
  {"type":"divider"},
  {"type":"actions","elements":[{"type":"button","text":{"type":"plain_text","text":"Open build"},"url":"https://ci.example.com/42"}]}
]`

const adaptiveCard = `{
  "type":"AdaptiveCard","version":"1.5",
  "body":[
    {"type":"TextBlock","text":"Build 42 failed","weight":"bolder"},
    {"type":"FactSet","facts":[{"title":"Branch","value":"main"}]}
  ]
}`

const messageCard = `{
  "@type":"MessageCard","@context":"https://schema.org/extensions",
  "summary":"Nightly run","title":"Nightly run",
  "sections":[{"activityTitle":"3 tests failed","text":"See the build log."}]
}`

func TestNormalizeTrimsAndDefaults(t *testing.T) {
	m := &chat.Message{
		Channel: chat.ChannelRef{ID: "  C0123ABCD  ", Name: " general "},
		Author:  chat.Author{Name: "  deploy-bot ", IconURL: " https://example.com/a.png "},
		Text:    "shipped",
		TS:      " 1700000000.000100 ",
	}
	m.Normalize()

	if m.Channel.ID != "C0123ABCD" || m.Channel.Name != "general" {
		t.Errorf("channel = %+v, want trimmed", m.Channel)
	}
	if m.Author.Name != "deploy-bot" || m.Author.IconURL != "https://example.com/a.png" {
		t.Errorf("author = %+v, want trimmed", m.Author)
	}
	if m.TS != "1700000000.000100" {
		t.Errorf("ts = %q, want trimmed", m.TS)
	}
	if m.IsReply() {
		t.Error("a message with no thread_ts must not be a reply")
	}
}

// Slack sets thread_ts equal to ts on the parent of a thread. That means "I am
// the parent", not "I reply to myself", and the model must not turn it into a
// self-referential thread.
func TestNormalizeSelfThreadIsNotAReply(t *testing.T) {
	m := &chat.Message{TS: "1700000000.000100", ThreadTS: "1700000000.000100"}
	m.Normalize()
	if m.IsReply() {
		t.Error("thread_ts == ts must be treated as the thread parent, not a reply")
	}
	if m.ThreadTS != "" {
		t.Errorf("ThreadTS = %q, want cleared", m.ThreadTS)
	}
}

func TestNormalizeDropsEmptyContent(t *testing.T) {
	m := &chat.Message{
		Text: "hello",
		Contents: []chat.Content{
			{Format: chat.FormatSlackBlocks, Data: json.RawMessage("null")},
			{Format: chat.FormatSlackAttachments, Data: nil},
			{Format: chat.FormatSlackBlocks, Data: json.RawMessage(slackBlocks)},
		},
	}
	m.Normalize()
	if len(m.Contents) != 1 || m.Contents[0].Format != chat.FormatSlackBlocks {
		t.Fatalf("contents = %+v, want only the one real payload", m.Contents)
	}
}

// The whole point of the fallback: a message that arrived as nothing but a card
// still reads as text, which is what makes it useful before any renderer exists.
func TestTextIsAlwaysPopulated(t *testing.T) {
	cases := []struct {
		name   string
		format chat.Format
		data   string
		want   []string
	}{
		{"block kit", chat.FormatSlackBlocks, slackBlocks, []string{"Deploy finished", "*api* is now on v1.4.2", "Open build"}},
		{"adaptive card", chat.FormatTeamsAdaptiveCard, adaptiveCard, []string{"Build 42 failed", "Branch", "main"}},
		{"message card", chat.FormatTeamsMessageCard, messageCard, []string{"Nightly run", "3 tests failed", "See the build log."}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &chat.Message{Contents: []chat.Content{{Format: tc.format, Data: json.RawMessage(tc.data)}}}
			m.Normalize()
			if strings.TrimSpace(m.Text) == "" {
				t.Fatal("Text must never be empty when structured content was supplied")
			}
			for _, want := range tc.want {
				if !strings.Contains(m.Text, want) {
					t.Errorf("fallback text %q does not carry %q", m.Text, want)
				}
			}
			// Schema noise must not leak into a line meant to read as prose.
			for _, unwanted := range []string{"mrkdwn", "AdaptiveCard", "MessageCard", "https://ci.example.com/42"} {
				if strings.Contains(m.Text, unwanted) {
					t.Errorf("fallback text %q leaked the schema token %q", m.Text, unwanted)
				}
			}
		})
	}
}

func TestFallbackTextIsNotUsedWhenTheProviderSuppliedText(t *testing.T) {
	m := &chat.Message{
		Text:     "Deploy finished",
		Contents: []chat.Content{{Format: chat.FormatSlackBlocks, Data: json.RawMessage(slackBlocks)}},
	}
	m.Normalize()
	if m.Text != "Deploy finished" {
		t.Errorf("Text = %q, want the provider's own text left alone", m.Text)
	}
}

func TestFallbackTextIsDeterministic(t *testing.T) {
	contents := []chat.Content{{Format: chat.FormatTeamsMessageCard, Data: json.RawMessage(messageCard)}}
	first := chat.FallbackText(contents)
	for range 20 {
		if got := chat.FallbackText(contents); got != first {
			t.Fatalf("fallback text is not deterministic:\n%q\n%q", first, got)
		}
	}
}

func TestFallbackTextSurvivesGarbage(t *testing.T) {
	deep := strings.Repeat(`{"a":`, 200) + `"x"` + strings.Repeat(`}`, 200)
	for _, data := range []string{`"just a string"`, `[]`, `{}`, `12`, deep, `{"text":123}`} {
		c := chat.Content{Format: "vendor.unknown", Data: json.RawMessage(data)}
		_ = chat.FallbackText([]chat.Content{c}) // must not panic or hang
	}
	broken := chat.Content{Format: chat.FormatSlackBlocks, Data: json.RawMessage(`{not json`)}
	if got := chat.FallbackText([]chat.Content{broken}); got != "" {
		t.Errorf("FallbackText on invalid JSON = %q, want empty", got)
	}
}

// Structured content is kept byte for byte: the whole reason for keeping the
// original JSON plus a discriminator is that a renderer gets the real payload.
func TestContentIsVerbatim(t *testing.T) {
	m := &chat.Message{Contents: []chat.Content{{Format: chat.FormatSlackBlocks, Data: json.RawMessage(slackBlocks)}}}
	m.Normalize()
	if string(m.Contents[0].Data) != slackBlocks {
		t.Error("the original JSON must be kept verbatim, not re-encoded")
	}

	var blocks []struct {
		Type string `json:"type"`
	}
	if err := m.Contents[0].Decode(&blocks); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(blocks) != 4 || blocks[0].Type != "header" {
		t.Errorf("decoded blocks = %+v", blocks)
	}
}

func TestFormatLabelAndKnown(t *testing.T) {
	for _, f := range chat.Formats() {
		if !f.Known() {
			t.Errorf("%s should be a known format", f)
		}
		if f.Label() == "" {
			t.Errorf("%s has no label", f)
		}
	}
	unknown := chat.Format("vendor.something")
	if unknown.Known() {
		t.Error("an undeclared format must not report as known")
	}
	if unknown.Label() != "vendor.something" {
		t.Errorf("unknown label = %q, want the format itself", unknown.Label())
	}
}

func TestAuthorDisplayAndInitials(t *testing.T) {
	cases := []struct {
		author         chat.Author
		display, inits string
	}{
		{chat.Author{Name: "deploy bot"}, "deploy bot", "DB"},
		{chat.Author{Name: "deploy-bot"}, "deploy-bot", "DB"},
		{chat.Author{Name: "Nightly"}, "Nightly", "N"},
		{chat.Author{ID: "U012"}, "U012", "U"},
		{chat.Author{}, "(unknown)", "U"},
		{chat.Author{Name: "@ci"}, "@ci", "C"},
	}
	for _, tc := range cases {
		if got := tc.author.Display(); got != tc.display {
			t.Errorf("Display(%+v) = %q, want %q", tc.author, got, tc.display)
		}
		if got := tc.author.Initials(); got != tc.inits {
			t.Errorf("Initials(%+v) = %q, want %q", tc.author, got, tc.inits)
		}
	}
}

func TestChannelRefDisplay(t *testing.T) {
	if got := (chat.ChannelRef{ID: "C1", Name: "general"}).Display(); got != "general" {
		t.Errorf("Display = %q, want the name", got)
	}
	if got := (chat.ChannelRef{ID: "C1"}).Display(); got != "C1" {
		t.Errorf("Display = %q, want the id", got)
	}
	if got := (chat.ChannelRef{}).Display(); got != "(unknown channel)" {
		t.Errorf("Display = %q, want a placeholder", got)
	}
}

func TestSummaryFilesUnderTheChannel(t *testing.T) {
	m := msg("C0123ABCD", "deploy-bot", "shipped v1.4.2\nand it works", "1.1")
	m.Channel.Name = "general"
	m.Normalize()
	s := m.Summary()
	if s.Title != "general" {
		t.Errorf("Summary.Title = %q, want the channel", s.Title)
	}
	if s.From != "deploy-bot" {
		t.Errorf("Summary.From = %q", s.From)
	}
	if len(s.To) != 1 || s.To[0] != "general" {
		t.Errorf("Summary.To = %v", s.To)
	}
	if strings.Contains(s.Snippet, "\n") {
		t.Errorf("Summary.Snippet = %q, want a single line", s.Snippet)
	}
}

func TestPreview(t *testing.T) {
	m := &chat.Message{Contents: []chat.Content{{Format: chat.FormatSlackBlocks, Data: json.RawMessage(`[{"type":"divider"}]`)}}}
	m.Normalize()
	if got := m.Preview(); got != "(Block Kit)" {
		t.Errorf("Preview with only unharvestable content = %q", got)
	}
	empty := &chat.Message{}
	empty.Normalize()
	if got := empty.Preview(); got != "(empty message)" {
		t.Errorf("Preview of an empty message = %q", got)
	}
}

// A store that round-trips events through JSON must not break the read
// surfaces: MessageOf has to cope with a payload that is no longer a *Message.
func TestMessageOfSurvivesJSONRoundTrip(t *testing.T) {
	m := msg("C1", "deploy-bot", "hello", "1.1")
	m.Contents = []chat.Content{{Format: chat.FormatSlackBlocks, Data: json.RawMessage(slackBlocks)}}
	ev := chat.NewEvent("fake", m)

	encoded, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var decoded event.Event
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	got, ok := chat.MessageOf(&decoded)
	if !ok {
		t.Fatal("MessageOf must decode a payload that has been through JSON")
	}
	if got.Text != "hello" || got.Channel.ID != "C1" || got.Author.Name != "deploy-bot" {
		t.Errorf("round-tripped message = %+v", got)
	}
	if len(got.Contents) != 1 || got.Contents[0].Format != chat.FormatSlackBlocks {
		t.Fatalf("round-tripped contents = %+v", got.Contents)
	}
	var blocks []any
	if err := got.Contents[0].Decode(&blocks); err != nil || len(blocks) != 4 {
		t.Errorf("structured content did not survive the round trip: %v %d", err, len(blocks))
	}
}

func TestMessageOfIgnoresForeignPayloads(t *testing.T) {
	if _, ok := chat.MessageOf(nil); ok {
		t.Error("nil event")
	}
	if _, ok := chat.MessageOf(&event.Event{}); ok {
		t.Error("event with no payload")
	}
	if _, ok := chat.MessageOf(&event.Event{Payload: map[string]any{"unrelated": 1}}); ok {
		t.Error("an unrelated payload must not decode as a chat message")
	}
}

func TestMessagesSkipsOtherTypes(t *testing.T) {
	events := []*event.Event{
		chat.NewEvent("fake", msg("C1", "bot", "one", "1.1")),
		{Plugin: chat.PluginName, Provider: "fake", Type: "chat.reaction", Payload: map[string]any{"emoji": "tada"}},
		nil,
	}
	events[0].ID = "a"
	if got := chat.Messages(events); len(got) != 1 {
		t.Fatalf("Messages() returned %d, want only the chat.message event", len(got))
	}
}

func TestIdentityFallsBackToTheEventID(t *testing.T) {
	// A Teams incoming webhook has no message id of its own, so the event id
	// has to stand in - otherwise every such message would share one identity.
	m := &chat.Message{Channel: chat.ChannelRef{ID: "wh"}, Text: "no ts here"}
	m.Normalize()
	if got := m.Identity("abc123"); got != "abc123" {
		t.Errorf("Identity = %q, want the event id", got)
	}
	if got := m.RootKey("abc123"); got != "abc123" {
		t.Errorf("RootKey = %q, want its own identity", got)
	}
	m.ThreadTS = "1.1"
	if got := m.RootKey("abc123"); got != "1.1" {
		t.Errorf("RootKey of a reply = %q, want the parent ts", got)
	}
}

// Slug is what turns a provider identifier into a URL segment. Two different
// ids must never collapse onto one key, or two channels would merge in a link.
func TestSlug(t *testing.T) {
	if got := chat.Slug("C0123ABCD"); got != "C0123ABCD" {
		t.Errorf("an already-safe id must be used verbatim, got %q", got)
	}
	if got := chat.Slug("1700000000.000100"); got != "1700000000.000100" {
		t.Errorf("a Slack ts is already path safe, got %q", got)
	}
	if chat.Slug("") != "none" {
		t.Error("an empty id needs a placeholder key")
	}
	a, b := chat.Slug("#general"), chat.Slug("/general")
	if a == b {
		t.Errorf("distinct ids collapsed onto one key: %q", a)
	}
	for _, in := range []string{"#general", "/general", "a b", "..", "https://outlook.office.com/webhookb2/x@y/IncomingWebhook/1/2"} {
		got := chat.Slug(in)
		if strings.ContainsAny(got, "/?#% ") || got == "." || got == ".." {
			t.Errorf("Slug(%q) = %q, which is not a safe path segment", in, got)
		}
	}
}
