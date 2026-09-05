package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/server/ui"
)

// APIVersion is what the description calls itself. It is the version of the
// interface, not of the binary: putting the build version here would rewrite
// the checked-in document on every release for no change anyone can act on.
const APIVersion = "v1"

// specDescription is the top of the document, and the place to say what it does
// not cover.
const specDescription = `Tommy captures what an application sent to the services it talks to - mail
providers, SMS gateways, file transfer, chat webhooks, EDI partners - and this
is the API for reading it back.

Every captured message is an event. The generic routes below serve all of them;
each plugin adds routes that serve its own content type in a shape that suits
it, and both are described here.

This describes tommy's own API only. The fake vendor endpoints - Mailjet's
send API, Twilio's, Slack's - live on a separate ingress listener and are
deliberately absent: they are those vendors' specifications, not tommy's, and a
partial copy of somebody else's API is worse than none. Run ` + "`tommy providers`" + `
for those, or read each provider's own documentation.

This document is generated from the running server's route table and the
plugins' own endpoint declarations. It cannot describe a route that does not
exist, and CI fails if the checked-in copy stops matching the code.`

// Spec is an OpenAPI 3.1 document.
type Spec struct {
	OpenAPI string       `json:"openapi"`
	Info    SpecInfo     `json:"info"`
	Servers []SpecServer `json:"servers,omitempty"`
	Tags    []SpecTag    `json:"tags,omitempty"`
	// Security is an empty list on purpose, and it is not an oversight: this
	// API has no authentication, deliberately, because tommy is a local
	// development tool holding data an application under test sent to itself.
	// Saying so explicitly is what stops a reader assuming a scheme was
	// forgotten.
	Security   []map[string][]string `json:"security"`
	Paths      map[string]PathItem   `json:"paths"`
	Components SpecComponents        `json:"components"`
}

// SpecInfo is the document's own header.
type SpecInfo struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
}

// SpecServer is one base URL the paths are relative to.
type SpecServer struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

// SpecTag groups operations in a rendered document.
type SpecTag struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// SpecComponents holds the reusable schemas.
type SpecComponents struct {
	Schemas map[string]*Schema `json:"schemas,omitempty"`
}

// PathItem is the set of operations on one path.
type PathItem struct {
	Get    *Operation `json:"get,omitempty"`
	Post   *Operation `json:"post,omitempty"`
	Put    *Operation `json:"put,omitempty"`
	Patch  *Operation `json:"patch,omitempty"`
	Delete *Operation `json:"delete,omitempty"`
}

// Operation is one method on one path.
type Operation struct {
	OperationID string              `json:"operationId,omitempty"`
	Summary     string              `json:"summary,omitempty"`
	Tags        []string            `json:"tags,omitempty"`
	Parameters  []SpecParameter     `json:"parameters,omitempty"`
	Responses   map[string]Response `json:"responses"`
}

// SpecParameter is one path or query parameter.
type SpecParameter struct {
	Name        string  `json:"name"`
	In          string  `json:"in"`
	Description string  `json:"description,omitempty"`
	Required    bool    `json:"required,omitempty"`
	Schema      *Schema `json:"schema,omitempty"`
}

// Response is one status code's answer.
type Response struct {
	Description string               `json:"description"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

// MediaType is one content type of a response.
type MediaType struct {
	Schema *Schema `json:"schema,omitempty"`
}

// SpecOptions configures a generated description.
type SpecOptions struct {
	// Registry supplies the plugin routes. Nil describes the core alone.
	Registry *plugin.Registry
	// ServerURL is what paths are relative to. The checked-in document uses
	// the relative "/api/v1"; a running server names itself, so a reader can
	// paste a request straight out of the rendered page.
	ServerURL string
}

// coreEndpoints are the routes the core mounts itself. They are declared here,
// beside the handlers, for the same reason a plugin declares its own: a route
// and its description drift the moment they live in different files.
func coreEndpoints() []plugin.Endpoint {
	eventFilters := append(plugin.CoreListParams(),
		plugin.Param{Name: "plugin", Description: "Only events from this plugin, such as mail."},
		plugin.Param{Name: "include_raw", Description: "Include the raw request body, which listings omit because it can be megabytes.", Type: "boolean"},
	)
	return []plugin.Endpoint{
		{Method: "GET", Path: "/health", Description: "Liveness, uptime, the enabled plugins and how many events are held.",
			Response: HealthInfo{}},
		{Method: "GET", Path: "/plugins", Description: "Every plugin and provider, with its endpoints and its snippets rendered against the ports this instance actually bound.",
			Response: []plugin.PluginInfo{}},
		{Method: "GET", Path: "/openapi.json", Description: "This document, generated from the running server's own routes and describing only the plugins this instance enabled.",
			Produces: "application/json"},

		{Method: "GET", Path: "/events", Description: "Every captured event, newest first, whatever plugin captured it.",
			Query: eventFilters, Response: []ui.EventJSON{}},
		{Method: "GET", Path: "/events/{id}", Description: "One event in full, raw request body included.",
			Response: ui.EventJSON{}},
		{Method: "GET", Path: "/events/stream", Description: "The same events as they arrive, as Server-Sent Events. Each event produces a JSON frame carrying the event without its raw body, and a frame named after the event type carrying just the id.",
			Query: eventFilters, Produces: "text/event-stream"},
		{Method: "DELETE", Path: "/events", Description: "Clear captured events, all of them or one plugin's.",
			Query:  []plugin.Param{{Name: "plugin", Description: "Clear only this plugin's events."}},
			Status: http.StatusNoContent},
		{Method: "DELETE", Path: "/events/{id}", Description: "Delete one captured event.",
			Status: http.StatusNoContent},

		{Method: "GET", Path: "/blobs/{id}", Description: "One stored payload - an attachment, an uploaded file - streamed with range support.",
			Query:    []plugin.Param{{Name: "inline", Description: "Serve with an inline Content-Disposition rather than as a download.", Type: "boolean"}},
			Produces: "application/octet-stream"},
	}
}

// BuildSpec generates the OpenAPI description.
func BuildSpec(opts SpecOptions) *Spec {
	if opts.ServerURL == "" {
		opts.ServerURL = Prefix
	}
	b := newSchemaBuilder()
	spec := &Spec{
		OpenAPI: "3.1.0",
		Info: SpecInfo{
			Title:       "tommy",
			Version:     APIVersion,
			Description: specDescription,
		},
		Servers:  []SpecServer{{URL: opts.ServerURL, Description: "This tommy's API."}},
		Security: []map[string][]string{},
		Tags:     []SpecTag{{Name: "core", Description: "Events, blobs, discovery: everything that is not one plugin's own."}},
		Paths:    map[string]PathItem{},
	}

	for _, e := range coreEndpoints() {
		spec.add("", e, b)
	}

	if opts.Registry != nil {
		for _, p := range opts.Registry.Plugins() {
			endpoints := p.APIEndpoints()
			if len(endpoints) == 0 {
				continue
			}
			spec.Tags = append(spec.Tags, SpecTag{Name: p.Name(), Description: p.Description()})
			for _, e := range endpoints {
				spec.add(p.Name(), e, b)
			}
		}
	}

	spec.Components = SpecComponents{Schemas: b.Components()}
	return spec
}

// add mounts one endpoint on the document. plugin is "" for a core route.
func (s *Spec) add(pluginName string, e plugin.Endpoint, b *schemaBuilder) {
	path := specPath(e.Path)
	tag := "core"
	if pluginName != "" {
		path = "/" + pluginName + path
		tag = pluginName
	}

	op := &Operation{
		OperationID: operationID(e.Method, path),
		Summary:     e.Description,
		Tags:        []string{tag},
		Responses:   map[string]Response{},
	}
	for _, name := range pathParams(path) {
		op.Parameters = append(op.Parameters, SpecParameter{
			Name:        name,
			In:          "path",
			Required:    true,
			Description: pathParamDescription(name),
			Schema:      &Schema{Type: "string"},
		})
	}
	for _, q := range e.Query {
		kind := q.Type
		if kind == "" {
			kind = "string"
		}
		op.Parameters = append(op.Parameters, SpecParameter{
			Name:        q.Name,
			In:          "query",
			Description: q.Description,
			Schema:      &Schema{Type: kind},
		})
	}

	status := e.Status
	if status == 0 {
		status = http.StatusOK
	}
	resp := Response{Description: successDescription(status)}
	switch {
	case e.Response != nil:
		resp.Content = map[string]MediaType{"application/json": {Schema: b.For(e.Response)}}
	case e.Produces != "":
		resp.Content = map[string]MediaType{e.Produces: {}}
	}
	op.Responses[strconv.Itoa(status)] = resp
	if len(pathParams(path)) > 0 {
		op.Responses["404"] = Response{
			Description: "No such resource, or it belongs to another plugin.",
			Content:     map[string]MediaType{"application/json": {Schema: b.For(Error{})}},
		}
	}
	if len(e.Query) > 0 {
		op.Responses["400"] = Response{
			Description: "A query parameter could not be parsed - a malformed since, a negative limit.",
			Content:     map[string]MediaType{"application/json": {Schema: b.For(Error{})}},
		}
	}

	item := s.Paths[path]
	switch strings.ToUpper(e.Method) {
	case "", http.MethodGet:
		item.Get = op
	case http.MethodPost:
		item.Post = op
	case http.MethodPut:
		item.Put = op
	case http.MethodPatch:
		item.Patch = op
	case http.MethodDelete:
		item.Delete = op
	}
	s.Paths[path] = item
}

// Error is the shape every error response carries. It is a type so the schema
// and the handlers cannot disagree about it.
type Error struct {
	Error string `json:"error"`
}

// HealthInfo is what GET /health returns.
type HealthInfo struct {
	Status  string   `json:"status"`
	Uptime  string   `json:"uptime"`
	Plugins []string `json:"plugins"`
	// Events is how many are currently held, when the store can say.
	Events  *int   `json:"events,omitempty"`
	Version string `json:"version,omitempty"`
}

func successDescription(status int) string {
	switch status {
	case http.StatusNoContent:
		return "Done. No body."
	default:
		return "Success."
	}
}

// pathParams lists the wildcards in a path, in order.
func pathParams(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}")
		name = strings.TrimSuffix(name, "...")
		if name == "$" || name == "" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// specPath rewrites a Go route pattern as an OpenAPI path template. The only
// difference that matters is the trailing wildcard: net/http writes
// {path...} for "the rest of the path, slashes included", and OpenAPI has no
// spelling for that at all - {path} is as close as it gets, and the parameter
// description says the rest.
func specPath(path string) string {
	return strings.ReplaceAll(path, "...}", "}")
}

func pathParamDescription(name string) string {
	switch name {
	case "id":
		return "The event id, as returned by any listing."
	case "idx":
		return "Zero-based index into the attachments or media of that message."
	case "path":
		return "Path inside the shared virtual filesystem. It may contain slashes: this is the whole remaining path, not one segment."
	default:
		return ""
	}
}

// operationID builds a stable, readable identifier: generated clients use it
// for method names.
func operationID(method, path string) string {
	if method == "" {
		method = http.MethodGet
	}
	id := strings.ToLower(method)
	for _, seg := range strings.Split(path, "/") {
		if seg == "" {
			continue
		}
		if strings.HasPrefix(seg, "{") {
			name := strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}"), "...")
			id += "By" + title(name)
			continue
		}
		// A path segment may carry a dot (openapi.json); drop it rather than
		// emit an identifier no generator will accept.
		seg = strings.ReplaceAll(seg, ".", "-")
		for _, part := range strings.Split(seg, "-") {
			id += title(part)
		}
	}
	return id
}

func title(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// JSON renders the document the way it is checked in: indented, with a
// trailing newline, so a diff is readable and the file is a well-behaved text
// file.
func (s *Spec) JSON() ([]byte, error) {
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// openapi serves the description of this running server, including only the
// plugins this instance actually enabled.
func (a *API) openapi(w http.ResponseWriter, r *http.Request) {
	spec := BuildSpec(SpecOptions{
		Registry:  a.opts.Registry,
		ServerURL: a.serverURL(r),
	})
	body, err := spec.JSON()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}

// serverURL is the absolute base a reader can paste requests against.
func (a *API) serverURL(r *http.Request) string {
	if u := a.opts.SnippetCtx().APIURL; u != "" {
		return u
	}
	if origin := ui.Origin("", r); origin != "" {
		return origin + Prefix
	}
	return Prefix
}
