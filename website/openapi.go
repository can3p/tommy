package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The subset of OpenAPI 3.1 that tommy's generated documents actually use.
// They are produced from the server's own route table, so their shape is
// stable; anything they do not emit is deliberately not modeled here.
type OpenAPI struct {
	OpenAPI string `json:"openapi"`
	Info    struct {
		Title       string `json:"title"`
		Version     string `json:"version"`
		Description string `json:"description"`
	} `json:"info"`
	Servers []struct {
		URL         string `json:"url"`
		Description string `json:"description"`
	} `json:"servers"`
	Paths      map[string]map[string]Operation `json:"paths"`
	Components struct {
		Schemas map[string]*Schema `json:"schemas"`
	} `json:"components"`
}

type Operation struct {
	OperationID string              `json:"operationId"`
	Summary     string              `json:"summary"`
	Parameters  []Param             `json:"parameters"`
	Responses   map[string]Response `json:"responses"`
}

type Param struct {
	Name        string  `json:"name"`
	In          string  `json:"in"`
	Description string  `json:"description"`
	Required    bool    `json:"required"`
	Schema      *Schema `json:"schema"`
}

type Response struct {
	Description string               `json:"description"`
	Content     map[string]MediaType `json:"content"`
}

type MediaType struct {
	Schema *Schema `json:"schema"`
}

type Schema struct {
	Ref                  string             `json:"$ref"`
	Type                 string             `json:"type"`
	Format               string             `json:"format"`
	ContentEncoding      string             `json:"contentEncoding"`
	Items                *Schema            `json:"items"`
	Properties           map[string]*Schema `json:"properties"`
	Required             []string           `json:"required"`
	AdditionalProperties *Schema            `json:"additionalProperties"`
}

// APIView is what the API reference template renders.
type APIView struct {
	Title       string
	Version     string
	Description template.HTML
	Servers     []Server
	Operations  []OperationView
	Schemas     []SchemaView
}

type Server struct{ URL, Description string }

type OperationView struct {
	ID         string
	Method     string
	Path       string
	Summary    string
	Parameters []Param
	Responses  []ResponseView
}

type ResponseView struct {
	Status      string
	Description string
	Content     []ContentView
}

type ContentView struct {
	MediaType string
	Type      template.HTML
}

type SchemaView struct {
	ID         string
	Name       string
	Type       string
	Properties []PropertyView
}

type PropertyView struct {
	Name     string
	Type     template.HTML
	Required bool
}

var methodOrder = []string{"get", "head", "post", "put", "patch", "delete", "options", "trace"}

func loadOpenAPI(repo, repoPath string) (*OpenAPI, error) {
	raw, err := os.ReadFile(filepath.Join(repo, filepath.FromSlash(repoPath)))
	if err != nil {
		return nil, err
	}
	var doc OpenAPI
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", repoPath, err)
	}
	return &doc, nil
}

// apiView turns a spec into the flat, ordered form the template renders.
// Descriptions in these documents come from Go doc comments, so they are
// rendered as Markdown like every other piece of prose on the site.
func (s *Site) apiView(doc *OpenAPI, page *Page) (*APIView, error) {
	v := &APIView{Title: doc.Info.Title, Version: doc.Info.Version}
	if doc.Info.Description != "" {
		html, err := s.md.RenderText(doc.Info.Description, page.Src, page.Path)
		if err != nil {
			return nil, err
		}
		v.Description = html
	}
	for _, srv := range doc.Servers {
		v.Servers = append(v.Servers, Server{URL: srv.URL, Description: srv.Description})
	}

	paths := make([]string, 0, len(doc.Paths))
	for p := range doc.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		item := doc.Paths[p]
		for _, m := range methodOrder {
			op, ok := item[m]
			if !ok {
				continue
			}
			ov := OperationView{
				ID:         "op-" + strings.ToLower(m) + "-" + slug(p),
				Method:     strings.ToUpper(m),
				Path:       p,
				Summary:    op.Summary,
				Parameters: op.Parameters,
			}
			codes := make([]string, 0, len(op.Responses))
			for c := range op.Responses {
				codes = append(codes, c)
			}
			sort.Strings(codes)
			for _, c := range codes {
				r := op.Responses[c]
				rv := ResponseView{Status: c, Description: r.Description}
				media := make([]string, 0, len(r.Content))
				for mt := range r.Content {
					media = append(media, mt)
				}
				sort.Strings(media)
				for _, mt := range media {
					rv.Content = append(rv.Content, ContentView{
						MediaType: mt,
						Type:      schemaType(r.Content[mt].Schema),
					})
				}
				ov.Responses = append(ov.Responses, rv)
			}
			v.Operations = append(v.Operations, ov)
		}
	}

	names := make([]string, 0, len(doc.Components.Schemas))
	for n := range doc.Components.Schemas {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sc := doc.Components.Schemas[n]
		sv := SchemaView{ID: schemaID(n), Name: n, Type: sc.Type}
		props := make([]string, 0, len(sc.Properties))
		for p := range sc.Properties {
			props = append(props, p)
		}
		sort.Strings(props)
		required := map[string]bool{}
		for _, r := range sc.Required {
			required[r] = true
		}
		for _, p := range props {
			sv.Properties = append(sv.Properties, PropertyView{
				Name: p, Type: schemaType(sc.Properties[p]), Required: required[p],
			})
		}
		v.Schemas = append(v.Schemas, sv)
	}
	return v, nil
}

// schemaType describes a schema in one readable phrase, linking any $ref to
// the schema's own entry further down the page.
func schemaType(s *Schema) template.HTML {
	if s == nil {
		return ""
	}
	if s.Ref != "" {
		name := s.Ref[strings.LastIndex(s.Ref, "/")+1:]
		return template.HTML(fmt.Sprintf(`<a href="#%s"><code>%s</code></a>`,
			template.HTMLEscapeString(schemaID(name)), template.HTMLEscapeString(name))) //nolint:gosec // both halves escaped
	}
	switch s.Type {
	case "array":
		return template.HTML("array of ") + schemaType(s.Items) //nolint:gosec // constant prefix
	case "object":
		if s.AdditionalProperties != nil {
			return template.HTML("object of ") + schemaType(s.AdditionalProperties) //nolint:gosec // constant prefix
		}
	}
	out := s.Type
	if out == "" {
		out = "any"
	}
	if s.Format != "" {
		out += " (" + s.Format + ")"
	}
	if s.ContentEncoding != "" {
		out += ", " + s.ContentEncoding + "-encoded"
	}
	return template.HTML("<code>" + template.HTMLEscapeString(out) + "</code>") //nolint:gosec // escaped
}

func schemaID(name string) string { return "schema-" + slug(name) }

func slug(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
