// cmd/snmp.go is the snmp half of the single-plugin shortcuts described in
// cmd/mail.go's package comment. It shares that file's flag set and
// bootstrap helper - both live in cmd/mail.go since they are plugin-agnostic
// - and adds only what is specific to snmp: which providers exist, how to
// build the plugin, and the CLI flag for the trap provider's own listener.
package cmd

import (
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/snmp"
	"github.com/can3p/tommy/plugins/snmp/providers/trap"
	"github.com/spf13/cobra"
)

var snmpFlags singlePluginFlags

// trapOptionFlags are the trap provider's own CLI flags - the counterpart of
// [plugins.snmp.providers.trap] in tommy.toml. Port is the only flag-worthy
// setting: SNMP traps carry a community string, not a login, and this
// provider - like every provider in tommy - accepts any of them and simply
// records what arrived, so there is nothing to pin the way smtp's or ftp's
// username/password flags do.
type trapOptionFlags struct {
	port int
}

var snmpTrapFlags trapOptionFlags

func registerTrapOptionFlags(cmd *cobra.Command, f *trapOptionFlags) {
	fl := cmd.Flags()
	fl.IntVar(&f.port, "trap-port", trap.DefaultPort, "port for the trap provider's own udp listener (0 picks a free one)")
}

// snmpProviders returns fresh instances of every snmp provider this binary
// ships, kept in sync with plugins/all/all.go by hand.
func snmpProviders() []plugin.Provider {
	return []plugin.Provider{trap.New()}
}

var snmpCmd = &cobra.Command{
	Use:   "snmp",
	Short: "Run only the snmp plugin: trap",
	Long: `Run tommy with just the snmp plugin enabled - a shortcut for tommy serve
with every other plugin switched off, for a test suite that only needs to
catch SNMP traps and informs.

  tommy snmp --ui-port 8811 --in-port 8822 --trap-port 1162

builds the same Config struct tommy serve --config would build from a TOML
file whose [plugins] section mentions only snmp, and runs it through the
exact same bootstrap. With no --enabled-providers every snmp provider this
binary ships is enabled.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		providers := snmpProviders()
		opts := newProviderOptionBuilder(cmd)
		opts.set(trap.ProviderName, "trap-port", "port", snmpTrapFlags.port)
		return runSinglePlugin(cmd, snmp.Name, func() plugin.Plugin {
			return snmp.New(trap.New())
		}, providerNames(providers), snmpFlags, opts.options)
	},
}

func init() {
	registerSinglePluginFlags(snmpCmd, &snmpFlags)
	registerTrapOptionFlags(snmpCmd, &snmpTrapFlags)
	rootCmd.AddCommand(snmpCmd)
}
