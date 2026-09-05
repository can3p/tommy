package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestSite(t *testing.T, repo string) *Site {
	t.Helper()
	s := &Site{
		Repo: repo,
		repoToSite: map[string]string{
			"README.md":                             "readme.html",
			"docs/catalogue.md":                     "index.html",
			"docs/contracts.md":                     "docs/contracts.html",
			"docs/archive/history.md":               "docs/history.html",
			"docs/openapi.json":                     "api/events.html",
			"plugins/mail/README.md":                "plugins/mail.html",
			"plugins/mail":                          "plugins/mail.html",
			"plugins/mail/providers/smtp/README.md": "plugins/mail/smtp.html",
			"plugins/files/providers/ftp/README.md": "plugins/files/ftp.html",
		},
		unpublished: map[string][]string{},
		byPath:      map[string]*Page{},
	}
	s.md = NewRenderer(s)
	return s
}

// Link rewriting is the fiddly part of the generator: the documentation is
// full of paths that are correct relative to the file they live in and wrong
// everywhere else.
func TestResolveLink(t *testing.T) {
	s := newTestSite(t, "..")
	const gh = "https://github.com/can3p/tommy"

	cases := []struct {
		name          string
		src, page, in string
		want          string
	}{
		{"external link is untouched", "README.md", "readme.html",
			"https://github.com/can3p/kleiner", "https://github.com/can3p/kleiner"},
		{"scheme-relative is untouched", "README.md", "readme.html", "//example.com/x", "//example.com/x"},
		{"mailto is untouched", "README.md", "readme.html", "mailto:a@example.com", "mailto:a@example.com"},
		{"in-page anchor is untouched", "README.md", "readme.html", "#api-surface", "#api-surface"},
		{"empty is untouched", "README.md", "readme.html", "", ""},

		{"sibling document from the root", "README.md", "readme.html",
			"./docs/contracts.md", "docs/contracts.html"},
		{"a plugin README, linked from docs/catalogue.md", "docs/catalogue.md", "index.html",
			"../plugins/mail/README.md", "plugins/mail.html"},
		{"a provider README, linked from docs/catalogue.md", "docs/catalogue.md", "index.html",
			"../plugins/files/providers/ftp/README.md", "plugins/files/ftp.html"},
		{"two levels up, from a plugin README to the API reference",
			"plugins/mail/README.md", "plugins/mail.html",
			"../../docs/openapi.json", "../api/events.html"},
		{"four levels up, from a provider README to the API reference",
			"plugins/mail/providers/smtp/README.md", "plugins/mail/smtp.html",
			"../../../../docs/openapi.json", "../../api/events.html"},
		{"a fragment survives the rewrite", "README.md", "readme.html",
			"./docs/contracts.md#the-event", "docs/contracts.html#the-event"},
		{"a query string survives the rewrite", "README.md", "readme.html",
			"./docs/contracts.md?x=1", "docs/contracts.html?x=1"},
		{"an archived document keeps its new home", "docs/contracts.md", "docs/contracts.html",
			"./archive/history.md", "history.html"},
		{"docs/catalogue.md is the landing page", "docs/contracts.md", "docs/contracts.html",
			"./catalogue.md", "../index.html"},
		{"a directory the site publishes", "docs/catalogue.md", "index.html",
			"../plugins/mail", "plugins/mail.html"},

		{"a file the site does not publish goes to GitHub", "README.md", "readme.html",
			"./tommy.toml", gh + "/blob/main/tommy.toml"},
		{"a directory the site does not publish goes to GitHub's tree view",
			"docs/clients.md", "docs/clients.html",
			"../clienthelp", gh + "/tree/main/clienthelp"},
		{"an unpublished target keeps its fragment", "docs/clients.md", "docs/clients.html",
			"../clienthelp/clienthelp.go#L10", gh + "/blob/main/clienthelp/clienthelp.go#L10"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := s.ResolveLink(c.src, c.page, c.in); got != c.want {
				t.Errorf("ResolveLink(%q, %q, %q) = %q, want %q", c.src, c.page, c.in, got, c.want)
			}
		})
	}

	// Everything that could not be published is recorded, with the file that
	// linked to it, so the coverage test can assert the whole set.
	if from := s.Unpublished()["tommy.toml"]; len(from) != 1 || from[0] != "README.md" {
		t.Errorf("unpublished links not recorded: %v", s.Unpublished())
	}
}

func TestRelPath(t *testing.T) {
	cases := []struct{ from, to, want string }{
		{"index.html", "style.css", "style.css"},
		{"index.html", "plugins/mail.html", "plugins/mail.html"},
		{"plugins/mail.html", "style.css", "../style.css"},
		{"plugins/mail/smtp.html", "style.css", "../../style.css"},
		{"plugins/mail/smtp.html", "plugins/mail.html", "../mail.html"},
		{"plugins/mail/smtp.html", "plugins/files/ftp.html", "../files/ftp.html"},
		{"docs/plan.html", "api/events.html", "../api/events.html"},
		{"docs/plan.html", "docs/lessons.html", "lessons.html"},
	}
	for _, c := range cases {
		if got := relPath(c.from, c.to); got != c.want {
			t.Errorf("relPath(%q, %q) = %q, want %q", c.from, c.to, got, c.want)
		}
	}
}

// The rewriting has to survive the round trip through goldmark, including
// links inside tables and reference-style links.
func TestMarkdownRendering(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "plugins", "mail", "providers", "smtp"), 0o755); err != nil {
		t.Fatal(err)
	}
	md := "# `smtp` mail provider\n\n" +
		"See [the plugin](../../README.md) and [the events API](../../../../docs/openapi.json).\n\n" +
		"| Flag | What |\n|---|---|\n| `--smtp-port` | see [contracts][c] |\n\n" +
		"[c]: ../../../../docs/contracts.md#listeners\n\n" +
		"## What it is\n\nA real SMTP server.\n\n" +
		"## What it's for\n\n```bash\ncurl -s smtp://127.0.0.1:1025\n```\n"
	src := "plugins/mail/providers/smtp/README.md"
	if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(src)), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}

	s := newTestSite(t, repo)
	doc, err := s.md.Parse(src, "plugins/mail/smtp.html")
	if err != nil {
		t.Fatal(err)
	}
	html, err := doc.HTML()
	if err != nil {
		t.Fatal(err)
	}
	got := string(html)

	for _, want := range []string{
		`href="../mail.html"`,                        // relative link, one directory up
		`href="../../api/events.html"`,               // the OpenAPI document as a page
		`href="../../docs/contracts.html#listeners"`, // a reference-style link with a fragment
		"<table>", "<th>Flag</th>", // GFM tables are enabled
		`<code class="language-bash">`, // fenced code keeps its language
		`<h2 id="what-it-is">`,         // headings get anchors
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML does not contain %q:\n%s", want, got)
		}
	}

	if h := doc.Heading(); h != "smtp mail provider" {
		t.Errorf("Heading() = %q", h)
	}
	heads := doc.Headings()
	if len(heads) != 2 || heads[0].Text != "What it is" || heads[0].ID != "what-it-is" {
		t.Errorf("Headings() = %+v", heads)
	}

	section, err := doc.Section("What it is")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(section), "A real SMTP server.") ||
		strings.Contains(string(section), "curl") {
		t.Errorf("Section() spilled past its own heading: %s", section)
	}
	if _, err := doc.Section("No Such Section"); err == nil {
		t.Error("Section() of a missing heading should fail the build")
	}
}

// Raw HTML must never reach a page: the documents in this repository contain
// none, and the renderer is configured so that any that appeared could not.
func TestRawHTMLIsNotPassedThrough(t *testing.T) {
	repo := t.TempDir()
	src := "docs/evil.md"
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(src)),
		[]byte("<script>alert(1)</script>\n\nplain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newTestSite(t, repo)
	doc, err := s.md.Parse(src, "docs/evil.html")
	if err != nil {
		t.Fatal(err)
	}
	html, err := doc.HTML()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(html), "<script>") {
		t.Errorf("raw HTML reached the page: %s", html)
	}
}
