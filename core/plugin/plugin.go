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
	"strconv"
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

// AddressableProvider is an optional interface a ListenerProvider may
// implement to report the address it actually bound.
//
// A provider that took an ephemeral port is the only thing that knows where it
// ended up listening, so this is the authority for a *running* listener and
// wins over anything derived from configuration. PortProvider answers the
// other half - where a listener would bind, asked of a process with nothing
// running.
//
// Addr blocks until the listener has bound or the timeout elapses, and returns
// an error if it never does.
type AddressableProvider interface {
	ListenerProvider
	Addr(timeout time.Duration) (string, error)
}

// PortProvider reports where a listener provider would bind under a given
// configuration, resolved without binding anything.
//
// Every ListenerProvider must implement it - plugintest.Conformance fails one
// that does not - because otherwise tommy's own default ports live only in
// package-level constants, and `tommy providers` on a process with nothing
// running can say nothing about them. That is the fact the Docker EXPOSE list,
// the compose file and the site's port table are all derived from, so a
// listener provider that does not report it silently drops out of all three.
//
// The provider already resolves this value at Listen time (pc.Int("port",
// DefaultPort)); ListenPort just says it out loud, so it is a report of a
// decision the provider already makes rather than a second one that can drift.
type PortProvider interface {
	ListenerProvider
	ListenPort(pc ProviderConfig) ListenPort
}

// ListenPort is where a listener provider would bind, and what it speaks there.
type ListenPort struct {
	// Port is the configured port, or the provider's own default when the
	// configuration names none. Zero means the configuration asked for an
	// ephemeral port (port = 0), which nothing can resolve before binding:
	// AddressableProvider.Addr is the only answer in that case, and it needs
	// a running listener. Zero is therefore reported as zero rather than
	// papered over with the default, which would name a port nothing is
	// listening on.
	Port int `json:"port"`
	// Network is "tcp" or "udp" - the transport a client must use to reach
	// it. Docker's EXPOSE and `docker run -P` need it (a UDP trap receiver
	// published as TCP is not published at all), and a port on its own does
	// not carry it.
	Network string `json:"network"`
}

// Ephemeral reports whether the port is only knowable once the listener binds.
func (l ListenPort) Ephemeral() bool { return l.Port == 0 }

// String renders the port the way Docker's EXPOSE, `docker run -p` and every
// port table want it: "2575/tcp", "6969/udp".
func (l ListenPort) String() string {
	network := l.Network
	if network == "" {
		network = "tcp"
	}
	return strconv.Itoa(l.Port) + "/" + network
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
