package mail_test

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/mail"
	"github.com/can3p/tommy/plugins/mail/mailtest"
)

// page fetches a full page the way a browser would.
func page(t *testing.T, in *testutil.Instance, url string) *goquery.Document {
	t.Helper()
	resp := in.Get(url)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	d, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		t.Fatalf("parse %s: %v", url, err)
	}
	return d
}

// fragmentBody fetches a fragment the way htmx would, returning its markup.
func fragmentBody(t *testing.T, in *testutil.Instance, method, url string) string {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("HX-Request", "true")
	resp := in.Do(req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: status %d", method, url, resp.StatusCode)
	}
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

func fragment(t *testing.T, in *testutil.Instance, method, url string) (*goquery.Document, string) {
	t.Helper()
	markup := fragmentBody(t, in, method, url)
	d, err := goquery.NewDocumentFromReader(strings.NewReader(markup))
	if err != nil {
		t.Fatalf("parse fragment %s: %v", url, err)
	}
	return d, markup
}

func TestTabRendersInTheShell(t *testing.T) {
	in := start(t)
	d := page(t, in, in.UI("/mail/"))

	var active string
	d.Find("nav.tabs a.tab").Each(func(_ int, s *goquery.Selection) {
		if s.HasClass("active") {
			active = strings.TrimSpace(s.Text())
		}
	})
	if active != "Mail" {
		t.Errorf("active tab = %q, want Mail", active)
	}
	if d.Find(".master-detail").Length() == 0 {
		t.Error("the tab should use the shared master-detail layout")
	}
	if d.Find("#list").Length() == 0 || d.Find("#detail").Length() == 0 {
		t.Error("the inbox needs a list pane and a detail pane")
	}
	if !strings.Contains(d.Find(".empty-state").Text(), "No mail yet") {
		t.Errorf("empty state = %q", d.Find(".empty-state").Text())
	}
	// One SSE connection, held by the shell.
	if connect, ok := d.Find("body").Attr("sse-connect"); !ok || connect == "" {
		t.Error("the shell should be holding the SSE connection")
	}
}

func TestListRendersMessages(t *testing.T) {
	in := start(t)
	older, middle, newer := seedInbox(t, in)

	d := page(t, in, in.UI("/mail/"))
	rows := d.Find("#list table.data tbody tr.row")
	if rows.Length() != 3 {
		t.Fatalf("rows = %d, want 3", rows.Length())
	}

	var hrefs []string
	rows.Each(func(_ int, s *goquery.Selection) {
		href, _ := s.Attr("hx-get")
		hrefs = append(hrefs, href)
		if target, _ := s.Attr("hx-target"); target != "#detail" {
			t.Errorf("row target = %q, want #detail", target)
		}
	})
	want := []string{
		"/ui/mail/messages/" + string(newer.ID),
		"/ui/mail/messages/" + string(middle.ID),
		"/ui/mail/messages/" + string(older.ID),
	}
	for i := range want {
		if hrefs[i] != want[i] {
			t.Errorf("row %d links to %q, want %q (newest first)", i, hrefs[i], want[i])
		}
	}

	text := d.Find("#list").Text()
	for _, wantText := range []string{"Invoice 42", "Welcome aboard", "Password reset", "Please pay the attached invoice.", "3 messages"} {
		if !strings.Contains(text, wantText) {
			t.Errorf("the list is missing %q", wantText)
		}
	}
	// The message with attachments advertises them.
	if !strings.Contains(d.Find("#list").Find(".badge").Text(), "2 files") {
		t.Errorf("attachment badge is missing: %q", d.Find("#list").Find(".badge").Text())
	}
}

// The HTML body is untrusted content written by the application under test, so
// it is loaded into a fully sandboxed iframe and never injected into the page.
func TestHTMLBodyIsOnlyEverShownInASandboxedIframe(t *testing.T) {
	in := start(t)
	_, middle, _ := seedInbox(t, in)

	d, markup := fragment(t, in, http.MethodGet, in.UI("/mail/messages/"+string(middle.ID)))

	if strings.Contains(markup, "<b>attached</b>") {
		t.Fatalf("the untrusted html body was injected into the page:\n%s", markup)
	}

	frame := d.Find("iframe.mail-html")
	if frame.Length() != 1 {
		t.Fatalf("want exactly one iframe, got %d", frame.Length())
	}
	sandbox, ok := frame.Attr("sandbox")
	if !ok || sandbox != "" {
		t.Errorf("sandbox = %q (present=%v), want the fully restricted empty value", sandbox, ok)
	}
	src, _ := frame.Attr("src")
	if want := "/api/v1/mail/messages/" + string(middle.ID) + "/html"; src != want {
		t.Errorf("iframe src = %q, want %q", src, want)
	}
	// And that source really is the html part.
	status, body := in.GetBody(in.API("/mail/messages/" + string(middle.ID) + "/html"))
	if status != http.StatusOK || !strings.Contains(body, "<b>attached</b>") {
		t.Errorf("iframe source: status %d, body %q", status, body)
	}
}

func TestDetailShowsHeadersAndAttachments(t *testing.T) {
	in := start(t)
	_, middle, _ := seedInbox(t, in)

	d, _ := fragment(t, in, http.MethodGet, in.UI("/mail/messages/"+string(middle.ID)))

	headers := map[string]string{}
	d.Find("table.kv tr").Each(func(_ int, s *goquery.Selection) {
		headers[strings.TrimSpace(s.Find("th").Text())] = strings.TrimSpace(s.Find("td").Text())
	})
	for key, want := range map[string]string{
		"From":       `"Alice" <alice@example.com>`,
		"To":         `"Bob" <bob@example.com>`,
		"Cc":         "carol@example.com",
		"Bcc":        "dan@example.com",
		"Reply-To":   "no-reply@example.com",
		"Subject":    "Invoice 42",
		"X-Campaign": "billing",
	} {
		if headers[key] != want {
			t.Errorf("header %s = %q, want %q", key, headers[key], want)
		}
	}

	links := d.Find("table.data a")
	if links.Length() != 2 {
		t.Fatalf("attachment links = %d, want 2", links.Length())
	}
	href, _ := links.First().Attr("href")
	if want := "/api/v1/mail/messages/" + string(middle.ID) + "/attachments/0"; href != want {
		t.Errorf("attachment link = %q, want %q", href, want)
	}
	if body := d.Find("table.data").Text(); !strings.Contains(body, "invoice.csv") || !strings.Contains(body, "logo.png") || !strings.Contains(body, "text/csv") {
		t.Errorf("attachment table = %q", body)
	}
	// Provider metadata is shown as metadata, not smuggled into the message.
	if d.Find(".json-inspector").Length() == 0 {
		t.Error("provider metadata should be rendered in the JSON inspector")
	}
}

func TestBodyToggles(t *testing.T) {
	in := start(t)
	_, middle, _ := seedInbox(t, in)
	base := in.UI("/mail/messages/" + string(middle.ID))

	d, _ := fragment(t, in, http.MethodGet, base)
	buttons := d.Find(".mail-views button")
	if buttons.Length() != 3 {
		t.Fatalf("body toggles = %d, want html/text/raw", buttons.Length())
	}
	var labels []string
	buttons.Each(func(_ int, s *goquery.Selection) {
		labels = append(labels, strings.TrimSpace(s.Text()))
		if target, _ := s.Attr("hx-target"); target != "#mail-body" {
			t.Errorf("toggle %q target = %q", s.Text(), target)
		}
		if get, _ := s.Attr("hx-get"); !strings.Contains(get, "/body?view=") {
			t.Errorf("toggle %q hx-get = %q", s.Text(), get)
		}
	})
	if strings.Join(labels, ",") != "HTML,Text,Raw" {
		t.Errorf("toggles = %v", labels)
	}
	if !buttons.First().HasClass("active") {
		t.Error("a message with an html part should open on the HTML view")
	}

	tests := []struct {
		view       string
		wantIn     string
		wantNotIn  string
		wantIframe bool
	}{
		{"html", "sandboxed", "<b>attached</b>", true},
		{"text", "Please pay the attached invoice.", "<iframe", false},
		{"raw", "Content-Type", "<iframe", false},
	}
	for _, tt := range tests {
		t.Run(tt.view, func(t *testing.T) {
			markup := fragmentBody(t, in, http.MethodGet, base+"/body?view="+tt.view)
			if !strings.Contains(markup, tt.wantIn) {
				t.Errorf("the %s view is missing %q:\n%s", tt.view, tt.wantIn, markup)
			}
			if strings.Contains(markup, tt.wantNotIn) {
				t.Errorf("the %s view should not contain %q", tt.view, tt.wantNotIn)
			}
			if got := strings.Contains(markup, "<iframe"); got != tt.wantIframe {
				t.Errorf("the %s view iframe = %v", tt.view, got)
			}
		})
	}
}

// A text-only message must not greet you with an empty frame.
func TestDefaultViewFollowsWhatTheMessageHas(t *testing.T) {
	in := start(t)
	textOnly := inject(t, in, &mail.Message{
		From: mail.Address{Email: "a@example.com"},
		To:   []mail.Address{{Email: "b@example.com"}},
		Text: "text only, please",
	})
	bodyless := inject(t, in, &mail.Message{
		From:    mail.Address{Email: "a@example.com"},
		Subject: "nothing but headers",
	})

	tests := []struct {
		name, id, wantActive string
	}{
		{"text only", string(textOnly.ID), "Text"},
		{"no body at all", string(bodyless.ID), "Raw"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := fragment(t, in, http.MethodGet, in.UI("/mail/messages/"+tt.id))
			active := strings.TrimSpace(d.Find(".mail-views button.active").Text())
			if active != tt.wantActive {
				t.Errorf("active view = %q, want %q", active, tt.wantActive)
			}
		})
	}
}

func TestListRefreshesOnMailEvents(t *testing.T) {
	in := start(t)
	d := page(t, in, in.UI("/mail/"))

	live := d.Find("#list-inner")
	if live.Length() == 0 {
		t.Fatal("the list has no live-refresh wrapper")
	}
	trigger, _ := live.Attr("hx-trigger")
	if !strings.Contains(trigger, "sse:"+mail.TypeMessage) {
		t.Errorf("hx-trigger = %q, want it to listen for sse:%s", trigger, mail.TypeMessage)
	}
	if get, _ := live.Attr("hx-get"); get != "/ui/mail/list" {
		t.Errorf("refresh url = %q", get)
	}

	// The refresh must carry the filter, or a live update would silently widen
	// what the user is looking at.
	d = page(t, in, in.UI("/mail/?search=invoice&provider=sendgrid&attachments=1"))
	get, _ := d.Find("#list-inner").Attr("hx-get")
	for _, want := range []string{"search=invoice", "provider=sendgrid", "attachments=1"} {
		if !strings.Contains(get, want) {
			t.Errorf("refresh url %q is missing %q", get, want)
		}
	}
}

func TestFiltersNarrowTheList(t *testing.T) {
	in := start(t)
	seedInbox(t, in)

	tests := []struct {
		name  string
		query string
		want  int
	}{
		{"no filter", "", 3},
		{"search", "?search=Invoice", 1},
		{"provider", "?provider=mailjet", 1},
		{"with attachments", "?attachments=1", 1},
		{"without attachments", "?attachments=0", 2},
		{"nonsense attachments value is ignored", "?attachments=maybe", 3},
		{"no match", "?search=nothing-like-this", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := fragment(t, in, http.MethodGet, in.UI("/mail/list"+tt.query))
			if got := d.Find("tbody tr.row").Length(); got != tt.want {
				t.Errorf("rows = %d, want %d", got, tt.want)
			}
		})
	}

	// The provider dropdown offers every provider seen, not just the ones in
	// the current, narrowed listing.
	d := page(t, in, in.UI("/mail/?provider=mailjet"))
	if got := d.Find("select[name=provider] option").Length(); got != 4 {
		t.Errorf("provider options = %d, want the empty choice plus three providers", got)
	}
}

func TestDeepLinkRendersTheWholeTab(t *testing.T) {
	in := start(t)
	_, middle, _ := seedInbox(t, in)

	d := page(t, in, in.UI("/mail/messages/"+string(middle.ID)))
	if d.Find("nav.tabs").Length() == 0 {
		t.Error("a deep link should render inside the shell")
	}
	if d.Find(".mail-detail").Length() == 0 {
		t.Error("the message should be open")
	}
	selected := d.Find("tbody tr.row.selected")
	if selected.Length() != 1 {
		t.Fatalf("selected rows = %d, want 1", selected.Length())
	}
	if href, _ := selected.Attr("hx-get"); !strings.HasSuffix(href, string(middle.ID)) {
		t.Errorf("the wrong row is selected: %q", href)
	}

	if status, _ := in.GetBody(in.UI("/mail/messages/does-not-exist")); status != http.StatusNotFound {
		t.Errorf("unknown message status = %d, want 404", status)
	}
}

// The generic event view fills in unclaimed routes; the mail tab claims the
// event-shaped ones too, so one tab never shows two different views.
func TestEventRoutesAreTheMailViews(t *testing.T) {
	in := start(t)
	_, middle, _ := seedInbox(t, in)

	d, _ := fragment(t, in, http.MethodGet, in.UI("/mail/events/"+string(middle.ID)))
	if d.Find(".mail-detail").Length() == 0 {
		t.Error("/events/{id} should render the mail detail, not the generic one")
	}
	if d.Find(".event-detail").Length() != 0 {
		t.Error("the generic event detail leaked into the mail tab")
	}
}

func TestClearFromTheTab(t *testing.T) {
	in := start(t)
	seedInbox(t, in)

	for _, path := range []string{"/mail/messages", "/mail/events"} {
		t.Run(path, func(t *testing.T) {
			seedInbox(t, in)
			d, _ := fragment(t, in, http.MethodDelete, in.UI(path))
			if got := d.Find("tbody tr.row").Length(); got != 0 {
				t.Errorf("rows after clearing = %d", got)
			}
			if !strings.Contains(d.Text(), "0 messages") {
				t.Errorf("the count was not refreshed: %q", d.Text())
			}
			if len(in.Events(store.Query{Plugin: mail.PluginName})) != 0 {
				t.Error("the store still holds mail events")
			}
		})
	}
}

// The how-to-test panel is a contract obligation (docs/implementation-plan.md
// §5, §6.4): every tab must show it, listing each enabled provider's
// description and a runnable, rendered snippet. It should stay out of the way
// once the inbox actually has mail in it.
func TestHowToTestPanelWithMessages(t *testing.T) {
	in := start(t)
	seedInbox(t, in)

	d := page(t, in, in.UI("/mail/"))
	panel := d.Find(".how-to-test")
	if panel.Length() == 0 {
		t.Fatal("the mail tab has no how-to-test panel")
	}
	if _, open := panel.Attr("open"); open {
		t.Error("the panel should be collapsed once the inbox has mail")
	}

	code := panel.Find(".snippet pre code").Text()
	if code == "" {
		t.Fatal("no snippet rendered in the panel")
	}
	if strings.Contains(code, "{{") {
		t.Errorf("snippet template was not rendered:\n%s", code)
	}
	if !strings.Contains(code, in.IngressURL) {
		t.Errorf("snippet must carry the live ingress URL %q:\n%s", in.IngressURL, code)
	}
	if panel.Find(".copy-btn").Length() == 0 {
		t.Error("the snippet needs a copy button")
	}
	if !strings.Contains(panel.Text(), "Mail") {
		t.Errorf("the panel should name the mail plugin: %q", panel.Text())
	}
}

// An empty inbox is exactly when someone needs the panel open, so it defaults
// open there instead of making them go find the disclosure triangle.
func TestHowToTestPanelOpenWhenInboxIsEmpty(t *testing.T) {
	in := start(t)

	d := page(t, in, in.UI("/mail/"))
	panel := d.Find(".how-to-test")
	if panel.Length() == 0 {
		t.Fatal("the mail tab has no how-to-test panel")
	}
	if _, open := panel.Attr("open"); !open {
		t.Error("the panel should default open when the inbox is empty")
	}
}

// Filtering to zero results and having zero mail ever land are different
// situations and should not read the same way.
func TestEmptyListMessageDistinguishesFilterFromNoMail(t *testing.T) {
	in := start(t)

	d, _ := fragment(t, in, http.MethodGet, in.UI("/mail/list"))
	if !strings.Contains(d.Find("table.data").Text(), "No mail captured yet.") {
		t.Errorf("empty inbox message = %q", d.Find("table.data").Text())
	}

	seedInbox(t, in)
	d, _ = fragment(t, in, http.MethodGet, in.UI("/mail/list?search=nothing-like-this"))
	if !strings.Contains(d.Find("table.data").Text(), "No message matches this filter.") {
		t.Errorf("filtered-to-nothing message = %q", d.Find("table.data").Text())
	}
}

// Attachments get a short category badge next to the full MIME type, so a
// crowded attachment list can be scanned at a glance.
func TestAttachmentTypeBadges(t *testing.T) {
	in := start(t)
	_, middle, _ := seedInbox(t, in)

	d, _ := fragment(t, in, http.MethodGet, in.UI("/mail/messages/"+string(middle.ID)))
	table := d.Find("table.data").First()
	badges := table.Find(".badge").Map(func(_ int, s *goquery.Selection) string { return strings.TrimSpace(s.Text()) })
	joined := strings.Join(badges, ",")
	if !strings.Contains(joined, "sheet") {
		t.Errorf("csv attachment should carry a %q badge, got %v", "sheet", badges)
	}
	if !strings.Contains(joined, "image") {
		t.Errorf("png attachment should carry an %q badge, got %v", "image", badges)
	}
}

func TestUIStreamCarriesMailEvents(t *testing.T) {
	in := start(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.UI("/stream"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("preamble: %v", err)
	}

	inject(t, in, &mail.Message{
		From:    mail.Address{Email: "a@example.com"},
		To:      []mail.Address{{Email: "b@example.com"}},
		Subject: "streamed subject",
	}, mailtest.WithProvider("fake"))

	// Two frames per event: the JSON payload, then the type-named frame htmx
	// triggers on.
	var sawPayload, sawNamedFrame bool
	for !sawPayload || !sawNamedFrame {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("the stream never delivered both frames for the appended message (payload=%v, %s=%v): %v",
				sawPayload, mail.TypeMessage, sawNamedFrame, err)
		}
		if strings.Contains(line, "streamed subject") {
			sawPayload = true
		}
		if strings.TrimSpace(line) == "event: "+mail.TypeMessage {
			sawNamedFrame = true
		}
	}
}
