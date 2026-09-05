package cmd

import (
	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/server/api"
	"github.com/can3p/tommy/plugins/all"
	"github.com/spf13/cobra"
)

var openapiFlags struct {
	serverURL string
}

var openapiCmd = &cobra.Command{
	Use:   "openapi",
	Short: "Print the OpenAPI description of tommy's own API",
	Long: `Print the OpenAPI 3.1 description of /api/v1 as JSON.

It is generated from the route table and each plugin's endpoint declarations,
so it cannot describe a route that does not exist. This prints the description
of every plugin tommy ships, whatever the local configuration says; a running
server answers GET /api/v1/openapi.json with the same document narrowed to the
plugins that instance actually enabled.

The repository's docs/openapi.json is exactly this output, and a test fails
when the two stop matching - run ` + "`make openapi`" + ` after changing an API route.

Note that the fake vendor endpoints are deliberately not described here: they
are the vendors' specifications, not tommy's. Use ` + "`tommy providers`" + ` for those.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Deliberately not the local configuration: this describes tommy, not
		// one deployment of it, so a tommy.toml that disables a plugin must
		// not silently shrink the document that gets checked in.
		cfg := config.Default()
		reg, err := plugin.New(cfg, all.Plugins()...)
		if err != nil {
			return err
		}

		spec := api.BuildSpec(api.SpecOptions{
			Registry:  reg,
			ServerURL: openapiFlags.serverURL,
		})
		body, err := spec.JSON()
		if err != nil {
			return err
		}
		_, err = cmd.OutOrStdout().Write(body)
		return err
	},
}

func init() {
	rootCmd.AddCommand(openapiCmd)
	openapiCmd.Flags().StringVar(&openapiFlags.serverURL, "server-url", "",
		"base URL the described paths are relative to (default /api/v1)")
}
