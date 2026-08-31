package ingress

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/can3p/tommy/core/plugin"
)

// InfoFunc returns the current plugin/provider description tree.
type InfoFunc func() []plugin.PluginInfo

// NotFoundHandler answers unmatched ingress requests with the list of routes
// that are actually mounted. Pointing an SDK at the wrong path is the most
// common way to use tommy wrong, so the 404 body is part of the product.
func NotFoundHandler(info InfoFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plugins := []plugin.PluginInfo{}
		if info != nil {
			plugins = info()
		}

		if wantsJSON(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":   fmt.Sprintf("tommy: no provider handles %s %s", r.Method, r.URL.Path),
				"plugins": plugins,
			})
			return
		}

		var b strings.Builder
		fmt.Fprintf(&b, "tommy: no provider handles %s %s\n\n", r.Method, r.URL.Path)
		if len(plugins) == 0 {
			b.WriteString("No plugins are enabled. Check your config or run `tommy providers`.\n")
		} else {
			b.WriteString("Enabled providers and the routes they serve:\n\n")
			for _, p := range plugins {
				fmt.Fprintf(&b, "  %s (%s)\n", p.Title, p.Name)
				for _, prov := range p.Providers {
					fmt.Fprintf(&b, "    %s - %s\n", prov.Name, prov.Description)
					if prov.Listener {
						fmt.Fprintf(&b, "      listens on %s\n", prov.Addr)
					}
					for _, e := range prov.Endpoints {
						method := e.Method
						if method == "" {
							method = "ANY"
						}
						fmt.Fprintf(&b, "      %-6s %s\n", method, e.Path)
					}
				}
			}
			b.WriteString("\nRun `tommy providers` for copy-paste examples.\n")
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(b.String()))
	})
}

func wantsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") {
		return true
	}
	return strings.Contains(r.Header.Get("Content-Type"), "application/json") && !strings.Contains(accept, "text/")
}
