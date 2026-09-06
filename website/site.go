package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// repoURL is where a file the site does not publish is linked instead.
const (
	repoURL = "https://github.com/can3p/tommy"
	repoRef = "main"
)

// Plugin and Provider mirror the shape of `tommy providers --json`. The
// generator never imports tommy; it consumes that output, so a change to
// tommy's own dependencies can never break this module.
type Plugin struct {
	Name        string     `json:"name"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Enabled     bool       `json:"enabled"`
	Providers   []Provider `json:"providers"`
}

type Provider struct {
	Name        string     `json:"name"`
	Plugin      string     `json:"plugin"`
	Description string     `json:"description"`
	Enabled     bool       `json:"enabled"`
	Listener    bool       `json:"listener"`
	Endpoints   []Endpoint `json:"endpoints"`
	Snippets    []Snippet  `json:"snippets"`
}

type Endpoint struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description"`
}

type Snippet struct {
	Title string `json:"title"`
	Lang  string `json:"lang"`
	Code  string `json:"code"`
}

// Kind distinguishes how a page's body is produced.
type Kind int

const (
	KindLanding Kind = iota
	KindMarkdown
	KindOpenAPI
)

// Page is one HTML file on the site.
type Page struct {
	Path     string // site-relative output path, e.g. "plugins/mail/mailjet.html"
	Title    string // <title> and nav label
	Heading  string // the source document's own H1, when it has one
	Kind     Kind
	Src      string // repo-relative source file ("" for the landing page)
	Plugin   string // set for plugin, provider and plugin API pages
	Provider string // set for provider pages
	Body     template.HTML
	TOC      []Head
}

type Head struct {
	ID, Text string
}

// NavItem is one entry in the sidebar.
type NavItem struct {
	Title string
	Path  string
	Sub   []NavItem
}

type NavSection struct {
	Title string
	Items []NavItem
}

// Site holds everything the templates need, plus the two maps that make link
// rewriting possible: repo path -> site path, and site path -> page.
type Site struct {
	Repo    string
	Plugins []Plugin
	Pages   []*Page
	Nav     []NavSection

	byPath     map[string]*Page
	repoToSite map[string]string

	// unpublished records repo-relative targets that no page renders, and the
	// source files that linked to them. The coverage test asserts the set.
	unpublished map[string][]string

	md *Renderer
}

func (s *Site) Page(sitePath string) *Page { return s.byPath[sitePath] }

// SitePathFor maps a repo-relative path to the page that renders it.
func (s *Site) SitePathFor(repoPath string) (string, bool) {
	p, ok := s.repoToSite[repoPath]
	return p, ok
}

// Unpublished returns repo paths linked from the documentation that the site
// does not render, sorted, with the files that linked to them.
func (s *Site) Unpublished() map[string][]string { return s.unpublished }

// loadProviders reads `tommy providers --json`, either from a file (tests) or
// by running the binary out of the repository.
func loadProviders(repo, file string) ([]Plugin, error) {
	var raw []byte
	var err error
	if file != "" {
		raw, err = os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read providers file: %w", err)
		}
	} else {
		cmd := exec.Command("go", "run", ".", "providers", "--json")
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "TOMMY_NO_UPDATE_CHECK=1")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		raw, err = cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go run . providers --json in %s: %w: %s", repo, err, stderr.String())
		}
	}
	var plugins []Plugin
	if err := json.Unmarshal(raw, &plugins); err != nil {
		return nil, fmt.Errorf("parse providers json: %w", err)
	}
	if len(plugins) == 0 {
		return nil, fmt.Errorf("providers json listed no plugins")
	}
	return plugins, nil
}

// docPages are the repository documents that get a page, with the short label
// the sidebar uses for each. Navigation labels only - every word of the page
// itself comes from the file.
var docPages = []struct {
	Src, Out, Nav string
}{
	{"docs/plan.md", "docs/plan.html", "Design brief"},
	{"docs/contracts.md", "docs/contracts.html", "Core contracts"},
	{"docs/implementation-plan.md", "docs/implementation-plan.html", "Implementation plan"},
	{"docs/lessons.md", "docs/lessons.html", "Lessons"},
	{"docs/docker.md", "docs/docker.html", "Docker"},
	{"docs/clients.md", "docs/clients.html", "Official SDKs"},
	{"docs/archive/history.md", "docs/history.html", "History"},
}

// discover builds the page list and the repo->site path map.
func (s *Site) discover() error {
	s.byPath = map[string]*Page{}
	s.repoToSite = map[string]string{}
	s.unpublished = map[string][]string{}

	add := func(p *Page, alsoRepoPaths ...string) {
		s.Pages = append(s.Pages, p)
		s.byPath[p.Path] = p
		if p.Src != "" {
			s.repoToSite[p.Src] = p.Path
		}
		for _, rp := range alsoRepoPaths {
			s.repoToSite[rp] = p.Path
		}
	}

	// The landing page also stands in for docs/catalogue.md: it renders that
	// file's own text and tables, so a link to it lands here.
	add(&Page{Path: "index.html", Title: "tommy", Kind: KindLanding}, "docs/catalogue.md")

	add(&Page{Path: "readme.html", Title: "README", Kind: KindMarkdown, Src: "README.md"})

	for _, d := range docPages {
		add(&Page{Path: d.Out, Title: d.Nav, Kind: KindMarkdown, Src: d.Src})
	}

	// Plugins and providers, in the order `tommy providers --json` reports them.
	for _, pl := range s.Plugins {
		src := path.Join("plugins", pl.Name, "README.md")
		if err := s.mustExist(src); err != nil {
			return err
		}
		add(&Page{
			Path: path.Join("plugins", pl.Name+".html"), Title: pl.Name,
			Kind: KindMarkdown, Src: src, Plugin: pl.Name,
		}, path.Join("plugins", pl.Name))

		for _, pr := range pl.Providers {
			psrc := path.Join("plugins", pl.Name, "providers", pr.Name, "README.md")
			if err := s.mustExist(psrc); err != nil {
				return err
			}
			add(&Page{
				Path: path.Join("plugins", pl.Name, pr.Name+".html"), Title: pr.Name,
				Kind: KindMarkdown, Src: psrc, Plugin: pl.Name, Provider: pr.Name,
			}, path.Join("plugins", pl.Name, "providers", pr.Name))
		}
	}

	// One API reference page per generated OpenAPI document.
	add(&Page{Path: "api/events.html", Title: "events API", Kind: KindOpenAPI, Src: "docs/openapi.json"})
	specs, err := filepath.Glob(filepath.Join(s.Repo, "docs", "openapi-*.json"))
	if err != nil {
		return err
	}
	sort.Strings(specs)
	for _, spec := range specs {
		base := filepath.Base(spec)
		plugin := strings.TrimSuffix(strings.TrimPrefix(base, "openapi-"), ".json")
		add(&Page{
			Path: path.Join("api", plugin+".html"), Title: plugin + " API",
			Kind: KindOpenAPI, Src: path.Join("docs", base), Plugin: plugin,
		})
	}
	return nil
}

func (s *Site) mustExist(repoPath string) error {
	if _, err := os.Stat(filepath.Join(s.Repo, filepath.FromSlash(repoPath))); err != nil {
		return fmt.Errorf("%s: %w", repoPath, err)
	}
	return nil
}

func (s *Site) buildNav() {
	plugins := NavSection{Title: "Plugins"}
	for _, pl := range s.Plugins {
		item := NavItem{Title: pl.Name, Path: path.Join("plugins", pl.Name+".html")}
		for _, pr := range pl.Providers {
			item.Sub = append(item.Sub, NavItem{
				Title: pr.Name,
				Path:  path.Join("plugins", pl.Name, pr.Name+".html"),
			})
		}
		plugins.Items = append(plugins.Items, item)
	}

	api := NavSection{Title: "API reference"}
	docs := NavSection{Title: "Project documents"}
	for _, p := range s.Pages {
		switch {
		case p.Kind == KindOpenAPI:
			api.Items = append(api.Items, NavItem{Title: p.Title, Path: p.Path})
		case p.Kind == KindMarkdown && strings.HasPrefix(p.Path, "docs/"):
			docs.Items = append(docs.Items, NavItem{Title: p.Title, Path: p.Path})
		}
	}

	s.Nav = []NavSection{
		{Title: "tommy", Items: []NavItem{
			{Title: "Home", Path: "index.html"},
			{Title: "README", Path: "readme.html"},
		}},
		plugins, api, docs,
	}
}

// relPath renders target as a link relative to the page at from. Slash-based
// throughout: these are URLs, not filesystem paths.
func relPath(from, to string) string {
	fromDir := path.Dir(from)
	var f []string
	if fromDir != "." {
		f = strings.Split(fromDir, "/")
	}
	t := strings.Split(to, "/")
	i := 0
	for i < len(f) && i < len(t)-1 && f[i] == t[i] {
		i++
	}
	var parts []string
	for j := i; j < len(f); j++ {
		parts = append(parts, "..")
	}
	parts = append(parts, t[i:]...)
	return strings.Join(parts, "/")
}
