package push

import (
	"embed"
	"io/fs"

	"github.com/can3p/tommy/core/plugin"
)

// APIPrefix is where the core mounts this plugin's API. Routes are registered
// with the prefix already stripped; it is spelled out here so the API and the
// tab can hand out links a browser can follow.
const APIPrefix = "/api/v1/" + Name

// UIPrefix is where the core mounts this plugin's tab, likewise stripped from
// the patterns RegisterUI uses.
const UIPrefix = "/ui/" + Name

//go:embed ui/*.html
var uiFS embed.FS

// Plugin is tommy's push-notification content type: the canonical Message, the
// read-back API and the lock-screen tab. Providers are supplied at construction
// time so the plugin never imports them, which is what keeps a provider free to
// depend on whatever its wire format needs - and what lets the APNs provider,
// which needs HTTP/2, arrive after the FCM one, which does not.
type Plugin struct {
	providers []plugin.Provider
}

// New returns the push plugin serving the given providers.
//
// Passing none is valid and is how the plugin core was built and tested before
// the FCM and APNs providers existed: a message can be put in the store
// directly, and every read surface works. Conformance will say the plugin has
// no provider, which is exactly the right complaint - without one nothing can
// reach it - and is why this plugin is not in plugins/all/all.go yet.
func New(providers ...plugin.Provider) *Plugin {
	return &Plugin{providers: providers}
}

// Name is the URL segment the plugin is mounted under.
func (p *Plugin) Name() string { return Name }

// Title is the UI tab label.
func (p *Plugin) Title() string { return "Push" }

// Description says what this fakes and why.
func (p *Plugin) Description() string {
	return "Captures the push notifications your app's backend sends instead of handing them to Apple or " +
		"Google, and shows each one the way a phone would: a lock-screen card with the title, subtitle and " +
		"body, next to the payload exactly as it was posted. It says plainly whether a push displays " +
		"anything at all, which is what most silent-push debugging comes down to, and it keeps APNs and " +
		"FCM apart rather than pretending a device token and a topic are the same thing. The endpoints " +
		"that feed it arrive with the providers: fcm speaks the Firebase HTTP v1 send API, apns speaks " +
		"Apple's HTTP/2 provider API."
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
		panic("push: embedded ui templates: " + err.Error())
	}
	return sub
}

// RegisterAPI mounts the plugin's routes under /api/v1/push/.
func (p *Plugin) RegisterAPI(mux plugin.Mux, d plugin.Deps) {
	(&apiHandler{deps: d.Normalize()}).mount(mux)
}

// RegisterUI mounts the push tab under /ui/push/. Whatever it does not claim -
// the per-event raw inspector, for one - falls back to the core's generic view.
func (p *Plugin) RegisterUI(mux plugin.Mux, d plugin.Deps) {
	newUIHandler(p, d.Normalize()).mount(mux)
}
