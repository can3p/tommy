package sms

import (
	"embed"
	"encoding/json"
	"io/fs"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
)

//go:embed ui/*.html
var uiFS embed.FS

// Plugin is tommy's SMS content type: the canonical Message, the read-back API
// and the conversation tab. Providers are supplied at construction time so the
// plugin never has to import them, which is what keeps a provider's package
// free to depend on whatever its vendor SDK needs.
type Plugin struct {
	providers []plugin.Provider
}

// Option configures the plugin.
type Option func(*Plugin)

// WithProviders adds providers to the plugin. The wiring in plugins/all names
// them explicitly (sms.New(twilio.New())); tests pass their own fake.
func WithProviders(providers ...plugin.Provider) Option {
	return func(p *Plugin) { p.providers = append(p.providers, providers...) }
}

// New returns the sms plugin. With no options it has no providers and can only
// be filled directly from a test; the wiring in plugins/all passes the real
// ones.
func New(opts ...Option) *Plugin {
	p := &Plugin{}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Name is the URL segment the plugin is mounted under.
func (p *Plugin) Name() string { return Name }

// Title is the UI tab label.
func (p *Plugin) Title() string { return "SMS" }

// Description says what this fakes and why.
func (p *Plugin) Description() string {
	return "Captures the SMS and MMS your code sends through a provider API instead of delivering them, " +
		"and shows each one as a phone-style conversation with the segment count and encoding it would " +
		"really cost on the wire."
}

// Providers returns the providers this plugin was built with.
func (p *Plugin) Providers() []plugin.Provider {
	if p.providers == nil {
		return []plugin.Provider{}
	}
	return p.providers
}

// Templates returns the tab's embedded templates.
func (p *Plugin) Templates() fs.FS {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		panic(err) // impossible: the directory is embedded above
	}
	return sub
}

// RegisterAPI mounts the plugin's routes under /api/v1/sms/.
func (p *Plugin) RegisterAPI(mux plugin.Mux, d plugin.Deps) {
	(&apiHandler{deps: d.Normalize()}).mount(mux)
}

// RegisterUI mounts the SMS tab under /ui/sms/. Whatever it does not claim -
// the per-event inspector, for one - falls back to the core's generic view.
func (p *Plugin) RegisterUI(mux plugin.Mux, d plugin.Deps) {
	newUIHandler(p, d.Normalize()).mount(mux)
}

// MessageOf extracts the canonical message from an event, re-deriving the
// segment information so a message is never shown with stale numbers.
//
// It accepts the in-process payload a provider appended (*Message), a value
// copy, and a payload that has been through JSON, so a store that round-trips
// events later does not break every read surface.
func MessageOf(e *event.Event) (*Message, bool) {
	if e == nil || e.Payload == nil {
		return nil, false
	}
	var m *Message
	switch p := e.Payload.(type) {
	case *Message:
		if p == nil {
			return nil, false
		}
		clone := *p
		m = &clone
	case Message:
		clone := p
		m = &clone
	default:
		encoded, err := json.Marshal(e.Payload)
		if err != nil {
			return nil, false
		}
		var decoded Message
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return nil, false
		}
		if decoded.To == "" && decoded.Body == "" && len(decoded.Media) == 0 && decoded.From == "" {
			return nil, false
		}
		m = &decoded
	}
	m.Normalize()
	return m, true
}

// Messages extracts every decodable message from a list of events, keeping the
// event alongside it. Events of another type - a provider's future status
// callback, say - are skipped rather than guessed at.
func Messages(events []*event.Event) []Captured {
	out := make([]Captured, 0, len(events))
	for _, e := range events {
		if e.Type != EventType {
			continue
		}
		m, ok := MessageOf(e)
		if !ok {
			continue
		}
		out = append(out, Captured{Event: e, Message: m})
	}
	return out
}

// Captured pairs an event with the message decoded from it.
type Captured struct {
	Event   *event.Event
	Message *Message
}
