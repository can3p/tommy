package msteams_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/chat"
	"github.com/can3p/tommy/plugins/chat/providers/msteams"
)

// --- conformance -------------------------------------------------------

func TestConformance(t *testing.T) {
	t.Parallel()
	plugintest.ConformanceProvider(t, msteams.New())
	plugintest.Conformance(t, chat.New(msteams.New()))
}

// --- test harness --------------------------------------------------------

const (
	testGUID   = "11111111-1111-1111-1111-111111111111"
	testTenant = "22222222-2222-2222-2222-222222222222"
	testID     = "33333333333333333333333333333333"
	testKey    = "44444444-4444-4444-4444-444444444444"
)

// webhookPath is the concrete path a real Teams webhook client posts to,
// built from fixed test identifiers so assertions can be exact.
const webhookPath = "/webhookb2/" + testGUID + "@" + testTenant + "/IncomingWebhook/" + testID + "/" + testKey

type harness struct {
	t  *testing.T
	ts *httptest.Server
	d  plugin.Deps
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	d := plugintest.NewDeps()
	mux := http.NewServeMux()
	msteams.New().RegisterIngress(mux, d)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &harness{t: t, ts: ts, d: d}
}

func (h *harness) post(body []byte) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.ts.URL+webhookPath, bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		h.t.Fatalf("do request: %v", err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (h *harness) events() []*event.Event {
	h.t.Helper()
	evs, err := h.d.Store.List(h.t.Context(), store.Query{Plugin: chat.PluginName, Provider: msteams.ProviderName})
	if err != nil {
		h.t.Fatalf("list events: %v", err)
	}
	return evs
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func readAll(t *testing.T, r *http.Response) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r.Body); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return buf.Bytes()
}

// --- MessageCard: the "1"/text-plain success contract ---------------------

func TestMessageCardResponseContract(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	fixture := loadFixture(t, "messagecard_basic.json")

	resp := h.post(fixture)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/plain")
	}
	body := readAll(t, resp)
	if string(body) != "1" {
		t.Errorf("body = %q, want literal \"1\"", body)
	}

	evs := h.events()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	ev := evs[0]

	msg, ok := chat.MessageOf(ev)
	if !ok {
		t.Fatal("event carries no canonical message")
	}
	if msg.Channel.ID != webhookPath {
		t.Errorf("Channel.ID = %q, want the webhook path %q", msg.Channel.ID, webhookPath)
	}
	if !msg.Author.Bot {
		t.Error("Author.Bot should be true for a webhook post")
	}
	if msg.Author.Name != "deploy-bot" {
		t.Errorf("Author.Name = %q, want the section's activityTitle", msg.Author.Name)
	}
	if msg.Author.IconURL != "https://example.com/avatar.png" {
		t.Errorf("Author.IconURL = %q, want the section's activityImage", msg.Author.IconURL)
	}
	if msg.Text != "It works." {
		t.Errorf("Text = %q, want the card's top-level text", msg.Text)
	}
	if msg.ThreadTS != "" {
		t.Errorf("ThreadTS = %q, want empty: Teams webhooks have no thread semantics", msg.ThreadTS)
	}
	if len(msg.Contents) != 1 {
		t.Fatalf("got %d contents, want 1", len(msg.Contents))
	}
	c := msg.Contents[0]
	if c.Format != chat.FormatTeamsMessageCard {
		t.Errorf("Format = %q, want %q", c.Format, chat.FormatTeamsMessageCard)
	}
	if !bytes.Equal([]byte(c.Data), bytes.TrimSpace(fixture)) {
		t.Errorf("Content.Data = %s, want the request body verbatim %s", c.Data, bytes.TrimSpace(fixture))
	}

	if ev.Meta["guid"] != testGUID {
		t.Errorf("Meta[guid] = %v, want %q", ev.Meta["guid"], testGUID)
	}
	if ev.Meta["tenant"] != testTenant {
		t.Errorf("Meta[tenant] = %v, want %q", ev.Meta["tenant"], testTenant)
	}
	if ev.Meta["id"] != testID {
		t.Errorf("Meta[id] = %v, want %q", ev.Meta["id"], testID)
	}
	if ev.Meta["key"] != testKey {
		t.Errorf("Meta[key] = %v, want %q", ev.Meta["key"], testKey)
	}
	if ev.Meta["path"] != webhookPath {
		t.Errorf("Meta[path] = %v, want %q", ev.Meta["path"], webhookPath)
	}
	if ev.Meta["generation"] != "connector" {
		t.Errorf("Meta[generation] = %v, want %q", ev.Meta["generation"], "connector")
	}
	if ev.Meta["themeColor"] != "FF0000" {
		t.Errorf("Meta[themeColor] = %v, want %q", ev.Meta["themeColor"], "FF0000")
	}
	if ev.Meta["@context"] != "https://schema.org/extensions" {
		t.Errorf("Meta[@context] = %v", ev.Meta["@context"])
	}
	if _, ok := ev.Meta["potentialAction"]; !ok {
		t.Error("Meta[potentialAction] is missing")
	}

	if ev.Raw.Transport != "http" || ev.Raw.Method != http.MethodPost || ev.Raw.Path != webhookPath {
		t.Errorf("Raw = %+v", ev.Raw)
	}
	if !ev.Raw.Text {
		t.Error("Raw.Text should be true for a JSON body")
	}
	if !bytes.Equal(ev.Raw.Body, fixture) {
		t.Error("Raw.Body does not match the request body sent")
	}
}

// --- MessageCard with neither top-level text nor summary: FallbackText ----

func TestMessageCardFallbackTextFromSections(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	resp := h.post(loadFixture(t, "messagecard_no_top_text.json"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.StatusCode, readAll(t, resp))
	}
	_ = readAll(t, resp)

	msg, ok := chat.MessageOf(h.events()[0])
	if !ok {
		t.Fatal("no canonical message")
	}
	if strings.TrimSpace(msg.Text) == "" {
		t.Fatal("Text must never be empty, even for a card with no top-level text or summary")
	}
	if !strings.Contains(msg.Text, "release-bot") {
		t.Errorf("Text = %q, want it to include the harvested activityTitle", msg.Text)
	}
	if !strings.Contains(msg.Text, "Everything shipped cleanly.") {
		t.Errorf("Text = %q, want it to include the harvested section text", msg.Text)
	}
}

// --- Adaptive Card: the 202 success contract, and unwrapped storage -------

func TestAdaptiveCardResponseContract(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	resp := h.post(loadFixture(t, "adaptivecard_basic.json"))

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		t.Errorf("Content-Type = %q, want empty for an empty 202 body", ct)
	}
	body := readAll(t, resp)
	if len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}

	evs := h.events()
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	ev := evs[0]
	msg, ok := chat.MessageOf(ev)
	if !ok {
		t.Fatal("no canonical message")
	}
	if len(msg.Contents) != 1 {
		t.Fatalf("got %d contents, want 1", len(msg.Contents))
	}
	c := msg.Contents[0]
	if c.Format != chat.FormatTeamsAdaptiveCard {
		t.Errorf("Format = %q, want %q", c.Format, chat.FormatTeamsAdaptiveCard)
	}

	// The stored content must be the inner card object, unwrapped: it starts
	// at "type":"AdaptiveCard" and must not carry the envelope's own
	// "attachments" or "contentType" keys.
	var decoded map[string]any
	if err := json.Unmarshal(c.Data, &decoded); err != nil {
		t.Fatalf("decode stored content: %v", err)
	}
	if decoded["type"] != "AdaptiveCard" {
		t.Errorf("stored content type = %v, want AdaptiveCard (the card itself, not the envelope)", decoded["type"])
	}
	if _, ok := decoded["attachments"]; ok {
		t.Error("stored content still carries the envelope's attachments key")
	}

	wantContent := `{"$schema":"http://adaptivecards.io/schemas/adaptive-card.json","type":"AdaptiveCard","version":"1.4","body":[{"type":"TextBlock","text":"It works.","weight":"bolder"}]}`
	if string(c.Data) != wantContent {
		t.Errorf("Content.Data = %s, want the inner content object byte for byte:\n%s", c.Data, wantContent)
	}

	if ev.Meta["generation"] != "workflow" {
		t.Errorf("Meta[generation] = %v, want %q", ev.Meta["generation"], "workflow")
	}
}

// --- Adaptive Card with no top-level text: FallbackText from the card body

func TestAdaptiveCardFallbackTextFromCardBody(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	resp := h.post(loadFixture(t, "adaptivecard_no_top_text.json"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", resp.StatusCode, readAll(t, resp))
	}
	_ = readAll(t, resp)

	msg, ok := chat.MessageOf(h.events()[0])
	if !ok {
		t.Fatal("no canonical message")
	}
	if msg.Text != "Deployment finished successfully." {
		t.Errorf("Text = %q, want the TextBlock's text harvested by FallbackText", msg.Text)
	}
}

// --- multiple attachments produce multiple Contents, order preserved ------

func TestAdaptiveCardMultipleAttachments(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	resp := h.post(loadFixture(t, "adaptivecard_multi_attachment.json"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body %s", resp.StatusCode, readAll(t, resp))
	}
	_ = readAll(t, resp)

	msg, ok := chat.MessageOf(h.events()[0])
	if !ok {
		t.Fatal("no canonical message")
	}
	if len(msg.Contents) != 2 {
		t.Fatalf("got %d contents, want 2", len(msg.Contents))
	}
	for i, c := range msg.Contents {
		if c.Format != chat.FormatTeamsAdaptiveCard {
			t.Errorf("contents[%d].Format = %q, want %q", i, c.Format, chat.FormatTeamsAdaptiveCard)
		}
	}
	if !strings.Contains(string(msg.Contents[0].Data), "First card.") {
		t.Errorf("contents[0] = %s, want the first attachment's card", msg.Contents[0].Data)
	}
	if !strings.Contains(string(msg.Contents[1].Data), "Second card.") {
		t.Errorf("contents[1] = %s, want the second attachment's card, in order", msg.Contents[1].Data)
	}
}

// --- the bare {"text":"..."} shape a workflow trigger also accepts --------

func TestBareTextWorkflowPayload(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	resp := h.post(loadFixture(t, "bare_text.json"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if body := readAll(t, resp); len(body) != 0 {
		t.Errorf("body = %q, want empty", body)
	}

	msg, ok := chat.MessageOf(h.events()[0])
	if !ok {
		t.Fatal("no canonical message")
	}
	if msg.Text != "Hello from a webhook workflow!" {
		t.Errorf("Text = %q", msg.Text)
	}
	if msg.HasContent() {
		t.Errorf("a bare text payload should carry no structured content, got %+v", msg.Contents)
	}
}

// --- malformed-payload error handling --------------------------------------

func TestMalformedPayloadIsRejected(t *testing.T) {
	t.Parallel()

	t.Run("invalid json", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		resp := h.post(loadFixture(t, "error_malformed.json"))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("Content-Type = %q, want text/plain", ct)
		}
		if len(h.events()) != 0 {
			t.Error("a rejected request must not append any event")
		}
	})

	t.Run("valid json, no card and no text/summary", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		resp := h.post(loadFixture(t, "error_no_text_or_card.json"))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		body := readAll(t, resp)
		if !strings.Contains(string(body), "Summary or Text is required") {
			t.Errorf("body = %q, want the real endpoint's own validation message", body)
		}
		if len(h.events()) != 0 {
			t.Error("a rejected request must not append any event")
		}
	})

	t.Run("oversized body", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		huge := bytes.Repeat([]byte("a"), msteams.MaxBody+1)
		payload, err := json.Marshal(map[string]string{"text": string(huge)})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		resp := h.post(payload)
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", resp.StatusCode)
		}
		if len(h.events()) != 0 {
			t.Error("an oversized request must not append any event")
		}
	})
}

// --- end to end through the full chat plugin and a real ingress -----------

func TestEndToEndThroughChatPlugin(t *testing.T) {
	t.Parallel()
	in := testutil.Start(t, nil, chat.New(msteams.New()))

	req, err := http.NewRequest(http.MethodPost, in.Ingress(webhookPath), bytes.NewReader(loadFixture(t, "messagecard_basic.json")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp := in.Do(req)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	events := in.WaitForEvents(1, store.Query{Plugin: chat.PluginName, Provider: msteams.ProviderName}, 2*time.Second)
	if events[0].Summary.Snippet == "" {
		t.Error("summary snippet should not be empty")
	}

	status, body := in.GetBody(in.API("/chat/messages"))
	if status != http.StatusOK {
		t.Fatalf("GET /chat/messages: status %d", status)
	}
	if !strings.Contains(body, "It works.") {
		t.Errorf("GET /chat/messages did not include the sent text: %s", body)
	}

	status, body = in.GetBody(in.API("/chat/channels"))
	if status != http.StatusOK {
		t.Fatalf("GET /chat/channels: status %d", status)
	}
	if !strings.Contains(body, webhookPath) {
		t.Errorf("GET /chat/channels did not include the derived channel id: %s", body)
	}
}
