package ui_test

import (
	"bufio"
	"context"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/server/ui"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/core/testutil/fakeplugin"
)

// customUIPlugin overrides its tab, which must suppress the generic view for it
// and only for it.
type customUIPlugin struct{ *fakeplugin.Plugin }

func (p customUIPlugin) Name() string  { return "custom" }
func (p customUIPlugin) Title() string { return "Custom" }
func (p customUIPlugin) Providers() []plugin.Provider {
	return []plugin.Provider{customProvider{}}
}
func (p customUIPlugin) RegisterUI(mux plugin.Mux, d plugin.Deps) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		_ = ui.Render(w, r, "Custom", `<div id="my-own-view">bespoke</div>`)
	})
}
func (p customUIPlugin) Templates() fs.FS { return nil }

type customProvider struct{}

func (customProvider) Name() string   { return "custom-provider" }
func (customProvider) Plugin() string { return "custom" }
func (customProvider) Description() string {
	return "A provider whose plugin renders its own tab instead of the generic event view."
}
func (customProvider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{{Method: "POST", Path: "/custom/send", Description: "Record something custom."}}
}
func (customProvider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{{Title: "Send", Lang: "bash", Code: "curl {{.IngressURL}}/custom/send -d x"}}
}
func (customProvider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	mux.HandleFunc("POST /custom/send", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}

func start(t *testing.T, plugins ...plugin.Plugin) *testutil.Instance {
	t.Helper()
	return testutil.Start(t, nil, plugins...)
}

func doc(t *testing.T, in *testutil.Instance, url string) *goquery.Document {
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

func fragment(t *testing.T, in *testutil.Instance, url string) *goquery.Document {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("HX-Request", "true")
	resp := in.Do(req)
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

func inject(t *testing.T, in *testutil.Instance, pluginName, title, body string) *event.Event {
	t.Helper()
	e := &event.Event{
		Plugin:   pluginName,
		Provider: "echo",
		Type:     pluginName + ".message",
		Summary:  event.Summary{From: "a@example.com", To: []string{"b@example.com"}, Title: title, Snippet: body},
		Meta:     map[string]any{"custom_id": "abc"},
		Payload:  map[string]any{"text": body},
		Raw: event.Raw{
			Transport: "http", Method: "POST", Path: "/fake/v1/send",
			Headers: http.Header{"Content-Type": {"application/json"}},
			Body:    []byte(`{"text":"` + body + `"}`), Text: true,
		},
	}
	if err := in.Store.Append(context.Background(), e); err != nil {
		t.Fatalf("append: %v", err)
	}
	return e
}

func TestEmptyTabBar(t *testing.T) {
	in := start(t)

	d := doc(t, in, in.UIURL)
	tabs := d.Find("nav.tabs .tab")
	if tabs.Length() != 2 { // Overview plus the "no plugins" note
		t.Errorf("tabs = %d: %q", tabs.Length(), tabs.Text())
	}
	if !strings.Contains(d.Find("nav.tabs").Text(), "no plugins enabled") {
		t.Errorf("an empty tommy must say so: %q", d.Find("nav.tabs").Text())
	}
	if d.Find(".how-to-test").Length() == 0 {
		t.Error("the how-to-test panel must render even with nothing enabled")
	}
}

func TestTabBarListsEveryEnabledPlugin(t *testing.T) {
	in := start(t, fakeplugin.New(), customUIPlugin{fakeplugin.New()})

	d := doc(t, in, in.UIURL)
	var labels []string
	d.Find("nav.tabs a.tab").Each(func(_ int, s *goquery.Selection) {
		labels = append(labels, strings.TrimSpace(s.Text()))
	})
	want := []string{"Overview", "Fake", "Custom"}
	if strings.Join(labels, ",") != strings.Join(want, ",") {
		t.Errorf("tabs = %v, want %v", labels, want)
	}

	// Each tab links at its own prefix.
	href, _ := d.Find("nav.tabs a.tab").Eq(1).Attr("href")
	if href != "/ui/fake/" {
		t.Errorf("fake tab href = %q", href)
	}
}

func TestDisabledPluginHasNoTab(t *testing.T) {
	cfg := config.Ephemeral()
	cfg.SetPluginEnabled("fake", false)
	in := testutil.Start(t, cfg, fakeplugin.New())

	d := doc(t, in, in.UIURL)
	if strings.Contains(d.Find("nav.tabs").Text(), "Fake") {
		t.Error("a disabled plugin must not get a tab")
	}
}

func TestGenericEventViewRendersEvents(t *testing.T) {
	in := start(t, fakeplugin.New())
	inject(t, in, "fake", "Invoice 42", "please pay")
	inject(t, in, "fake", "Welcome", "hello there")

	d := doc(t, in, in.UI("/fake/"))
	rows := d.Find("table.data tbody tr.row")
	if rows.Length() != 2 {
		t.Fatalf("rows = %d\n%s", rows.Length(), d.Find("table.data").Text())
	}
	// Newest first.
	if !strings.Contains(rows.Eq(0).Text(), "Welcome") {
		t.Errorf("first row = %q, want the newest event", rows.Eq(0).Text())
	}
	// A row is an htmx link into the detail pane.
	target, _ := rows.Eq(0).Attr("hx-target")
	if target != "#detail" {
		t.Errorf("row hx-target = %q", target)
	}
	if href, _ := rows.Eq(0).Attr("hx-get"); !strings.HasPrefix(href, "/ui/fake/events/") {
		t.Errorf("row hx-get = %q", href)
	}
	// The list refreshes itself from the shell's single SSE connection.
	trigger, _ := d.Find("#list-inner").Attr("hx-trigger")
	if !strings.Contains(trigger, "sse:") {
		t.Errorf("list must be driven by SSE, hx-trigger = %q", trigger)
	}
	if connect, ok := d.Find("body").Attr("sse-connect"); !ok || connect != "/ui/stream" {
		t.Errorf("the shell must hold one SSE connection, got %q", connect)
	}
}

func TestGenericEventViewFilters(t *testing.T) {
	in := start(t, fakeplugin.New())
	inject(t, in, "fake", "Invoice 42", "please pay")
	inject(t, in, "fake", "Welcome", "hello there")

	d := fragment(t, in, in.UI("/fake/list?search=invoice"))
	rows := d.Find("table.data tbody tr.row")
	if rows.Length() != 1 || !strings.Contains(rows.Text(), "Invoice") {
		t.Errorf("filtered rows = %d: %q", rows.Length(), rows.Text())
	}
}

func TestEventDetailShowsPayloadMetaAndRaw(t *testing.T) {
	in := start(t, fakeplugin.New())
	e := inject(t, in, "fake", "Invoice 42", "please pay")

	d := fragment(t, in, in.UI("/fake/events/"+string(e.ID)))
	text := d.Text()
	for _, want := range []string{"Invoice 42", "fake.message", "custom_id", "please pay"} {
		if !strings.Contains(text, want) {
			t.Errorf("detail must show %q\n%s", want, text)
		}
	}
	if d.Find(".json-inspector").Length() < 2 {
		t.Error("payload and metadata must each get a JSON inspector")
	}
	if d.Find(".raw-viewer").Length() == 0 {
		t.Error("the raw request must be shown")
	}
	// A text body renders as text, not hex.
	if d.Find(".hex-viewer").Length() != 0 {
		t.Error("a text body must not fall back to the hex viewer")
	}
}

func TestBinaryRawBodyFallsBackToHex(t *testing.T) {
	in := start(t, fakeplugin.New())
	e := &event.Event{
		Plugin: "fake", Provider: "echo", Type: "fake.binary",
		Summary: event.Summary{Title: "binary"},
		Raw:     event.Raw{Transport: "tcp", Body: []byte{0x00, 0x01, 0xff, 0xfe, 'h', 'i'}},
	}
	if err := in.Store.Append(context.Background(), e); err != nil {
		t.Fatal(err)
	}

	d := fragment(t, in, in.UI("/fake/events/"+string(e.ID)))
	if d.Find(".hex-viewer").Length() == 0 {
		t.Fatalf("a binary body must render in the hex viewer\n%s", d.Text())
	}
	if !strings.Contains(d.Find(".hex-viewer").Text(), "00000000") {
		t.Errorf("hex viewer = %q", d.Find(".hex-viewer").Text())
	}
}

func TestDeepLinkToAnEventRendersTheWholePage(t *testing.T) {
	in := start(t, fakeplugin.New())
	e := inject(t, in, "fake", "Invoice 42", "please pay")

	d := doc(t, in, in.UI("/fake/events/"+string(e.ID)))
	if d.Find("nav.tabs").Length() == 0 {
		t.Error("a pasted event URL must render the full shell")
	}
	if !strings.Contains(d.Find("#detail").Text(), "Invoice 42") {
		t.Error("the linked event must be selected")
	}
}

func TestHowToTestPanelShowsRenderedSnippets(t *testing.T) {
	in := start(t, fakeplugin.New())

	d := doc(t, in, in.UI("/fake/"))
	panel := d.Find(".how-to-test")
	if panel.Length() == 0 {
		t.Fatal("no how-to-test panel")
	}
	code := panel.Find(".snippet pre code").Text()
	if !strings.Contains(code, in.IngressURL) {
		t.Errorf("snippet must carry the live ingress URL %q:\n%s", in.IngressURL, code)
	}
	if strings.Contains(code, "{{") {
		t.Errorf("snippet was not rendered:\n%s", code)
	}
	if panel.Find(".copy-btn").Length() == 0 {
		t.Error("snippets need copy buttons")
	}
	if !strings.Contains(panel.Text(), "POST") || !strings.Contains(panel.Text(), "/fake/v1/send") {
		t.Error("the panel must list the endpoints the provider serves")
	}
}

func TestEmptyStateCarriesTheSnippets(t *testing.T) {
	in := start(t, fakeplugin.New())

	d := doc(t, in, in.UI("/fake/"))
	empty := d.Find(".empty-state")
	if empty.Length() == 0 {
		t.Fatal("an empty tab must show an empty state")
	}
	if empty.Find(".snippet").Length() == 0 {
		t.Error("an empty tab is exactly when someone needs the snippets inline")
	}
}

func TestPluginCanOverrideItsTab(t *testing.T) {
	in := start(t, fakeplugin.New(), customUIPlugin{fakeplugin.New()})

	custom := doc(t, in, in.UI("/custom/"))
	if custom.Find("#my-own-view").Length() == 0 {
		t.Error("a plugin that registers its tab root must keep it")
	}
	if custom.Find("table.data").Length() != 0 {
		t.Error("the generic view must not be mounted over a bespoke tab")
	}
	if custom.Find("nav.tabs").Length() == 0 {
		t.Error("a bespoke tab still gets the shell")
	}

	// The routes it did not claim still fall back to the generic view.
	list := fragment(t, in, in.UI("/custom/list"))
	if list.Find("table.data").Length() == 0 {
		t.Error("unclaimed generic routes must still be mounted")
	}

	// And the other plugin is unaffected.
	if doc(t, in, in.UI("/fake/")).Find("table.data").Length() == 0 {
		t.Error("the plugin that overrides nothing must still get the generic view")
	}
}

func TestStaticAssetsAreServedFromTheBinary(t *testing.T) {
	in := start(t)
	for _, asset := range []string{"htmx.min.js", "htmx-ext-sse.js", "app.css", "app.js", "favicon.svg"} {
		resp := in.Get(in.UI("/static/" + asset))
		size := resp.ContentLength
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", asset, resp.StatusCode)
		}
		if size == 0 {
			t.Errorf("%s is empty", asset)
		}
	}

	// The page must not reach for a CDN.
	d := doc(t, in, in.UIURL)
	d.Find("script,link").Each(func(_ int, s *goquery.Selection) {
		for _, attr := range []string{"src", "href"} {
			if v, ok := s.Attr(attr); ok && strings.HasPrefix(v, "http") {
				t.Errorf("the UI must not hotlink %q", v)
			}
		}
	})
}

func TestUIStream(t *testing.T) {
	in := start(t, fakeplugin.New())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, in.UI("/stream"), nil)
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
	inject(t, in, "fake", "streamed", "body")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if strings.Contains(line, "streamed") {
			return
		}
	}
	t.Fatal("the UI stream never delivered the appended event")
}

func TestClearFromTheUI(t *testing.T) {
	in := start(t, fakeplugin.New())
	inject(t, in, "fake", "Invoice 42", "please pay")

	req, _ := http.NewRequest(http.MethodDelete, in.UI("/fake/events"), nil)
	req.Header.Set("HX-Request", "true")
	resp := in.Do(req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	d, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if d.Find("table.data tbody tr.row").Length() != 0 {
		t.Error("clearing must return an empty list fragment")
	}
	if len(in.Events(store.Query{})) != 0 {
		t.Error("clearing must empty the store")
	}
}

func TestRootRedirectsToTheUI(t *testing.T) {
	in := start(t)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	base := strings.TrimSuffix(in.UIURL, "/ui/")
	resp, err := client.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/ui/" {
		t.Errorf("Location = %q", loc)
	}
}
