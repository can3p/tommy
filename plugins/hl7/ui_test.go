package hl7_test

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

func TestUITabIsRegistered(t *testing.T) {
	in := start(t)
	doc := uiDoc(t, in, in.UIURL)

	var hrefs []string
	doc.Find("nav.tabs a").Each(func(_ int, s *goquery.Selection) {
		if href, ok := s.Attr("href"); ok {
			hrefs = append(hrefs, s.Text()+"="+href)
		}
	})
	if !strings.Contains(strings.Join(hrefs, " "), "HL7=/ui/hl7/") {
		t.Errorf("tab bar = %v, want an HL7 tab pointing at /ui/hl7/", hrefs)
	}
}

// An empty tab is exactly when somebody needs to know how to fill it, so the
// how-to-test panel starts open.
func TestUIEmptyState(t *testing.T) {
	in := start(t)
	doc := uiDoc(t, in, in.UI("/hl7/"))

	panel := doc.Find("details.how-to-test")
	if panel.Length() != 1 {
		t.Fatalf("found %d how-to-test panels, want 1", panel.Length())
	}
	if _, open := panel.Attr("open"); !open {
		t.Error("the how-to-test panel is closed on an empty tab")
	}
	if doc.Find("code").FilterFunction(func(_ int, s *goquery.Selection) bool {
		return strings.Contains(s.Text(), "/fake-hl7/messages")
	}).Length() == 0 {
		t.Error("the empty tab does not carry a runnable snippet")
	}
}

// The segment tree is the reason this tab exists rather than the generic event
// view: every field at its own position, with the names the dictionary knows.
func TestUISegmentTree(t *testing.T) {
	in := start(t)
	injectFixture(t, in, "adt_a01.hl7")

	doc := uiDoc(t, in, in.UI("/hl7/"))

	segments := texts(doc, "details.hl7-segment > summary")
	if len(segments) != 5 {
		t.Fatalf("rendered %d segments, want 5: %v", len(segments), segments)
	}
	if !strings.Contains(segments[0], "MSH") || !strings.Contains(segments[0], "Message Header") {
		t.Errorf("first segment summary = %q, want MSH with its dictionary name", segments[0])
	}

	paths := texts(doc, "th.hl7-path")
	for _, want := range []string{"MSH-1", "MSH-9", "MSH-9.1", "MSH-9.2", "PID-5", "PID-5.1", "PID-5.2", "PV1-3.4.2"} {
		if !contains(paths, want) {
			t.Errorf("no row for %s; paths were %v", want, paths)
		}
	}

	// The value shown next to a path is the decoded one.
	row := doc.Find("tr.hl7-row").FilterFunction(func(_ int, s *goquery.Selection) bool {
		return strings.TrimSpace(s.Find("th.hl7-path").Text()) == "PID-5.1"
	})
	if got := strings.TrimSpace(row.Find("td.hl7-value").Text()); got != "DOE" {
		t.Errorf("PID-5.1 renders as %q, want DOE", got)
	}
	if got := strings.TrimSpace(row.Find("td.hl7-name").Text()); got != "" {
		t.Errorf("component rows should not claim a field name, got %q", got)
	}

	// Field names come from the dictionary where it has one.
	field := doc.Find("tr.hl7-row").FilterFunction(func(_ int, s *goquery.Selection) bool {
		return strings.TrimSpace(s.Find("th.hl7-path").Text()) == "PID-5"
	})
	if got := strings.TrimSpace(field.Find("td.hl7-name").Text()); got != "Patient Name" {
		t.Errorf("PID-5 is labeled %q, want Patient Name", got)
	}

	// The escape sequences are decoded for display: \T\ is an ampersand and
	// \.br\ is a line break, not literal backslashes on the page.
	note := doc.Find("tr.hl7-row").FilterFunction(func(_ int, s *goquery.Selection) bool {
		return strings.TrimSpace(s.Find("th.hl7-path").Text()) == "NTE-3"
	})
	if got := strings.TrimSpace(note.Find("td.hl7-value").Text()); got != "Fever & chills reported\nReview in 2 weeks" {
		t.Errorf("NTE-3 renders as %q, want the escapes decoded", got)
	}

	// Empty positions are listed once at the end rather than as a row each.
	if empties := texts(doc, "tr.hl7-empty-row"); len(empties) == 0 || !strings.Contains(empties[0], "MSH-8") {
		t.Errorf("empty fields = %v, want them named compactly", empties)
	}
}

// Repetitions must be visibly distinct. Showing PID-3 as one tilde-joined
// string is exactly the failure this plugin exists to avoid.
func TestUIRepetitionsAreDistinct(t *testing.T) {
	in := start(t)
	injectFixture(t, in, "adt_a01.hl7")
	doc := uiDoc(t, in, in.UI("/hl7/"))

	paths := texts(doc, "th.hl7-path")
	for _, want := range []string{"PID-3", "PID-3[1]", "PID-3[1].1", "PID-3[2]", "PID-3[2].1"} {
		if !contains(paths, want) {
			t.Errorf("no row for %s; paths were %v", want, paths)
		}
	}
	reps := texts(doc, "tr.hl7-kind-repetition td.hl7-value")
	if !contains(reps, "MRN12345^^^HOSP^MR") || !contains(reps, "999887777^^^SSA^SS") {
		t.Errorf("repetition rows = %v, want the two identifiers apart", reps)
	}
}

// A message that declared its own delimiters is the one another parser is most
// likely to be getting wrong, so the tab says so out loud.
func TestUIFlagsNonStandardSeparators(t *testing.T) {
	in := start(t)
	injectFixture(t, in, "custom_separators.hl7")
	doc := uiDoc(t, in, in.UI("/hl7/"))

	badges := texts(doc, ".hl7-badges .badge")
	found := false
	for _, b := range badges {
		if strings.HasPrefix(b, "separators !") {
			found = true
		}
	}
	if !found {
		t.Errorf("badges = %v, want one naming the declared separators", badges)
	}
	paths := texts(doc, "th.hl7-path")
	if !contains(paths, "PID-3[2].1") || !contains(paths, "PV1-3.2.2") {
		t.Errorf("the tree was not built with the message's own separators: %v", paths)
	}
}

// A headerless fragment still renders, and says what the parser had to work
// around rather than pretending the message was fine.
func TestUIShowsParseIssues(t *testing.T) {
	in := start(t)
	injectFixture(t, in, "no_msh.hl7")
	doc := uiDoc(t, in, in.UI("/hl7/"))

	issues := texts(doc, ".hl7-issues li")
	if len(issues) != 1 || !strings.Contains(issues[0], "no-header") {
		t.Errorf("issues = %v, want the missing header called out", issues)
	}
}

func TestUIListAndSelection(t *testing.T) {
	in := start(t)
	adt := injectFixture(t, in, "adt_a01.hl7")
	oru := injectFixture(t, in, "oru_r01.hl7")

	doc := uiDoc(t, in, in.UI("/hl7/"))
	types := texts(doc, ".hl7-msg-type")
	if len(types) != 2 || types[0] != "ORU^R01" || types[1] != "ADT^A01" {
		t.Errorf("list = %v, want the ORU first", types)
	}
	// With nothing chosen the newest message is open, so the pane is never
	// blank next to a full list.
	if got := texts(doc, ".hl7-detail-head h2"); len(got) != 1 || got[0] != "ORU^R01 · MSG00002" {
		t.Errorf("detail heading = %v, want the newest message %s", got, oru.ID)
	}

	// A deep link opens that message as a whole page.
	deep := uiDoc(t, in, in.UI("/hl7/messages/"+string(adt.ID)))
	if got := texts(deep, ".hl7-detail-head h2"); len(got) != 1 || got[0] != "ADT^A01 · MSG00001" {
		t.Errorf("deep link heading = %v", got)
	}
	if deep.Find("nav.tabs").Length() == 0 {
		t.Error("a deep link returned a bare fragment rather than the whole page")
	}

	// htmx gets a fragment.
	status, frag := uiFragment(t, in, http.MethodGet, in.UI("/hl7/messages/"+string(adt.ID)))
	if status != http.StatusOK {
		t.Fatalf("fragment status = %d", status)
	}
	if frag.Find("nav.tabs").Length() != 0 {
		t.Error("an htmx request got the whole page rather than a fragment")
	}

	status, _ = uiFragment(t, in, http.MethodGet, in.UI("/hl7/messages/nope"))
	if status != http.StatusNotFound {
		t.Errorf("unknown message status = %d, want 404", status)
	}
}

func TestUISearchAndClear(t *testing.T) {
	in := start(t)
	injectFixture(t, in, "adt_a01.hl7")
	injectFixture(t, in, "oru_r01.hl7")

	_, frag := uiFragment(t, in, http.MethodGet, in.UI("/hl7/list?search=ALICE"))
	if got := texts(frag, ".hl7-msg-type"); len(got) != 1 || got[0] != "ORU^R01" {
		t.Errorf("search results = %v, want just the ORU", got)
	}

	status, cleared := uiFragment(t, in, http.MethodDelete, in.UI("/hl7/events"))
	if status != http.StatusOK {
		t.Fatalf("clear status = %d", status)
	}
	if got := texts(cleared, ".hl7-msg-type"); len(got) != 0 {
		t.Errorf("after clearing, the list still shows %v", got)
	}
	if cleared.Find("details.how-to-test[open]").Length() != 1 {
		t.Error("clearing left the tab without an open how-to-test panel")
	}
}

// HL7 carries patient names and free-text notes written by whatever system is
// under test. None of it may reach the page as markup.
//
// The assertions are against the parsed document, not against the HTML source:
// grepping the response for "<script" proves nothing, since the escaped text
// contains no such thing either way. What matters is that the browser's own
// parser found no script element and no event handler attribute.
func TestUIHostileMessageRendersInert(t *testing.T) {
	in := start(t)
	injectFixture(t, in, "hostile.hl7")

	for _, url := range []string{in.UI("/hl7/"), in.UI("/hl7/list")} {
		doc := uiDoc(t, in, url)

		doc.Find("script").Each(func(_ int, s *goquery.Selection) {
			if strings.Contains(s.Text(), "alert(1)") {
				t.Errorf("%s: an injected script tag survived into the page", url)
			}
		})
		doc.Find("*").Each(func(_ int, s *goquery.Selection) {
			for _, attr := range []string{"onerror", "onload", "onclick"} {
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
	}

	// And the text is still shown - escaped, not dropped, because seeing what
	// was actually sent is the point.
	doc := uiDoc(t, in, in.UI("/hl7/"))
	if !contains(texts(doc, "td.hl7-value"), `<img src=x onerror="alert(1)">`) {
		t.Error("the hostile NTE value was not shown as text")
	}
	if !contains(texts(doc, ".hl7-msg-preview"), `<script>alert(1)</script>^"><script>alert(1)</script> · MSH PID NTE`) {
		t.Errorf("the hostile patient name was not shown as text in the list: %v", texts(doc, ".hl7-msg-preview"))
	}
}
