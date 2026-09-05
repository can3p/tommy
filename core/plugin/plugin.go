// Package plugin defines what a tommy plugin and provider are.
//
// A plugin owns a content type (mail, sms, files): a canonical model, an API
// surface, a UI tab and a set of providers. A provider imitates one real vendor
// API or protocol and converts its wire format into the plugin's model.
// Providers never talk to each other and never share files.
package plugin

import (
	"context"
	"io/fs"
	"net/http"
	"time"

	"github.com/can3p/tommy/core/config"
)

// Mux is the registration surface handed to plugins and providers.
//
// It is an interface rather than *http.ServeMux so the core can see every route
// as it is mounted: ingress registration has to detect collisions between two
// providers and against reserved core prefixes, and name both claimants. That
// is impossible if routes go straight onto a concrete ServeMux the core cannot
// observe. *http.ServeMux satisfies this interface, so tests can pass one.
type Mux interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Plugin owns a content type and a UI tab.
type Plugin interface {
	Name() string        // "mail" - url segment, lowercase
	Title() string       // "Mail" - UI tab label
	Description() string // one or two sentences: what this fakes and why
	Providers() []Provider
	RegisterAPI(mux Mux, d Deps) // mounted under /api/v1/<name>/
	RegisterUI(mux Mux, d Deps)  // mounted under /ui/<name>/
	Templates() fs.FS            // embedded templates for the tab; nil is fine

	// APIEndpoints documents every route RegisterAPI mounts, paths relative to
	// /api/v1/<name>. It is to a plugin's API what Endpoints() is to a
	// provider's ingress routes, and it is checked the same way: a mounted
	// route that is not declared, or a declared route that is not mounted,
	// fails plugintest.Conformance.
	//
	// It exists because the OpenAPI description is generated from it. The
	// routes could be read off the mux on their own, but the prose could not,
	// and a description kept in one core file drifts the moment somebody adds
	// a route without opening that file.
	APIEndpoints() []Endpoint
}

// Provider imitates one vendor API or protocol.
type Provider interface {
	Name() string          // "mailjet"
	Plugin() string        // "mail" - must match the plugin that lists it
	Description() string   // one or two sentences: which real API, which parts
	Endpoints() []Endpoint // discovery + docs; every mounted route is declared
	Snippets() []Snippet   // copy-paste manual tests; at least one
	RegisterIngress(mux Mux, d Deps)
}

// ListenerProvider is a provider that needs a listener of its own (SMTP, FTP,
// SFTP) instead of a route on the shared HTTP ingress.
type ListenerProvider interface {
	Provider
	// Listen blocks until ctx is done. It reads its own networking settings
	// from d.Config, because a single addr argument cannot carry an FTP control
	// port plus a passive range plus an advertised host.
	Listen(ctx context.Context, d Deps) error
}

// Endpoint documents one route a provider mounts on the ingress. It is the
// discovery surface: /api/v1/plugins, the UI and `tommy providers` all read it,
// and plugintest.Conformance checks that every declared endpoint is reachable.
// AddressableProvider is an optional interface a ListenerProvider may
// implement to report the address it actually bound.
//
// A provider that takes an ephemeral port, or that falls back to its own
// default when the configuration names none, is the only thing that knows
// where it ended up listening. Without this the discovery surface has to guess
// from configuration alone and gets it wrong in exactly those two cases, which
// leaves a snippet telling someone to connect to the wrong port - or to no
// port at all.
//
// Addr blocks until the listener has bound or the timeout elapses, and returns
// an error if it never does.
type AddressableProvider interface {
	ListenerProvider
	Addr(timeout time.Duration) (string, error)
}

type Endpoint struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description"`

	// Query documents the query parameters the route accepts. Optional, and
	// never exhaustive by contract - an undeclared parameter is not rejected.
	Query []Param `json:"-"`

	// Response is a zero value of what the route returns, and is what the
	// OpenAPI schema is generated from. Leave it nil for a route whose body is
	// not JSON, or whose shape is the vendor's rather than tommy's.
	Response any `json:"-"`

	// Produces is the response media type when it is not application/json.
	Produces string `json:"-"`

	// Status is the success status when it is not 200.
	Status int `json:"-"`
}

// Param documents one query parameter.
//
// These three fields are deliberately all there is: this describes tommy's own
// read-back API, where every filter is an optional string, an integer or a
// flag. Anything richer belongs in Description.
type Param struct {
	Name        string
	Description string
	// Type is "string" (the default), "integer" or "boolean".
	Type string
}

// CoreListParams are the filters every listing route inherits from
// api.ParseQuery. A plugin's list endpoint declares these plus its own, rather
// than restating them, so the six of them cannot drift apart across eight
// plugins.
func CoreListParams() []Param {
	return []Param{
		{Name: "provider", Description: "Only events captured by this provider."},
		{Name: "type", Description: "Only events of this type, such as mail.message."},
		{Name: "search", Description: "Case-insensitive substring over the summary and type."},
		{Name: "since", Description: "RFC3339 timestamp, a duration such as 5m, or unix milliseconds."},
		{Name: "limit", Description: "Maximum number of entries to return.", Type: "integer"},
		{Name: "offset", Description: "How many entries to skip.", Type: "integer"},
	}
}

// ProviderConfig is the per-provider config section handed to a provider in
// Deps. Aliased so provider code never has to import core/config for it.
type ProviderConfig = config.ProviderConfig
