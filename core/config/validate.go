package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Validate checks the config for anything that would fail at runtime, and
// reports every problem at once rather than one per run.
func (c *Config) Validate() error {
	var errs []error

	for _, l := range []struct {
		name string
		lc   ListenerConfig
	}{
		{"ui", c.UI}, {"api", c.API}, {"ingress", c.Ingress},
	} {
		if l.lc.Port == nil {
			errs = append(errs, fmt.Errorf("%s: port is not set (call ApplyDefaults)", l.name))
			continue
		}
		if p := *l.lc.Port; p < 0 || p > 65535 {
			errs = append(errs, fmt.Errorf("%s: port %d out of range", l.name, p))
		}
	}

	if c.Storage.Capacity <= 0 {
		errs = append(errs, fmt.Errorf("storage: capacity must be > 0, got %d", c.Storage.Capacity))
	}
	for plugin, n := range c.Storage.PluginCapacity {
		if n <= 0 {
			errs = append(errs, fmt.Errorf("storage: plugin_capacity.%s must be > 0, got %d", plugin, n))
		}
	}
	if c.Storage.BlobLimit <= 0 {
		errs = append(errs, fmt.Errorf("storage: blob_limit must be > 0, got %s", c.Storage.BlobLimit))
	}

	// Dedicated provider ports must not collide with each other or with a core
	// listener. Port 0 is "ephemeral", so it never collides.
	type claim struct{ who string }
	claimed := map[int]string{}
	if c.UI.Port != nil && *c.UI.Port != 0 {
		claimed[*c.UI.Port] = "ui"
	}
	if c.API.Port != nil && *c.API.Port != 0 && !c.apiShared {
		if who, ok := claimed[*c.API.Port]; ok {
			errs = append(errs, fmt.Errorf("api: port %d already used by %s", *c.API.Port, who))
		} else {
			claimed[*c.API.Port] = "api"
		}
	}
	if c.Ingress.Port != nil && *c.Ingress.Port != 0 && !c.ingressShared {
		if who, ok := claimed[*c.Ingress.Port]; ok {
			errs = append(errs, fmt.Errorf("ingress: port %d already used by %s", *c.Ingress.Port, who))
		} else {
			claimed[*c.Ingress.Port] = "ingress"
		}
	}

	for _, plugin := range sortedKeys(c.Plugins) {
		pc := c.Plugins[plugin]
		if strings.TrimSpace(plugin) == "" {
			errs = append(errs, errors.New("plugins: empty plugin name"))
			continue
		}
		for _, provider := range sortedKeys(pc.Providers) {
			prov := pc.Providers[provider]
			if strings.TrimSpace(provider) == "" {
				errs = append(errs, fmt.Errorf("plugins.%s: empty provider name", plugin))
				continue
			}
			if prov.Port == 0 {
				continue
			}
			if prov.Port < 0 || prov.Port > 65535 {
				errs = append(errs, fmt.Errorf("plugins.%s.providers.%s: port %d out of range", plugin, provider, prov.Port))
				continue
			}
			if !c.ProviderEnabled(plugin, provider) {
				continue
			}
			who := fmt.Sprintf("plugins.%s.providers.%s", plugin, provider)
			if other, ok := claimed[prov.Port]; ok {
				errs = append(errs, fmt.Errorf("%s: port %d already used by %s", who, prov.Port, other))
				continue
			}
			claimed[prov.Port] = who
		}
	}

	return errors.Join(errs...)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
