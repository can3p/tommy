package slack_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/chat"
	"github.com/can3p/tommy/plugins/chat/providers/slack"
)

// TestEndToEnd boots a whole tommy with chat.New(slack.New()) and exercises
// both surfaces exactly as an outside client would: post through the ingress,
// then read the message back through the plugin's own /api/v1/chat/messages,
// proving the two never disagree.
func TestEndToEnd(t *testing.T) {
	in := testutil.Start(t, nil, chat.New(slack.New()))

	t.Run("incoming webhook", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost,
			in.Ingress("/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"),
			strings.NewReader(`{"text":"It works.","channel":"#general","username":"deploy-bot"}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp := in.Do(req)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
			t.Errorf("Content-Type = %q, want text/plain", ct)
		}

		events := in.WaitForEvents(1, store.Query{Plugin: chat.PluginName, Provider: slack.Name}, 2*time.Second)
		if len(events) != 1 {
			t.Fatalf("got %d events", len(events))
		}

		var envelopes []struct {
			Message chat.Message `json:"message"`
		}
		if status := in.GetJSON(in.API("chat/messages"), &envelopes); status != http.StatusOK {
			t.Fatalf("GET messages status = %d", status)
		}
		if len(envelopes) != 1 || envelopes[0].Message.Text != "It works." {
			t.Fatalf("read-back mismatch: %+v", envelopes)
		}
	})

	t.Run("chat.postMessage", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, in.Ingress("/api/chat.postMessage"),
			strings.NewReader(`{"channel":"C0123ABCD","text":"via web api"}`))
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer xoxb-fake-token")
		resp := in.Do(req)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}

		var got struct {
			OK      bool   `json:"ok"`
			Channel string `json:"channel"`
			TS      string `json:"ts"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !got.OK || got.Channel != "C0123ABCD" || got.TS == "" {
			t.Fatalf("got %+v", got)
		}

		var channels []struct {
			ID string `json:"id"`
		}
		if status := in.GetJSON(in.API("chat/channels"), &channels); status != http.StatusOK {
			t.Fatalf("GET channels status = %d", status)
		}
		found := false
		for _, c := range channels {
			if c.ID == "C0123ABCD" {
				found = true
			}
		}
		if !found {
			t.Errorf("channel C0123ABCD not present in read-back: %+v", channels)
		}
	})
}
