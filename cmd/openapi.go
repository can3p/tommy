package cmd

import (
	"fmt"
	"sort"
	"strings"

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
	Use:   "openapi [plugin]",
	Short: "Print the OpenAPI description of the events API, or of one plugin's",
	Long: `Print an OpenAPI 3.1 description as JSON.

With no argument, the events API: the routes that read back what an application
sent, whatever captured it - list events, fetch one, stream them live, delete
them, download the bytes they carried. Generate a client from it and a test can
assert on what your code sent without knowing anything about the vendor it
thought it was talking to.

With a plugin name, that plugin's own read-back API: the same captures in the
shape that content type wants, with the filters and the byte-serving routes that
only mean something there. One document per surface, so the events description
stays the small one everybody reads.

Both are generated from the server's route table, so neither can describe a
route that does not exist. A running server serves them at
GET /api/v1/openapi.json and GET /api/v1/<plugin>/openapi.json, naming itself as
the server URL.

The repository's docs/openapi.json and docs/openapi-<plugin>.json are exactly
this output, and a test fails when they stop matching - run ` + "`make openapi`" + ` after
changing a route.

The fake vendor endpoints are described nowhere: they are the vendors'
specifications, not tommy's. Use ` + "`tommy providers`" + ` for those.`,
	Args:         cobra.MaximumNArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		var spec *api.Spec
		if len(args) == 0 {
			spec = api.BuildSpec(api.SpecOptions{ServerURL: openapiFlags.serverURL})
		} else {
			p, err := describablePlugin(args[0])
			if err != nil {
				return err
			}
			spec = api.BuildPluginSpec(api.PluginSpecOptions{Plugin: p, ServerURL: openapiFlags.serverURL})
		}
		body, err := spec.JSON()
		if err != nil {
			return err
		}
		_, err = cmd.OutOrStdout().Write(body)
		return err
	},
}

// describablePlugin finds a plugin by name, insisting it has an API to
// describe. Deliberately not the local configuration: the checked-in documents
// describe tommy, not one deployment of it, so a tommy.toml that disables a
// plugin must not make its document unbuildable.
func describablePlugin(name string) (plugin.Plugin, error) {
	reg, err := plugin.New(config.Default(), all.Plugins()...)
	if err != nil {
		return nil, err
	}
	var describable []string
	for _, p := range reg.AllPlugins() {
		if api.BuildPluginSpec(api.PluginSpecOptions{Plugin: p}) == nil {
			continue
		}
		describable = append(describable, p.Name())
		if p.Name() == name {
			return p, nil
		}
	}
	sort.Strings(describable)
	return nil, fmt.Errorf("no plugin %q with an API of its own; try one of: %s",
		name, strings.Join(describable, ", "))
}

func init() {
	rootCmd.AddCommand(openapiCmd)
	openapiCmd.Flags().StringVar(&openapiFlags.serverURL, "server-url", "",
		"base URL the described paths are relative to (default /api/v1)")
}
