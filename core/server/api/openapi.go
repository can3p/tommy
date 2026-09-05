package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/can3p/tommy/core/server/ui"
)

// APIVersion is what the description calls itself. It is the version of the
// interface, not of the binary: putting the build version here would rewrite
// the checked-in document on every release for no change anyone can act on.
const APIVersion = "v1"

// specDescription is the top of the document, and the place to say what it
// does not cover.
const specDescription = `The events API: everything an application under test needs to read back what it
sent.

Tommy stands in for the services an application talks to - mail providers, SMS
gateways, file transfer, chat webhooks, EDI partners - and records every message
it receives as an event. These routes list those events, fetch one in full,
stream them as they arrive, delete them, and download the bytes any of them
carried. Every event names the page that renders it, so a test can print a link
to what it just sent.

Two things are deliberately absent. The fake vendor endpoints - Mailjet's send
API, Twilio's, Slack's - are those vendors' specifications rather than tommy's,
and a partial copy of somebody else's API is worse than none; run ` + "`tommy providers`" + `
for those. And each plugin's own read-back routes (` + "`/api/v1/mail/messages`" + ` and its
kin) are a convenience on top of these, shaped by the content type rather than
by a contract worth generating a client from; each plugin's README documents its
own.

This document is generated from the server's route table. It cannot describe a
route that does not exist, and CI fails if the checked-in copy stops matching
the code.`

// Spec is an OpenAPI 3.1 document.
type Spec struct {
	OpenAPI string       `json:"openapi"`
	Info    SpecInfo     `json:"info"`
	Servers []SpecServer `json:"servers,omitempty"`
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
	// ServerURL is what paths are relative to. The checked-in document uses
	// the relative "/api/v1"; a running server names itself, so a reader can
	// paste a request straight out of the rendered page.
	ServerURL string
}

// endpoint documents one route of the events API. It is local to this package
// on purpose: plugin.Endpoint is the *discovery* surface a provider advertises,
// and this is the input to a generated document. Sharing one struct between the
// two would put schema and response-code fields on a type that half its users
// have no use for.
type endpoint struct {
	Method      string
	Path        string
	Description string
	// Query documents the query parameters. Never exhaustive by contract: an
	// undeclared parameter is ignored, not rejected.
	Query []param
	// Response is a zero value of the JSON body; the schema is generated from
	// its type. Nil for a route whose body is not JSON.
	Response any
	// Produces is the media type when it is not application/json.
	Produces string
	// Status is the success status when it is not 200.
	Status int
}

// param is one query parameter. Three fields is all this API needs: every
// filter is an optional string, an integer or a flag.
type param struct {
	Name        string
	Description string
	// Type is "string" (the default), "integer" or "boolean".
	Type string
}

// listParams are the filters both listing routes share, declared once so the
// two cannot drift apart.
func listParams() []param {
	return []param{
		{Name: "plugin", Description: "Only events from this plugin, such as mail."},
		{Name: "provider", Description: "Only events captured by this provider, such as mailjet."},
		{Name: "type", Description: "Only events of this type, such as mail.message."},
		{Name: "search", Description: "Case-insensitive substring over the summary and type."},
		{Name: "since", Description: "RFC3339 timestamp, a duration such as 5m, or unix milliseconds."},
		{Name: "limit", Description: "Maximum number of events to return.", Type: "integer"},
		{Name: "offset", Description: "How many events to skip.", Type: "integer"},
	}
}

// eventEndpoints is the whole document: the event surface and the blobs its
// events point at.
//
// Nothing else is here, and that is the scope decision this file exists to
// record. /health, /plugins and this document's own route are operational
// details of one server rather than a contract to program against, and a
// plugin's read-back routes are shaped by its content type - the events API is
// the one surface every consumer of tommy uses, whatever it is capturing.
func eventEndpoints() []endpoint {
	return []endpoint{
		{Method: "GET", Path: "/events", Description: "Every captured event, newest first, whatever plugin captured it. Raw request bodies are omitted unless asked for: they can be megabytes each.",
			Query: append(listParams(),
				param{Name: "include_raw", Description: "Include each event's raw request body.", Type: "boolean"}),
			Response: []ui.EventJSON{}},
		{Method: "GET", Path: "/events/{id}", Description: "One event in full, raw request body included.",
			Response: ui.EventJSON{}},
		{Method: "GET", Path: "/events/stream", Description: "The same events as they arrive, as Server-Sent Events. Each event produces a JSON frame carrying the event without its raw body, and a frame named after the event type carrying just the id, so an htmx page can trigger on it.",
			Query: listParams(), Produces: "text/event-stream"},
		{Method: "DELETE", Path: "/events", Description: "Clear captured events, all of them or one plugin's. Stored payloads deliberately survive, so a link to one already handed out keeps working.",
			Query:  []param{{Name: "plugin", Description: "Clear only this plugin's events."}},
			Status: http.StatusNoContent},
		{Method: "DELETE", Path: "/events/{id}", Description: "Delete one captured event.",
			Status: http.StatusNoContent},
		{Method: "GET", Path: "/blobs/{id}", Description: "The bytes an event carried - a mail attachment, an uploaded file - streamed with range support. Events reference these by the id in their payload rather than inlining them.",
			Query:    []param{{Name: "inline", Description: "Serve with an inline Content-Disposition rather than as a download.", Type: "boolean"}},
			Produces: "application/octet-stream"},
	}
}

// BuildSpec generates the OpenAPI description of the events API.
func BuildSpec(opts SpecOptions) *Spec {
	if opts.ServerURL == "" {
		opts.ServerURL = Prefix
	}
	b := newSchemaBuilder()
	spec := &Spec{
		OpenAPI: "3.1.0",
		Info: SpecInfo{
			Title:       "tommy events API",
			Version:     APIVersion,
			Description: specDescription,
		},
		Servers:  []SpecServer{{URL: opts.ServerURL, Description: "This tommy's API."}},
		Security: []map[string][]string{},
		Paths:    map[string]PathItem{},
	}
	for _, e := range eventEndpoints() {
		spec.add(e, b)
	}
	spec.Components = SpecComponents{Schemas: b.Components()}
	return spec
}

// add mounts one endpoint on the document.
func (s *Spec) add(e endpoint, b *schemaBuilder) {
	path := specPath(e.Path)

	op := &Operation{
		OperationID: operationID(e.Method, path),
		Summary:     e.Description,
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
	spec := BuildSpec(SpecOptions{ServerURL: a.serverURL(r)})
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
