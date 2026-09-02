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

// providerOptionBuilder accumulates the provider-config overrides a
// single-plugin shortcut's provider-specific flags produce (--smtp-port,
// --ftp-passive-ports, ...), contributing a key only when its flag was
// actually changed on the command line - so a flag nobody set never
// overrides a provider's own default. Built fresh in each subcommand's RunE
// and handed to singlePluginConfig as its providerOptions.
type providerOptionBuilder struct {
	cmd     *cobra.Command
	options map[string]map[string]any
}

func newProviderOptionBuilder(cmd *cobra.Command) *providerOptionBuilder {
	return &providerOptionBuilder{cmd: cmd, options: map[string]map[string]any{}}
}

// set records value under provider/key, but only when flagName was changed on
// the command line. value should be the Go type config.ProviderConfig expects
// (int for an integer key, never a string), since these values are merged
// straight into the map config.NewProviderConfig builds a section from.
func (b *providerOptionBuilder) set(provider, flagName, key string, value any) {
	if !b.cmd.Flags().Changed(flagName) {
		return
	}
	m := b.options[provider]
	if m == nil {
		m = map[string]any{}
		b.options[provider] = m
	}
	m[key] = value
}

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
	h2c              bool
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
	fl.BoolVar(&f.h2c, "h2c", true,
		"serve cleartext HTTP/2 (h2c) on the ingress alongside HTTP/1.1; --h2c=false disables it")
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
//
// providerOptions carries provider-specific flag overrides, keyed by provider
// name and then by the same TOML key config.NewProviderConfig would read from
// a file (map[string]map[string]any, e.g. {"smtp": {"port": 1025}}). It is
// merged into that provider's "enabled": true map before the section is
// built. A provider named in providerOptions that --enabled-providers
// excluded is an error - a flag that would silently do nothing is worse than
// one that fails loudly - reported here, before anything binds.
func singlePluginConfig(pluginName string, allProviders []string, f singlePluginFlags, providerOptions map[string]map[string]any) (*config.Config, error) {
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

	for provider := range providerOptions {
		if !containsString(want, provider) {
			return nil, fmt.Errorf("%s: flags were given for provider %q, but --enabled-providers only enables %s",
				pluginName, provider, strings.Join(want, ", "))
		}
	}

	for _, name := range want {
		values := map[string]any{"enabled": true}
		for k, v := range providerOptions[name] {
			values[k] = v
		}
		cfg.SetProvider(pluginName, name, config.NewProviderConfig(values))
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
	// Unconditional: a shortcut builds its config from scratch, so there is no
	// file value for the flag's default to clobber.
	cfg.Ingress.H2C = config.Bool(f.h2c)

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
func runSinglePlugin(cmd *cobra.Command, pluginName string, newPlugin func() plugin.Plugin, allProviders []string, f singlePluginFlags, providerOptions map[string]map[string]any) error {
	cfg, err := singlePluginConfig(pluginName, allProviders, f, providerOptions)
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

// smtpOptionFlags are the smtp provider's own CLI flags - the counterpart of
// [plugins.mail.providers.smtp] in tommy.toml. Port and credentials get
// flags, matching CLAUDE.md rule 10 and the plan's call for smtp's port to be
// reachable from the command line: port decides whether an application
// pointed at the default 1025 finds anything there at all, and credentials
// decide whether AUTH is checked or merely recorded. domain,
// max_message_bytes, max_recipients, max_line_length, read_timeout,
// write_timeout and bind are tuning knobs an application never needs to flip
// per test run, so they stay config-file-only - see tommy.toml's
// [plugins.mail.providers.smtp] comments for that reasoning spelled out.
type smtpOptionFlags struct {
	port     int
	username string
	password string
}

var mailSMTPFlags smtpOptionFlags

func registerSMTPOptionFlags(cmd *cobra.Command, f *smtpOptionFlags) {
	fl := cmd.Flags()
	fl.IntVar(&f.port, "smtp-port", smtp.DefaultPort, "port for the smtp provider's own listener (0 picks a free one)")
	fl.StringVar(&f.username, "smtp-username", "", "pin the AUTH username the smtp provider accepts (unset accepts any, or none)")
	fl.StringVar(&f.password, "smtp-password", "", "pin the AUTH password the smtp provider accepts")
}

// mailjetOptionFlags are the mailjet provider's own CLI flags - the
// counterpart of [plugins.mail.providers.mailjet] in tommy.toml. Pinning
// api_key/secret_key is the same error-path test as pinning smtp's AUTH
// credentials (a mismatch then gets Mailjet's real 401), so it gets a flag
// on the same reasoning. mailjet has no port of its own to expose: every
// HTTP provider shares the one ingress listener and is told apart by path,
// and core has no per-provider-listener mechanism for an HTTP provider (see
// core/config/provider.go's Port field, and core/server/ingress/mount.go,
// which path-routes every HTTP provider onto the shared ingress with no
// notion of a per-provider port at all) - a --mailjet-port flag would set a
// config key nothing reads.
type mailjetOptionFlags struct {
	apiKey    string
	secretKey string
}

var mailMailjetFlags mailjetOptionFlags

func registerMailjetOptionFlags(cmd *cobra.Command, f *mailjetOptionFlags) {
	fl := cmd.Flags()
	fl.StringVar(&f.apiKey, "mailjet-api-key", "", "pin the api_key the mailjet provider accepts; a mismatch then gets Mailjet's real 401")
	fl.StringVar(&f.secretKey, "mailjet-secret-key", "", "pin the secret_key the mailjet provider accepts (only checked when --mailjet-api-key is also set)")
}

// sendgridOptionFlags are the sendgrid provider's own CLI flags - the
// counterpart of [plugins.mail.providers.sendgrid] in tommy.toml. Same
// reasoning as mailjet: pinning api_key is the error-path test that gets
// SendGrid's real 401 on a mismatch, and there is no port flag for the same
// reason mailjet has none.
type sendgridOptionFlags struct {
	apiKey string
}

var mailSendgridFlags sendgridOptionFlags

func registerSendgridOptionFlags(cmd *cobra.Command, f *sendgridOptionFlags) {
	fl := cmd.Flags()
	fl.StringVar(&f.apiKey, "sendgrid-api-key", "", "pin the bearer token the sendgrid provider accepts; a mismatch then gets SendGrid's real 401")
}

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
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		providers := mailProviders()
		opts := newProviderOptionBuilder(cmd)
		opts.set(smtp.ProviderName, "smtp-port", "port", mailSMTPFlags.port)
		opts.set(smtp.ProviderName, "smtp-username", "username", mailSMTPFlags.username)
		opts.set(smtp.ProviderName, "smtp-password", "password", mailSMTPFlags.password)
		opts.set(mailjet.ProviderName, "mailjet-api-key", "api_key", mailMailjetFlags.apiKey)
		opts.set(mailjet.ProviderName, "mailjet-secret-key", "secret_key", mailMailjetFlags.secretKey)
		opts.set(sendgrid.ProviderName, "sendgrid-api-key", "api_key", mailSendgridFlags.apiKey)
		return runSinglePlugin(cmd, mail.PluginName, func() plugin.Plugin {
			return mail.New(mailProviders()...)
		}, providerNames(providers), mailFlags, opts.options)
	},
}

func init() {
	registerSinglePluginFlags(mailCmd, &mailFlags)
	registerSMTPOptionFlags(mailCmd, &mailSMTPFlags)
	registerMailjetOptionFlags(mailCmd, &mailMailjetFlags)
	registerSendgridOptionFlags(mailCmd, &mailSendgridFlags)
	rootCmd.AddCommand(mailCmd)
}
