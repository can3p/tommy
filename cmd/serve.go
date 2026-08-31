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
	"github.com/spf13/cobra"
)

var serveFlags struct {
	configPath  string
	uiPort      int
	apiPort     int
	ingressPort int
	bind        string
	host        string
	logLevel    string
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run tommy: the UI, the API and every enabled fake service",
	Long: `Run tommy with all enabled plugins.

With no --config, every compiled-in plugin runs with its defaults: the UI and
API on :8811 and the shared ingress on :8822.`,
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

	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
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

	rootCmd.AddCommand(serveCmd)
}
