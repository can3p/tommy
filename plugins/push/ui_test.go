package push_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/can3p/tommy/core/testutil"
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

// texts collects the trimmed text of every match, which is what an assertion
// about a rendered page should be made against - not a substring search over
// the HTML, which cannot tell markup from text.
func texts(doc *goquery.Document, selector string) []string {
	var out []string
	doc.Find(selector).Each(func(_ int, s *goquery.Selection) {
		out = append(out, strings.TrimSpace(s.Text()))
	})
	return out
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func containsSub(hay []string, needle string) bool {
	for _, s := range hay {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
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
	if !strings.Contains(strings.Join(hrefs, " "), "Push=/ui/push/") {
		t.Errorf("tab bar = %v, want a Push tab pointing at /ui/push/", hrefs)
	}
}

// An empty tab is exactly when somebody needs to know how to fill it, so the
// how-to-test panel starts open and carries a runnable snippet.
func TestUIEmptyState(t *testing.T) {
	in := start(t)
	doc := uiDoc(t, in, in.UI("/push/"))

	panel := doc.Find("details.how-to-test")
	if panel.Length() != 1 {
		t.Fatalf("found %d how-to-test panels, want 1", panel.Length())
	}
	if _, open := panel.Attr("open"); !open {
		t.Error("the how-to-test panel is closed on an empty tab")
	}
	if doc.Find("code").FilterFunction(func(_ int, s *goquery.Selection) bool {
		return strings.Contains(s.Text(), "/fake-push/apns/")
	}).Length() == 0 {
		t.Error("the empty tab does not carry a runnable snippet")
	}
}

// The lock screen is the reason this tab exists rather than the generic event
// view: an alert renders as a card a phone would show.
func TestUILockScreenCard(t *testing.T) {
	in := start(t)
	injectAPNs(t, in, "00fc13adff78", apnsAlertHeaders, fixture(t, "apns_alert.json"))

	doc := uiDoc(t, in, in.UI("/push/"))

	if got := texts(doc, ".push-cards .push-card-title"); !contains(got, "Game Request") {
		t.Errorf("card titles = %v", got)
	}
	if got := texts(doc, ".push-cards .push-card-subtitle"); !contains(got, "Five Card Draw") {
		t.Errorf("card subtitles = %v", got)
	}
	if got := texts(doc, ".push-cards .push-card-body"); !contains(got, "Bob wants to play poker") {
		t.Errorf("card bodies = %v", got)
	}
	// The app line is what a phone shows above a notification.
	if got := texts(doc, ".push-cards .push-card-appname"); !contains(got, "com.example.MyApp") {
		t.Errorf("app names = %v", got)
	}
	// A displaying push must not be styled as a silent one.
	if doc.Find(".push-cards .push-kind-silent").Length() != 0 {
		t.Error("an alert push was styled as silent")
	}
	if doc.Find(".push-cards .push-kind-notification").Length() == 0 {
		t.Error("no notification-styled card was rendered")
	}
}

// A silent push must be visibly distinguishable from one that displays. This is
// the single assertion the tab is for.
func TestUISilentPushIsVisiblyDifferent(t *testing.T) {
	in := start(t)
	injectAPNs(t, in, "00fc13adff78", apnsAlertHeaders, fixture(t, "apns_alert.json"))
	injectAPNs(t, in, "00fc13adff78",
		map[string]string{"apns-push-type": "background"}, fixture(t, "apns_silent.json"))
	injectFCM(t, in, "my-project", fixture(t, "fcm_topic_data.json"))

	doc := uiDoc(t, in, in.UI("/push/"))

	cards := doc.Find(".push-cards .push-card")
	if cards.Length() != 3 {
		t.Fatalf("rendered %d cards, want 3", cards.Length())
	}
	if got := doc.Find(".push-cards .push-kind-silent").Length(); got != 2 {
		t.Errorf("%d cards styled silent, want 2", got)
	}
	// A silent card carries the sentence saying nothing is displayed, and no
	// title or body element at all - not an empty one.
	silent := texts(doc, ".push-cards .push-kind-silent .push-card-silent")
	if len(silent) != 2 {
		t.Fatalf("silent explanations = %v", silent)
	}
	for _, s := range silent {
		if !strings.Contains(s, "displays nothing") {
			t.Errorf("silent card says %q; it must say the device displays nothing", s)
		}
	}
	if doc.Find(".push-cards .push-kind-silent .push-card-title").Length() != 0 {
		t.Error("a silent card must not render a notification title")
	}
	// The data keys are the only thing there is to see on a data push.
	if got := texts(doc, ".push-cards .push-kind-silent .push-card-note"); !containsSub(got, "kind, region") {
		t.Errorf("silent card notes = %v, want the data keys named", got)
	}
	// And the count line says how many displayed nothing.
	if got := texts(doc, "#push-messages .count"); !containsSub(got, "2 displaying nothing") {
		t.Errorf("count line = %v", got)
	}
}

// A push carrying neither an alert nor data is almost always a mistake, and the
// tab says so rather than drawing a blank card.
func TestUIEmptyPushIsCalledOut(t *testing.T) {
	in := start(t)
	injectAPNs(t, in, "00fc13adff78", nil, fixture(t, "apns_empty.json"))

	doc := uiDoc(t, in, in.UI("/push/"))
	if doc.Find(".push-cards .push-kind-empty").Length() != 1 {
		t.Fatal("an empty push was not styled differently from a silent one")
	}
	if got := texts(doc, ".push-card-silent"); !containsSub(got, "almost always a mistake") {
		t.Errorf("empty card says %v", got)
	}
	if got := texts(doc, ".push-badges .badge"); !contains(got, "empty") {
		t.Errorf("badges = %v, want one naming the kind", got)
	}
}

// A push that only bumps the badge or plays a sound does interact with the
// user, but there is no banner to draw and the card says which.
func TestUIBadgeOnlyPush(t *testing.T) {
	in := start(t)
	injectAPNs(t, in, "00fc13adff78", nil, fixture(t, "apns_badge_only.json"))

	doc := uiDoc(t, in, in.UI("/push/"))
	if doc.Find(".push-cards .push-kind-notification").Length() != 1 {
		t.Error("a badge-and-sound push still displays and must be styled as a notification")
	}
	if got := texts(doc, ".push-card-note"); !containsSub(got, "clears the badge") {
		t.Errorf("card notes = %v", got)
	}
}

// A notification whose text is a resource key looks empty on a real device too,
// so the card says the text is a key.
func TestUILocalizedPush(t *testing.T) {
	in := start(t)
	injectAPNs(t, in, "00fc13adff78", nil, fixture(t, "apns_localized.json"))

	doc := uiDoc(t, in, in.UI("/push/"))
	if got := texts(doc, ".push-card-note"); !containsSub(got, "GAME_PLAY_REQUEST_FORMAT") {
		t.Errorf("card notes = %v, want the localization keys named", got)
	}
	if got := texts(doc, ".push-detail th"); !contains(got, "Body key") {
		t.Errorf("detail keys = %v, want the localization rows", got)
	}
}

// The detail pane says where the push went, and honestly: which wire location
// the address came out of, and whether it fans out.
func TestUIDetailRouting(t *testing.T) {
	in := start(t)
	injectAPNs(t, in, "00fc13adff785122b4ad28809a3420982341241421348097878e577c991de8f0",
		apnsAlertHeaders, fixture(t, "apns_alert.json"))

	doc := uiDoc(t, in, in.UI("/push/"))
	rows := map[string]string{}
	doc.Find(".push-detail tr").Each(func(_ int, s *goquery.Selection) {
		rows[strings.TrimSpace(s.Find("th").Text())] = strings.TrimSpace(s.Find("td").Text())
	})
	if rows["Device"] == "" {
		t.Errorf("no Device row; rows were %v", rows)
	}
	if !strings.Contains(rows["Read from"], "/3/device/") {
		t.Errorf("Read from = %q, want the APNs request path named", rows["Read from"])
	}
	if rows["App"] != "com.example.MyApp" {
		t.Errorf("App = %q", rows["App"])
	}
	if !strings.Contains(rows["Priority"], "high") || !strings.Contains(rows["Priority"], "10") {
		t.Errorf("Priority = %q, want both the level and the raw value", rows["Priority"])
	}
	if rows["Collapse key"] != "poker" {
		t.Errorf("Collapse key = %q", rows["Collapse key"])
	}
	// The verbatim payload is in an inspector, and the request itself below it.
	if doc.Find(".push-detail .json-inspector").Length() == 0 {
		t.Error("the detail pane carries no JSON inspector")
	}
	if doc.Find(".push-detail .raw-viewer").Length() == 0 {
		t.Error("the detail pane does not show the request as it arrived")
	}
}

func TestUIDetailFanout(t *testing.T) {
	in := start(t)
	injectFCM(t, in, "my-project", fixture(t, "fcm_topic_data.json"))

	doc := uiDoc(t, in, in.UI("/push/"))
	rows := map[string]string{}
	doc.Find(".push-detail tr").Each(func(_ int, s *goquery.Selection) {
		rows[strings.TrimSpace(s.Find("th").Text())] = strings.TrimSpace(s.Find("td").Text())
	})
	if rows["Topic"] != "weather" {
		t.Errorf("Topic = %q; rows were %v", rows["Topic"], rows)
	}
	if !strings.Contains(rows["Fan-out"], "every subscriber") {
		t.Errorf("Fan-out = %q", rows["Fan-out"])
	}
	if !strings.Contains(rows["Read from"], `"topic"`) {
		t.Errorf("Read from = %q, want the body field named", rows["Read from"])
	}
	// ttl "0s" and apns-expiration 0 mean the same thing and read the same way.
	if !strings.Contains(rows["Expiry"], "deliver immediately or drop") {
		t.Errorf("Expiry = %q", rows["Expiry"])
	}
}

func TestUIListAndSelection(t *testing.T) {
	in := start(t)
	first := injectAPNs(t, in, "00fc13adff78", apnsAlertHeaders, fixture(t, "apns_alert.json"))
	injectFCM(t, in, "my-project", fixture(t, "fcm_condition.json"))

	doc := uiDoc(t, in, in.UI("/push/"))
	// With nothing chosen the newest push is open, so the pane is never blank
	// next to a full lock screen.
	if got := texts(doc, ".push-detail-head h2"); len(got) != 1 || got[0] != "GOOG up 5%" {
		t.Errorf("detail heading = %v, want the newest push", got)
	}

	// A deep link opens that push as a whole page.
	deep := uiDoc(t, in, in.UI("/push/messages/"+string(first.ID)))
	if got := texts(deep, ".push-detail-head h2"); len(got) != 1 || got[0] != "Game Request" {
		t.Errorf("deep link heading = %v", got)
	}
	if deep.Find("nav.tabs").Length() == 0 {
		t.Error("a deep link returned a bare fragment rather than the whole page")
	}

	// htmx gets a fragment.
	status, frag := uiFragment(t, in, http.MethodGet, in.UI("/push/messages/"+string(first.ID)))
	if status != http.StatusOK {
		t.Fatalf("fragment status = %d", status)
	}
	if frag.Find("nav.tabs").Length() != 0 {
		t.Error("an htmx request got the whole page rather than a fragment")
	}

	status, _ = uiFragment(t, in, http.MethodGet, in.UI("/push/messages/nope"))
	if status != http.StatusNotFound {
		t.Errorf("unknown message status = %d, want 404", status)
	}
}

func TestUISearchAndClear(t *testing.T) {
	in := start(t)
	injectAPNs(t, in, "00fc13adff78", apnsAlertHeaders, fixture(t, "apns_alert.json"))
	injectFCM(t, in, "my-project", fixture(t, "fcm_topic_data.json"))

	// A silent push has no title to search for, which is why its data keys go
	// into the event summary.
	_, frag := uiFragment(t, in, http.MethodGet, in.UI("/push/list?search=region"))
	if got := texts(frag, ".push-card-note"); !containsSub(got, "kind, region") {
		t.Errorf("search results = %v, want the data push found by a data key", got)
	}
	_, frag = uiFragment(t, in, http.MethodGet, in.UI("/push/list?search=poker"))
	if got := texts(frag, ".push-card-title"); len(got) != 1 || got[0] != "Game Request" {
		t.Errorf("search results = %v, want just the alert", got)
	}

	status, cleared := uiFragment(t, in, http.MethodDelete, in.UI("/push/events"))
	if status != http.StatusOK {
		t.Fatalf("clear status = %d", status)
	}
	if got := texts(cleared, ".push-card-title"); len(got) != 0 {
		t.Errorf("after clearing, the lock screen still shows %v", got)
	}
	if cleared.Find("details.how-to-test[open]").Length() != 1 {
		t.Error("clearing left the tab without an open how-to-test panel")
	}
}

// A push title, body, category and data block are written by whatever backend
// is under test. None of it may reach the page as markup.
//
// The assertions are against the parsed document, not against the HTML source:
// grepping the response for "<script" proves nothing, since the escaped text
// contains no such thing either way. What matters is that the browser's own
// parser found no script element, no iframe, no image and no event handler
// attribute that the payload put there.
func TestUIHostilePushRendersInert(t *testing.T) {
	in := start(t)
	ev := injectAPNs(t, in, `"><script>alert(1)</script>`, map[string]string{
		"apns-topic":       `<script>alert(1)</script>`,
		"apns-collapse-id": `"><img src=x onerror="alert(1)">`,
	}, fixture(t, "apns_hostile.json"))

	urls := []string{
		in.UI("/push/"),
		in.UI("/push/list"),
		in.UI("/push/messages/" + string(ev.ID)),
	}
	for _, url := range urls {
		doc := uiDoc(t, in, url)

		doc.Find("script").Each(func(_ int, s *goquery.Selection) {
			if strings.Contains(s.Text(), "alert(1)") {
				t.Errorf("%s: an injected script tag survived into the page", url)
			}
		})
		if n := doc.Find("iframe").Length(); n != 0 {
			t.Errorf("%s: %d iframes in the page; the payload put them there", url, n)
		}
		doc.Find("*").Each(func(_ int, s *goquery.Selection) {
			for _, attr := range []string{"onerror", "onload", "onclick", "onmouseover"} {
				if _, ok := s.Attr(attr); ok {
					t.Errorf("%s: an injected %s handler survived into the page", url, attr)
				}
			}
		})
		doc.Find("img").Each(func(_ int, s *goquery.Selection) {
			if src, _ := s.Attr("src"); src == "x" {
				t.Errorf("%s: an injected img tag survived into the page", url)
			}
		})
		// Nothing captured may become a link target either: a javascript: URL
		// in a payload must stay text.
		doc.Find("a[href], img[src], iframe[src]").Each(func(_ int, s *goquery.Selection) {
			for _, attr := range []string{"href", "src"} {
				v, ok := s.Attr(attr)
				if ok && strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "javascript:") {
					t.Errorf("%s: a javascript: URL from the payload reached an %s", url, attr)
				}
			}
		})
	}

	// And the text is still shown - escaped, not dropped, because seeing what
	// was actually sent is the point.
	doc := uiDoc(t, in, in.UI("/push/"))
	if got := texts(doc, ".push-card-title"); !contains(got, "<script>alert(1)</script>") {
		t.Errorf("the hostile title was not shown as text: %v", got)
	}
	if got := texts(doc, ".push-card-subtitle"); !contains(got, `<img src=x onerror="alert(1)">`) {
		t.Errorf("the hostile subtitle was not shown as text: %v", got)
	}
	if got := texts(doc, ".push-detail .json-leaf"); !containsSub(got, "javascript:alert(1)") {
		t.Errorf("the hostile data value was not shown in the inspector: %v", got)
	}
}

// An image URL is a third-party address the sender chose. Tommy never fetches
// it, and the page must not make a browser fetch it either.
func TestUIImageURLIsTextNotAnImage(t *testing.T) {
	in := start(t)
	injectFCM(t, in, "my-project", fixture(t, "fcm_notification.json"))

	doc := uiDoc(t, in, in.UI("/push/"))
	if got := texts(doc, ".push-card-note"); !containsSub(got, "https://example.test/breakfast.png") {
		t.Errorf("card notes = %v, want the image URL shown as text", got)
	}
	doc.Find("img, a").Each(func(_ int, s *goquery.Selection) {
		for _, attr := range []string{"src", "href"} {
			if v, ok := s.Attr(attr); ok && strings.Contains(v, "example.test") {
				t.Errorf("a captured image URL reached an %s: %q", attr, v)
			}
		}
	})
}
