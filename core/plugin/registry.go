package plugin

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/can3p/tommy/core/config"
)

// Ref pairs a provider with the plugin that owns it.
type Ref struct {
	Plugin   Plugin
	Provider Provider
}

// Registry is the set of plugins this process runs, filtered by config.
// Registration is explicit (plugins/all/all.go), never init() magic.
type Registry struct {
	cfg      *config.Config
	all      []Plugin
	enabled  []Plugin
	disabled []Plugin
	byName   map[string]Plugin
}

// New validates a plugin list against a config and returns the registry.
// It fails loudly on duplicate or malformed names rather than letting two
// plugins fight over a URL segment at runtime.
func New(cfg *config.Config, plugins ...Plugin) (*Registry, error) {
	if cfg == nil {
		cfg = config.Default()
	}
	r := &Registry{cfg: cfg, byName: map[string]Plugin{}}

	var errs []error
	for _, p := range plugins {
		name := p.Name()
		if err := validName(name); err != nil {
			errs = append(errs, fmt.Errorf("plugin %q: %w", name, err))
			continue
		}
		if prev, ok := r.byName[name]; ok {
			errs = append(errs, fmt.Errorf("plugin %q is registered twice (%T and %T)", name, prev, p))
			continue
		}
		seen := map[string]Provider{}
		for _, prov := range p.Providers() {
			pn := prov.Name()
			if err := validName(pn); err != nil {
				errs = append(errs, fmt.Errorf("plugin %q provider %q: %w", name, pn, err))
				continue
			}
			if prev, ok := seen[pn]; ok {
				errs = append(errs, fmt.Errorf("plugin %q registers provider %q twice (%T and %T)", name, pn, prev, prov))
				continue
			}
			if prov.Plugin() != name {
				errs = append(errs, fmt.Errorf("provider %q says it belongs to plugin %q but is listed by %q", pn, prov.Plugin(), name))
				continue
			}
			seen[pn] = prov
		}
		r.byName[name] = p
		r.all = append(r.all, p)
		if cfg.PluginEnabled(name) {
			r.enabled = append(r.enabled, p)
		} else {
			r.disabled = append(r.disabled, p)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return r, nil
}

func validName(name string) error {
	if name == "" {
		return errors.New("name is empty")
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return fmt.Errorf("name %q must be lowercase letters, digits or dashes", name)
		}
	}
	return nil
}

// Config returns the config the registry was built from.
func (r *Registry) Config() *config.Config { return r.cfg }

// Plugins returns the enabled plugins in registration order.
func (r *Registry) Plugins() []Plugin { return append([]Plugin(nil), r.enabled...) }

// AllPlugins returns every registered plugin, enabled or not.
func (r *Registry) AllPlugins() []Plugin { return append([]Plugin(nil), r.all...) }

// DisabledPlugins returns the plugins switched off by config.
func (r *Registry) DisabledPlugins() []Plugin { return append([]Plugin(nil), r.disabled...) }

// Plugin looks up an enabled plugin by name.
func (r *Registry) Plugin(name string) (Plugin, bool) {
	p, ok := r.byName[name]
	if !ok || !r.cfg.PluginEnabled(name) {
		return nil, false
	}
	return p, true
}

// Enabled reports whether a plugin is registered and switched on.
func (r *Registry) Enabled(name string) bool {
	_, ok := r.Plugin(name)
	return ok
}

// Providers returns the enabled providers of a plugin, in declaration order.
func (r *Registry) Providers(plugin string) []Provider {
	p, ok := r.Plugin(plugin)
	if !ok {
		return nil
	}
	var out []Provider
	for _, prov := range p.Providers() {
		if r.cfg.ProviderEnabled(plugin, prov.Name()) {
			out = append(out, prov)
		}
	}
	return out
}

// Refs returns every enabled provider paired with its plugin.
func (r *Registry) Refs() []Ref {
	var out []Ref
	for _, p := range r.enabled {
		for _, prov := range r.Providers(p.Name()) {
			out = append(out, Ref{Plugin: p, Provider: prov})
		}
	}
	return out
}

// ListenerRefs returns the enabled providers that need a listener of their own.
func (r *Registry) ListenerRefs() []Ref {
	var out []Ref
	for _, ref := range r.Refs() {
		if _, ok := ref.Provider.(ListenerProvider); ok {
			out = append(out, ref)
		}
	}
	return out
}

// IngressRefs returns the enabled providers served by the shared HTTP ingress.
func (r *Registry) IngressRefs() []Ref {
	var out []Ref
	for _, ref := range r.Refs() {
		if _, ok := ref.Provider.(ListenerProvider); !ok {
			out = append(out, ref)
		}
	}
	return out
}

// ProviderConfig returns the config section of a provider.
func (r *Registry) ProviderConfig(plugin, provider string) ProviderConfig {
	return r.cfg.Provider(plugin, provider)
}

// DepsFor returns base specialised with the provider's config section and a
// logger tagged with the plugin and provider.
func (r *Registry) DepsFor(base Deps, plugin, provider string) Deps {
	d := base.Normalize().WithConfig(r.ProviderConfig(plugin, provider))
	d.Logger = d.Logger.With("plugin", plugin, "provider", provider)
	return d
}

// Describe renders the registry for humans: `tommy providers` and the UI both
// use it, so the two never drift.
func (r *Registry) Describe(ctx SnippetCtx) ([]PluginInfo, error) {
	var out []PluginInfo
	for _, p := range r.enabled {
		info := PluginInfo{
			Name:        p.Name(),
			Title:       p.Title(),
			Description: p.Description(),
			Enabled:     true,
		}
		for _, prov := range r.Providers(p.Name()) {
			snippets, err := RenderSnippets(prov.Snippets(), ctx)
			if err != nil {
				return nil, fmt.Errorf("plugin %q provider %q: %w", p.Name(), prov.Name(), err)
			}
			_, listener := prov.(ListenerProvider)
			pi := ProviderInfo{
				Name:        prov.Name(),
				Plugin:      p.Name(),
				Description: prov.Description(),
				Endpoints:   prov.Endpoints(),
				Snippets:    snippets,
				Listener:    listener,
				Addr:        ctx.Addr(p.Name(), prov.Name()),
				Enabled:     true,
			}
			if pi.Endpoints == nil {
				pi.Endpoints = []Endpoint{}
			}
			info.Providers = append(info.Providers, pi)
		}
		if info.Providers == nil {
			info.Providers = []ProviderInfo{}
		}
		out = append(out, info)
	}
	if out == nil {
		out = []PluginInfo{}
	}
	return out, nil
}

// PluginInfo is the JSON shape of /api/v1/plugins.
type PluginInfo struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Enabled     bool           `json:"enabled"`
	Providers   []ProviderInfo `json:"providers"`
}

// ProviderInfo is one provider inside PluginInfo.
type ProviderInfo struct {
	Name        string            `json:"name"`
	Plugin      string            `json:"plugin"`
	Description string            `json:"description"`
	Enabled     bool              `json:"enabled"`
	Listener    bool              `json:"listener"`
	Addr        string            `json:"addr,omitempty"`
	Endpoints   []Endpoint        `json:"endpoints"`
	Snippets    []RenderedSnippet `json:"snippets"`
}

// SortedNames returns the enabled plugin names, sorted. Handy in tests.
func (r *Registry) SortedNames() []string {
	out := make([]string, 0, len(r.enabled))
	for _, p := range r.enabled {
		out = append(out, p.Name())
	}
	sort.Strings(out)
	return out
}

// String renders the registry compactly, for logs.
func (r *Registry) String() string {
	parts := make([]string, 0, len(r.enabled))
	for _, p := range r.enabled {
		names := make([]string, 0)
		for _, prov := range r.Providers(p.Name()) {
			names = append(names, prov.Name())
		}
		parts = append(parts, fmt.Sprintf("%s[%s]", p.Name(), strings.Join(names, ",")))
	}
	return strings.Join(parts, " ")
}
