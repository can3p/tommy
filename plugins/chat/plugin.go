package chat

import (
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"

	"github.com/can3p/tommy/core/plugin"
)

// APIPrefix is where the core mounts this plugin's API. Routes are registered
// with the prefix already stripped; it is spelled out here so the API and the
// tab can hand out links a browser can follow.
const APIPrefix = "/api/v1/" + PluginName

// UIPrefix is where the core mounts this plugin's tab, likewise stripped from
// the patterns RegisterUI uses.
const UIPrefix = "/ui/" + PluginName

//go:embed ui/*.html
var uiFS embed.FS

// RichRenderer turns one piece of structured content into HTML.
//
// This is the seam the card-rendering task fills. The chat plugin ships the
// plain-text fallback plus a collapsible JSON inspector for every format, which
// is why message capture never waits on rendering fidelity; a renderer for
// Block Kit or Adaptive Cards is then dropped in here without touching capture,
// the model or the API.
//
// It takes the format discriminator and the verbatim JSON as primitives rather
// than a chat.Content, so the package implementing it can live under
// plugins/chat/ui/... without importing this package and creating a cycle.
// Returning false means "I do not handle this one", and the content falls back
// to text plus the inspector.
//
// The HTML it returns is injected into the page unescaped, so a renderer must
// escape every string it takes out of the payload: card content is written by
// the application under test and is untrusted.
type RichRenderer func(format string, data json.RawMessage) (template.HTML, bool)

// Plugin is tommy's chat content type: the canonical Message, the read-back API
// and the channel-and-stream tab. Providers are supplied at construction time
// so the plugin never imports them, which is what keeps a provider free to
// depend on whatever its vendor's wire format needs.
type Plugin struct {
	providers []plugin.Provider
	rich      RichRenderer
}

// New returns the chat plugin serving the given providers. Passing none is
// valid for a test that injects messages into the store directly, though a
// plugin with no provider can never receive anything and conformance says so.
func New(providers ...plugin.Provider) *Plugin {
	return &Plugin{providers: providers}
}

// WithRichRenderer installs a renderer for structured content and returns the
// plugin, so the wiring reads chat.New(slack.New(), msteams.New()).
// WithRichRenderer(blocks.Render). Without one, every card renders as its text
// fallback plus a JSON inspector.
func (p *Plugin) WithRichRenderer(r RichRenderer) *Plugin {
	p.rich = r
	return p
}

// Name is the URL segment the plugin is mounted under.
func (p *Plugin) Name() string { return PluginName }

// Title is the UI tab label.
func (p *Plugin) Title() string { return "Chat" }

// Description says what this fakes and why.
func (p *Plugin) Description() string {
	return "Captures the Slack and Microsoft Teams messages your application posts instead of delivering them, " +
		"keeping each one's Block Kit blocks or Adaptive Card exactly as it was sent. " +
		"The chat tab groups them by channel and nests replies under the message they answer."
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
		panic("chat: embedded ui templates: " + err.Error())
	}
	return sub
}

// RegisterAPI mounts the plugin's routes under /api/v1/chat/.
func (p *Plugin) RegisterAPI(mux plugin.Mux, d plugin.Deps) {
	(&apiHandler{deps: d.Normalize()}).mount(mux)
}

// RegisterUI mounts the chat tab under /ui/chat/. Whatever it does not claim -
// the per-event raw inspector, for one - falls back to the core's generic view.
func (p *Plugin) RegisterUI(mux plugin.Mux, d plugin.Deps) {
	newUIHandler(p, d.Normalize()).mount(mux)
}
