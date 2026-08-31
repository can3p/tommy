// Package all is the single shared wiring point: it names every plugin this
// binary ships. Registration is explicit rather than init() magic, so what runs
// is decided in one readable file.
//
// It is empty until Wave 1 lands the mail and sms plugins; the core, the CLI
// and the test harness all work with an empty list.
package all

import "github.com/can3p/tommy/core/plugin"

// Plugins returns every plugin compiled into this binary.
func Plugins() []plugin.Plugin {
	return []plugin.Plugin{}
}
