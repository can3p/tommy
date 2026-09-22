package s3_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/can3p/tommy/core/server/ui"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/plugins/s3"
)

func s3UIDoc(t *testing.T, h *apiHarness, target string) *goquery.Document {
	t.Helper()
	resp := h.Get(target)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", target, resp.StatusCode)
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func s3UIFragment(t *testing.T, h *apiHarness, method, target string) (int, *goquery.Document) {
	t.Helper()
	req, _ := http.NewRequest(method, target, nil)
	req.Header.Set("HX-Request", "true")
	resp := h.Do(req)
	defer func() { _ = resp.Body.Close() }()
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, doc
}

func TestS3UITabEmptyStateAndTemplates(t *testing.T) {
	h := startS3(t)
	doc := s3UIDoc(t, h, h.UIURL)
	if link := doc.Find(`nav.tabs a[href="/ui/s3/"]`); link.Length() != 1 || strings.TrimSpace(link.Text()) != "S3" {
		t.Fatal("S3 tab is not registered")
	}

	empty := s3UIDoc(t, h, h.UI("/s3/"))
	if empty.Find(".empty-state").Length() != 1 || !strings.Contains(empty.Find(".empty-state").Text(), "No S3 buckets yet") {
		t.Fatalf("empty state = %q", empty.Find(".empty-state").Text())
	}
	panel := empty.Find("details.how-to-test")
	if panel.Length() != 1 {
		t.Fatal("how-to-test panel is missing")
	}
	if _, open := panel.Attr("open"); !open {
		t.Fatal("how-to-test panel should open while the catalog is empty")
	}
	if !strings.Contains(panel.Text(), "/fake-s3") || !strings.Contains(panel.Text(), "curl") {
		t.Fatalf("how-to panel does not use Shell.Info: %q", panel.Text())
	}
	if _, err := ui.PluginTemplates(h.plugin.Templates()); err != nil {
		t.Fatalf("embedded template parse: %v", err)
	}
}

func TestS3UIBucketPrefixNavigationAndLiveRefresh(t *testing.T) {
	h := startS3(t)
	h.put(t, "alpha", "a//b.txt", "b", s3.PutOptions{Headers: s3.ContentHeaders{ContentType: "text/plain"}})
	h.put(t, "alpha", "a/child/c.txt", "ccc", s3.PutOptions{})
	h.put(t, "beta", "top.txt", "top", s3.PutOptions{})

	doc := s3UIDoc(t, h, h.UI("/s3/?bucket=alpha&prefix="+url.QueryEscape("a/")+"&delimiter="+url.QueryEscape("/")))
	if doc.Find(".s3-buckets li").Length() != 2 || doc.Find(".s3-buckets li.selected .s3-bucket-name").Text() != "alpha" {
		t.Fatalf("bucket selector = %q", doc.Find(".s3-buckets").Text())
	}
	var prefixes []string
	doc.Find(".s3-prefix").Each(func(_ int, item *goquery.Selection) { prefixes = append(prefixes, strings.TrimSpace(item.Text())) })
	if strings.Join(prefixes, "|") != "a//|a/child/" {
		t.Fatalf("prefixes = %v", prefixes)
	}
	prefixURL, _ := doc.Find(".s3-prefix").First().Attr("href")
	parsed, err := url.Parse(prefixURL)
	if err != nil || parsed.Query().Get("prefix") != "a//" {
		t.Fatalf("prefix URL = %q (%v)", prefixURL, err)
	}

	deep := s3UIDoc(t, h, h.UI("/s3/?bucket=alpha&prefix="+url.QueryEscape("a//")+"&delimiter="+url.QueryEscape("/")))
	var crumbs []string
	deep.Find(".s3-crumbs .s3-crumb").Each(func(_ int, item *goquery.Selection) { crumbs = append(crumbs, strings.TrimSpace(item.Text())) })
	if strings.Join(crumbs, "|") != "/|a|(empty)" {
		t.Fatalf("crumbs = %v", crumbs)
	}
	current := deep.Find(".s3-crumb.current")
	if current.Length() != 1 || current.Text() != "(empty)" {
		t.Fatalf("current crumb = %q", current.Text())
	}
	if deep.Find(".s3-object-row .s3-key").Text() != "a//b.txt" {
		t.Fatalf("exact key was changed: %q", deep.Find(".s3-object-row .s3-key").Text())
	}

	trigger, ok := deep.Find("#s3-refresh").Attr("hx-trigger")
	if !ok {
		t.Fatal("live refresh trigger is missing")
	}
	for _, typ := range s3.EventTypes {
		if !strings.Contains(trigger, "sse:"+typ) {
			t.Errorf("trigger %q does not include %s", trigger, typ)
		}
	}
	refresh, _ := deep.Find("#s3-refresh").Attr("hx-get")
	refreshURL, _ := url.Parse(refresh)
	if refreshURL.Query().Get("bucket") != "alpha" || refreshURL.Query().Get("prefix") != "a//" {
		t.Errorf("refresh loses location: %q", refresh)
	}
}

func TestS3UIActivityMissingBucketAndActions(t *testing.T) {
	h := startS3(t)
	h.put(t, "bucket", "one.txt", "one", s3.PutOptions{})
	doc := s3UIDoc(t, h, h.UI("/s3/?bucket=missing"))
	if doc.Find(".s3-missing").Length() != 1 || !strings.Contains(doc.Find(".s3-missing").Text(), "missing") {
		t.Fatalf("missing-bucket fallback = %q", doc.Find(".s3-missing").Text())
	}
	rows := doc.Find(".s3-activity tbody tr")
	if rows.Length() != 2 || !strings.Contains(rows.First().Text(), "stored s3://bucket/one.txt") {
		t.Fatalf("activity = %q", doc.Find(".s3-activity").Text())
	}
	eventHref, _ := rows.First().Find("a.s3-event").Attr("href")
	if !strings.HasPrefix(eventHref, "/ui/events/") {
		t.Fatalf("event link = %q", eventHref)
	}

	deleteTarget := h.UI("/s3/object?bucket=bucket&key=one.txt")
	status, deleted := s3UIFragment(t, h, http.MethodDelete, deleteTarget)
	if status != http.StatusOK || h.plugin.Store().Stats().Objects != 0 {
		t.Fatalf("delete = %d, stats %+v", status, h.plugin.Store().Stats())
	}
	if !strings.Contains(deleted.Find(".s3-activity").Text(), "deleted s3://bucket/one.txt") {
		t.Fatal("UI deletion did not emit an activity event")
	}
	events := h.Events(store.Query{Plugin: s3.PluginName, Type: s3.EventObjectDelete})
	if len(events) != 1 || events[0].Raw.Method != http.MethodDelete {
		t.Fatalf("UI delete raw event = %+v", events)
	}

	status, cleared := s3UIFragment(t, h, http.MethodDelete, h.UI("/s3/buckets"))
	if status != http.StatusOK || h.plugin.Store().Stats().Buckets != 0 || cleared.Find(".empty-state").Length() != 1 {
		t.Fatalf("clear = %d, stats %+v", status, h.plugin.Store().Stats())
	}
}

func TestS3UIEscapesHostileBucketKeyAndMetadata(t *testing.T) {
	h := startS3(t)
	const bucket = `"><script data-owned=bucket>x</script>`
	const key = `<img src=x onerror=alert(1)>.txt`
	const metaKey = `<svg onload=alert(2)>`
	const metaValue = `</dd><iframe src=evil>`
	h.put(t, bucket, key, "hostile", s3.PutOptions{
		Metadata: map[string]string{metaKey: metaValue},
		Headers:  s3.ContentHeaders{ContentType: `<script>type</script>`},
	})

	status, body := h.GetBody(h.UI("/s3/?bucket=" + url.QueryEscape(bucket)))
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Find(".s3-browser script, .s3-browser img, .s3-browser svg, .s3-browser iframe").Length() != 0 {
		t.Fatal("hostile S3 state injected markup into the parsed DOM")
	}
	if doc.Find(".s3-bucket-name").Text() != bucket || doc.Find(".s3-key").Text() != key {
		t.Fatalf("hostile text did not survive as text: bucket=%q key=%q", doc.Find(".s3-bucket-name").Text(), doc.Find(".s3-key").Text())
	}
	if !strings.Contains(doc.Find(".s3-metadata").Text(), metaKey) || !strings.Contains(doc.Find(".s3-metadata").Text(), metaValue) {
		t.Fatalf("metadata text = %q", doc.Find(".s3-metadata").Text())
	}
	download, _ := doc.Find(".s3-key").Attr("href")
	if strings.Contains(download, "<") || strings.Contains(download, ">") || strings.Contains(download, " ") {
		t.Fatalf("hostile download URL is not escaped: %q", download)
	}
}

func TestS3UIEventDetailUsesGenericInspector(t *testing.T) {
	h := startS3(t)
	h.put(t, "bucket", "a.txt", "a", s3.PutOptions{})
	events := h.Events(store.Query{Plugin: s3.PluginName})
	if len(events) == 0 {
		t.Fatal("no events")
	}
	status, body := h.GetBody(h.UI("/s3/events/" + string(events[0].ID)))
	if status != http.StatusOK || !strings.Contains(body, events[0].Summary.Title) {
		t.Fatalf("generic event detail = %d %q", status, body)
	}
}

func TestS3UIPrefixNavigationDoesNotCleanKeys(t *testing.T) {
	h := startS3(t)
	session := h.session("fake")
	if _, err := session.CreateBucket(context.Background(), "bucket"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a/../b", "a/./c", "a//d"} {
		if _, err := session.PutObject(context.Background(), "bucket", key, strings.NewReader(key), s3.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	doc := s3UIDoc(t, h, h.UI("/s3/?bucket=bucket&prefix="+url.QueryEscape("a/")+"&delimiter="+url.QueryEscape("/")))
	var got []string
	doc.Find(".s3-prefix").Each(func(_ int, item *goquery.Selection) { got = append(got, strings.TrimSpace(item.Text())) })
	if strings.Join(got, "|") != "a/../|a/./|a//" {
		t.Fatalf("exact dot/repeated-slash prefixes = %v", got)
	}
}
