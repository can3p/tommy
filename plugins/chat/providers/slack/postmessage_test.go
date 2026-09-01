package slack_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/plugins/chat"
	"github.com/can3p/tommy/plugins/chat/providers/slack"
)

const postMessageURL = "/api/chat.postMessage"

// apiSuccess mirrors chat.postMessage's success envelope field for field, so
// tests assert on the exact shape a real client would decode.
type apiSuccess struct {
	OK      bool   `json:"ok"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	Message struct {
		Type        string          `json:"type"`
		Text        string          `json:"text"`
		Username    string          `json:"username"`
		BotID       string          `json:"bot_id"`
		TS          string          `json:"ts"`
		ThreadTS    string          `json:"thread_ts"`
		Blocks      json.RawMessage `json:"blocks"`
		Attachments json.RawMessage `json:"attachments"`
	} `json:"message"`
}

type apiFailure struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func postToChatAPI(t *testing.T, mux *http.ServeMux, contentType string, body []byte, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, postMessageURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestPostMessageJSON covers the plain JSON-body case: a 200 with the real
// {ok, channel, ts, message} envelope, and the canonical chat.Message actually
// stored.
func TestPostMessageJSON(t *testing.T) {
	mux, d := newProvider(t)
	rec := postToChatAPI(t, mux, "application/json", readFixture(t, "postmessage.json"), "xoxb-fake-token")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}

	var got apiSuccess
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v, body=%s", err, rec.Body.String())
	}
	if !got.OK {
		t.Fatalf("ok = false, body = %s", rec.Body.String())
	}
	if got.Channel != "C0123ABCD" {
		t.Errorf("channel = %q, want C0123ABCD", got.Channel)
	}
	if got.TS == "" {
		t.Errorf("ts is empty")
	}
	if got.Message.TS != got.TS {
		t.Errorf("message.ts = %q, want it to match the top-level ts %q", got.Message.TS, got.TS)
	}
	if got.Message.Type != "message" {
		t.Errorf("message.type = %q, want %q", got.Message.Type, "message")
	}
	if got.Message.Text != "It works." {
		t.Errorf("message.text = %q, want %q", got.Message.Text, "It works.")
	}
	if got.Message.BotID == "" {
		t.Errorf("message.bot_id is empty")
	}

	events, err := d.Store.List(t.Context(), store.Query{Plugin: chat.PluginName, Provider: slack.Name})
	if err != nil || len(events) != 1 {
		t.Fatalf("store.List: %v, %d events", err, len(events))
	}
	m, ok := chat.MessageOf(events[0])
	if !ok {
		t.Fatalf("event carries no chat.Message")
	}
	if m.Channel.ID != "C0123ABCD" || m.Text != "It works." || m.TS != got.TS {
		t.Errorf("stored message mismatch: %+v (want ts=%s)", m, got.TS)
	}
	if events[0].Meta["bearer_token"] != "xoxb-fake-token" || events[0].Meta["bearer_presented"] != true {
		t.Errorf("Meta missing bearer capture: %+v", events[0].Meta)
	}
	if events[0].Raw.Transport != "http" || events[0].Raw.Method != "POST" || !events[0].Raw.Text {
		t.Errorf("Raw not populated: %+v", events[0].Raw)
	}
	if !bytes.Equal(events[0].Raw.Body, readFixture(t, "postmessage.json")) {
		t.Errorf("Raw.Body does not carry the exact request body")
	}
}

// TestPostMessageForm covers the form-encoded case, including blocks arriving
// as a JSON-encoded string that must be stored verbatim once decoded.
func TestPostMessageForm(t *testing.T) {
	mux, d := newProvider(t)
	rec := postToChatAPI(t, mux, "application/x-www-form-urlencoded", readFixture(t, "postmessage.form"), "xoxb-fake-token")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got apiSuccess
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || got.Channel != "C0123ABCD" {
		t.Fatalf("got %+v", got)
	}
	if !jsonEqual(t, got.Message.Blocks, json.RawMessage(`[{"type":"section","text":{"type":"mrkdwn","text":"Hi"}}]`)) {
		t.Errorf("message.blocks = %s", got.Message.Blocks)
	}

	events, _ := d.Store.List(t.Context(), store.Query{Plugin: chat.PluginName, Provider: slack.Name})
	m, ok := chat.MessageOf(events[0])
	if !ok || len(m.Contents) != 1 || m.Contents[0].Format != chat.FormatSlackBlocks {
		t.Fatalf("message = %+v, ok=%v", m, ok)
	}
	if !jsonEqual(t, m.Contents[0].Data, json.RawMessage(`[{"type":"section","text":{"type":"mrkdwn","text":"Hi"}}]`)) {
		t.Errorf("stored blocks = %s", m.Contents[0].Data)
	}
}

// TestPostMessageBlocksVerbatim proves a JSON-body blocks array is stored
// byte-for-byte.
func TestPostMessageBlocksVerbatim(t *testing.T) {
	mux, d := newProvider(t)
	rec := postToChatAPI(t, mux, "application/json", readFixture(t, "postmessage_blocks.json"), "xoxb-fake-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events, _ := d.Store.List(t.Context(), store.Query{Plugin: chat.PluginName, Provider: slack.Name})
	m, ok := chat.MessageOf(events[0])
	if !ok || len(m.Contents) != 1 {
		t.Fatalf("message = %+v, ok=%v", m, ok)
	}
	var payload struct {
		Blocks json.RawMessage `json:"blocks"`
	}
	if err := json.Unmarshal(readFixture(t, "postmessage_blocks.json"), &payload); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if !jsonEqual(t, m.Contents[0].Data, payload.Blocks) {
		t.Errorf("blocks not stored verbatim:\n got  = %s\n want = %s", m.Contents[0].Data, payload.Blocks)
	}
}

// TestPostMessageThread proves thread_ts passes through untouched.
func TestPostMessageThread(t *testing.T) {
	mux, d := newProvider(t)
	rec := postToChatAPI(t, mux, "application/json", readFixture(t, "postmessage_thread.json"), "xoxb-fake-token")
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

	var got apiSuccess
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Message.ThreadTS != "1700000000.000100" {
		t.Errorf("message.thread_ts = %q", got.Message.ThreadTS)
	}
}

// TestPostMessageAuth covers auth capture (default-accept, presented token
// recorded) and the not_authed / invalid_auth error shapes.
func TestPostMessageAuth(t *testing.T) {
	t.Run("no token at all is not_authed", func(t *testing.T) {
		mux, d := newProvider(t)
		rec := postToChatAPI(t, mux, "application/json", readFixture(t, "postmessage.json"), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (Slack returns 200 with ok:false for app-level errors)", rec.Code)
		}
		var got apiFailure
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.OK || got.Error != "not_authed" {
			t.Errorf("got %+v, want {ok:false, error:not_authed}", got)
		}
		events, _ := d.Store.List(t.Context(), store.Query{Plugin: chat.PluginName, Provider: slack.Name})
		if len(events) != 0 {
			t.Errorf("a rejected auth must not append an event, got %d", len(events))
		}
	})

	t.Run("any bearer token is accepted by default and recorded", func(t *testing.T) {
		mux, d := newProvider(t)
		rec := postToChatAPI(t, mux, "application/json", readFixture(t, "postmessage.json"), "xoxb-whatever")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		events, _ := d.Store.List(t.Context(), store.Query{Plugin: chat.PluginName, Provider: slack.Name})
		if events[0].Meta["bearer_token"] != "xoxb-whatever" {
			t.Errorf("bearer_token = %v", events[0].Meta["bearer_token"])
		}
	})

	t.Run("token in the JSON body is accepted when no header is presented", func(t *testing.T) {
		mux, d := newProvider(t)
		rec := postToChatAPI(t, mux, "application/json", []byte(`{"channel":"C0123ABCD","text":"hi","token":"xoxb-body-token"}`), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var got apiSuccess
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !got.OK {
			t.Fatalf("decode: %v, body=%s", err, rec.Body.String())
		}
		events, _ := d.Store.List(t.Context(), store.Query{Plugin: chat.PluginName, Provider: slack.Name})
		if events[0].Meta["bearer_token"] != "xoxb-body-token" {
			t.Errorf("bearer_token = %v", events[0].Meta["bearer_token"])
		}
	})
}

// TestPostMessageErrors covers channel_not_found and no_text.
func TestPostMessageErrors(t *testing.T) {
	tests := []struct {
		name      string
		fixture   string
		wantError string
	}{
		{name: "missing channel", fixture: "postmessage_no_channel.json", wantError: "channel_not_found"},
		{name: "no text, blocks or attachments", fixture: "postmessage_empty.json", wantError: "no_text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux, d := newProvider(t)
			rec := postToChatAPI(t, mux, "application/json", readFixture(t, tt.fixture), "xoxb-fake-token")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			var got apiFailure
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v, body=%s", err, rec.Body.String())
			}
			if got.OK || got.Error != tt.wantError {
				t.Errorf("got %+v, want error %q", got, tt.wantError)
			}
			events, _ := d.Store.List(t.Context(), store.Query{Plugin: chat.PluginName, Provider: slack.Name})
			if len(events) != 0 {
				t.Errorf("a rejected postMessage must not append an event, got %d", len(events))
			}
		})
	}
}
