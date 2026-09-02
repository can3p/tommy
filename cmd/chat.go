// cmd/chat.go is the chat half of the single-plugin shortcuts described in
// cmd/mail.go's package comment. It shares that file's flag set and bootstrap
// helper - both live in cmd/mail.go since they are plugin-agnostic - and adds
// only what is specific to chat: which providers exist and how to build the
// plugin, including the rich Block Kit / Adaptive Card renderer
// plugins/all/all.go wires in, so tommy chat renders exactly what tommy
// serve does.
package cmd

import (
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/chat"
	"github.com/can3p/tommy/plugins/chat/providers/msteams"
	"github.com/can3p/tommy/plugins/chat/providers/slack"
	"github.com/can3p/tommy/plugins/chat/ui/blocks"
	"github.com/spf13/cobra"
)

var chatFlags singlePluginFlags

// chatProviders returns fresh instances of every chat provider this binary
// ships, kept in sync with plugins/all/all.go by hand. Neither slack nor
// msteams reads any provider-specific config key at all - both HTTP
// providers share the one ingress listener and are told apart by path, and
// core has no per-provider-listener mechanism for an HTTP provider (see
// core/config/provider.go and core/server/ingress/mount.go) - so unlike
// files and mail this command has no provider-specific flags to register.
func chatProviders() []plugin.Provider {
	return []plugin.Provider{slack.New(), msteams.New()}
}

var chatCmd = &cobra.Command{
	Use:   "chat",
	Short: "Run only the chat plugin: slack and msteams",
	Long: `Run tommy with just the chat plugin enabled - a shortcut for tommy serve
with every other plugin switched off, for a test suite that only needs to
catch chat webhooks.

  tommy chat --ui-port 8811 --in-port 8822 --enabled-providers slack

builds the same Config struct tommy serve --config would build from a TOML
file whose [plugins] section mentions only chat, and runs it through the exact
same bootstrap. With no --enabled-providers every chat provider this binary
ships is enabled.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		providers := chatProviders()
		return runSinglePlugin(cmd, chat.PluginName, func() plugin.Plugin {
			// Match plugins/all/all.go exactly, rich renderer included, or
			// tommy chat would silently render less than tommy serve does.
			return chat.New(slack.New(), msteams.New()).WithRichRenderer(blocks.Render)
		}, providerNames(providers), chatFlags, nil)
	},
}

func init() {
	registerSinglePluginFlags(chatCmd, &chatFlags)
	rootCmd.AddCommand(chatCmd)
}
