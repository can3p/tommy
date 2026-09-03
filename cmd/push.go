// cmd/push.go is the push half of the single-plugin shortcuts described in
// cmd/mail.go's package comment. It shares that file's flag set and
// bootstrap helper - both live in cmd/mail.go since they are plugin-agnostic
// - and adds only what is specific to push: which providers exist, how to
// build the plugin, and the CLI flags for fcm's own settings.
//
// Two providers ship: fcm and apns. apns needs nothing extra from the
// command line to work - the ingress serves cleartext HTTP/2 by default, and
// --h2c is already part of the shared single-plugin flag set - so its own
// flags are only the two values worth pinning.
package cmd

import (
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/push"
	"github.com/can3p/tommy/plugins/push/providers/apns"
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

// apnsOptionFlags are the apns provider's own CLI flags - the counterpart of
// [plugins.push.providers.apns] in tommy.toml.
//
// Both are pins, and both are off by default: the provider accepts any topic
// and any provider token, verifies no signature, and records what it was
// given (CLAUDE.md rule 1). Pinning is for the error path - "does my app
// notice a 403?" - and each pin answers the reason APNs itself answers:
// a topic that is not the pinned one gets 400/TopicDisallowed, a token whose
// kid header is not the pinned key id gets 403/InvalidProviderToken.
//
// apns has no port of its own to expose: like fcm it is path-routed onto the
// one shared ingress. Whether that ingress speaks HTTP/2 is the shared --h2c
// flag, which apns needs left on.
type apnsOptionFlags struct {
	topic string
	keyID string
}

var pushAPNSFlags apnsOptionFlags

func registerAPNSOptionFlags(cmd *cobra.Command, f *apnsOptionFlags) {
	fl := cmd.Flags()
	fl.StringVar(&f.topic, "apns-topic", "",
		"pin the apns-topic (the app's bundle ID) the apns provider accepts; anything else gets APNs' real 400/TopicDisallowed")
	fl.StringVar(&f.keyID, "apns-key-id", "",
		"pin the 10-character key id expected in the provider token's kid header; a mismatch gets APNs' real 403/InvalidProviderToken")
}

func registerFCMOptionFlags(cmd *cobra.Command, f *fcmOptionFlags) {
	fl := cmd.Flags()
	fl.StringVar(&f.bearerToken, "fcm-bearer-token", "",
		"pin the OAuth2 bearer token the fcm provider accepts; a mismatch then gets FCM's real 401/UNAUTHENTICATED")
}

// pushProviders returns fresh instances of every push provider this binary
// ships, kept in sync with plugins/all/all.go by hand.
func pushProviders() []plugin.Provider {
	return []plugin.Provider{fcm.New(), apns.New()}
}

var pushCmd = &cobra.Command{
	Use:   "push",
	Short: "Run only the push plugin: fcm, apns",
	Long: `Run tommy with just the push plugin enabled - a shortcut for tommy serve
with every other plugin switched off, for a test suite that only needs to
catch push notifications.

  tommy push --ui-port 8811 --in-port 8822 --enabled-providers fcm,apns

apns speaks Apple's HTTP/2-only provider API, so it needs the ingress's
cleartext HTTP/2, which is on by default; running with --h2c=false leaves
its route reachable over HTTP/1.1 only, which no real APNs client speaks.

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
		opts.set(apns.ProviderName, "apns-topic", "topic", pushAPNSFlags.topic)
		opts.set(apns.ProviderName, "apns-key-id", "key_id", pushAPNSFlags.keyID)
		return runSinglePlugin(cmd, push.Name, func() plugin.Plugin {
			return push.New(pushProviders()...)
		}, providerNames(providers), pushFlags, opts.options)
	},
}

func init() {
	registerSinglePluginFlags(pushCmd, &pushFlags)
	registerFCMOptionFlags(pushCmd, &pushFCMFlags)
	registerAPNSOptionFlags(pushCmd, &pushAPNSFlags)
	rootCmd.AddCommand(pushCmd)
}
