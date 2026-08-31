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
type Endpoint struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description"`
}

// ProviderConfig is the per-provider config section handed to a provider in
// Deps. Aliased so provider code never has to import core/config for it.
type ProviderConfig = config.ProviderConfig
