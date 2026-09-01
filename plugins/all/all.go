// Package all is the single shared wiring point: it names every plugin this
// binary ships. Registration is explicit rather than init() magic, so what runs
// is decided in one readable file.
package all

import (
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/mail"
	"github.com/can3p/tommy/plugins/sms"
)

// Plugins returns every plugin compiled into this binary.
//
// The plugins carry no providers yet, so both tabs come up empty; the provider
// arguments are filled in as each provider lands.
func Plugins() []plugin.Plugin {
	return []plugin.Plugin{
		mail.New(),
		sms.New(),
	}
}
