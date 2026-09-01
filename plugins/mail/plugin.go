// Package mail is tommy's mail content type: one canonical Message every
// provider converts into, a read-back API, and an inbox tab.
//
// Providers (Mailjet, SendGrid, SMTP) live in plugins/mail/providers/... and
// never talk to each other; all they share is the Message in message.go. A
// single API request may fan out into several Messages, and each one becomes
// its own event.
package mail

import (
	"embed"
	"io/fs"

	"github.com/can3p/tommy/core/plugin"
)

// APIPrefix is where the core mounts this plugin's API. Routes are registered
// with the prefix already stripped; it is spelled out here only so the API can
// hand out links a browser can follow.
const APIPrefix = "/api/v1/" + PluginName

// UIPrefix is where the core mounts this plugin's tab, likewise stripped from
// the patterns RegisterUI uses.
const UIPrefix = "/ui/" + PluginName

//go:embed ui/*.html
var uiFS embed.FS

// Plugin is the mail content type.
type Plugin struct {
	providers []plugin.Provider
}

// New returns the mail plugin serving the given providers. Passing none is
// valid and yields a plugin that can still show and serve whatever is injected
// into the store directly, which is how the tests drive it.
func New(providers ...plugin.Provider) *Plugin {
	return &Plugin{providers: providers}
}

// Name implements plugin.Plugin.
func (p *Plugin) Name() string { return PluginName }

// Title implements plugin.Plugin.
func (p *Plugin) Title() string { return "Mail" }

// Description implements plugin.Plugin.
func (p *Plugin) Description() string {
	return "Captures the email your application sends instead of delivering it, whether it went out through a vendor's HTTP API or plain SMTP. " +
		"Every message is parsed into one canonical model you can read back over the API or browse in the inbox tab, attachments included."
}

// Providers implements plugin.Plugin.
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
		panic("mail: embedded ui templates: " + err.Error())
	}
	return sub
}
