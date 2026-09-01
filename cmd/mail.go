// cmd/mail.go and cmd/sms.go are the single-plugin shortcuts from
// docs/implementation-plan.md §4: a way to run just one content type without
// writing a TOML file. They build the very same config.Config that
// `tommy serve --config` would build from a TOML file that mentions only that
// plugin, and hand it to the very same core/server bootstrap `tommy serve`
// uses (server.New / Start / Shutdown) - there is exactly one code path.
package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/server"
	"github.com/can3p/tommy/generated/buildinfo"
	"github.com/can3p/tommy/plugins/mail"
	"github.com/can3p/tommy/plugins/mail/providers/mailjet"
	"github.com/can3p/tommy/plugins/mail/providers/sendgrid"
	"github.com/can3p/tommy/plugins/mail/providers/smtp"
	"github.com/spf13/cobra"
)

// singlePluginFlags are the flags every single-plugin shortcut offers. They
// mirror `tommy serve`'s where those make sense (--ui-port, --api-port,
// --bind, --host, --log-level); the ingress port is spelled --in-port to
// match the shortcut's own convention (docs/implementation-plan.md §4), and
// --enabled-providers has no equivalent on `serve` at all, since serve runs
// every configured plugin's providers rather than picking from one plugin.
type singlePluginFlags struct {
	uiPort           int
	apiPort          int
	inPort           int
	bind             string
	host             string
	logLevel         string
	enabledProviders string
}

// registerSinglePluginFlags wires the shared flag set onto cmd.
func registerSinglePluginFlags(cmd *cobra.Command, f *singlePluginFlags) {
	fl := cmd.Flags()
	fl.IntVar(&f.uiPort, "ui-port", -1, "port for the web UI (0 picks a free one)")
	fl.IntVar(&f.apiPort, "api-port", -1, "port for the API (defaults to the UI port)")
	fl.IntVar(&f.inPort, "in-port", -1, "port for the fake-API ingress (0 picks a free one)")
	fl.StringVar(&f.bind, "bind", "", "interface to bind (default 127.0.0.1)")
	fl.StringVar(&f.host, "host", "", "hostname used in printed URLs and snippets (default localhost)")
	fl.StringVar(&f.logLevel, "log-level", "info", "debug, info, warn or error")
	fl.StringVar(&f.enabledProviders, "enabled-providers", "",
		"comma-separated providers to enable (default: every provider this plugin ships)")
}

// providerNames extracts the Name() of each provider, in order.
func providerNames(providers []plugin.Provider) []string {
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.Name())
	}
	return names
}

// containsString reports whether s is present in list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// singlePluginConfig builds the Config a single-plugin shortcut runs: the
// named plugin is the only one enabled, and its providers are limited to
// those named by f.enabledProviders (default: every provider that plugin
// ships). This is the one place the CLI-flags path and the TOML path
// diverge - it ends with a plain *config.Config, and everything downstream
// (server.New, Start, Shutdown) is identical to `tommy serve`.
//
// An unknown provider name is rejected here, naming the valid ones, rather
// than left to fail confusingly once the server is already starting.
func singlePluginConfig(pluginName string, allProviders []string, f singlePluginFlags) (*config.Config, error) {
	cfg := &config.Config{DefaultEnabled: config.Bool(false)}
	cfg.SetPluginEnabled(pluginName, true)

	want := allProviders
	if strings.TrimSpace(f.enabledProviders) != "" {
		want = nil
		for _, name := range strings.Split(f.enabledProviders, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if !containsString(allProviders, name) {
				return nil, fmt.Errorf("unknown %s provider %q: valid providers are %s",
					pluginName, name, strings.Join(allProviders, ", "))
			}
			want = append(want, name)
		}
		if len(want) == 0 {
			return nil, fmt.Errorf("--enabled-providers must name at least one of: %s", strings.Join(allProviders, ", "))
		}
	}
	for _, name := range want {
		cfg.SetProvider(pluginName, name, config.NewProviderConfig(map[string]any{"enabled": true}))
	}

	if f.uiPort >= 0 {
		cfg.UI.Port = config.Int(f.uiPort)
	}
	if f.apiPort >= 0 {
		cfg.API.Port = config.Int(f.apiPort)
	}
	if f.inPort >= 0 {
		cfg.Ingress.Port = config.Int(f.inPort)
	}
	if f.bind != "" {
		cfg.Bind = f.bind
	}
	if f.host != "" {
		cfg.Host = f.host
	}

	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// runSinglePlugin builds the config, boots it through the exact bootstrap
// `tommy serve` uses (server.New / Start / Shutdown - see cmd/serve.go), and
// blocks until interrupted. newPlugin is called once the config is known
// good, so a bad --enabled-providers value never touches the network.
func runSinglePlugin(cmd *cobra.Command, pluginName string, newPlugin func() plugin.Plugin, allProviders []string, f singlePluginFlags) error {
	cfg, err := singlePluginConfig(pluginName, allProviders, f)
	if err != nil {
		return err
	}

	logger, err := newLogger(f.logLevel)
	if err != nil {
		return err
	}

	srv, err := server.New(server.Options{
		Config:  cfg,
		Plugins: []plugin.Plugin{newPlugin()},
		Logger:  logger,
		Version: buildinfo.Version().String(),
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Start(ctx); err != nil {
		return err
	}

	uiURL, apiURL, ingressURL := srv.URLs()
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "tommy is running (%s only)\n", pluginName)
	fmt.Fprintf(out, "  ui       %s\n", uiURL)
	fmt.Fprintf(out, "  api      %s\n", apiURL)
	fmt.Fprintf(out, "  ingress  %s\n", ingressURL)
	if info, err := srv.Describe(); err == nil {
		for _, p := range info {
			names := make([]string, 0, len(p.Providers))
			for _, prov := range p.Providers {
				names = append(names, prov.Name)
			}
			fmt.Fprintf(out, "  plugin   %s (%v)\n", p.Name, names)
		}
	}
	fmt.Fprintf(out, "run `tommy providers %s` for copy-paste examples\n", pluginName)

	<-ctx.Done()
	fmt.Fprintln(out, "shutting down")
	return srv.Shutdown(context.WithoutCancel(ctx))
}

var mailFlags singlePluginFlags

// mailProviders returns fresh instances of every mail provider this binary
// ships. Kept in sync with plugins/all/all.go by hand - there being only one
// wiring list to update per new provider is the tradeoff of I1 owning the
// shortcut instead of every provider registering itself.
func mailProviders() []plugin.Provider {
	return []plugin.Provider{mailjet.New(), sendgrid.New(), smtp.New()}
}

var mailCmd = &cobra.Command{
	Use:   "mail",
	Short: "Run only the mail plugin: mailjet, sendgrid and smtp",
	Long: `Run tommy with just the mail plugin enabled - a shortcut for tommy serve
with every other plugin switched off, for a test suite that only needs to
catch email.

  tommy mail --ui-port 8811 --in-port 8822 --enabled-providers mailjet,sendgrid

builds the same Config struct tommy serve --config would build from a TOML
file whose [plugins] section mentions only mail, and runs it through the
exact same bootstrap. With no --enabled-providers every mail provider this
binary ships is enabled.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		providers := mailProviders()
		return runSinglePlugin(cmd, mail.PluginName, func() plugin.Plugin {
			return mail.New(mailProviders()...)
		}, providerNames(providers), mailFlags)
	},
}

func init() {
	registerSinglePluginFlags(mailCmd, &mailFlags)
	rootCmd.AddCommand(mailCmd)
}
