package as2_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/as2"
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
	in := startWithFixtureIdentity(t)
	doc := uiDoc(t, in, in.UIURL)

	var hrefs []string
	doc.Find("nav.tabs a").Each(func(_ int, s *goquery.Selection) {
		if href, ok := s.Attr("href"); ok {
			hrefs = append(hrefs, s.Text()+"="+href)
		}
	})
	if !strings.Contains(strings.Join(hrefs, " "), "AS2=/ui/as2/") {
		t.Errorf("tab bar = %v, want an AS2 tab pointing at /ui/as2/", hrefs)
	}
}

// An empty tab is exactly when somebody needs to know how to fill it, so the
// how-to-test panel starts open - and so does the certificate panel, because
// importing that certificate is the first thing anyone has to do.
func TestUIEmptyStateOpensWhatIsNeeded(t *testing.T) {
	in := startWithFixtureIdentity(t)
	doc := uiDoc(t, in, in.UI("/as2/"))

	if doc.Find(".empty-state").Length() == 0 {
		t.Error("an empty tab shows no empty state")
	}
	if _, open := doc.Find("details.how-to-test").Attr("open"); !open {
		t.Error("the how-to-test panel is closed on an empty tab")
	}
	if _, open := doc.Find("details.as2-identity").Attr("open"); !open {
		t.Error("the certificate panel is closed on an empty tab; importing it is the first thing to do")
	}
	if !strings.Contains(doc.Find("details.as2-identity").Text(), "-----BEGIN CERTIFICATE-----") {
		t.Error("the certificate panel does not show the PEM a partner has to import")
	}
}

func TestUIRendersACapturedExchange(t *testing.T) {
	in := startWithFixtureIdentity(t)
	resp := post(t, in, "signed_encrypted.mime", signedReceipt("sha256"))
	_ = resp.Body.Close()
	in.WaitForEvents(1, storeQueryAll(), 2*time.Second)

	doc := uiDoc(t, in, in.UI("/as2/"))
	detail := doc.Find("#as2-detail")
	if detail.Length() == 0 {
		t.Fatal("no detail pane rendered for the newest message")
	}
	text := detail.Text()

	for _, want := range []string{
		"PARTNER",              // the AS2 identifiers
		"ISA*00*",              // the decrypted EDI, inlined
		"aes-256-cbc",          // what it was encrypted with
		"Received-Content-MIC", // the digest panel
		"automatic-action/MDN-sent-automatically; processed", // the disposition
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the detail pane does not mention %q", want)
		}
	}

	// The layer table is the point of the tab: it says what the onion was.
	labels := texts(doc, "#as2-detail table.as2-layers th")
	for _, want := range []string{"Encrypted", "Signed", "Payload"} {
		if !contains(labels, want) {
			t.Errorf("layer table = %v, want a %q row", labels, want)
		}
	}
}

// The distinction the whole plugin turns on has to be visible: a valid
// signature with no configured partner must not read as a verified sender.
func TestUIDoesNotOverstateWhatASignatureProved(t *testing.T) {
	in := startWithFixtureIdentity(t)
	resp := post(t, in, "signed.mime", signedReceipt("sha256"))
	_ = resp.Body.Close()
	in.WaitForEvents(1, storeQueryAll(), 2*time.Second)

	doc := uiDoc(t, in, in.UI("/as2/"))
	assurance := strings.Join(texts(doc, ".as2-assurance"), " ")
	if !strings.Contains(assurance, "unproven") {
		t.Errorf("assurance = %q, want it to say who signed is unproven with no partner certificate", assurance)
	}
	badges := strings.Join(texts(doc, "#as2-detail .as2-badges"), " ")
	if strings.Contains(badges, "signed by partner") {
		t.Errorf("badges = %q, want no claim of a partner match", badges)
	}
	if !strings.Contains(badges, "signer unverified") {
		t.Errorf("badges = %q, want the signer marked unverified", badges)
	}
}

// Everything captured is untrusted. An AS2 name, a subject and an EDI payload
// full of markup must all come back as text, never as elements.
func TestUIEscapesHostileContent(t *testing.T) {
	in := startWithFixtureIdentity(t)

	hostile := http.Header{}
	hostile.Set("AS2-From", `<script>alert('from')</script>`)
	hostile.Set("Subject", `<img src=x onerror=alert('subject')>`)
	hostile.Set("Disposition-Notification-To", "as2@partner.example")
	resp := post(t, in, "plain.mime", hostile)
	_ = resp.Body.Close()
	in.WaitForEvents(1, storeQueryAll(), 2*time.Second)

	doc := uiDoc(t, in, in.UI("/as2/"))

	// Assert against parsed markup, not substrings: a substring search cannot
	// tell an escaped tag from a live one.
	if n := doc.Find("#as2-detail script").Length(); n != 0 {
		t.Errorf("%d script elements came from captured content", n)
	}
	if n := doc.Find("#as2-detail img").Length(); n != 0 {
		t.Errorf("%d img elements came from captured content", n)
	}
	text := doc.Find("#as2-detail").Text()
	if !strings.Contains(text, "<script>alert('from')</script>") {
		t.Error("the hostile AS2-From is not shown as text")
	}
	if !strings.Contains(text, "<img src=x onerror=alert('subject')>") {
		t.Error("the hostile subject is not shown as text")
	}
}

// A value containing CRLF must not be able to write its own MDN fields. This is
// the one place captured text reaches a protocol boundary rather than a
// template.
func TestMDNRejectsHeaderInjection(t *testing.T) {
	r, _ := receiverWith(t, as2.IdentityConfig{})
	req := request(t, "plain.mime", signedReceipt("sha256"))
	req.Header.Set("Message-ID", "<a@b>\r\nDisposition: automatic-action/MDN-sent-automatically; processed")
	req.Header.Set("AS2-To", "TOMMY\r\nReceived-Content-MIC: forged, sha1")
	res := receive(t, r, req)
	body := string(res.Response.Body)

	// The real property is about the machine-readable part: a captured value
	// must not be able to add a field there. Assert on its field names rather
	// than on substrings, which cannot tell an injected field from the same
	// words appearing inside a value.
	fields := dispositionFields(t, body)
	want := []string{
		"Reporting-UA", "Original-Recipient", "Final-Recipient",
		"Original-Message-ID", "Disposition", "Received-Content-MIC",
	}
	if len(fields) != len(want) {
		t.Fatalf("disposition-notification fields = %v, want exactly %v", fields, want)
	}
	for i, name := range want {
		if fields[i] != name {
			t.Errorf("field %d = %q, want %q (fields: %v)", i, fields[i], name, fields)
		}
	}
	if strings.Contains(body, "\r\nReceived-Content-MIC: forged") {
		t.Errorf("a captured value injected a Received-Content-MIC:\n%s", body)
	}
	// And the CRLF is gone from the values themselves.
	if strings.Contains(res.Message.MDN.Disposition, "\r") || strings.Contains(res.Message.MDN.Disposition, "\n") {
		t.Error("the recorded disposition carries a line break")
	}
}

// dispositionFields returns the field names of the message/disposition-
// notification part, in order.
func dispositionFields(t *testing.T, body string) []string {
	t.Helper()
	const marker = "Content-Type: message/disposition-notification"
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no disposition-notification part in:\n%s", body)
	}
	rest := body[i:]
	j := strings.Index(rest, "\r\n\r\n")
	if j < 0 {
		t.Fatalf("disposition-notification part has no body:\n%s", rest)
	}
	var out []string
	for _, line := range strings.Split(rest[j+4:], "\r\n") {
		if line == "" || strings.HasPrefix(line, "--") {
			break
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue // a folded continuation, not a new field
		}
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out = append(out, name)
	}
	return out
}

func TestUIDetailDeepLinkAndFragment(t *testing.T) {
	in := startWithFixtureIdentity(t)
	resp := post(t, in, "signed.mime", signedReceipt("sha256"))
	_ = resp.Body.Close()
	events := in.WaitForEvents(1, storeQueryAll(), 2*time.Second)
	id := string(events[0].ID)

	// A deep link renders the whole tab, so a URL can be pasted into a bug
	// report.
	doc := uiDoc(t, in, in.UI("/as2/messages/"+id))
	if doc.Find("nav.tabs").Length() == 0 {
		t.Error("a deep link did not render the page shell")
	}

	// htmx gets a bare fragment.
	status, frag := uiFragment(t, in, http.MethodGet, in.UI("/as2/messages/"+id))
	if status != http.StatusOK {
		t.Fatalf("fragment status = %d", status)
	}
	if frag.Find("nav.tabs").Length() != 0 {
		t.Error("an htmx fragment carried the page shell")
	}
	if frag.Find("#as2-detail").Length() == 0 {
		t.Error("the fragment has no detail pane")
	}
}

func TestUIClearEmptiesTheTab(t *testing.T) {
	in := startWithFixtureIdentity(t)
	resp := post(t, in, "signed.mime", nil)
	_ = resp.Body.Close()
	in.WaitForEvents(1, storeQueryAll(), 2*time.Second)

	status, doc := uiFragment(t, in, http.MethodDelete, in.UI("/as2/events"))
	if status != http.StatusOK {
		t.Fatalf("clear status = %d", status)
	}
	if doc.Find(".empty-state").Length() == 0 {
		t.Error("clearing did not bring back the empty state")
	}
	if got := in.Events(storeQueryAll()); len(got) != 0 {
		t.Errorf("store still holds %d events", len(got))
	}
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
