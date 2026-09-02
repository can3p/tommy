// cmd/hl7.go is the hl7 half of the single-plugin shortcuts described in
// cmd/mail.go's package comment. It shares that file's flag set and
// bootstrap helper - both live in cmd/mail.go since they are plugin-agnostic
// - and adds only what is specific to hl7: which providers exist, how to
// build the plugin, and the CLI flag for mllp's own listener.
package cmd

import (
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/hl7"
	"github.com/can3p/tommy/plugins/hl7/providers/mllp"
	"github.com/spf13/cobra"
)

var hl7Flags singlePluginFlags

// mllpOptionFlags are the mllp provider's own CLI flags - the counterpart of
// [plugins.hl7.providers.mllp] in tommy.toml. Port is the only flag-worthy
// setting: MLLP (the framing HL7 v2 is carried in) has no login step at all,
// so there are no credentials to pin the way ftp's and sftp's flags do, and
// max_message_bytes/read_timeout/write_timeout are tuning knobs an
// application never needs to flip per test run - see tommy.toml's
// [plugins.hl7.providers.mllp] comments for that reasoning spelled out, the
// same shape as tftp's config-only knobs.
type mllpOptionFlags struct {
	port int
}

var hl7MLLPFlags mllpOptionFlags

func registerMLLPOptionFlags(cmd *cobra.Command, f *mllpOptionFlags) {
	fl := cmd.Flags()
	fl.IntVar(&f.port, "mllp-port", mllp.DefaultPort, "port for the mllp provider's own listener (0 picks a free one)")
}

// hl7Providers returns fresh instances of every hl7 provider this binary
// ships, kept in sync with plugins/all/all.go by hand.
func hl7Providers() []plugin.Provider {
	return []plugin.Provider{mllp.New()}
}

var hl7Cmd = &cobra.Command{
	Use:   "hl7",
	Short: "Run only the hl7 plugin: mllp",
	Long: `Run tommy with just the hl7 plugin enabled - a shortcut for tommy serve
with every other plugin switched off, for a test suite that only needs to
catch HL7 v2 messages.

  tommy hl7 --ui-port 8811 --in-port 8822 --mllp-port 2575

builds the same Config struct tommy serve --config would build from a TOML
file whose [plugins] section mentions only hl7, and runs it through the
exact same bootstrap. With no --enabled-providers every hl7 provider this
binary ships is enabled.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		providers := hl7Providers()
		opts := newProviderOptionBuilder(cmd)
		opts.set(mllp.ProviderName, "mllp-port", "port", hl7MLLPFlags.port)
		return runSinglePlugin(cmd, hl7.Name, func() plugin.Plugin {
			return hl7.New(mllp.New())
		}, providerNames(providers), hl7Flags, opts.options)
	},
}

func init() {
	registerSinglePluginFlags(hl7Cmd, &hl7Flags)
	registerMLLPOptionFlags(hl7Cmd, &hl7MLLPFlags)
	rootCmd.AddCommand(hl7Cmd)
}
