package cmd

import (
	"github.com/can3p/tommy/core/server/api"
	"github.com/spf13/cobra"
)

var openapiFlags struct {
	serverURL string
}

var openapiCmd = &cobra.Command{
	Use:   "openapi",
	Short: "Print the OpenAPI description of the events API",
	Long: `Print the OpenAPI 3.1 description of tommy's events API as JSON.

These are the routes that read back what an application sent: list events,
fetch one, stream them live, delete them, download the bytes they carried.
Generate a client from it and a test can assert on what your code sent without
knowing anything about the vendor it thought it was talking to.

It is generated from the server's route table, so it cannot describe a route
that does not exist. A running server answers GET /api/v1/openapi.json with the
same document, naming itself as the server URL.

The repository's docs/openapi.json is exactly this output, and a test fails when
the two stop matching - run ` + "`make openapi`" + ` after changing an events route.

Two things are deliberately not described. The fake vendor endpoints are the
vendors' specifications, not tommy's - use ` + "`tommy providers`" + ` for those. Each
plugin's own read-back routes are documented in that plugin's README.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		spec := api.BuildSpec(api.SpecOptions{ServerURL: openapiFlags.serverURL})
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
