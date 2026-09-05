package main

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"os"
	"path"
	"path/filepath"
	"strings"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets/style.css
var styleCSS []byte

// Config is what the command line supplies.
type Config struct {
	Repo      string // repository root
	Out       string // output directory
	Providers string // optional `tommy providers --json` capture, for tests
}

// view is the data every template renders against.
type view struct {
	Site    *Site
	Page    *Page
	Nav     []NavSection
	API     *APIView
	Landing *LandingView
}

func (v view) Rel(to string) string { return relPath(v.Page.Path, to) }

func (v view) Active(p string) bool { return v.Page.Path == p }

func (v view) SourceURL() string {
	return fmt.Sprintf("%s/blob/%s/%s", repoURL, repoRef, v.Page.Src)
}

func (p *Page) IsLanding() bool { return p.Kind == KindLanding }

// LandingView is the landing page: rendered slices of files that already
// exist, plus cards built from `tommy providers --json` and docs/catalogue.md.
type LandingView struct {
	Lede           template.HTML
	Quickstart     template.HTML
	Install        template.HTML
	CatalogueTitle string
	CatalogueIntro template.HTML
	NotDoing       template.HTML
	Plugins        []PluginCard
	Nav            []NavSection
}

type PluginCard struct {
	Name, Title, Path string
	StandsFor         template.HTML
	Providers         []ProviderChip
}

type ProviderChip struct {
	Name, Path string
	ReachFor   template.HTML
}

// Build generates the whole site into cfg.Out and returns the site model, so
// tests can assert against the same structure the pages were written from.
func Build(cfg Config) (*Site, error) {
	plugins, err := loadProviders(cfg.Repo, cfg.Providers)
	if err != nil {
		return nil, err
	}
	site := &Site{Repo: cfg.Repo, Plugins: plugins}
	site.md = NewRenderer(site)
	if err := site.discover(); err != nil {
		return nil, err
	}
	site.buildNav()

	tmpl, err := template.New("site").Funcs(template.FuncMap{
		"lower":      strings.ToLower,
		"schemaType": schemaType,
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}

	// Bodies first: the landing page renders sections of files that other
	// pages also render, and every link is rewritten through the same map.
	for _, p := range site.Pages {
		switch p.Kind {
		case KindMarkdown:
			doc, err := site.md.Parse(p.Src, p.Path)
			if err != nil {
				return nil, err
			}
			body, err := doc.HTML()
			if err != nil {
				return nil, err
			}
			p.Body = body
			p.Heading = doc.Heading()
			if h := doc.Headings(); len(h) > 2 {
				p.TOC = h
			}
		case KindOpenAPI:
			spec, err := loadOpenAPI(cfg.Repo, p.Src)
			if err != nil {
				return nil, err
			}
			api, err := site.apiView(spec, p)
			if err != nil {
				return nil, err
			}
			p.Heading = spec.Info.Title
			for _, op := range api.Operations {
				p.TOC = append(p.TOC, Head{ID: op.ID, Text: op.Method + " " + op.Path})
			}
			if len(api.Schemas) > 0 {
				p.TOC = append(p.TOC, Head{ID: "schemas", Text: "Schemas"})
			}
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, "api", api); err != nil {
				return nil, err
			}
			p.Body = template.HTML(buf.String()) //nolint:gosec // rendered by our own template
		case KindLanding:
			landing, err := site.landing()
			if err != nil {
				return nil, err
			}
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, "landing", landing); err != nil {
				return nil, err
			}
			p.Body = template.HTML(buf.String()) //nolint:gosec // rendered by our own template
		}
	}

	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return nil, err
	}
	for _, p := range site.Pages {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "layout", view{Site: site, Page: p, Nav: site.Nav}); err != nil {
			return nil, fmt.Errorf("%s: %w", p.Path, err)
		}
		out := filepath.Join(cfg.Out, filepath.FromSlash(p.Path))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(cfg.Out, "style.css"), styleCSS, 0o644); err != nil {
		return nil, err
	}
	// GitHub Pages runs Jekyll over the artifact unless told not to.
	if err := os.WriteFile(filepath.Join(cfg.Out, ".nojekyll"), nil, 0o644); err != nil {
		return nil, err
	}
	return site, nil
}

// landing assembles the one page with any words of its own. Everything with
// substance in it is a rendered slice of README.md or docs/catalogue.md; the
// cards come from `tommy providers --json` crossed with docs/catalogue.md, so a
// provider missing from either fails the build.
func (s *Site) landing() (*LandingView, error) {
	readme, err := s.md.Parse("README.md", "index.html")
	if err != nil {
		return nil, err
	}
	cat, err := s.md.Parse("docs/catalogue.md", "index.html")
	if err != nil {
		return nil, err
	}

	v := &LandingView{CatalogueTitle: cat.Heading()}
	if v.Lede, err = readme.Preamble(); err != nil {
		return nil, err
	}
	if v.Quickstart, err = readme.Section("30-second quickstart"); err != nil {
		return nil, err
	}
	if v.Install, err = readme.Section("Installation"); err != nil {
		return nil, err
	}
	if v.CatalogueIntro, err = cat.Preamble(); err != nil {
		return nil, err
	}
	if v.NotDoing, err = cat.Section("What tommy deliberately will not do"); err != nil {
		return nil, err
	}

	pluginRows, err := cat.Table("Plugin")
	if err != nil {
		return nil, err
	}
	providerRows, err := cat.Table("Provider")
	if err != nil {
		return nil, err
	}
	standsFor := map[string]template.HTML{}
	for _, r := range pluginRows {
		standsFor[r.Key] = r.Text
	}
	for _, r := range providerRows {
		standsFor[r.Key] = r.Text
	}

	for _, pl := range s.Plugins {
		text, ok := standsFor[pl.Name]
		if !ok {
			return nil, fmt.Errorf("docs/catalogue.md has no row for plugin %q", pl.Name)
		}
		card := PluginCard{
			Name: pl.Name, Title: pl.Title, StandsFor: text,
			Path: path.Join("plugins", pl.Name+".html"),
		}
		for _, pr := range pl.Providers {
			key := pl.Name + "/" + pr.Name
			text, ok := standsFor[key]
			if !ok {
				return nil, fmt.Errorf("docs/catalogue.md has no row for provider %q", key)
			}
			card.Providers = append(card.Providers, ProviderChip{
				Name: pr.Name, ReachFor: text,
				Path: path.Join("plugins", pl.Name, pr.Name+".html"),
			})
		}
		v.Plugins = append(v.Plugins, card)
	}

	for _, sec := range s.Nav {
		if sec.Title == "API reference" || sec.Title == "Project documents" {
			v.Nav = append(v.Nav, sec)
		}
	}
	return v, nil
}
