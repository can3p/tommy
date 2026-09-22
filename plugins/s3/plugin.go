// Package s3 is tommy's shared in-memory S3 state and logical event model.
// Object bytes live in the core blob store, independently of event retention.
package s3

import (
	"embed"
	"io/fs"

	"github.com/can3p/tommy/core/plugin"
	coreapi "github.com/can3p/tommy/core/server/api"
	coreui "github.com/can3p/tommy/core/server/ui"
)

const (
	// PluginName is the URL-safe plugin identity.
	PluginName = "s3"
	// Name is an alias convenient for providers.
	Name = PluginName
	// APIPrefix is the public base for the plugin read-back API.
	APIPrefix = coreapi.Prefix + "/" + PluginName
	// UIPrefix is the public base for the plugin tab.
	UIPrefix = coreui.Prefix + "/" + PluginName
)

//go:embed ui/*.html
var uiFS embed.FS

// StoreBinder is implemented by providers that use the shared S3 catalog.
type StoreBinder interface {
	BindStore(*Store)
}

// Plugin owns the S3 content type and one Store shared by all providers.
type Plugin struct {
	providers []plugin.Provider
	store     *Store
}

func New(providers ...plugin.Provider) *Plugin {
	return NewWithStore(NewStore(), providers...)
}

func NewWithStore(store *Store, providers ...plugin.Provider) *Plugin {
	if store == nil {
		store = NewStore()
	}
	p := &Plugin{providers: providers, store: store}
	for _, provider := range providers {
		if binder, ok := provider.(StoreBinder); ok {
			binder.BindStore(store)
		}
	}
	return p
}

func (p *Plugin) Store() *Store { return p.store }

func (p *Plugin) Name() string  { return PluginName }
func (p *Plugin) Title() string { return "S3" }
func (p *Plugin) Description() string {
	return "Accepts S3-compatible bucket and object operations without sending data to cloud storage, and keeps the resulting object catalog available for inspection. " +
		"Logical bucket and object mutations are also captured as searchable events while object bytes remain in the independent blob store."
}

func (p *Plugin) Providers() []plugin.Provider {
	if p.providers == nil {
		return []plugin.Provider{}
	}
	return p.providers
}

// Templates returns the embedded S3 tab templates.
func (p *Plugin) Templates() fs.FS {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		panic("s3: embedded ui templates: " + err.Error())
	}
	return sub
}

func (p *Plugin) session(deps plugin.Deps) *Session {
	return NewSession(p.store, deps, WithTransport("http"))
}
