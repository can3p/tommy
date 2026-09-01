package slack_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/plugins/chat"
	"github.com/can3p/tommy/plugins/chat/providers/slack"
)

const webhookURL = "/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"

func newProvider(t *testing.T) (*http.ServeMux, plugin.Deps) {
	t.Helper()
	d := plugintest.NewDeps()
	mux := http.NewServeMux()
	slack.New().RegisterIngress(mux, d)
	return mux, d
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return bytes.TrimRight(b, "\n")
}

func postJSON(t *testing.T, mux *http.ServeMux, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestWebhookOK proves the single easiest thing to get wrong about incoming
// webhooks: the success response is the literal text "ok", not a JSON object.
func TestWebhookOK(t *testing.T) {
	mux, d := newProvider(t)
	rec := postJSON(t, mux, webhookURL, readFixture(t, "webhook_text.json"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("body = %q, want exactly \"ok\"", got)
	}

	events, err := d.Store.List(t.Context(), store.Query{Plugin: chat.PluginName, Provider: slack.Name})
	if err != nil || len(events) != 1 {
		t.Fatalf("store.List: %v, %d events", err, len(events))
	}
	m, ok := chat.MessageOf(events[0])
	if !ok {
		t.Fatalf("event carries no chat.Message")
	}
	want := &chat.Message{
		Channel: chat.ChannelRef{ID: "#general", Name: "general"},
		Author:  chat.Author{Name: "deploy-bot", IconURL: "https://example.com/icon.png", Bot: true},
		Text:    "It works.",
	}
	want.Normalize()
	if !messageEqual(m, want) {
		t.Errorf("stored message mismatch:\n got  = %+v\n want = %+v", m, want)
	}
	if events[0].Type != chat.TypeMessage {
		t.Errorf("event.Type = %q, want %q", events[0].Type, chat.TypeMessage)
	}
	if events[0].Raw.Transport != "http" || events[0].Raw.Method != "POST" || events[0].Raw.Path != webhookURL || !events[0].Raw.Text {
		t.Errorf("Raw not populated as expected: %+v", events[0].Raw)
	}
	if !bytes.Equal(events[0].Raw.Body, readFixture(t, "webhook_text.json")) {
		t.Errorf("Raw.Body does not carry the exact request body")
	}
	if events[0].Meta["team"] != "T00000000" || events[0].Meta["bot"] != "B00000000" || events[0].Meta["webhook_token"] != "XXXXXXXXXXXXXXXXXXXXXXXX" {
		t.Errorf("Meta missing team/bot/webhook_token: %+v", events[0].Meta)
	}
}

// TestWebhookBlocksVerbatim proves the blocks array is stored byte for byte,
// never re-marshaled, and that Text falls back to a harvested summary when the
// payload carried none of its own.
func TestWebhookBlocksVerbatim(t *testing.T) {
	mux, d := newProvider(t)
	rec := postJSON(t, mux, webhookURL, readFixture(t, "webhook_blocks.json"))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}

	events, _ := d.Store.List(t.Context(), store.Query{Plugin: chat.PluginName, Provider: slack.Name})
	m, ok := chat.MessageOf(events[0])
	if !ok || len(m.Contents) != 1 {
		t.Fatalf("message = %+v, ok=%v", m, ok)
	}
	if m.Contents[0].Format != chat.FormatSlackBlocks {
		t.Errorf("format = %q, want %q", m.Contents[0].Format, chat.FormatSlackBlocks)
	}
	var payload struct {
		Blocks json.RawMessage `json:"blocks"`
	}
	if err := json.Unmarshal(readFixture(t, "webhook_blocks.json"), &payload); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if !jsonEqual(t, m.Contents[0].Data, payload.Blocks) {
		t.Errorf("blocks not stored verbatim:\n got  = %s\n want = %s", m.Contents[0].Data, payload.Blocks)
	}
	if m.Text != "*Build failed.*" {
		t.Errorf("fallback text = %q, want %q", m.Text, "*Build failed.*")
	}
}

// TestWebhookAttachmentsVerbatim proves legacy attachments round-trip
// byte-for-byte too, alongside real text.
func TestWebhookAttachmentsVerbatim(t *testing.T) {
	mux, d := newProvider(t)
	rec := postJSON(t, mux, webhookURL, readFixture(t, "webhook_attachments.json"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	events, _ := d.Store.List(t.Context(), store.Query{Plugin: chat.PluginName, Provider: slack.Name})
	m, ok := chat.MessageOf(events[0])
	if !ok || len(m.Contents) != 1 {
		t.Fatalf("message = %+v, ok=%v", m, ok)
	}
	if m.Contents[0].Format != chat.FormatSlackAttachments {
		t.Errorf("format = %q, want %q", m.Contents[0].Format, chat.FormatSlackAttachments)
	}
	if m.Text != "fallback text" {
		t.Errorf("text = %q, want the payload's own text preserved", m.Text)
	}
}

// TestWebhookThread proves thread_ts passes through untouched into ThreadTS.
func TestWebhookThread(t *testing.T) {
	mux, d := newProvider(t)
	rec := postJSON(t, mux, webhookURL, readFixture(t, "webhook_thread.json"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	events, _ := d.Store.List(t.Context(), store.Query{Plugin: chat.PluginName, Provider: slack.Name})
	m, ok := chat.MessageOf(events[0])
	if !ok {
		t.Fatalf("no message")
	}
	if m.ThreadTS != "1700000000.000100" {
		t.Errorf("thread_ts = %q, want passthrough", m.ThreadTS)
	}
	if m.Channel.ID != "C0123ABCD" {
		t.Errorf("channel id = %q, want the given C… id", m.Channel.ID)
	}
}

// TestWebhookChannelFallback proves a webhook payload with no channel override
// still gets a stable, non-empty channel id derived from the webhook path.
func TestWebhookChannelFallback(t *testing.T) {
	mux, d := newProvider(t)
	body := []byte(`{"text":"no channel override here"}`)
	rec := postJSON(t, mux, webhookURL, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events, _ := d.Store.List(t.Context(), store.Query{Plugin: chat.PluginName, Provider: slack.Name})
	m, ok := chat.MessageOf(events[0])
	if !ok {
		t.Fatalf("no message")
	}
	if m.Channel.ID == "" {
		t.Fatalf("channel id must be non-empty")
	}
	if m.Channel.ID != "webhook:T00000000/B00000000" {
		t.Errorf("channel id = %q, want a stable id derived from team/bot", m.Channel.ID)
	}

	// Posting again through the same webhook must land in the same channel.
	rec2 := postJSON(t, mux, webhookURL, []byte(`{"text":"second post"}`))
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d", rec2.Code)
	}
	events, _ = d.Store.List(t.Context(), store.Query{Plugin: chat.PluginName, Provider: slack.Name})
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	m2, _ := chat.MessageOf(events[0])
	if m2.Channel.ID != m.Channel.ID {
		t.Errorf("second post landed in a different channel: %q vs %q", m2.Channel.ID, m.Channel.ID)
	}
}

// TestWebhookErrors covers the two webhook error shapes: invalid JSON and a
// payload with no text, no blocks and no attachments. Both are plain text,
// like the success response, not JSON.
func TestWebhookErrors(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		wantCode string
	}{
		{name: "invalid json", fixture: "webhook_invalid.json", wantCode: "invalid_payload"},
		{name: "no text, blocks or attachments", fixture: "webhook_empty.json", wantCode: "no_text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, d := newProvider(t)
			rec := postJSON(t, mux, webhookURL, readFixture(t, tt.fixture))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "text/plain" {
				t.Errorf("Content-Type = %q, want text/plain", ct)
			}
			if got := rec.Body.String(); got != tt.wantCode {
				t.Errorf("body = %q, want %q", got, tt.wantCode)
			}
			events, _ := d.Store.List(t.Context(), store.Query{Plugin: chat.PluginName, Provider: slack.Name})
			if len(events) != 0 {
				t.Errorf("a rejected webhook must not append an event, got %d", len(events))
			}
		})
	}
}

func messageEqual(got, want *chat.Message) bool {
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	return bytes.Equal(g, w)
}

func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("decode a: %v", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("decode b: %v", err)
	}
	ab, _ := json.Marshal(av)
	bb, _ := json.Marshal(bv)
	return bytes.Equal(ab, bb)
}
