// Package ingress is the shared, path-routed HTTP surface the fake vendor APIs
// are mounted on.
//
// Its job beyond routing is to fail loudly at startup: two providers claiming
// the same path, or a provider claiming a path the core reserves, is a
// misconfiguration that would otherwise show up as a silently shadowed route
// hours later.
package ingress

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
)

// ReservedPrefixes are the path prefixes the core owns. A provider may not
// claim them, because the ingress mux can be asked to share the UI listener.
//
// Note it is /api/v1/ and not /api/: a chat provider legitimately wants
// /api/chat.postMessage, and that must keep working.
var ReservedPrefixes = []string{"/api/v1/", "/ui/", "/_tommy/"}

// Route is one registered ingress route.
type Route struct {
	Plugin   string
	Provider string
	Pattern  Pattern
}

// Owner names the claimant of a route in error messages.
func (r Route) Owner() string {
	if r.Plugin == "" {
		return r.Provider
	}
	return r.Plugin + "/" + r.Provider
}

// Ingress is the shared ingress mux.
type Ingress struct {
	mux      *http.ServeMux
	routes   []Route
	errs     []error
	logger   *slog.Logger
	notFound http.Handler
	eventURL func(event.ID) string
}

// Option tunes an ingress mux.
type Option func(*Ingress)

// WithEventURL makes every provider response carry a link to what it captured,
// through the LinkHeader. The builder is a function rather than a base URL
// because the addresses are not known when the ingress is built: a listener
// takes its port when it binds.
func WithEventURL(f func(event.ID) string) Option {
	return func(i *Ingress) { i.eventURL = f }
}

// New returns an empty ingress mux.
func New(logger *slog.Logger, opts ...Option) *Ingress {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	i := &Ingress{mux: http.NewServeMux(), logger: logger}
	for _, opt := range opts {
		opt(i)
	}
	return i
}

// For returns the registration surface of one provider. Everything it mounts is
// checked and recorded before it reaches the underlying ServeMux.
func (i *Ingress) For(pluginName, providerName string) plugin.Mux {
	return &providerMux{ingress: i, plugin: pluginName, provider: providerName}
}

type providerMux struct {
	ingress  *Ingress
	plugin   string
	provider string
}

func (m *providerMux) Handle(pattern string, h http.Handler) {
	m.ingress.register(m.plugin, m.provider, pattern, h)
}

func (m *providerMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.ingress.register(m.plugin, m.provider, pattern, http.HandlerFunc(h))
}

func (i *Ingress) register(pluginName, providerName, pattern string, h http.Handler) {
	owner := Route{Plugin: pluginName, Provider: providerName}.Owner()

	p, err := ParsePattern(pattern)
	if err != nil {
		i.errs = append(i.errs, fmt.Errorf("ingress: %s: bad route %q: %w", owner, pattern, err))
		return
	}
	// Key() normalizes wildcard names, so /{anything...} is caught too.
	if p.Path == "/" || p.Key() == "/{...}" {
		i.errs = append(i.errs, fmt.Errorf(
			"ingress: %s: route %q would swallow every unmatched request; mount a real prefix instead",
			owner, pattern))
		return
	}
	for _, reserved := range ReservedPrefixes {
		if strings.HasPrefix(p.Path, reserved) {
			i.errs = append(i.errs, fmt.Errorf(
				"ingress: %s: route %q collides with the reserved core prefix %q",
				owner, pattern, reserved))
			return
		}
	}
	for _, existing := range i.routes {
		if !existing.Pattern.Conflicts(p) {
			continue
		}
		i.errs = append(i.errs, fmt.Errorf(
			"ingress: route conflict on %q: claimed by %s (%s) and by %s (%s)",
			p.Key(), existing.Owner(), existing.Pattern.String(), owner, p.String()))
		return
	}

	if err := i.handleSafely(pattern, i.instrument(h)); err != nil {
		i.errs = append(i.errs, fmt.Errorf("ingress: %s: route %q: %w", owner, pattern, err))
		return
	}
	i.routes = append(i.routes, Route{Plugin: pluginName, Provider: providerName, Pattern: p})
	i.logger.Debug("ingress route registered", "owner", owner, "pattern", p.String())
}

// handleSafely turns ServeMux's panic on an unusable pattern into an error, so
// one bad provider cannot take the process down at startup.
func (i *Ingress) handleSafely(pattern string, h http.Handler) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("rejected by net/http: %v", r)
		}
	}()
	i.mux.Handle(pattern, h)
	return nil
}

// Err returns every registration problem found, or nil.
func (i *Ingress) Err() error { return errors.Join(i.errs...) }

// Routes returns the registered routes, sorted by path.
func (i *Ingress) Routes() []Route {
	out := append([]Route(nil), i.routes...)
	sort.Slice(out, func(a, b int) bool {
		if out[a].Pattern.Path != out[b].Pattern.Path {
			return out[a].Pattern.Path < out[b].Pattern.Path
		}
		return out[a].Pattern.Method < out[b].Pattern.Method
	})
	return out
}

// SetNotFound installs the handler for unmatched ingress requests. It exists
// because "my SDK gets a 404" is the most common misconfiguration and deserves
// a body that lists what is actually mounted.
func (i *Ingress) SetNotFound(h http.Handler) { i.notFound = h }

// Handler returns the ingress HTTP handler.
func (i *Ingress) Handler() http.Handler { return i }

func (i *Ingress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if i.notFound != nil {
		if _, pattern := i.mux.Handler(r); pattern == "" {
			i.notFound.ServeHTTP(w, r)
			return
		}
	}
	i.mux.ServeHTTP(w, r)
}

// Has reports whether a request for method+path is routed. Used by the
// conformance test to check declared endpoints are really mounted.
func (i *Ingress) Has(method, path string) bool {
	req, err := http.NewRequest(method, "http://ingress.invalid"+path, nil)
	if err != nil {
		return false
	}
	_, pattern := i.mux.Handler(req)
	return pattern != ""
}
