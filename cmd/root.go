package cmd

import (
	"os"

	cmd "github.com/can3p/kleiner/shared/cmd/cobra"
	"github.com/can3p/kleiner/shared/published"
	"github.com/can3p/kleiner/shared/types"
	"github.com/can3p/tommy/generated/buildinfo"
	"github.com/spf13/cobra"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "tommy",
	Short: "Mock the services that are painful to test locally",
	Long: `tommy stands in for the services an application talks to but which are
awkward to run locally - mail providers, SMS gateways, file transfer - and shows
you exactly what your code sent.`,
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

// maybeCheckForUpdates runs kleiner's update notifier for released builds.
//
// It is wrapped for two reasons. Setting TOMMY_NO_UPDATE_CHECK skips it, so
// tests, CI and container images never reach out to the network at startup.
// The recover guards against kleiner v0.0.14, which prints the error but does
// not return when the GitHub API is unreachable, then calls Newer on the nil
// version it just failed to fetch - panicking before any command can run. An
// update check must never stop tommy from starting.
func maybeCheckForUpdates(info *types.BuildInfo) {
	if os.Getenv("TOMMY_NO_UPDATE_CHECK") != "" {
		return
	}

	defer func() {
		_ = recover()
	}()

	published.MaybeNotifyAboutNewVersion(info)
}

func init() {
	info := buildinfo.Info()

	cmd.Setup(info, rootCmd)
	maybeCheckForUpdates(info)
}
