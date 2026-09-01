package files_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/files"
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
	h := start(t)
	doc := uiDoc(t, h.Instance, h.UIURL)

	var tabs []string
	doc.Find("nav.tabs a").Each(func(_ int, s *goquery.Selection) {
		if href, ok := s.Attr("href"); ok {
			tabs = append(tabs, s.Text()+"="+href)
		}
	})
	if !strings.Contains(strings.Join(tabs, " "), "Files=/ui/files/") {
		t.Errorf("tab bar = %v, want a Files tab pointing at /ui/files/", tabs)
	}
}

func TestUIEmptyState(t *testing.T) {
	h := start(t)
	doc := uiDoc(t, h.Instance, h.UI("/files/"))

	if doc.Find(".empty-state").Length() == 0 {
		t.Fatal("an empty files tab must show an empty state")
	}
	if !strings.Contains(doc.Find(".empty-state").Text(), "No files yet") {
		t.Errorf("empty state text = %q", doc.Find(".empty-state").Text())
	}
	// The how-to-test panel is open on an empty tab, and carries the enabled
	// provider's endpoint and a runnable snippet.
	panel := doc.Find("details.how-to-test")
	if panel.Length() == 0 {
		t.Fatal("the how-to-test panel is missing")
	}
	if _, open := panel.Attr("open"); !open {
		t.Error("the how-to-test panel must start open when the tab is empty")
	}
	if !strings.Contains(doc.Text(), "/fake-files/") {
		t.Error("the panel should list the enabled provider's endpoints")
	}
	if !strings.Contains(doc.Find("figure.snippet").Text(), "curl") {
		t.Errorf("the panel should carry a runnable snippet; got %q", doc.Find("figure.snippet").Text())
	}
}

func TestUIDirectoryListing(t *testing.T) {
	h := start(t)
	h.upload(t, "ftp", "/upload/report.csv", "a,b\n1,2\n")
	h.upload(t, "sftp", "/upload/deep/notes.txt", "notes")
	h.upload(t, "ftp", "/top.txt", "top")

	doc := uiDoc(t, h.Instance, h.UI("/files/"))
	rows := doc.Find(".files-table tr.files-row")
	if rows.Length() != 2 {
		t.Fatalf("root listing has %d rows, want 2", rows.Length())
	}
	// Directories first.
	if got := strings.TrimSpace(rows.Eq(0).Find(".files-name").Text()); got != "📁 upload/" {
		t.Errorf("first row = %q, want the upload directory", got)
	}
	if got := strings.TrimSpace(rows.Eq(1).Find(".files-name").Text()); got != "top.txt" {
		t.Errorf("second row = %q", got)
	}
	// A file row carries a download link into the API and the provider badge.
	href, _ := rows.Eq(1).Find(".files-name").Attr("href")
	if !strings.HasSuffix(href, "/api/v1/files/content/top.txt") {
		t.Errorf("download link = %q", href)
	}
	if _, ok := rows.Eq(1).Find(".files-name").Attr("download"); !ok {
		t.Error("a file link must be a download")
	}
	if got := rows.Eq(1).Find(".badge").Text(); got != "ftp" {
		t.Errorf("provider badge = %q, want ftp", got)
	}
	if got := rows.Eq(1).Text(); !strings.Contains(got, "3 B") {
		t.Errorf("row = %q, want a human size", got)
	}

	// The empty state is gone once something is there, and the counters are
	// for the whole filesystem rather than this directory.
	if doc.Find(".empty-state").Length() != 0 {
		t.Error("a non-empty tab must not show the empty state")
	}
	if got := doc.Find(".files-stats").Text(); !strings.Contains(got, "3 files") || !strings.Contains(got, "2 directories") {
		t.Errorf("stats line = %q", got)
	}
}

func TestUIBreadcrumbNavigation(t *testing.T) {
	h := start(t)
	h.upload(t, "ftp", "/a/b/c.txt", "c")

	doc := uiDoc(t, h.Instance, h.UI("/files/?path=/a/b"))
	var crumbs []string
	doc.Find(".files-crumbs .files-crumb").Each(func(_ int, s *goquery.Selection) {
		crumbs = append(crumbs, strings.TrimSpace(s.Text()))
	})
	if strings.Join(crumbs, "|") != "/|a|b" {
		t.Errorf("breadcrumb = %v", crumbs)
	}
	// The last crumb is the current directory and is not a link.
	if doc.Find(".files-crumb.current").Length() != 1 {
		t.Error("exactly one crumb must be marked current")
	}
	href, _ := doc.Find(".files-crumbs a").First().Attr("href")
	if href != "/ui/files/" {
		t.Errorf("root crumb href = %q", href)
	}
	// And each link swaps a fragment rather than reloading the page.
	fetch, _ := doc.Find(".files-crumbs a").First().Attr("hx-get")
	if !strings.HasPrefix(fetch, "/ui/files/list?path=") {
		t.Errorf("root crumb hx-get = %q", fetch)
	}
	// The listing shows the one file in /a/b, plus a link back up.
	if doc.Find(".files-up a").Length() != 1 {
		t.Error("a subdirectory listing must offer a way back up")
	}
	if got := doc.Find(".files-table tr.files-row .files-name").Text(); got != "c.txt" {
		t.Errorf("listing = %q", got)
	}
}

// A directory that vanished while it was open falls back to the root rather
// than 404ing, because it happens routinely.
func TestUIMissingDirectoryFallsBackToTheRoot(t *testing.T) {
	h := start(t)
	h.upload(t, "ftp", "/gone/x.txt", "x")
	if _, _, err := h.VFS.RemoveAll(context.Background(), "/gone"); err != nil {
		t.Fatal(err)
	}
	doc := uiDoc(t, h.Instance, h.UI("/files/?path=/gone"))
	if doc.Find(".files-missing").Length() == 0 {
		t.Error("the tab should say the directory is gone")
	}
}

func TestUILiveRefreshTriggers(t *testing.T) {
	h := start(t)
	h.upload(t, "ftp", "/a.txt", "a")
	doc := uiDoc(t, h.Instance, h.UI("/files/"))

	trigger, ok := doc.Find("#files-refresh").Attr("hx-trigger")
	if !ok {
		t.Fatal("the listing must carry a live-refresh trigger")
	}
	for _, typ := range files.EventTypes {
		if !strings.Contains(trigger, "sse:"+typ) {
			t.Errorf("trigger %q does not subscribe to %s", trigger, typ)
		}
	}
	// The refresh URL carries the directory that is open, so a live update
	// does not jump back to the root.
	sub := uiDoc(t, h.Instance, h.UI("/files/?path=/"))
	if got, _ := sub.Find("#files-refresh").Attr("hx-get"); !strings.Contains(got, "path=") {
		t.Errorf("refresh URL = %q, want the open directory", got)
	}
}

func TestUIActivityList(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	s := h.session("ftp")
	if _, err := s.Mkdir(ctx, "/inbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutBytes(ctx, "/inbox/a.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remove(ctx, "/inbox/a.txt"); err != nil {
		t.Fatal(err)
	}

	doc := uiDoc(t, h.Instance, h.UI("/files/"))
	rows := doc.Find(".files-activity tbody tr")
	if rows.Length() != 3 {
		t.Fatalf("activity list has %d rows, want 3", rows.Length())
	}
	// Newest first, and the log still describes a file that is gone from the
	// tree - the whole point of keeping both.
	if got := rows.Eq(0).Text(); !strings.Contains(got, "deleted /inbox/a.txt") {
		t.Errorf("newest activity row = %q", got)
	}
	if got := rows.Eq(1).Text(); !strings.Contains(got, "uploaded /inbox/a.txt") {
		t.Errorf("second activity row = %q", got)
	}
	raw, ok := rows.Eq(0).Find("a.files-raw").Attr("href")
	if !ok || !strings.HasPrefix(raw, "/ui/files/events/") {
		t.Errorf("raw link = %q", raw)
	}
	// That link lands on the core's generic event inspector.
	if status, _ := h.GetBody(h.UI(strings.TrimPrefix(raw, "/ui/"))); status != 200 {
		t.Errorf("the raw link is broken: status %d", status)
	}
}

// Filenames are untrusted input. A name containing markup must be escaped by
// html/template and must never reach the page as HTML.
func TestUIEscapesHostileFilenames(t *testing.T) {
	h := start(t)
	const evil = `<img src=x onerror=alert(1)>.txt`
	const evilDir = `"><script>x`
	h.upload(t, "ftp", "/"+evil, "boom")
	if _, err := h.session("ftp").Mkdir(context.Background(), "/"+evilDir); err != nil {
		t.Fatal(err)
	}

	status, body := h.GetBody(h.UI("/files/"))
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}

	if strings.Contains(body, "<img src=x") || strings.Contains(body, "<script>x") {
		t.Fatal("a filename was interpolated as HTML")
	}
	if !strings.Contains(body, "&lt;img src=x onerror=alert(1)&gt;.txt") {
		t.Error("the escaped filename is missing from the page")
	}

	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	// Parsed back, the name is text in a cell and produced no elements.
	if doc.Find(".files-table img, .files-table script").Length() != 0 {
		t.Fatal("a filename created elements in the page")
	}
	var names []string
	doc.Find(".files-table .files-name").Each(func(_ int, s *goquery.Selection) {
		names = append(names, strings.TrimSpace(s.Text()))
	})
	if strings.Join(names, "|") != `📁 "><script>x/|`+evil {
		t.Errorf("names = %v", names)
	}
	// The activity list carries the same names and escapes them too.
	if doc.Find(".files-activity script, .files-activity img").Length() != 0 {
		t.Fatal("a filename created elements in the activity list")
	}
}

func TestUIDeleteEntry(t *testing.T) {
	h := start(t)
	h.upload(t, "ftp", "/dir/a.txt", "a")
	h.upload(t, "ftp", "/b.txt", "b")

	status, doc := uiFragment(t, h.Instance, http.MethodDelete, h.UI("/files/entry?path=/b.txt"))
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if h.VFS.Exists("/b.txt") {
		t.Error("the file survived")
	}
	// The fragment that comes back is the refreshed listing.
	if doc.Find("#files-refresh").Length() == 0 {
		t.Error("the delete must return the refreshed listing")
	}
	if strings.Contains(doc.Find(".files-table").Text(), "b.txt") {
		t.Error("the deleted file is still listed")
	}
	// But the log still says it was there, and that it went.
	if !strings.Contains(doc.Find(".files-activity").Text(), "deleted /b.txt") {
		t.Error("the deletion was not recorded")
	}

	// A whole directory goes recursively, because the button offers no choice.
	if status, _ := uiFragment(t, h.Instance, http.MethodDelete, h.UI("/files/entry?path=/dir")); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if h.VFS.Exists("/dir") {
		t.Error("the directory survived")
	}
}

func TestUIClearEmptiesTheTreeButKeepsTheLog(t *testing.T) {
	h := start(t)
	h.upload(t, "ftp", "/a/b.txt", "b")

	status, doc := uiFragment(t, h.Instance, http.MethodDelete, h.UI("/files/tree"))
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got := h.VFS.Stats(); got != (files.Stats{}) {
		t.Errorf("Stats() = %+v, want empty", got)
	}
	if doc.Find(".empty-state").Length() == 0 {
		t.Error("the cleared tab must show the empty state again")
	}
	// The activity list still explains what was there.
	if !strings.Contains(doc.Find(".files-activity").Text(), "uploaded /a/b.txt") {
		t.Errorf("the log was cleared with the tree: %q", doc.Find(".files-activity").Text())
	}
}

// The tab does not claim GET /events/{id}, so the core's generic inspector
// fills it in - which is where the raw record of a transfer is read.
func TestUIEventDetailFallsBackToTheGenericView(t *testing.T) {
	h := start(t)
	h.upload(t, "ftp", "/a.txt", "a")
	events := h.Events(store.Query{Plugin: files.PluginName})
	if len(events) == 0 {
		t.Fatal("no events")
	}
	status, body := h.GetBody(h.UI("/files/events/" + string(events[0].ID)))
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if !strings.Contains(body, "/a.txt") {
		t.Error("the generic inspector does not show the operation")
	}
}
