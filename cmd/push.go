// cmd/push.go is the push half of the single-plugin shortcuts described in
// cmd/mail.go's package comment. It shares that file's flag set and
// bootstrap helper - both live in cmd/mail.go since they are plugin-agnostic
// - and adds only what is specific to push: which providers exist, how to
// build the plugin, and the CLI flags for fcm's own settings.
//
// Only fcm ships today. apns follows in a later wave once the ingress
// speaks HTTP/2 (see docs/implementation-plan.md, Wave 7); this file adds
// its provider and flags the same way once it lands.
package cmd

import (
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/push"
	"github.com/can3p/tommy/plugins/push/providers/fcm"
	"github.com/spf13/cobra"
)

var pushFlags singlePluginFlags

// fcmOptionFlags are the fcm provider's own CLI flags - the counterpart of
// [plugins.push.providers.fcm] in tommy.toml. Pinning bearer_token is the
// same error-path test as pinning sendgrid's api_key or smtp's AUTH
// credentials (a mismatch then gets FCM's real 401/UNAUTHENTICATED), so it
// gets a flag on the same reasoning (CLAUDE.md rule 10). fcm has no port of
// its own to expose: it is an HTTP provider path-routed onto the one shared
// ingress, the same reasoning cmd/mail.go's sendgrid and mailjet flags spell
// out in full.
type fcmOptionFlags struct {
	bearerToken string
}

var pushFCMFlags fcmOptionFlags

func registerFCMOptionFlags(cmd *cobra.Command, f *fcmOptionFlags) {
	fl := cmd.Flags()
	fl.StringVar(&f.bearerToken, "fcm-bearer-token", "",
		"pin the OAuth2 bearer token the fcm provider accepts; a mismatch then gets FCM's real 401/UNAUTHENTICATED")
}

// pushProviders returns fresh instances of every push provider this binary
// ships, kept in sync with plugins/all/all.go by hand.
func pushProviders() []plugin.Provider {
	return []plugin.Provider{fcm.New()}
}

var pushCmd = &cobra.Command{
	Use:   "push",
	Short: "Run only the push plugin: fcm",
	Long: `Run tommy with just the push plugin enabled - a shortcut for tommy serve
with every other plugin switched off, for a test suite that only needs to
catch push notifications.

  tommy push --ui-port 8811 --in-port 8822 --enabled-providers fcm

builds the same Config struct tommy serve --config would build from a TOML
file whose [plugins] section mentions only push, and runs it through the
exact same bootstrap. With no --enabled-providers every push provider this
binary ships is enabled.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		providers := pushProviders()
		opts := newProviderOptionBuilder(cmd)
		opts.set(fcm.ProviderName, "fcm-bearer-token", "bearer_token", pushFCMFlags.bearerToken)
		return runSinglePlugin(cmd, push.Name, func() plugin.Plugin {
			return push.New(pushProviders()...)
		}, providerNames(providers), pushFlags, opts.options)
	},
}

func init() {
	registerSinglePluginFlags(pushCmd, &pushFlags)
	registerFCMOptionFlags(pushCmd, &pushFCMFlags)
	rootCmd.AddCommand(pushCmd)
}
