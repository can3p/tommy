package chat_test

import (
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/plugins/chat"
)

// The conformance gate: descriptions are real, snippets render, and every
// endpoint the fake provider declares is actually mounted.
func TestConformance(t *testing.T) {
	plugintest.Conformance(t, chat.New(fakeProvider{}))
}

func TestPluginIdentity(t *testing.T) {
	p := chat.New(fakeProvider{})
	if p.Name() != "chat" {
		t.Errorf("Name() = %q", p.Name())
	}
	if p.Title() != "Chat" {
		t.Errorf("Title() = %q", p.Title())
	}
	if len(p.Description()) < 40 || !strings.Contains(strings.ToLower(p.Description()), "chat") {
		t.Errorf("Description() = %q, want a couple of real sentences", p.Description())
	}
	if got := p.Providers(); len(got) != 1 || got[0].Name() != "fake" {
		t.Errorf("Providers() = %v", got)
	}
	if got := chat.New().Providers(); got == nil || len(got) != 0 {
		t.Errorf("Providers() with none supplied = %v, want an empty non-nil slice", got)
	}
}

func TestTemplatesAreEmbedded(t *testing.T) {
	entries, err := fs.ReadDir(chat.New().Templates(), ".")
	if err != nil {
		t.Fatalf("Templates(): %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("the chat tab ships no templates")
	}
}

// The ingress route the fake provider declares actually answers, which is what
// proves a real provider's path into the plugin.
func TestFakeProviderIngress(t *testing.T) {
	in := start(t)
	status, body := in.PostJSON(in.Ingress("/fake-chat/v1/messages"), map[string]any{
		"channel": map[string]string{"id": "C0123ABCD", "name": "general"},
		"author":  map[string]any{"name": "deploy-bot", "bot": true},
		"text":    "It works.",
		"ts":      "1700000000.000100",
	})
	if status != 200 {
		t.Fatalf("POST returned %d: %s", status, body)
	}
	events := in.WaitForEvents(1, store.Query{Plugin: chat.PluginName}, 2*time.Second)
	m, ok := chat.MessageOf(events[0])
	if !ok {
		t.Fatal("the event the fake provider appended carries no chat message")
	}
	if m.Channel.ID != "C0123ABCD" || m.Text != "It works." || !m.Author.Bot {
		t.Errorf("captured message = %+v", m)
	}
	if events[0].Type != chat.TypeMessage {
		t.Errorf("event type = %q, want %q", events[0].Type, chat.TypeMessage)
	}
	if len(events[0].Raw.Body) == 0 || events[0].Raw.Transport != "http" {
		t.Errorf("Raw was not populated: %+v", events[0].Raw)
	}
}

var _ plugin.Plugin = chat.New()
