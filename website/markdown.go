package main

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var (
	ctxSrc  = parser.NewContextKey() // repo-relative path of the file being parsed
	ctxPage = parser.NewContextKey() // site path of the page it is rendered into
)

// Renderer parses repository Markdown and rewrites its links on the way
// through. It is deliberately not configured with html.WithUnsafe: no
// document in this repository contains raw HTML, and captured content must
// never reach a page as markup.
type Renderer struct {
	md   goldmark.Markdown
	site *Site
}

func NewRenderer(s *Site) *Renderer {
	r := &Renderer{site: s}
	r.md = goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
			parser.WithASTTransformers(
				util.Prioritized(&linkTransformer{r: r}, 100),
			),
		),
	)
	return r
}

// Doc is one parsed Markdown file, kept as an AST so that a page can render
// the whole thing and the landing page can render named sections of it.
type Doc struct {
	Src    string
	root   ast.Node
	source []byte
	r      *Renderer
}

func (r *Renderer) Parse(repoPath, pagePath string) (*Doc, error) {
	source, err := os.ReadFile(filepath.Join(r.site.Repo, filepath.FromSlash(repoPath)))
	if err != nil {
		return nil, err
	}
	pc := parser.NewContext()
	pc.Set(ctxSrc, repoPath)
	pc.Set(ctxPage, pagePath)
	root := r.md.Parser().Parse(text.NewReader(source), parser.WithContext(pc))
	return &Doc{Src: repoPath, root: root, source: source, r: r}, nil
}

func (d *Doc) render(nodes ...ast.Node) (template.HTML, error) {
	var buf bytes.Buffer
	for _, n := range nodes {
		if err := d.r.md.Renderer().Render(&buf, d.source, n); err != nil {
			return "", err
		}
	}
	return template.HTML(buf.String()), nil //nolint:gosec // repository documentation, rendered without raw HTML
}

// HTML renders the whole document.
func (d *Doc) HTML() (template.HTML, error) { return d.render(d.root) }

// Heading returns the text of the document's first level-1 heading.
func (d *Doc) Heading() string {
	for n := d.root.FirstChild(); n != nil; n = n.NextSibling() {
		if h, ok := n.(*ast.Heading); ok && h.Level == 1 {
			return nodeText(h, d.source)
		}
	}
	return ""
}

// Headings lists the level-2 headings, for the on-page contents list.
func (d *Doc) Headings() []Head {
	var out []Head
	for n := d.root.FirstChild(); n != nil; n = n.NextSibling() {
		h, ok := n.(*ast.Heading)
		if !ok || h.Level != 2 {
			continue
		}
		id, _ := h.AttributeString("id")
		idStr, _ := id.([]byte)
		out = append(out, Head{ID: string(idStr), Text: nodeText(h, d.source)})
	}
	return out
}

// Section renders one level-2 section - its heading and everything under it -
// so the landing page can show a slice of a file that already exists instead
// of a paraphrase of it.
func (d *Doc) Section(title string) (template.HTML, error) {
	var nodes []ast.Node
	found := false
	for n := d.root.FirstChild(); n != nil; n = n.NextSibling() {
		if h, ok := n.(*ast.Heading); ok && h.Level <= 2 {
			if found {
				break
			}
			found = h.Level == 2 && strings.EqualFold(nodeText(h, d.source), title)
			if !found {
				continue
			}
		}
		if found {
			nodes = append(nodes, n)
		}
	}
	if !found {
		return "", fmt.Errorf("%s: no section %q", d.Src, title)
	}
	return d.render(nodes...)
}

// SectionBody is Section without the heading itself, for when the page
// supplies its own.
func (d *Doc) SectionBody(title string) (template.HTML, error) {
	h, err := d.Section(title)
	if err != nil {
		return "", err
	}
	s := string(h)
	if i := strings.Index(s, "</h2>"); i >= 0 {
		s = s[i+len("</h2>"):]
	}
	return template.HTML(s), nil //nolint:gosec // already-rendered documentation HTML
}

// Preamble renders everything above the first level-2 heading, minus the
// document's own H1.
func (d *Doc) Preamble() (template.HTML, error) {
	var nodes []ast.Node
	for n := d.root.FirstChild(); n != nil; n = n.NextSibling() {
		if h, ok := n.(*ast.Heading); ok {
			if h.Level == 1 {
				continue
			}
			break
		}
		nodes = append(nodes, n)
	}
	return d.render(nodes...)
}

// Row is one row of a Markdown table, reduced to what the landing page needs:
// the key in the first cell and the rendered prose of the second.
type Row struct {
	Key  string
	Text template.HTML
}

// Table finds the table whose header row starts with the given cell text and
// returns its rows.
func (d *Doc) Table(firstHeader string) ([]Row, error) {
	var out []Row
	var walkErr error
	err := ast.Walk(d.root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		tbl, ok := n.(*extast.Table)
		if !ok {
			return ast.WalkContinue, nil
		}
		head := tbl.FirstChild()
		if head == nil || !strings.EqualFold(nodeText(head.FirstChild(), d.source), firstHeader) {
			return ast.WalkSkipChildren, nil
		}
		for row := head.NextSibling(); row != nil; row = row.NextSibling() {
			cells := []ast.Node{}
			for c := row.FirstChild(); c != nil; c = c.NextSibling() {
				cells = append(cells, c)
			}
			if len(cells) < 2 {
				continue
			}
			var buf bytes.Buffer
			for c := cells[1].FirstChild(); c != nil; c = c.NextSibling() {
				if err := d.r.md.Renderer().Render(&buf, d.source, c); err != nil {
					walkErr = err
					return ast.WalkStop, nil
				}
			}
			out = append(out, Row{
				Key:  nodeText(cells[0], d.source),
				Text: template.HTML(buf.String()), //nolint:gosec // repository documentation
			})
		}
		return ast.WalkStop, nil
	})
	if err != nil {
		return nil, err
	}
	if walkErr != nil {
		return nil, walkErr
	}
	if out == nil {
		return nil, fmt.Errorf("%s: no table with first header %q", d.Src, firstHeader)
	}
	return out, nil
}

func nodeText(n ast.Node, source []byte) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := c.(type) {
		case *ast.Text:
			b.Write(t.Segment.Value(source))
		case *ast.String:
			b.Write(t.Value)
		case *ast.CodeSpan:
			for cc := t.FirstChild(); cc != nil; cc = cc.NextSibling() {
				if seg, ok := cc.(*ast.Text); ok {
					b.Write(seg.Segment.Value(source))
				}
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(b.String())
}

// --- link rewriting -------------------------------------------------------

type linkTransformer struct{ r *Renderer }

func (t *linkTransformer) Transform(doc *ast.Document, _ text.Reader, pc parser.Context) {
	src, _ := pc.Get(ctxSrc).(string)
	page, _ := pc.Get(ctxPage).(string)
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Link:
			v.Destination = []byte(t.r.site.ResolveLink(src, page, string(v.Destination)))
		case *ast.Image:
			v.Destination = []byte(t.r.site.ResolveLink(src, page, string(v.Destination)))
		}
		return ast.WalkContinue, nil
	})
}

// ResolveLink turns a link found in the file at srcRepoPath, rendered into the
// page at pagePath, into a link that works on the generated site.
//
//   - external, mail and in-page anchors are left exactly as they are;
//   - a repo-relative path the site publishes becomes a relative site link;
//   - anything else becomes a link to the file on GitHub, and is recorded so
//     the coverage test can assert the whole set of them.
func (s *Site) ResolveLink(srcRepoPath, pagePath, dest string) string {
	if dest == "" || strings.HasPrefix(dest, "#") {
		return dest
	}
	if i := strings.Index(dest, ":"); i > 0 && !strings.ContainsAny(dest[:i], "/.") {
		return dest // http:, https:, mailto:, ...
	}
	if strings.HasPrefix(dest, "//") {
		return dest
	}

	target, frag := dest, ""
	if i := strings.IndexAny(target, "#?"); i >= 0 {
		target, frag = target[:i], target[i:]
	}
	if target == "" {
		return dest
	}
	repoPath := path.Clean(path.Join(path.Dir(srcRepoPath), target))
	if strings.HasPrefix(repoPath, "..") {
		repoPath = strings.TrimPrefix(path.Clean("/"+repoPath), "/")
	}
	repoPath = strings.TrimSuffix(repoPath, "/")

	if sitePath, ok := s.repoToSite[repoPath]; ok {
		return relPath(pagePath, sitePath) + frag
	}

	s.unpublished[repoPath] = appendOnce(s.unpublished[repoPath], srcRepoPath)
	kind := "blob"
	if fi, err := os.Stat(filepath.Join(s.Repo, filepath.FromSlash(repoPath))); err == nil && fi.IsDir() {
		kind = "tree"
	}
	return fmt.Sprintf("%s/%s/%s/%s%s", repoURL, kind, repoRef, repoPath, frag)
}

func appendOnce(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	list = append(list, v)
	sort.Strings(list)
	return list
}

// RenderText renders a Markdown string that did not come from a file of its
// own - an OpenAPI description, generated from a Go doc comment - with the
// same link rewriting applied.
func (r *Renderer) RenderText(src, repoPath, pagePath string) (template.HTML, error) {
	pc := parser.NewContext()
	pc.Set(ctxSrc, repoPath)
	pc.Set(ctxPage, pagePath)
	source := []byte(src)
	root := r.md.Parser().Parse(text.NewReader(source), parser.WithContext(pc))
	var buf bytes.Buffer
	if err := r.md.Renderer().Render(&buf, source, root); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil //nolint:gosec // repository documentation
}
