// cmd/sms.go is the sms half of the single-plugin shortcuts described in
// cmd/mail.go's package comment. It shares that file's flag set and bootstrap
// helper - both live in cmd/mail.go since they are plugin-agnostic - and adds
// only what is specific to sms: which providers exist and how to build the
// plugin.
package cmd

import (
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/sms"
	"github.com/can3p/tommy/plugins/sms/providers/twilio"
	"github.com/spf13/cobra"
)

var smsFlags singlePluginFlags

// smsProviders returns fresh instances of every sms provider this binary
// ships, kept in sync with plugins/all/all.go by hand.
func smsProviders() []plugin.Provider {
	return []plugin.Provider{twilio.New()}
}

var smsCmd = &cobra.Command{
	Use:   "sms",
	Short: "Run only the sms plugin: twilio",
	Long: `Run tommy with just the sms plugin enabled - a shortcut for tommy serve
with every other plugin switched off, for a test suite that only needs to
catch text messages.

  tommy sms --ui-port 8811 --in-port 8822 --enabled-providers twilio

builds the same Config struct tommy serve --config would build from a TOML
file whose [plugins] section mentions only sms, and runs it through the exact
same bootstrap. With no --enabled-providers every sms provider this binary
ships is enabled.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		providers := smsProviders()
		return runSinglePlugin(cmd, sms.Name, func() plugin.Plugin {
			return sms.New(sms.WithProviders(smsProviders()...))
		}, providerNames(providers), smsFlags)
	},
}

func init() {
	registerSinglePluginFlags(smsCmd, &smsFlags)
	rootCmd.AddCommand(smsCmd)
}
