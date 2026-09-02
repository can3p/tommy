// cmd/sms.go is the sms half of the single-plugin shortcuts described in
// cmd/mail.go's package comment. It shares that file's flag set and bootstrap
// helper - both live in cmd/mail.go since they are plugin-agnostic - and adds
// only what is specific to sms: which providers exist, how to build the
// plugin, and twilio's own CLI flags.
package cmd

import (
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/sms"
	"github.com/can3p/tommy/plugins/sms/providers/twilio"
	"github.com/spf13/cobra"
)

var smsFlags singlePluginFlags

// twilioOptionFlags are the twilio provider's own CLI flags - the
// counterpart of [plugins.sms.providers.twilio] in tommy.toml. Pinning
// account_sid/auth_token is the same error-path test as pinning smtp's AUTH
// or mailjet's api_key: both must match to enable the check, so unset
// accepts anything. twilio has no port of its own to expose: every HTTP
// provider shares the one ingress listener and is told apart by path, and
// core has no per-provider-listener mechanism for an HTTP provider - a
// --twilio-port flag would set a config key nothing reads.
type twilioOptionFlags struct {
	accountSid string
	authToken  string
}

var smsTwilioFlags twilioOptionFlags

func registerTwilioOptionFlags(cmd *cobra.Command, f *twilioOptionFlags) {
	fl := cmd.Flags()
	fl.StringVar(&f.accountSid, "twilio-account-sid", "", "pin the account_sid the twilio provider accepts (both must match to enable the check)")
	fl.StringVar(&f.authToken, "twilio-auth-token", "", "pin the auth_token the twilio provider accepts")
}

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
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		providers := smsProviders()
		opts := newProviderOptionBuilder(cmd)
		opts.set(twilio.Name, "twilio-account-sid", "account_sid", smsTwilioFlags.accountSid)
		opts.set(twilio.Name, "twilio-auth-token", "auth_token", smsTwilioFlags.authToken)
		return runSinglePlugin(cmd, sms.Name, func() plugin.Plugin {
			return sms.New(sms.WithProviders(smsProviders()...))
		}, providerNames(providers), smsFlags, opts.options)
	},
}

func init() {
	registerSinglePluginFlags(smsCmd, &smsFlags)
	registerTwilioOptionFlags(smsCmd, &smsTwilioFlags)
	rootCmd.AddCommand(smsCmd)
}
