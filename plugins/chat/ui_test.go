package chat_test

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/chat"
)

func uiDoc(t *testing.T, in *testutil.Instance, url string) *goquery.Document {
	t.Helper()
	resp := in.Get(url)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		t.Fatalf("parse %s: %v", url, err)
	}
	return doc
}

// uiFragment asks the way htmx does, so the handler returns a bare fragment.
func uiFragment(t *testing.T, in *testutil.Instance, method, url string) (int, *goquery.Document) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("HX-Request", "true")
	resp := in.Do(req)
	defer func() { _ = resp.Body.Close() }()
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		t.Fatalf("parse %s: %v", url, err)
	}
	return resp.StatusCode, doc
}

func TestUITabIsRegistered(t *testing.T) {
	in := start(t)
	doc := uiDoc(t, in, in.UIURL)

	var hrefs []string
	doc.Find("nav.tabs a").Each(func(_ int, s *goquery.Selection) {
		if href, ok := s.Attr("href"); ok {
			hrefs = append(hrefs, s.Text()+"="+href)
		}
	})
	if !strings.Contains(strings.Join(hrefs, " "), "Chat=/ui/chat/") {
		t.Errorf("tab bar = %v, want a Chat tab pointing at /ui/chat/", hrefs)
	}
}

func TestUIEmptyState(t *testing.T) {
	in := start(t)
	doc := uiDoc(t, in, in.UI("/chat/"))

	if doc.Find(".empty-state").Length() == 0 {
		t.Fatal("an empty chat tab must show an empty state")
	}
	if !strings.Contains(doc.Find(".empty-state").Text(), "No chat messages yet") {
		t.Errorf("empty state text = %q", doc.Find(".empty-state").Text())
	}
	// The how-to-test panel is open when there is nothing on screen, which is
	// exactly when someone needs to know how to fill it.
	panel := doc.Find("details.how-to-test")
	if panel.Length() == 0 {
		t.Fatal("the chat tab must carry the shared how-to-test panel")
	}
	if _, open := panel.Attr("open"); !open {
		t.Error("the how-to-test panel should start open on an empty tab")
	}
	if !strings.Contains(doc.Text(), "/fake-chat/v1/messages") {
		t.Error("the empty tab should list the enabled providers' endpoints")
	}
	// And the snippet is rendered against the ports this instance bound.
	if !strings.Contains(doc.Find(".snippet").Text(), in.IngressURL) {
		t.Errorf("snippet should carry the live ingress URL %q", in.IngressURL)
	}
}

func TestUIEmptyStateWithNoProviders(t *testing.T) {
	in := testutil.Start(t, nil, chat.New())
	doc := uiDoc(t, in, in.UI("/chat/"))
	if !strings.Contains(doc.Text(), "No Chat provider is enabled") {
		t.Errorf("a provider-less chat tab should say so; got %q", strings.TrimSpace(doc.Find(".chat-tab").Text()))
	}
}

func TestUIChannelSidebar(t *testing.T) {
	in := start(t)
	seed(t, in)

	doc := uiDoc(t, in, in.UI("/chat/"))
	channels := doc.Find(".chat-channel")
	if channels.Length() != 2 {
		t.Fatalf("got %d channels in the sidebar, want 2", channels.Length())
	}
	if got := strings.TrimSpace(channels.Eq(0).Find(".chat-channel-name").Text()); got != "C-ops" {
		t.Errorf("first channel = %q, want the most recently active", got)
	}
	if got := channels.Eq(0).Find(".chat-channel-preview").Text(); !strings.Contains(got, "ops alert") {
		t.Errorf("preview = %q, want the newest message", got)
	}
	if got := channels.Eq(0).Find(".chat-channel-author").Text(); !strings.Contains(got, "pager") {
		t.Errorf("preview author = %q", got)
	}
	if !strings.Contains(doc.Find(".count").Text(), "4 messages") {
		t.Errorf("count line = %q, want a message total", doc.Find(".count").Text())
	}

	href, _ := channels.Eq(1).Find("a").Attr("href")
	if want := "/ui/chat/channels/" + chat.ChannelKey("C-general"); href != want {
		t.Errorf("channel link = %q, want %q", href, want)
	}
	// The general channel advertises its replies and its orphaned thread.
	badges := channels.Eq(1).Find(".badge").Text()
	if !strings.Contains(badges, "2 replies") || !strings.Contains(badges, "1 orphaned") {
		t.Errorf("channel badges = %q", badges)
	}
	// The tab opens on a channel rather than a blank pane.
	if channels.Eq(0).HasClass("selected") == false {
		t.Error("the most recently active channel should be selected by default")
	}
}

func TestUIStreamNestsRepliesUnderTheirParent(t *testing.T) {
	in := start(t)
	seed(t, in)

	doc := uiDoc(t, in, in.UI("/chat/channels/"+chat.ChannelKey("C-general")))

	threads := doc.Find(".chat-thread")
	if threads.Length() != 2 {
		t.Fatalf("got %d threads, want 2", threads.Length())
	}
	root := threads.Eq(0)
	if got := strings.TrimSpace(root.ChildrenFiltered(".chat-message").Find(".chat-text").First().Text()); got != "root in general" {
		t.Errorf("thread root = %q", got)
	}
	replies := root.Find(".chat-replies .chat-message")
	if replies.Length() != 1 {
		t.Fatalf("got %d nested replies, want 1", replies.Length())
	}
	if got := strings.TrimSpace(replies.Find(".chat-text").Text()); got != "a reply" {
		t.Errorf("nested reply = %q", got)
	}
	if got := strings.TrimSpace(replies.Find(".chat-author").Text()); got != "release-bot" {
		t.Errorf("reply author = %q", got)
	}
	// Avatars fall back to initials, and a bot is labeled as one.
	if got := strings.TrimSpace(root.Find(".chat-avatar-initials").First().Text()); got != "DB" {
		t.Errorf("avatar initials = %q, want the author's", got)
	}
	if root.Find(".chat-bot-tag").Length() == 0 {
		t.Error("a message posted by a bot should be labeled")
	}
	// Every message carries a timestamp with the full instant in its tooltip.
	if title, ok := root.Find(".chat-time").First().Attr("title"); !ok || title == "" {
		t.Error("a message needs a timestamp")
	}
}

// A reply whose parent was never captured must still be shown, and the view
// must say why the parent is missing rather than silently dropping it.
func TestUIOrphanedThread(t *testing.T) {
	in := start(t)
	seed(t, in)

	doc := uiDoc(t, in, in.UI("/chat/channels/"+chat.ChannelKey("C-general")))
	orphan := doc.Find(".chat-thread-orphan")
	if orphan.Length() != 1 {
		t.Fatalf("got %d orphaned threads, want 1", orphan.Length())
	}
	missing := orphan.Find(".chat-missing-parent").Text()
	if !strings.Contains(missing, "0.5") {
		t.Errorf("the orphan notice should name the missing parent: %q", missing)
	}
	if !strings.Contains(missing, "evicted") {
		t.Errorf("the orphan notice should explain itself: %q", missing)
	}
	if got := strings.TrimSpace(orphan.Find(".chat-replies .chat-text").Text()); got != "an orphan" {
		t.Errorf("the orphaned reply itself = %q, want it rendered anyway", got)
	}
}

func TestUIAvatarImage(t *testing.T) {
	in := start(t)
	m := msg("C1", "deploy-bot", "hello", "1.1")
	m.Author.IconURL = "https://example.com/avatar.png"
	injectAt(t, in, at(0), m)

	doc := uiDoc(t, in, in.UI("/chat/"))
	src, ok := doc.Find(".chat-message .chat-avatar-img").Attr("src")
	if !ok || src != "https://example.com/avatar.png" {
		t.Errorf("avatar src = %q, ok=%v", src, ok)
	}
}

// An icon URL is written by the application under test, so a javascript: URL
// must not survive into the page.
func TestUIAvatarURLIsSanitized(t *testing.T) {
	in := start(t)
	m := msg("C1", "deploy-bot", "hello", "1.1")
	m.Author.IconURL = "javascript:alert(1)"
	injectAt(t, in, at(0), m)

	doc := uiDoc(t, in, in.UI("/chat/"))
	src, _ := doc.Find(".chat-message .chat-avatar-img").Attr("src")
	if strings.Contains(strings.ToLower(src), "javascript:") {
		t.Errorf("avatar src = %q, want html/template to have filtered it", src)
	}
}

// Structured content lands as the plain-text fallback plus a collapsible JSON
// inspector - deliberately, so message capture never waits on a card renderer.
func TestUIStructuredContentFallsBackToTextAndJSON(t *testing.T) {
	in := start(t)
	m := msg("C1", "deploy-bot", "", "1.1")
	m.Contents = []chat.Content{
		{Format: chat.FormatSlackBlocks, Data: json.RawMessage(slackBlocks)},
		{Format: chat.FormatTeamsAdaptiveCard, Data: json.RawMessage(adaptiveCard)},
	}
	injectAt(t, in, at(0), m)

	doc := uiDoc(t, in, in.UI("/chat/"))

	// The text fallback is what makes the message readable at all.
	if got := doc.Find(".chat-message .chat-text").Text(); !strings.Contains(got, "Deploy finished") {
		t.Errorf("message text = %q, want the derived fallback", got)
	}
	cards := doc.Find(".chat-card")
	if cards.Length() != 2 {
		t.Fatalf("got %d cards, want one per structured payload", cards.Length())
	}
	formats := []string{}
	cards.Each(func(_ int, s *goquery.Selection) {
		f, _ := s.Attr("data-format")
		formats = append(formats, f)
	})
	if formats[0] != string(chat.FormatSlackBlocks) || formats[1] != string(chat.FormatTeamsAdaptiveCard) {
		t.Errorf("card formats = %v", formats)
	}
	// Each one is a collapsible inspector over the verbatim payload.
	if cards.Find("details.chat-card-json .json-inspector").Length() != 2 {
		t.Error("every card needs a collapsible JSON inspector")
	}
	if !strings.Contains(cards.Eq(0).Find(".json-tree").Text(), "mrkdwn") {
		t.Error("the inspector should show the payload as it actually arrived")
	}
	// And a badge naming the schema, which is what a renderer dispatches on.
	if got := doc.Find(".chat-message-meta .badge").Text(); !strings.Contains(got, "Block Kit") || !strings.Contains(got, "Adaptive Card") {
		t.Errorf("badges = %q, want the schemas named", got)
	}
}

func TestUIUnknownFormatIsStillShown(t *testing.T) {
	in := start(t)
	m := msg("C1", "deploy-bot", "something new", "1.1")
	m.Contents = []chat.Content{{Format: "vendor.future", Data: json.RawMessage(`{"hello":"world"}`)}}
	injectAt(t, in, at(0), m)

	doc := uiDoc(t, in, in.UI("/chat/"))
	card := doc.Find(`.chat-card[data-format="vendor.future"]`)
	if card.Length() != 1 {
		t.Fatal("a schema nobody has a renderer for must still be shown as JSON")
	}
	if !strings.Contains(card.Find(".chat-card-summary").Text(), "unrecognized") {
		t.Errorf("summary = %q, want it flagged as unrecognized", card.Find(".chat-card-summary").Text())
	}
}

// The seam a card renderer slots into: install one and its HTML is used in
// place of the fallback, with the inspector kept alongside.
func TestUIRichRendererSeam(t *testing.T) {
	var seen []string
	p := chat.New(fakeProvider{}).WithRichRenderer(func(format string, data json.RawMessage) (template.HTML, bool) {
		seen = append(seen, format)
		if format != string(chat.FormatSlackBlocks) {
			return "", false
		}
		var blocks []map[string]any
		if err := json.Unmarshal(data, &blocks); err != nil {
			return "", false
		}
		return template.HTML(`<div class="rendered-card">` + template.HTMLEscapeString(blocks[0]["type"].(string)) + `</div>`), true
	})
	in := testutil.Start(t, nil, p)

	m := msg("C1", "deploy-bot", "text fallback", "1.1")
	m.Contents = []chat.Content{
		{Format: chat.FormatSlackBlocks, Data: json.RawMessage(slackBlocks)},
		{Format: chat.FormatTeamsMessageCard, Data: json.RawMessage(messageCard)},
	}
	injectAt(t, in, at(0), m)

	doc := uiDoc(t, in, in.UI("/chat/"))
	if got := doc.Find(".chat-card-rich .rendered-card").Text(); got != "header" {
		t.Errorf("rendered card = %q, want the renderer's HTML", got)
	}
	if doc.Find(".chat-card-rich").Length() != 1 {
		t.Errorf("only the format the renderer handled should be rendered; got %d", doc.Find(".chat-card-rich").Length())
	}
	if doc.Find("details.chat-card-json").Length() != 2 {
		t.Error("the JSON inspector stays available even for a rendered card")
	}
	if len(seen) != 2 {
		t.Errorf("the renderer saw %v, want both formats offered to it", seen)
	}
}

// Message text and author names are whatever the application under test posted.
// They are escaped, always.
func TestUIEscapesUntrustedText(t *testing.T) {
	const nasty = `<script>alert(1)</script><img src=x onerror="alert(2)"> & "quoted" 'single'`
	in := start(t)
	m := &chat.Message{
		Channel: chat.ChannelRef{ID: "C1", Name: nasty},
		Author:  chat.Author{Name: nasty, Bot: true},
		Text:    nasty,
		TS:      "1.1",
	}
	injectAt(t, in, at(0), m)

	for _, url := range []string{in.UI("/chat/"), in.UI("/chat/channels/" + chat.ChannelKey("C1"))} {
		doc := uiDoc(t, in, url)
		assertNoInjectedMarkup(t, doc)

		body := doc.Find(".chat-message .chat-text")
		if body.Find("script, img").Length() != 0 {
			t.Errorf("%s: message text produced live markup", url)
		}
		if !strings.Contains(body.Text(), "<script>alert(1)</script>") {
			t.Errorf("%s: message text = %q, want the literal characters", url, body.Text())
		}
		author := doc.Find(".chat-message .chat-author")
		if author.Find("script, img").Length() != 0 {
			t.Errorf("%s: author name produced live markup", url)
		}
		if !strings.Contains(author.Text(), `<img src=x onerror="alert(2)">`) {
			t.Errorf("%s: author = %q, want the literal characters", url, author.Text())
		}
		// The channel name reaches the sidebar and the stream header too.
		if doc.Find(".chat-channel-name script, .chat-stream-head script").Length() != 0 {
			t.Errorf("%s: a channel name produced live markup", url)
		}
	}
}

// The same, through the fragment routes htmx uses, since those bypass the page.
func TestUIEscapesUntrustedTextInFragments(t *testing.T) {
	const nasty = `<script>alert(1)</script>`
	in := start(t)
	injectAt(t, in, at(0), &chat.Message{
		Channel: chat.ChannelRef{ID: "C1", Name: nasty},
		Author:  chat.Author{Name: nasty},
		Text:    nasty,
		TS:      "1.1",
	})
	for _, url := range []string{in.UI("/chat/list"), in.UI("/chat/channels/" + chat.ChannelKey("C1"))} {
		status, doc := uiFragment(t, in, http.MethodGet, url)
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d", url, status)
		}
		assertNoInjectedMarkup(t, doc)
	}
}

// A fallback derived from a card is untrusted too: it is text lifted straight
// out of the payload.
func TestUIEscapesTextDerivedFromStructuredContent(t *testing.T) {
	in := start(t)
	m := msg("C1", "deploy-bot", "", "1.1")
	m.Contents = []chat.Content{{
		Format: chat.FormatSlackBlocks,
		Data:   json.RawMessage(`[{"type":"section","text":{"type":"mrkdwn","text":"<script>alert(1)</script>"}}]`),
	}}
	injectAt(t, in, at(0), m)

	doc := uiDoc(t, in, in.UI("/chat/"))
	assertNoInjectedMarkup(t, doc)
	if !strings.Contains(doc.Find(".chat-message .chat-text").Text(), "<script>alert(1)</script>") {
		t.Error("the derived fallback should show the literal characters")
	}
}

// assertNoInjectedMarkup fails when markup a message carried survived into the
// page as live elements.
func assertNoInjectedMarkup(t *testing.T, doc *goquery.Document) {
	t.Helper()
	doc.Find("script").Each(func(_ int, s *goquery.Selection) {
		if strings.Contains(s.Text(), "alert(") {
			t.Error("an injected script tag survived into the page")
		}
	})
	doc.Find("img").Each(func(_ int, s *goquery.Selection) {
		if _, ok := s.Attr("onerror"); ok {
			t.Error("an injected event handler survived into the page")
		}
	})
}

func TestUILongMessageCollapses(t *testing.T) {
	in := start(t)
	long := strings.Repeat("a", chat.LongTextThreshold+50) + `<script>alert(1)</script>`
	injectAt(t, in, at(0), msg("C1", "deploy-bot", long, "1.1"))

	doc := uiDoc(t, in, in.UI("/chat/"))
	if doc.Find(".chat-text-details").Length() == 0 {
		t.Fatal("a very long message should collapse behind a toggle")
	}
	assertNoInjectedMarkup(t, doc)
	if !strings.Contains(doc.Find(".chat-text-toggle").Text(), "characters") {
		t.Errorf("toggle = %q", doc.Find(".chat-text-toggle").Text())
	}
}

func TestUIFragmentsAndDeepLinks(t *testing.T) {
	in := start(t)
	seed(t, in)
	key := chat.ChannelKey("C-general")

	status, doc := uiFragment(t, in, http.MethodGet, in.UI("/chat/list"))
	if status != http.StatusOK {
		t.Fatalf("GET /chat/list = %d", status)
	}
	if doc.Find("#chat-channel-list").Length() == 0 || doc.Find("html body header").Length() != 0 {
		t.Error("the list route should return a bare sidebar fragment")
	}

	status, doc = uiFragment(t, in, http.MethodGet, in.UI("/chat/channels/"+key))
	if status != http.StatusOK {
		t.Fatalf("GET /chat/channels/%s = %d", key, status)
	}
	if doc.Find("#chat-stream").Length() == 0 {
		t.Error("the channel route should return the stream fragment")
	}

	status, _ = uiFragment(t, in, http.MethodGet, in.UI("/chat/channels/does-not-exist"))
	if status != http.StatusNotFound {
		t.Errorf("an unknown channel returned %d, want 404", status)
	}

	// A deep link renders the whole tab with the channel open, so a channel URL
	// can be pasted into a bug report.
	page := uiDoc(t, in, in.UI("/chat/channels/"+key))
	if page.Find("nav.tabs").Length() == 0 {
		t.Error("a deep link should render the full page")
	}
	if !strings.Contains(page.Find(".chat-stream-head h2").Text(), "C-general") {
		t.Errorf("deep link opened %q", page.Find(".chat-stream-head h2").Text())
	}
}

func TestUILiveUpdateWiring(t *testing.T) {
	in := start(t)
	seed(t, in)
	doc := uiDoc(t, in, in.UI("/chat/"))

	var triggers []string
	doc.Find("[hx-trigger]").Each(func(_ int, s *goquery.Selection) {
		v, _ := s.Attr("hx-trigger")
		triggers = append(triggers, v)
	})
	joined := strings.Join(triggers, " ")
	if !strings.Contains(joined, "sse:"+chat.TypeMessage) {
		t.Errorf("hx-trigger attributes = %v, want the tab to refresh on sse:%s", triggers, chat.TypeMessage)
	}
	// Both panes update, not just one.
	if strings.Count(joined, "sse:"+chat.TypeMessage) < 2 {
		t.Errorf("both the sidebar and the stream should refresh live; got %v", triggers)
	}
}

func TestUISearch(t *testing.T) {
	in := start(t)
	seed(t, in)
	doc := uiDoc(t, in, in.UI("/chat/?search=ops+alert"))
	if got := doc.Find(".chat-channel").Length(); got != 1 {
		t.Fatalf("search matched %d channels, want 1", got)
	}
	if got := doc.Find(".chat-channel-name").Text(); !strings.Contains(got, "C-ops") {
		t.Errorf("search result = %q", got)
	}
}

func TestUIClear(t *testing.T) {
	in := start(t)
	seed(t, in)

	status, doc := uiFragment(t, in, http.MethodDelete, in.UI("/chat/events"))
	if status != http.StatusOK {
		t.Fatalf("DELETE /chat/events = %d", status)
	}
	if doc.Find(".empty-state").Length() == 0 {
		t.Error("clearing should hand back the empty tab")
	}
	if got := listMessages(t, in, ""); len(got) != 0 {
		t.Errorf("%d messages survived the clear", len(got))
	}
}
