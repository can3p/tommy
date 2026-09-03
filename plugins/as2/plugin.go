package as2

import (
	"embed"
	"io/fs"

	"github.com/can3p/tommy/core/plugin"
	coreapi "github.com/can3p/tommy/core/server/api"
	coreui "github.com/can3p/tommy/core/server/ui"
)

// APIPrefix is where the core mounts this plugin's API. Routes are registered
// with the prefix already stripped; it is spelled out here so the API and the
// tab can hand out links a browser can follow.
const APIPrefix = coreapi.Prefix + "/" + Name

// UIPrefix is where the core mounts this plugin's tab, likewise stripped from
// the patterns RegisterUI uses.
const UIPrefix = coreui.Prefix + "/" + Name

//go:embed ui/*.html
var uiFS embed.FS

// IdentityBinder is implemented by a provider that needs tommy's AS2 key pair.
// New calls BindIdentity on every provider that implements it, which is how a
// provider is handed the identity without the plugin importing it - the same
// shape files.VFSBinder uses for the shared filesystem.
//
// The binding is only half of it. The plugin can build an Identity but cannot
// configure one, because RegisterAPI and RegisterUI are handed Deps with an
// empty Config by contract: only a provider's RegisterIngress receives a
// ProviderConfig and a ConfigDir. So the plugin constructs the identity
// unconfigured and the provider calls Identity.Configure with the paths it was
// given. That is also what makes certificate generation lazy in the right way -
// it happens for an enabled provider and for nothing else, so somebody running
// `tommy mail` never meets a certificate.
type IdentityBinder interface {
	BindIdentity(id *Identity)
}

// Plugin is tommy's AS2 content type: the canonical Message, the S/MIME
// processing, the MDN, the read-back API and the exchange tab. Providers are
// supplied at construction time so the plugin never imports them.
type Plugin struct {
	providers []plugin.Provider
	identity  *Identity
}

// New returns the as2 plugin serving the given providers, all of them sharing
// one identity.
//
// Passing none is valid and is how this plugin core was built and tested before
// the HTTP provider existed: a message can be put through a Receiver directly
// and every read surface works. Conformance will say the plugin has no
// provider, which is exactly the right complaint - without one nothing can
// reach it - and is why this plugin is not in plugins/all/all.go yet.
func New(providers ...plugin.Provider) *Plugin {
	return NewWithIdentity(NewIdentity(), providers...)
}

// NewWithIdentity is New over an identity the caller built, for a test that
// wants an in-memory key pair or a wiring that shares one certificate with
// something else.
func NewWithIdentity(id *Identity, providers ...plugin.Provider) *Plugin {
	if id == nil {
		id = NewIdentity()
	}
	p := &Plugin{providers: providers, identity: id}
	for _, prov := range providers {
		if b, ok := prov.(IdentityBinder); ok {
			b.BindIdentity(id)
		}
	}
	return p
}

// Identity returns the shared key pair. Providers normally receive it through
// BindIdentity instead; this is for the wiring, the tab and the tests.
func (p *Plugin) Identity() *Identity { return p.identity }

// Name is the URL segment the plugin is mounted under.
func (p *Plugin) Name() string { return Name }

// Title is the UI tab label.
func (p *Plugin) Title() string { return "AS2" }

// Description says what this fakes and why.
func (p *Plugin) Description() string {
	return "Stands in for a trading partner's AS2 endpoint: it accepts the EDIINT messages your integration " +
		"posts - signed, encrypted, compressed, in any combination - and answers with the MDN receipt the " +
		"sender is waiting on, so an AS2 client runs unmodified against a fake. The AS2 tab shows each message " +
		"unwrapped layer by layer, the EDI document that was inside, the computed MIC and exactly what the " +
		"signature did and did not prove."
}

// Providers returns the providers this plugin was built with.
func (p *Plugin) Providers() []plugin.Provider {
	if p.providers == nil {
		return []plugin.Provider{}
	}
	return p.providers
}

// Templates returns the tab's templates, which compose the shared component
// library rather than re-implementing page chrome.
func (p *Plugin) Templates() fs.FS {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		// Impossible: the directory is embedded above.
		panic("as2: embedded ui templates: " + err.Error())
	}
	return sub
}

// RegisterAPI mounts the plugin's routes under /api/v1/as2/.
func (p *Plugin) RegisterAPI(mux plugin.Mux, d plugin.Deps) {
	(&apiHandler{deps: d.Normalize(), identity: p.identity}).mount(mux)
}

// RegisterUI mounts the AS2 tab under /ui/as2/. Whatever it does not claim -
// the per-event raw inspector, for one - falls back to the core's generic view.
func (p *Plugin) RegisterUI(mux plugin.Mux, d plugin.Deps) {
	newUIHandler(p, d.Normalize()).mount(mux)
}
