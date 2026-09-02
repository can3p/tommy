package hl7

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

// Plugin is tommy's HL7 v2 content type: the canonical Message, the read-back
// API and the segment tree tab. Providers are supplied at construction time so
// the plugin never imports them, which is what keeps a provider free to depend
// on whatever its wire protocol needs.
type Plugin struct {
	providers []plugin.Provider
}

// New returns the hl7 plugin serving the given providers.
//
// Passing none is valid and is how the plugin core was built and tested before
// the MLLP provider existed: a message can be put in the store directly, and
// every read surface works. Conformance will say the plugin has no provider,
// which is exactly the right complaint - without one nothing can reach it.
func New(providers ...plugin.Provider) *Plugin {
	return &Plugin{providers: providers}
}

// Name is the URL segment the plugin is mounted under.
func (p *Plugin) Name() string { return Name }

// Title is the UI tab label.
func (p *Plugin) Title() string { return "HL7" }

// Description says what this fakes and why.
func (p *Plugin) Description() string {
	return "Captures the HL7 v2 messages your integration sends instead of handing them to a hospital " +
		"system, parsing each one with the separators it declared for itself rather than the ones everybody " +
		"assumes. The HL7 tab shows a message as a segment tree - every field at its own position, every " +
		"repetition kept apart - next to the bytes exactly as they arrived. The MLLP listener that feeds it " +
		"comes with the mllp provider."
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
		panic("hl7: embedded ui templates: " + err.Error())
	}
	return sub
}

// RegisterAPI mounts the plugin's routes under /api/v1/hl7/.
func (p *Plugin) RegisterAPI(mux plugin.Mux, d plugin.Deps) {
	(&apiHandler{deps: d.Normalize()}).mount(mux)
}

// RegisterUI mounts the HL7 tab under /ui/hl7/. Whatever it does not claim -
// the per-event raw inspector, for one - falls back to the core's generic view.
func (p *Plugin) RegisterUI(mux plugin.Mux, d plugin.Deps) {
	newUIHandler(p, d.Normalize()).mount(mux)
}
