package snmp

import (
	"io/fs"

	"github.com/can3p/tommy/core/plugin"
)

// Plugin is tommy's SNMP trap content type: the canonical Trap and the
// provider that feeds it. Providers are supplied at construction time so the
// plugin never imports them, the same shape every other plugin in this
// codebase uses.
type Plugin struct {
	providers []plugin.Provider
}

// New returns the snmp plugin serving the given providers.
func New(providers ...plugin.Provider) *Plugin {
	return &Plugin{providers: providers}
}

// Name is the URL segment the plugin is mounted under.
func (p *Plugin) Name() string { return Name }

// Title is the UI tab label.
func (p *Plugin) Title() string { return "SNMP" }

// Description says what this fakes and why.
func (p *Plugin) Description() string {
	return "Captures the SNMP v1 and v2c traps and informs your infrastructure sends instead of a real network " +
		"management station, decoding every varbind by its actual wire type - integers, object identifiers, " +
		"counters, gauges, timeticks, IP addresses and octet strings (hex-dumped when not printable text) - " +
		"rather than flattening everything to one string. An inform gets the one reply this protocol actually " +
		"requires, a GetResponse echoing its request id and varbinds; a trap gets none. The udp listener that " +
		"feeds it comes with the trap provider."
}

// Providers returns the providers this plugin was built with.
func (p *Plugin) Providers() []plugin.Provider {
	if p.providers == nil {
		return []plugin.Provider{}
	}
	return p.providers
}

// Templates returns nil: this plugin has no bespoke UI - see RegisterUI.
func (p *Plugin) Templates() fs.FS { return nil }

// RegisterAPI mounts nothing.
//
// SNMP traps and informs are fire-and-forget UDP notifications: there is no
// query/response protocol here for a client to read anything back over the
// way a mail or sms SDK does, so there is no read-back contract (CLAUDE.md
// rule 7) for a bespoke route to satisfy. The generic cross-plugin API
// (GET /api/v1/events?plugin=snmp, GET /api/v1/events/{id}, the SSE stream)
// already lists, filters and serves every captured trap, full Payload
// included, with no code of this plugin's own.
func (p *Plugin) RegisterAPI(mux plugin.Mux, d plugin.Deps) {}

// RegisterUI mounts nothing, on purpose: the plugin gets the generic event
// view for free (core/server/ui's fallback, probed route by route).
//
// This is deliberate rather than an oversight, and it is also this plugin's
// reason for existing in the roadmap: a genuine test of whether a new
// protocol plugin is useful on day one with zero UI code, as the design
// intends (docs/implementation-plan.md §6c). It holds up here because the
// generic view's payload panel is a collapsible JSON inspector, and Trap's
// JSON shape - Version, V1/V2, and a Varbinds array carrying OID/Type/Value
// for every binding - already reads as the "varbind table per trap" the plan
// asks for; the raw panel hex-dumps the untouched datagram. A bespoke tab
// (an actual <table>, sortable, type-colored) would be a polish upgrade, not
// a capability the generic view is missing.
func (p *Plugin) RegisterUI(mux plugin.Mux, d plugin.Deps) {}
