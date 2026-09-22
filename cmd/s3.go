// cmd/s3.go is the s3 half of the single-plugin shortcuts described in
// cmd/mail.go's package comment. It shares that file's flag set and
// bootstrap helper - both live in cmd/mail.go since they are plugin-agnostic
// - and adds only what is specific to s3: its http provider, how to build the
// plugin, and the CLI flag for the provider's dedicated listener.
package cmd

import (
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/s3"
	s3http "github.com/can3p/tommy/plugins/s3/providers/http"
	"github.com/spf13/cobra"
)

var s3Flags singlePluginFlags

// s3HTTPOptionFlags are the http provider's own CLI flags - the counterpart
// of [plugins.s3.providers.http] in tommy.toml. Port is the only flag-worthy
// setting; body, object-count and timeout limits are config-only tuning knobs.
type s3HTTPOptionFlags struct {
	port int
}

var s3HTTPFlags s3HTTPOptionFlags

func registerS3HTTPOptionFlags(cmd *cobra.Command, f *s3HTTPOptionFlags) {
	cmd.Flags().IntVar(&f.port, "s3-port", s3http.DefaultPort,
		"port for the S3 provider's dedicated HTTP listener (0 picks a free one)")
}

// s3Providers returns fresh instances of every s3 provider this binary ships,
// kept in sync with plugins/all/all.go by hand.
func s3Providers() []plugin.Provider {
	return []plugin.Provider{s3http.New()}
}

var s3Cmd = &cobra.Command{
	Use:   "s3",
	Short: "Run only the s3 plugin: http",
	Long: `Run tommy with just the s3 plugin enabled - a shortcut for tommy serve
with every other plugin switched off, for a test suite that only needs an
S3-compatible object store.

The S3 provider uses a dedicated HTTP listener instead of the shared fake-API
ingress. Point an S3 client at its endpoint and enable path-style addressing,
so bucket and object names are sent as /bucket/key rather than as subdomains:

  tommy s3 --ui-port 8811 --s3-port 9000
  aws --endpoint-url http://localhost:9000 s3api put-object --bucket test --key hello.txt --body hello.txt

This builds the same Config struct tommy serve --config would build from a TOML
file whose [plugins] section mentions only s3, and runs it through the exact
same bootstrap. With no --enabled-providers the http provider is enabled.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		providers := s3Providers()
		opts := newProviderOptionBuilder(cmd)
		opts.set(s3http.ProviderName, "s3-port", "port", s3HTTPFlags.port)
		return runSinglePlugin(cmd, s3.PluginName, func() plugin.Plugin {
			return s3.New(s3http.New())
		}, providerNames(providers), s3Flags, opts.options)
	},
}

func init() {
	registerSinglePluginFlags(s3Cmd, &s3Flags)
	registerS3HTTPOptionFlags(s3Cmd, &s3HTTPFlags)
	rootCmd.AddCommand(s3Cmd)
}
