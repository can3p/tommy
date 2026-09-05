package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/server"
	"github.com/can3p/tommy/generated/buildinfo"
	"github.com/can3p/tommy/plugins/all"
	"github.com/can3p/tommy/plugins/as2"
	as2http "github.com/can3p/tommy/plugins/as2/providers/http"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var serveFlags struct {
	configPath  string
	uiPort      int
	apiPort     int
	ingressPort int
	bind        string
	host        string
	logLevel    string
	h2c         bool
	as2CertDir  string
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run tommy: the UI, the API and every enabled fake service",
	Long: `Run tommy with all enabled plugins.

With no --config, every compiled-in plugin runs with its defaults: the UI and
API on :8811 and the shared ingress on :8822.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}

		logger, err := newLogger(serveFlags.logLevel)
		if err != nil {
			return err
		}

		srv, err := server.New(server.Options{
			Config:  cfg,
			Plugins: all.Plugins(),
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
		fmt.Fprintf(out, "tommy is running\n")
		fmt.Fprintf(out, "  ui       %s\n", uiURL)
		fmt.Fprintf(out, "  api      %s\n", apiURL)
		fmt.Fprintf(out, "  ingress  %s\n", ingressURL)
		if info, err := srv.Describe(); err == nil {
			if len(info) == 0 {
				fmt.Fprintf(out, "  plugins  none enabled\n")
			} else {
				for _, p := range info {
					names := make([]string, 0, len(p.Providers))
					for _, prov := range p.Providers {
						names = append(names, prov.Name)
					}
					fmt.Fprintf(out, "  plugin   %s (%v)\n", p.Name, names)
				}
			}
		}
		fmt.Fprintf(out, "run `tommy providers` for copy-paste examples\n")

		<-ctx.Done()
		fmt.Fprintln(out, "shutting down")
		return srv.Shutdown(context.WithoutCancel(ctx))
	},
}

// serveFlagSet is serveCmd's flag set, kept so loadConfig can ask whether a
// flag was actually given without referring to serveCmd itself.
var serveFlagSet *pflag.FlagSet

// loadConfig builds the config from --config plus flag overrides. Both entry
// points produce the same Config struct and run the same bootstrap.
func loadConfig() (*config.Config, error) {
	var cfg *config.Config
	if serveFlags.configPath != "" {
		loaded, err := config.Load(serveFlags.configPath)
		if err != nil {
			return nil, err
		}
		cfg = loaded
	} else {
		cfg = &config.Config{}
	}

	if serveFlags.uiPort >= 0 {
		cfg.UI.Port = config.Int(serveFlags.uiPort)
	}
	if serveFlags.apiPort >= 0 {
		cfg.API.Port = config.Int(serveFlags.apiPort)
	}
	if serveFlags.ingressPort >= 0 {
		cfg.Ingress.Port = config.Int(serveFlags.ingressPort)
	}
	if serveFlags.bind != "" {
		cfg.Bind = serveFlags.bind
		cfg.UI.Bind, cfg.API.Bind, cfg.Ingress.Bind = "", "", ""
	}
	if serveFlags.host != "" {
		cfg.Host = serveFlags.host
	}
	// Only when the flag was actually given, so h2c = false in a config file
	// is not clobbered by the flag's own default. Held as a flag set rather
	// than reached through serveCmd, which would make loadConfig and serveCmd
	// a package-level initialisation cycle; tommy providers shares loadConfig
	// and never registers the flag, so there the file always wins.
	if serveFlagSet != nil && serveFlagSet.Changed("h2c") {
		cfg.Ingress.H2C = config.Bool(serveFlags.h2c)
	}
	// The one provider option `serve` carries a flag for. Every other provider
	// setting belongs in the config file or on a single-plugin shortcut, but
	// as2 writes a key pair on first use and picks the directory beside the
	// config file - which in a container image is a read-only path that the
	// non-root user cannot create anything in. Without a flag here the image's
	// only way to point that at a writable volume would be to ship an altered
	// config, and the shipped config is the repository's own (docs/docker.md).
	if serveFlags.as2CertDir != "" {
		setProviderOption(cfg, as2.Name, as2http.Name, "cert_dir", serveFlags.as2CertDir)
	}

	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// setProviderOption merges one key into a provider's config section, leaving
// every other key the file set alone. It is the flags-over-file rule applied to
// a provider section rather than a core listener.
func setProviderOption(cfg *config.Config, pluginName, providerName, key string, value any) {
	values := cfg.Provider(pluginName, providerName).Values()
	if values == nil {
		values = map[string]any{}
	}
	values[key] = value
	cfg.SetProvider(pluginName, providerName, config.NewProviderConfig(values))
}

func newLogger(level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q: use debug, info, warn or error", level)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})), nil
}

func init() {
	f := serveCmd.Flags()
	f.StringVarP(&serveFlags.configPath, "config", "c", "", "path to a tommy.toml config file")
	f.IntVar(&serveFlags.uiPort, "ui-port", -1, "port for the web UI (0 picks a free one)")
	f.IntVar(&serveFlags.apiPort, "api-port", -1, "port for the API (defaults to the UI port)")
	f.IntVar(&serveFlags.ingressPort, "ingress-port", -1, "port for the shared fake-API ingress")
	f.StringVar(&serveFlags.bind, "bind", "", "interface to bind (default 127.0.0.1)")
	f.StringVar(&serveFlags.host, "host", "", "hostname used in printed URLs and snippets (default localhost)")
	f.StringVar(&serveFlags.logLevel, "log-level", "info", "debug, info, warn or error")
	f.BoolVar(&serveFlags.h2c, "h2c", true,
		"serve cleartext HTTP/2 (h2c) on the ingress alongside HTTP/1.1; --h2c=false disables it")
	f.StringVar(&serveFlags.as2CertDir, "as2-cert-dir", "",
		"directory a generated AS2 certificate is written to and reused from (default: beside the config file, or the OS user config dir)")
	serveFlagSet = f

	rootCmd.AddCommand(serveCmd)
}
