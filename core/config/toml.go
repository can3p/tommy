package config

import (
	"fmt"

	toml "github.com/pelletier/go-toml/v2"
)

// The wire types are the decoding shape of the TOML document. They exist so the
// public Config can use types TOML cannot express directly (ByteSize accepting
// both `256` and `"256MB"`, ProviderConfig keeping unknown keys for the
// provider to decode) without depending on go-toml's unstable unmarshaler hook.
type wireConfig struct {
	Bind    string         `toml:"bind"`
	Host    string         `toml:"host"`
	UI      ListenerConfig `toml:"ui"`
	API     ListenerConfig `toml:"api"`
	Ingress ListenerConfig `toml:"ingress"`
	Storage wireStorage    `toml:"storage"`

	DefaultEnabled *bool                 `toml:"default_enabled"`
	Plugins        map[string]wirePlugin `toml:"plugins"`
}

type wireStorage struct {
	Capacity       int            `toml:"capacity"`
	PluginCapacity map[string]int `toml:"plugin_capacity"`
	BlobLimit      any            `toml:"blob_limit"`
}

type wirePlugin struct {
	Enabled   *bool                     `toml:"enabled"`
	Providers map[string]map[string]any `toml:"providers"`
}

// Parse decodes, defaults and validates a TOML document.
func Parse(data []byte) (*Config, error) {
	var w wireConfig
	if err := toml.Unmarshal(data, &w); err != nil {
		return nil, err
	}

	c := &Config{
		Bind:           w.Bind,
		Host:           w.Host,
		UI:             w.UI,
		API:            w.API,
		Ingress:        w.Ingress,
		DefaultEnabled: w.DefaultEnabled,
		Storage: StorageConfig{
			Capacity:       w.Storage.Capacity,
			PluginCapacity: w.Storage.PluginCapacity,
		},
		Plugins: map[string]PluginConfig{},
	}

	switch v := w.Storage.BlobLimit.(type) {
	case nil:
	case string:
		size, err := ParseByteSize(v)
		if err != nil {
			return nil, fmt.Errorf("storage.blob_limit: %w", err)
		}
		c.Storage.BlobLimit = size
	case int64:
		if v < 0 {
			return nil, fmt.Errorf("storage.blob_limit: byte size %d is negative", v)
		}
		c.Storage.BlobLimit = ByteSize(v)
	default:
		return nil, fmt.Errorf("storage.blob_limit: byte size must be an integer or a string like \"256MB\", got %T", v)
	}

	for name, wp := range w.Plugins {
		pc := PluginConfig{Enabled: wp.Enabled, Providers: map[string]ProviderConfig{}}
		for pname, values := range wp.Providers {
			pc.Providers[pname] = NewProviderConfig(values)
		}
		c.Plugins[name] = pc
	}

	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Marshal renders the config back to TOML, so a config assembled by the CLI can
// be printed or written out.
func (c *Config) Marshal() ([]byte, error) {
	w := wireConfig{
		Bind:    c.Bind,
		Host:    c.Host,
		UI:      c.UI,
		API:     c.API,
		Ingress: c.Ingress,
		Storage: wireStorage{
			Capacity:       c.Storage.Capacity,
			PluginCapacity: c.Storage.PluginCapacity,
			BlobLimit:      c.Storage.BlobLimit.String(),
		},
		DefaultEnabled: c.DefaultEnabled,
		Plugins:        map[string]wirePlugin{},
	}
	for name, pc := range c.Plugins {
		wp := wirePlugin{Enabled: pc.Enabled, Providers: map[string]map[string]any{}}
		for pname, prov := range pc.Providers {
			wp.Providers[pname] = prov.Values()
		}
		w.Plugins[name] = wp
	}
	return toml.Marshal(w)
}
