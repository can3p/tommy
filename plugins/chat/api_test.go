package chat_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/chat"
)

func listMessages(t *testing.T, in *testutil.Instance, query string) []chat.MessageEnvelope {
	t.Helper()
	var out []chat.MessageEnvelope
	if status := in.GetJSON(in.API("/chat/messages"+query), &out); status != http.StatusOK {
		t.Fatalf("GET /chat/messages%s = %d", query, status)
	}
	return out
}

func listChannels(t *testing.T, in *testutil.Instance, query string) []chat.ChannelSummary {
	t.Helper()
	var out []chat.ChannelSummary
	if status := in.GetJSON(in.API("/chat/channels"+query), &out); status != http.StatusOK {
		t.Fatalf("GET /chat/channels%s = %d", query, status)
	}
	return out
}

// seed fills an instance with two channels, a thread and an orphaned reply.
func seed(t *testing.T, in *testutil.Instance) {
	t.Helper()
	injectAt(t, in, at(0), msg("C-general", "deploy-bot", "root in general", "1.1"))
	injectAt(t, in, at(1), reply("C-general", "release-bot", "a reply", "1.2", "1.1"))
	injectAt(t, in, at(2), reply("C-general", "release-bot", "an orphan", "1.3", "0.5"))
	injectAt(t, in, at(3), msg("C-ops", "pager", "ops alert", "2.1"))
}

func TestAPIListMessages(t *testing.T) {
	in := start(t)
	seed(t, in)

	got := listMessages(t, in, "")
	if len(got) != 4 {
		t.Fatalf("got %d messages, want 4", len(got))
	}
	// Newest first, as the core event API does.
	if got[0].Message.Text != "ops alert" {
		t.Errorf("first message = %q, want the newest", got[0].Message.Text)
	}
	first := got[0]
	if first.ChannelKey != chat.ChannelKey("C-ops") {
		t.Errorf("ChannelKey = %q", first.ChannelKey)
	}
	if first.RootID != "2.1" || first.Reply {
		t.Errorf("root envelope = %+v", first)
	}
	if first.ThreadKey != chat.ThreadKey(first.ChannelKey, "2.1") {
		t.Errorf("ThreadKey = %q", first.ThreadKey)
	}
	if first.Provider != "fake" || first.Type != chat.TypeMessage || first.ID == "" {
		t.Errorf("envelope did not carry the event fields: %+v", first)
	}
}

func TestAPIMessageFilters(t *testing.T) {
	in := start(t)
	seed(t, in)
	injectAt(t, in, at(4), func() *chat.Message {
		m := msg("C-ops", "human", "with a card", "2.2")
		m.Author.Bot = false
		m.Contents = []chat.Content{{Format: chat.FormatTeamsAdaptiveCard, Data: json.RawMessage(adaptiveCard)}}
		return m
	}())

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"by channel id", "?channel=C-general", 3},
		{"by channel key", "?channel=" + chat.ChannelKey("C-ops"), 2},
		{"by author", "?author=pager", 1},
		{"replies only", "?replies=1", 2},
		{"roots only", "?replies=0", 3},
		{"bots only", "?bot=1", 4},
		{"humans only", "?bot=0", 1},
		{"by thread", "?thread=1.1", 2},
		{"by format", "?format=msteams.adaptivecard", 1},
		{"by format, none", "?format=slack.blocks", 0},
		{"by search", "?search=orphan", 1},
		{"paged", "?limit=2", 2},
		{"offset past the end", "?offset=99", 0},
		{"unknown channel", "?channel=nope", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := listMessages(t, in, tc.query); len(got) != tc.want {
				t.Errorf("%s returned %d messages, want %d", tc.query, len(got), tc.want)
			}
		})
	}

	// A bad query parameter is a 400, not a 500.
	var body any
	if status := in.GetJSON(in.API("/chat/messages?limit=nope"), &body); status != http.StatusBadRequest {
		t.Errorf("a malformed limit returned %d, want 400", status)
	}
}

// The structured payload must survive the API byte for byte: a client that
// renders Block Kit needs the real blocks, not a reshaped version of them.
func TestAPIKeepsStructuredContentVerbatim(t *testing.T) {
	in := start(t)
	m := msg("C1", "deploy-bot", "", "1.1")
	m.Contents = []chat.Content{
		{Format: chat.FormatSlackBlocks, Data: json.RawMessage(slackBlocks)},
		{Format: chat.FormatSlackAttachments, Data: json.RawMessage(`[{"color":"#36a64f","fallback":"legacy"}]`)},
	}
	ev := injectAt(t, in, at(0), m)

	var got chat.MessageEnvelope
	if status := in.GetJSON(in.API("/chat/messages/"+string(ev.ID)), &got); status != http.StatusOK {
		t.Fatalf("GET one message = %d", status)
	}
	if len(got.Formats) != 2 || got.Formats[0] != chat.FormatSlackBlocks || got.Formats[1] != chat.FormatSlackAttachments {
		t.Errorf("Formats = %v", got.Formats)
	}
	var blocks []map[string]any
	if err := got.Message.Contents[0].Decode(&blocks); err != nil {
		t.Fatalf("decode blocks: %v", err)
	}
	if len(blocks) != 4 || blocks[0]["type"] != "header" {
		t.Errorf("blocks = %+v", blocks)
	}
	// And the text fallback came along, so the message reads without a renderer.
	if got.Message.Text == "" {
		t.Error("the API must serve a message with a usable text fallback")
	}
}

func TestAPIGetMessageNotFound(t *testing.T) {
	in := start(t)
	var body map[string]string
	if status := in.GetJSON(in.API("/chat/messages/nope"), &body); status != http.StatusNotFound {
		t.Errorf("unknown id returned %d, want 404", status)
	}
	if body["error"] == "" {
		t.Error("a 404 should explain itself")
	}
}

func TestAPIChannels(t *testing.T) {
	in := start(t)
	seed(t, in)

	got := listChannels(t, in, "")
	if len(got) != 2 {
		t.Fatalf("got %d channels, want 2", len(got))
	}
	// Newest activity first.
	if got[0].ID != "C-ops" {
		t.Errorf("first channel = %q, want the most recently active", got[0].ID)
	}
	ops := got[0]
	if ops.Messages != 1 || ops.Threads != 1 || ops.Replies != 0 || ops.Orphans != 0 {
		t.Errorf("ops summary = %+v", ops)
	}
	if ops.LastAuthor != "pager" || ops.LastPreview != "ops alert" {
		t.Errorf("ops last message = %+v", ops)
	}
	if ops.UIURL != "/ui/chat/channels/"+ops.Key || ops.MessagesURL == "" {
		t.Errorf("links = %q %q", ops.UIURL, ops.MessagesURL)
	}

	general := got[1]
	if general.Messages != 3 || general.Threads != 2 || general.Replies != 2 {
		t.Errorf("general summary = %+v", general)
	}
	// The orphaned reply is reported rather than hidden.
	if general.Orphans != 1 {
		t.Errorf("general.Orphans = %d, want 1", general.Orphans)
	}
	if general.LastMessageAt.IsZero() || general.LastMessageID == "" {
		t.Errorf("general is missing its last-message fields: %+v", general)
	}
}

func TestAPIChannelsHonoursFilters(t *testing.T) {
	in := start(t)
	seed(t, in)
	if got := listChannels(t, in, "?channel=C-ops"); len(got) != 1 || got[0].ID != "C-ops" {
		t.Errorf("filtered channels = %+v", got)
	}
	if got := listChannels(t, in, "?search=orphan"); len(got) != 1 || got[0].Messages != 1 {
		t.Errorf("searched channels = %+v", got)
	}
	if got := listChannels(t, in, "?limit=1"); len(got) != 1 {
		t.Errorf("limited channels = %+v", got)
	}
}

func TestAPIChannelsWhenEmpty(t *testing.T) {
	in := start(t)
	var got []chat.ChannelSummary
	if status := in.GetJSON(in.API("/chat/channels"), &got); status != http.StatusOK {
		t.Fatalf("GET /chat/channels on an empty tommy = %d", status)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("empty channel index = %v, want []", got)
	}
}

func TestAPIDeleteMessages(t *testing.T) {
	in := start(t)
	seed(t, in)

	req, err := http.NewRequest(http.MethodDelete, in.API("/chat/messages"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp := in.Do(req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE returned %d, want 204", resp.StatusCode)
	}
	if got := listMessages(t, in, ""); len(got) != 0 {
		t.Errorf("%d messages survived the clear", len(got))
	}
	if got := listChannels(t, in, ""); len(got) != 0 {
		t.Errorf("%d channels survived the clear", len(got))
	}
}

// Read-back serves from the store, so a client that posts and immediately
// fetches sees its own write.
func TestAPIReadsBackItsOwnWrite(t *testing.T) {
	in := start(t)
	status, body := in.PostJSON(in.Ingress("/fake-chat/v1/messages"), map[string]any{
		"channel": map[string]string{"id": "C0123ABCD"},
		"author":  map[string]any{"name": "deploy-bot", "bot": true},
		"text":    "read me back",
		"ts":      "1700000000.000100",
	})
	if status != http.StatusOK {
		t.Fatalf("POST = %d %s", status, body)
	}
	got := listMessages(t, in, "?channel=C0123ABCD")
	if len(got) != 1 || got[0].Message.Text != "read me back" {
		t.Fatalf("read back %d messages: %+v", len(got), got)
	}
}
