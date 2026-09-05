package ui_test

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/server/ui"
	"github.com/can3p/tommy/core/testutil/fakeplugin"
)

// detailPlugin renders its own detail fragment, which is what the event page
// must embed instead of the generic inspector.
type detailPlugin struct{ *fakeplugin.Plugin }

func (p detailPlugin) Name() string  { return "custom" }
func (p detailPlugin) Title() string { return "Custom" }
func (p detailPlugin) Providers() []plugin.Provider {
	return []plugin.Provider{customProvider{}}
}
func (p detailPlugin) Templates() fs.FS { return nil }
func (p detailPlugin) RegisterUI(mux plugin.Mux, d plugin.Deps) {
	mux.HandleFunc("GET /events/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = ui.Render(w, r, "Custom", `<div id="my-own-detail">bespoke detail</div>`)
	})
}

// The page is a page: one event, not the list it happens to be in.
func TestEventPageShowsOneEvent(t *testing.T) {
	in := start(t, fakeplugin.New())
	inject(t, in, "fake", "Older one", "older body")
	e := inject(t, in, "fake", "The one I sent", "hello there")

	d := doc(t, in, in.UI("/events/"+string(e.ID)))

	if got := strings.TrimSpace(d.Find("article.event-page h1").Text()); got != "The one I sent" {
		t.Errorf("heading = %q, want the event title", got)
	}
	// The whole point: this is not the tab with a row selected.
	if n := d.Find("form.filters").Length(); n != 0 {
		t.Errorf("the page carries the tab's filter bar (%d), so it is still a list", n)
	}
	if n := d.Find("#list").Length(); n != 0 {
		t.Errorf("the page carries the event list, so it is still a list")
	}
	// It still knows where it came from.
	if href, _ := d.Find("article.event-page nav a").First().Attr("href"); href != "/ui/fake/" {
		t.Errorf("back link = %q, want /ui/fake/", href)
	}
	// The link the copy button hands over must be absolute: the point of it is
	// to paste it somewhere that is not this tab.
	share, ok := d.Find("article.event-page .copy-btn").Attr("data-copy")
	if !ok || !strings.HasPrefix(share, "http://") || !strings.HasSuffix(share, "/ui/events/"+string(e.ID)) {
		t.Errorf("share URL = %q, want an absolute link to this event", share)
	}
	if href, _ := d.Find("article.event-page a.btn").Attr("href"); href != "/api/v1/events/"+string(e.ID) {
		t.Errorf("JSON link = %q", href)
	}
	// The event body is on the page, through the generic inspector.
	if body := d.Find(".event-page-body").Text(); !strings.Contains(body, "hello there") {
		t.Errorf("page body does not carry the event: %q", body)
	}
	if body := d.Find(".event-page-body").Text(); strings.Contains(body, "older body") {
		t.Error("the page shows a second event; it is meant to show exactly one")
	}
}

// Newer and older walk the same plugin's events, so an inbox can be read from
// the page rather than the list.
func TestEventPageStepsBetweenSiblings(t *testing.T) {
	in := start(t, fakeplugin.New())
	oldest := inject(t, in, "fake", "Oldest", "1")
	middle := inject(t, in, "fake", "Middle", "2")
	newest := inject(t, in, "fake", "Newest", "3")

	d := doc(t, in, in.UI("/events/"+string(middle.ID)))
	newer, _ := d.Find(`article.event-page nav a[rel="prev"]`).Attr("href")
	older, _ := d.Find(`article.event-page nav a[rel="next"]`).Attr("href")
	if newer != "/ui/events/"+string(newest.ID) {
		t.Errorf("newer = %q, want the newest event", newer)
	}
	if older != "/ui/events/"+string(oldest.ID) {
		t.Errorf("older = %q, want the oldest event", older)
	}

	// The ends of the list have nowhere to step to.
	d = doc(t, in, in.UI("/events/"+string(newest.ID)))
	if n := d.Find(`article.event-page nav a[rel="prev"]`).Length(); n != 0 {
		t.Errorf("the newest event offers a newer one")
	}
}

// A plugin with a detail view of its own renders the page body, so a mail
// looks like a mail rather than a JSON dump. No plugin implements anything for
// this: the page asks the fragment route the plugin already serves.
func TestEventPageUsesThePluginsOwnView(t *testing.T) {
	in := start(t, detailPlugin{fakeplugin.New()})
	e := inject(t, in, "custom", "Bespoke", "body")

	d := doc(t, in, in.UI("/events/"+string(e.ID)))
	if n := d.Find(".event-page-body #my-own-detail").Length(); n != 1 {
		html, _ := d.Find(".event-page-body").Html()
		t.Errorf("the plugin's own detail is not on the page: %q", html)
	}
}

// The same URL still answers htmx with the fragment it has always returned, so
// selecting a row in a tab did not turn into a page load.
func TestEventPageServesFragmentToHtmx(t *testing.T) {
	in := start(t, fakeplugin.New())
	e := inject(t, in, "fake", "Fragment", "body")

	d := fragment(t, in, in.UI("/events/"+string(e.ID)))
	if n := d.Find("article.event-detail").Length(); n != 1 {
		t.Errorf("htmx did not get the detail fragment")
	}
	if n := d.Find("article.event-page").Length(); n != 0 {
		t.Errorf("htmx got the whole page")
	}
}

// A link outlives the event it points at: the store is a ring buffer, and an
// id pasted into an issue will eventually be gone.
func TestEventPageForAnEvictedEvent(t *testing.T) {
	in := start(t, fakeplugin.New())

	resp := in.Get(in.UI("/events/01a0000000000000deadbeef"))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type = %q, want an HTML page", ct)
	}
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), "No such event") {
		t.Errorf("the page does not say what went wrong")
	}
}

// EventURL is what every surface builds its link from, so its two forms are
// worth pinning.
func TestEventURL(t *testing.T) {
	if got := ui.EventURL("", "abc"); got != "/ui/events/abc" {
		t.Errorf("relative = %q", got)
	}
	if got := ui.EventURL("http://localhost:8811", "abc"); got != "http://localhost:8811/ui/events/abc" {
		t.Errorf("absolute = %q", got)
	}
	if got := ui.EventURL("http://localhost:8811/", "abc"); got != "http://localhost:8811/ui/events/abc" {
		t.Errorf("trailing slash = %q", got)
	}
}
