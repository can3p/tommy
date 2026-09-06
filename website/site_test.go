package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// The site is built once for the whole package: the build shells out to
// `go run . providers --json` in the repository, which is the point - the
// coverage assertions below are made against what tommy actually ships, not
// against a fixture that can go stale.
var (
	buildOnce sync.Once
	builtSite *Site
	builtDir  string
	buildErr  error
)

func build(t *testing.T) (*Site, string) {
	t.Helper()
	buildOnce.Do(func() {
		builtDir, buildErr = os.MkdirTemp("", "tommy-site")
		if buildErr != nil {
			return
		}
		builtSite, buildErr = Build(Config{Repo: "..", Out: builtDir})
	})
	if buildErr != nil {
		t.Fatalf("build the site: %v", buildErr)
	}
	return builtSite, builtDir
}

func TestMain(m *testing.M) {
	code := m.Run()
	if builtDir != "" {
		_ = os.RemoveAll(builtDir)
	}
	os.Exit(code)
}

// Every plugin and every provider `tommy providers --json` reports must have
// its own README rendered on the site. A provider added in a later wave that
// nobody remembered to link fails here rather than going missing quietly.
func TestEveryPluginAndProviderHasItsREADMERendered(t *testing.T) {
	site, dir := build(t)

	check := func(t *testing.T, sitePath, wantSrc, wantWord string) {
		t.Helper()
		page := site.Page(sitePath)
		if page == nil {
			t.Fatalf("no page at %s", sitePath)
		}
		if page.Src != wantSrc {
			t.Errorf("%s renders %s, want %s", sitePath, page.Src, wantSrc)
		}
		if page.Heading == "" {
			t.Errorf("%s: the source document has no H1", wantSrc)
		}
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(sitePath)))
		if err != nil {
			t.Fatalf("read %s: %v", sitePath, err)
		}
		// Rule 12's three sections are what makes a README user-facing.
		for _, want := range []string{wantWord, "What it is", "What it&#39;s for", "How to test it for real"} {
			if !strings.Contains(string(body), want) {
				t.Errorf("%s (from %s) does not contain %q", sitePath, wantSrc, want)
			}
		}
	}

	if len(site.Plugins) == 0 {
		t.Fatal("no plugins reported")
	}
	for _, pl := range site.Plugins {
		t.Run(pl.Name, func(t *testing.T) {
			check(t, path.Join("plugins", pl.Name+".html"),
				path.Join("plugins", pl.Name, "README.md"), pl.Name)
			if len(pl.Providers) == 0 {
				t.Fatalf("plugin %s reports no providers", pl.Name)
			}
			for _, pr := range pl.Providers {
				t.Run(pr.Name, func(t *testing.T) {
					check(t, path.Join("plugins", pl.Name, pr.Name+".html"),
						path.Join("plugins", pl.Name, "providers", pr.Name, "README.md"), pr.Name)
				})
			}
		})
	}
}

// The landing page is built from `tommy providers --json` crossed with
// docs/catalogue.md, so every plugin and provider must appear there too.
func TestLandingPageListsEveryProvider(t *testing.T) {
	site, dir := build(t)
	body, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pl := range site.Plugins {
		if !strings.Contains(string(body), `href="plugins/`+pl.Name+`.html"`) {
			t.Errorf("the landing page does not link plugin %s", pl.Name)
		}
		for _, pr := range pl.Providers {
			want := fmt.Sprintf(`href="plugins/%s/%s.html"`, pl.Name, pr.Name)
			if !strings.Contains(string(body), want) {
				t.Errorf("the landing page does not link provider %s/%s", pl.Name, pr.Name)
			}
		}
	}
}

// Every OpenAPI document in docs/ gets a reference page with its operations.
func TestEveryOpenAPIDocumentIsRendered(t *testing.T) {
	site, dir := build(t)
	specs, err := filepath.Glob(filepath.Join("..", "docs", "openapi*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) == 0 {
		t.Fatal("no OpenAPI documents found")
	}
	for _, spec := range specs {
		repoPath := "docs/" + filepath.Base(spec)
		sitePath, ok := site.SitePathFor(repoPath)
		if !ok {
			t.Errorf("%s is not rendered anywhere on the site", repoPath)
			continue
		}
		raw, err := os.ReadFile(spec)
		if err != nil {
			t.Fatal(err)
		}
		var doc OpenAPI
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(sitePath)))
		if err != nil {
			t.Fatal(err)
		}
		for p := range doc.Paths {
			if !strings.Contains(string(body), ">"+p+"<") {
				t.Errorf("%s: path %s is missing from %s", repoPath, p, sitePath)
			}
		}
		for name := range doc.Components.Schemas {
			if !strings.Contains(string(body), `id="`+schemaID(name)+`"`) {
				t.Errorf("%s: schema %s is missing from %s", repoPath, name, sitePath)
			}
		}
	}
}

// Every link the site emits must resolve: to a file it wrote, and to an
// anchor that exists on that page.
func TestEveryInternalLinkResolves(t *testing.T) {
	_, dir := build(t)
	problems, checked, err := checkLinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if checked < 100 {
		t.Fatalf("only %d links checked, the site cannot be complete", checked)
	}
	for _, p := range problems {
		t.Errorf("%s", p)
	}
}

// Repository files that are linked from the documentation but not published
// as pages become links to GitHub. The set is asserted so that a new one
// shows up here as a decision to make rather than as a 404.
func TestLinksToUnpublishedFilesAreKnown(t *testing.T) {
	site, _ := build(t)
	want := []string{
		"LICENSE",    // linked from the README; GitHub renders it better than we would
		"clienthelp", // a Go package, explained by docs/clients.md
		"clienthelp/clienthelp.go",
		"docker-compose.yml", // the compose stack, read as a file like tommy.toml
		"docker/tommy.toml",  // the tiny config that stack mounts over the image's
		"tommy.toml",         // the commented example config, read as a file
	}
	var got []string
	for repoPath := range site.Unpublished() {
		got = append(got, repoPath)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("links to unpublished files changed:\n got %v\nwant %v\n"+
			"either publish the file as a page or add it to this list deliberately", got, want)
	}
}

// A provider that exists in the binary but has no README fails the build,
// which is the whole point of the gate.
func TestBuildFailsForAProviderWithNoREADME(t *testing.T) {
	site, _ := build(t)
	plugins := append([]Plugin(nil), site.Plugins...)
	plugins[0].Providers = append(append([]Provider(nil), plugins[0].Providers...),
		Provider{Name: "nosuchprovider", Plugin: plugins[0].Name})
	raw, err := json.Marshal(plugins)
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(fixture, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Build(Config{Repo: "..", Out: t.TempDir(), Providers: fixture})
	if err == nil {
		t.Fatal("a provider with no README built happily; the drift gate is not working")
	}
	if !strings.Contains(err.Error(), "nosuchprovider") {
		t.Errorf("error does not name the missing provider: %v", err)
	}
}

// --- the link checker, used by the test above and tested itself -----------

var linkRE = regexp.MustCompile(`(?:href|src)="([^"]*)"`)

// checkLinks walks the generated site and reports every internal link that
// does not resolve to a file, or to an anchor on the page it names.
func checkLinks(dir string) (problems []string, checked int, err error) {
	pages := map[string]string{}
	err = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".html") {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		pages[filepath.ToSlash(rel)] = string(body)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	ids := map[string]map[string]bool{}
	idRE := regexp.MustCompile(`id="([^"]+)"`)
	for name, body := range pages {
		set := map[string]bool{}
		for _, m := range idRE.FindAllStringSubmatch(body, -1) {
			set[m[1]] = true
		}
		ids[name] = set
	}
	names := make([]string, 0, len(pages))
	for name := range pages {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, m := range linkRE.FindAllStringSubmatch(pages[name], -1) {
			href := m[1]
			checked++
			if href == "" || strings.HasPrefix(href, "//") {
				continue
			}
			if i := strings.Index(href, ":"); i > 0 && !strings.ContainsAny(href[:i], "/.") {
				continue // external scheme
			}
			target, frag := href, ""
			if i := strings.Index(href, "#"); i >= 0 {
				target, frag = href[:i], href[i+1:]
			}
			page := name
			if target != "" {
				page = path.Clean(path.Join(path.Dir(name), target))
				if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(page))); err != nil {
					problems = append(problems, fmt.Sprintf("%s links to %s, which the site does not contain", name, href))
					continue
				}
			}
			if frag != "" && !ids[page][frag] {
				problems = append(problems, fmt.Sprintf("%s links to %s, but %s has no such anchor", name, href, page))
			}
		}
	}
	return problems, checked, nil
}

func TestCheckLinksCatchesABrokenLink(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.html", `<a href="b.html">ok</a><a href="b.html#top">ok</a>`+
		`<a href="gone.html">bad</a><a href="b.html#nope">bad</a>`+
		`<a href="https://example.com/x.html">external</a>`)
	write("b.html", `<h1 id="top">b</h1>`)

	problems, checked, err := checkLinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if checked != 5 {
		t.Errorf("checked %d links, want 5", checked)
	}
	if len(problems) != 2 {
		t.Fatalf("want 2 problems, got %d: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], "gone.html") || !strings.Contains(problems[1], "no such anchor") {
		t.Errorf("unexpected problems: %v", problems)
	}
}
