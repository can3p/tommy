package sms_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/sms"
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
	if !strings.Contains(strings.Join(hrefs, " "), "SMS=/ui/sms/") {
		t.Errorf("tab bar = %v, want an SMS tab pointing at /ui/sms/", hrefs)
	}
}

func TestUIEmptyState(t *testing.T) {
	in := start(t)
	doc := uiDoc(t, in, in.UI("/sms/"))

	if doc.Find(".empty-state").Length() == 0 {
		t.Fatal("an empty SMS tab must show an empty state")
	}
	if !strings.Contains(doc.Find(".empty-state").Text(), "No SMS captured yet") {
		t.Errorf("empty state text = %q", doc.Find(".empty-state").Text())
	}
	// The fake provider is described inline, so an empty tab still says what
	// can put something in it.
	if !strings.Contains(doc.Text(), "/fake-sms/v1/messages") {
		t.Error("the empty state should list the enabled providers' endpoints")
	}
	// And it points at the overview for snippets rendered against live ports.
	if doc.Find(`.empty-state ~ p a[href="/ui/"], a[href="/ui/"]`).Length() == 0 {
		t.Error("the empty state should link to the overview tab for runnable snippets")
	}
}

func TestUIEmptyStateWithNoProviders(t *testing.T) {
	// The plugin as plugins/all wires it today: no providers until Wave 2.
	in := testutil.Start(t, nil, sms.New())
	doc := uiDoc(t, in, in.UI("/sms/"))
	if !strings.Contains(doc.Text(), "No SMS provider is enabled") {
		t.Errorf("a provider-less SMS tab should say so; got %q", strings.TrimSpace(doc.Find(".sms-tab").Text()))
	}
}

func TestUIConversationList(t *testing.T) {
	in := start(t)
	local, peerA, peerB := "+15005550006", "+15551234567", "+15559999999"
	inject(t, in, &sms.Message{From: local, To: peerA, Body: "first"})
	inject(t, in, &sms.Message{From: peerA, To: local, Body: "a reply", Direction: sms.Inbound})
	inject(t, in, &sms.Message{From: local, To: peerB, Body: "other thread"})

	doc := uiDoc(t, in, in.UI("/sms/"))

	convs := doc.Find(".sms-conv")
	if convs.Length() != 2 {
		t.Fatalf("got %d conversations in the list, want 2", convs.Length())
	}
	// Newest thread first.
	if got := strings.TrimSpace(convs.Eq(0).Find(".sms-conv-peer").Text()); got != peerB {
		t.Errorf("first conversation = %q, want %q", got, peerB)
	}
	if got := convs.Eq(1).Find(".sms-conv-preview").Text(); !strings.Contains(got, "a reply") {
		t.Errorf("preview = %q, want the newest message of the thread", got)
	}
	if !strings.Contains(doc.Find(".count").Text(), "3 messages") {
		t.Errorf("count line = %q, want a message total", doc.Find(".count").Text())
	}

	href, _ := convs.Eq(1).Find("a").Attr("href")
	wantKey := sms.ConversationKey(local, peerA)
	if href != "/ui/sms/conversations/"+wantKey {
		t.Errorf("conversation link = %q, want /ui/sms/conversations/%s", href, wantKey)
	}
}

func TestUIThreadBubbles(t *testing.T) {
	in := start(t)
	local, peer := "+15005550006", "+15551234567"
	inject(t, in, &sms.Message{From: local, To: peer, Body: strings.Repeat("a", 200)})
	inject(t, in, &sms.Message{From: peer, To: local, Body: "got it \U0001F600", Direction: sms.Inbound, Status: sms.StatusReceived})

	key := sms.ConversationKey(local, peer)
	doc := uiDoc(t, in, in.UI("/sms/conversations/"+key))

	bubbles := doc.Find(".sms-bubble")
	if bubbles.Length() != 2 {
		t.Fatalf("got %d bubbles, want 2", bubbles.Length())
	}
	// Oldest first, outbound on the right.
	if !bubbles.Eq(0).HasClass("sms-out") {
		t.Error("the first bubble should be outbound")
	}
	if !bubbles.Eq(1).HasClass("sms-in") {
		t.Error("the reply should be inbound")
	}

	tests := []struct {
		bubble int
		want   []string
	}{
		{0, []string{"2 segs", "GSM-7", "queued"}},
		{1, []string{"1 seg", "UCS-2", "received"}},
	}
	for _, tt := range tests {
		badges := bubbles.Eq(tt.bubble).Find(".badge").Text()
		for _, want := range tt.want {
			if !strings.Contains(badges, want) {
				t.Errorf("bubble %d badges = %q, want one saying %q", tt.bubble, badges, want)
			}
		}
	}

	// The encoding badge explains itself on hover, since that is the whole
	// point of showing it.
	title, _ := bubbles.Eq(1).Find(".badge").FilterFunction(func(_ int, s *goquery.Selection) bool {
		return s.Text() == "UCS-2"
	}).Attr("title")
	if !strings.Contains(title, "UCS-2") {
		t.Errorf("UCS-2 badge title = %q, want an explanation", title)
	}

	// Every message links to its raw request.
	if href, ok := bubbles.Eq(0).Find("a.sms-raw-link").Attr("href"); !ok || !strings.HasPrefix(href, "/ui/sms/events/") {
		t.Errorf("raw link = %q, want /ui/sms/events/<id>", href)
	}
}

// Message bodies are attacker-controlled: they must be escaped, never rendered.
func TestUIEscapesMessageBodies(t *testing.T) {
	in := start(t)
	local, peer := "+15005550006", "+15551234567"
	const nasty = `<script>alert(1)</script><img src=x onerror="alert(2)"> & "quoted" 'single'`
	ev := inject(t, in, &sms.Message{From: local, To: peer, Body: nasty})

	key := sms.ConversationKey(local, peer)

	t.Run("in a bubble", func(t *testing.T) {
		doc := uiDoc(t, in, in.UI("/sms/conversations/"+key))
		body := doc.Find(".sms-body")
		if body.Length() != 1 {
			t.Fatalf("got %d bodies, want 1", body.Length())
		}
		if got := body.Text(); got != nasty {
			t.Errorf("body text = %q, want the message verbatim", got)
		}
		if body.Find("script, img").Length() != 0 {
			t.Error("the message body was rendered as HTML")
		}
		assertNoInjectedScript(t, doc)
	})

	t.Run("in the conversation preview", func(t *testing.T) {
		doc := uiDoc(t, in, in.UI("/sms/"))
		preview := doc.Find(".sms-conv-preview")
		if preview.Length() != 1 {
			t.Fatalf("got %d previews, want 1", preview.Length())
		}
		if preview.Find("script, img").Length() != 0 {
			t.Error("the conversation preview rendered the body as HTML")
		}
		if !strings.Contains(preview.Text(), "<script>alert(1)</script>") {
			t.Errorf("preview text = %q, want the escaped body", preview.Text())
		}
		assertNoInjectedScript(t, doc)
	})

	t.Run("in the generic event view it falls back to", func(t *testing.T) {
		doc := uiDoc(t, in, in.UI("/sms/events/"+string(ev.ID)))
		assertNoInjectedScript(t, doc)
	})
}

// assertNoInjectedScript fails when a script the message body carried survived
// into the page as markup rather than as text.
func assertNoInjectedScript(t *testing.T, doc *goquery.Document) {
	t.Helper()
	doc.Find("script").Each(func(_ int, s *goquery.Selection) {
		if strings.Contains(s.Text(), "alert(1)") {
			t.Error("an injected script tag survived into the page")
		}
	})
	doc.Find("img").Each(func(_ int, s *goquery.Selection) {
		if _, ok := s.Attr("onerror"); ok {
			t.Error("an injected img tag survived into the page")
		}
	})
}

func TestUIMedia(t *testing.T) {
	in := start(t)
	ref := putBlob(t, in, "image/png", "cat.png", onePixelPNG)
	local, peer := "+15005550006", "+15551234567"
	ev := inject(t, in, &sms.Message{
		From: local, To: peer, Body: "look",
		Media: []sms.Media{
			{ContentType: "image/png", Filename: "cat.png", Blob: &ref},
			{ContentType: "application/pdf", Filename: "receipt.pdf", URL: "https://example.com/receipt.pdf"},
		},
	})

	doc := uiDoc(t, in, in.UI("/sms/conversations/"+sms.ConversationKey(local, peer)))

	src, ok := doc.Find("img.sms-media-thumb").Attr("src")
	if !ok {
		t.Fatal("a stored image should render as a thumbnail")
	}
	if want := "/api/v1/sms/messages/" + string(ev.ID) + "/media/0"; src != want {
		t.Errorf("thumbnail src = %q, want %q", src, want)
	}

	link := doc.Find("a.sms-media-link")
	if link.Length() != 1 {
		t.Fatalf("got %d non-image media links, want 1", link.Length())
	}
	if href, _ := link.Attr("href"); href != "https://example.com/receipt.pdf" {
		t.Errorf("media link href = %q, want the provider URL", href)
	}
	if !strings.Contains(link.Text(), "remote URL") {
		t.Errorf("a url-only attachment should say the bytes are not here; got %q", link.Text())
	}
	// And the MMS badge is on the bubble.
	if !strings.Contains(doc.Find(".sms-bubble .badge").Text(), "MMS") {
		t.Error("a message with media should carry an MMS badge")
	}
}

func TestUILiveUpdatesOverSSE(t *testing.T) {
	in := start(t)
	inject(t, in, &sms.Message{From: "+15005550006", To: "+15551234567", Body: "hi"})
	doc := uiDoc(t, in, in.UI("/sms/"))

	var triggers []string
	doc.Find("[hx-trigger]").Each(func(_ int, s *goquery.Selection) {
		v, _ := s.Attr("hx-trigger")
		triggers = append(triggers, v)
	})
	joined := strings.Join(triggers, " | ")
	if !strings.Contains(joined, "sse:sms.message") {
		t.Errorf("hx-trigger attributes = %q, want one listening on sse:sms.message", joined)
	}
	// The shell holds the connection; the tab only listens.
	if src, _ := doc.Find("body").Attr("sse-connect"); src == "" {
		t.Error("the shell should hold the single SSE connection")
	}
}

func TestUIFragmentsAndSearch(t *testing.T) {
	in := start(t)
	local := "+15005550006"
	inject(t, in, &sms.Message{From: local, To: "+15551234567", Body: "about cats"})
	inject(t, in, &sms.Message{From: local, To: "+15559999999", Body: "about dogs"})

	t.Run("the list fragment is bare html", func(t *testing.T) {
		status, doc := uiFragment(t, in, http.MethodGet, in.UI("/sms/list"))
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		if doc.Find("nav.tabs").Length() != 0 {
			t.Error("a fragment must not carry the shell")
		}
		if doc.Find(".sms-conv").Length() != 2 {
			t.Errorf("got %d conversations in the fragment, want 2", doc.Find(".sms-conv").Length())
		}
	})

	t.Run("search narrows the list", func(t *testing.T) {
		_, doc := uiFragment(t, in, http.MethodGet, in.UI("/sms/list?search=dogs"))
		if doc.Find(".sms-conv").Length() != 1 {
			t.Fatalf("got %d conversations for ?search=dogs, want 1", doc.Find(".sms-conv").Length())
		}
		if !strings.Contains(doc.Find(".sms-conv-preview").Text(), "about dogs") {
			t.Errorf("preview = %q", doc.Find(".sms-conv-preview").Text())
		}
	})

	t.Run("a search that matches nothing says so", func(t *testing.T) {
		_, doc := uiFragment(t, in, http.MethodGet, in.UI("/sms/list?search=zzz"))
		if !strings.Contains(doc.Text(), "No conversation matches") {
			t.Errorf("fragment = %q", strings.TrimSpace(doc.Text()))
		}
	})

	t.Run("the thread fragment is bare html", func(t *testing.T) {
		key := sms.ConversationKey(local, "+15551234567")
		status, doc := uiFragment(t, in, http.MethodGet, in.UI("/sms/conversations/"+key))
		if status != http.StatusOK {
			t.Fatalf("status = %d", status)
		}
		if doc.Find("nav.tabs").Length() != 0 {
			t.Error("a fragment must not carry the shell")
		}
		if doc.Find(".sms-bubble").Length() != 1 {
			t.Errorf("got %d bubbles, want 1", doc.Find(".sms-bubble").Length())
		}
	})

	t.Run("an unknown conversation is a 404", func(t *testing.T) {
		status, _ := uiFragment(t, in, http.MethodGet, in.UI("/sms/conversations/nope"))
		if status != http.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
	})
}

func TestUIClear(t *testing.T) {
	in := start(t)
	inject(t, in, &sms.Message{From: "+15005550006", To: "+15551234567", Body: "hi"})

	status, doc := uiFragment(t, in, http.MethodDelete, in.UI("/sms/events"))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if doc.Find(".sms-conv").Length() != 0 {
		t.Error("the tab returned by a clear still lists conversations")
	}
	if doc.Find(".empty-state").Length() == 0 {
		t.Error("a cleared tab should fall back to the empty state")
	}
	if got := listMessages(t, in, ""); len(got) != 0 {
		t.Errorf("%d messages survived the clear", len(got))
	}
}

// Routes the tab does not claim still fall back to the core's generic view, so
// an event id from the API opens a raw inspector inside the SMS tab.
func TestUIGenericEventFallback(t *testing.T) {
	in := start(t)
	ev := inject(t, in, &sms.Message{From: "+15005550006", To: "+15551234567", Body: "hi"})

	doc := uiDoc(t, in, in.UI("/sms/events/"+string(ev.ID)))
	if doc.Find(".event-detail").Length() == 0 {
		t.Fatal("the unclaimed /events/{id} route should render the generic detail view")
	}
	if !strings.Contains(doc.Text(), string(ev.ID)) {
		t.Error("the generic detail view should show the event id")
	}
}
