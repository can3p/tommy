package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/all"
	"github.com/spf13/cobra"
)

var providersFlags struct {
	asJSON bool
}

var providersCmd = &cobra.Command{
	Use:   "providers [plugin|plugin/provider]",
	Short: "Print what each plugin and provider fakes, and how to poke it",
	Long: `Print every enabled plugin and provider with its description, the endpoints it
serves, and copy-paste snippets rendered against the ports this configuration
would bind.

Useful before starting the server, and in CI logs. Pass a plugin name to narrow
the output, or plugin/provider for a single provider.`,
	Args:         cobra.MaximumNArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}

		reg, err := plugin.New(cfg, all.Plugins()...)
		if err != nil {
			return err
		}

		// The snippets are rendered against the addresses this configuration
		// would bind, so what is printed is what would actually work.
		ctx := plugin.NewSnippetCtx(cfg.Host, cfg.UIAddr(), cfg.APIAddr(), cfg.IngressAddr())
		for _, ref := range reg.ListenerRefs() {
			pc := reg.ProviderConfig(ref.Plugin.Name(), ref.Provider.Name())
			if pc.Port > 0 {
				ctx.SetAddr(ref.Plugin.Name(), ref.Provider.Name(), fmt.Sprintf("%s:%d", cfg.Host, pc.Port))
			}
		}

		info, err := reg.Describe(ctx)
		if err != nil {
			return err
		}

		wantPlugin, wantProvider := "", ""
		if len(args) == 1 {
			wantPlugin, wantProvider, _ = strings.Cut(args[0], "/")
		}
		info = filterInfo(info, wantPlugin, wantProvider)
		if len(info) == 0 && wantPlugin != "" {
			return fmt.Errorf("no enabled plugin or provider matches %q", args[0])
		}

		out := cmd.OutOrStdout()
		if providersFlags.asJSON {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(info)
		}
		printProviders(out, info)
		return nil
	},
}

func filterInfo(info []plugin.PluginInfo, wantPlugin, wantProvider string) []plugin.PluginInfo {
	if wantPlugin == "" {
		return info
	}
	var out []plugin.PluginInfo
	for _, p := range info {
		if p.Name != wantPlugin {
			continue
		}
		if wantProvider != "" {
			var kept []plugin.ProviderInfo
			for _, prov := range p.Providers {
				if prov.Name == wantProvider {
					kept = append(kept, prov)
				}
			}
			if len(kept) == 0 {
				continue
			}
			p.Providers = kept
		}
		out = append(out, p)
	}
	return out
}

func printProviders(w io.Writer, info []plugin.PluginInfo) {
	if len(info) == 0 {
		fmt.Fprintln(w, "No plugins are enabled.")
		fmt.Fprintln(w, "Check [plugins] in your config, or build a tommy with plugins compiled in.")
		return
	}
	sorted := append([]plugin.PluginInfo(nil), info...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	for i, p := range sorted {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s (%s)\n", p.Title, p.Name)
		fmt.Fprintf(w, "  %s\n", p.Description)
		for _, prov := range p.Providers {
			fmt.Fprintf(w, "\n  %s/%s", p.Name, prov.Name)
			if prov.Listener {
				fmt.Fprintf(w, "  [own listener%s]", addrSuffix(prov.Addr))
			}
			fmt.Fprintln(w)
			fmt.Fprintf(w, "    %s\n", prov.Description)
			for _, e := range prov.Endpoints {
				method := e.Method
				if method == "" {
					method = "ANY"
				}
				fmt.Fprintf(w, "    %-6s %-42s %s\n", method, e.Path, e.Description)
			}
			for _, s := range prov.Snippets {
				fmt.Fprintf(w, "\n    %s (%s):\n", s.Title, s.Lang)
				for _, line := range strings.Split(s.Code, "\n") {
					fmt.Fprintf(w, "      %s\n", line)
				}
			}
		}
	}
}

func addrSuffix(addr string) string {
	if addr == "" {
		return ""
	}
	return " on " + addr
}

func init() {
	providersCmd.Flags().BoolVar(&providersFlags.asJSON, "json", false, "print the machine-readable form")
	providersCmd.Flags().StringVarP(&serveFlags.configPath, "config", "c", "", "path to a tommy.toml config file")
	rootCmd.AddCommand(providersCmd)
}
