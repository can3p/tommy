// Package config holds tommy's configuration. The TOML file and the CLI flags
// build the very same Config struct, and there is exactly one server bootstrap
// that consumes it.
package config

import (
	"fmt"
	"os"
)

// Defaults.
const (
	DefaultUIPort      = 8811
	DefaultIngressPort = 8822
	DefaultCapacity    = 500
	DefaultBlobLimit   = ByteSize(256 << 20)
	DefaultBind        = "127.0.0.1"
	DefaultHost        = "localhost"
)

// DefaultIngressH2C is why the ingress speaks cleartext HTTP/2 unless told not
// to. Some vendor APIs have no HTTP/1.1 form at all - APNs is
// POST /3/device/{token} over HTTP/2 and nothing else - so a fake that only
// spoke HTTP/1.1 could not be pointed at by their clients, and a fake exists to
// be easy to point things at. It costs the HTTP/1.1 path nothing: h2c is
// recognized from the HTTP/2 client preface, a byte sequence no HTTP/1.1 client
// ever sends, and everything else is served exactly as before.
const DefaultIngressH2C = true

// ListenerConfig configures one of the core HTTP listeners.
//
// Port is a pointer so "unset" and "port 0" stay distinguishable: port 0 means
// "bind an ephemeral port", which is what the test harness uses.
type ListenerConfig struct {
	Port *int   `toml:"port"`
	Bind string `toml:"bind"`

	// H2C serves cleartext HTTP/2 (h2c) alongside HTTP/1.1 on this listener.
	// nil means unset, which defaults to true for the ingress and false for
	// the UI and the API - see DefaultIngressH2C and Config.H2C.
	H2C *bool `toml:"h2c"`
}

// StorageConfig configures retention.
type StorageConfig struct {
	// Capacity is the number of events retained per plugin.
	Capacity int
	// PluginCapacity overrides Capacity for individual plugins.
	PluginCapacity map[string]int
	// BlobLimit caps the total bytes held by the blob store.
	BlobLimit ByteSize
}

// PluginConfig is one `[plugins.<name>]` section.
type PluginConfig struct {
	// Enabled is nil when the section does not say, which means "inherit".
	Enabled   *bool
	Providers map[string]ProviderConfig
}

// Config is the whole configuration.
type Config struct {
	// Bind is the default interface for every core listener.
	Bind string
	// Host is the hostname used in snippets and printed URLs.
	Host string

	UI      ListenerConfig
	API     ListenerConfig
	Ingress ListenerConfig

	Storage StorageConfig

	// DefaultEnabled decides plugins and providers that say nothing. It is true
	// by default; `tommy mail --enabled-providers mailjet` sets it to false and
	// enables just what was asked for.
	DefaultEnabled *bool

	Plugins map[string]PluginConfig

	// Source is the file this config was read from, "" for programmatic ones.
	Source string

	apiShared     bool
	apiDefaulted  bool
	ingressShared bool
}

// Bool returns a pointer to b, for the *bool config fields.
func Bool(b bool) *bool { return &b }

// Int returns a pointer to n, for the *int port fields.
func Int(n int) *int { return &n }

// Default returns a Config with every default applied.
func Default() *Config {
	c := &Config{}
	c.ApplyDefaults()
	return c
}

// Ephemeral returns a Config whose listeners bind port 0, for tests.
//
// Defaults are deliberately not applied yet: a caller may still want to set
// API.Port explicitly, and "unset" has to stay distinguishable from "0" for
// that to mean anything. Every entry point applies defaults before use.
func Ephemeral() *Config {
	return &Config{
		UI:      ListenerConfig{Port: Int(0)},
		Ingress: ListenerConfig{Port: Int(0)},
	}
}

// Load reads and validates a TOML config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	c.Source = path
	return c, nil
}

// ApplyDefaults fills every unset field. It is idempotent, and every entry
// point calls it - the CLI path and the TOML path must produce the same struct.
func (c *Config) ApplyDefaults() {
	if c.Bind == "" {
		c.Bind = DefaultBind
	}
	if c.Host == "" {
		c.Host = DefaultHost
	}
	if c.UI.Port == nil {
		c.UI.Port = Int(DefaultUIPort)
	}
	if c.UI.Bind == "" {
		c.UI.Bind = c.Bind
	}

	// The API shares the UI listener unless it is given a port of its own.
	// apiDefaulted remembers that decision so re-applying defaults - which the
	// CLI and the bootstrap both do - cannot quietly split the two listeners
	// when the UI port is 0 (ephemeral).
	switch {
	case c.API.Port == nil:
		c.API.Port = Int(*c.UI.Port)
		c.apiShared = true
		c.apiDefaulted = true
	case c.apiDefaulted && *c.API.Port == *c.UI.Port:
		c.apiShared = true
	default:
		// The caller assigned a port of its own after defaulting.
		c.apiDefaulted = false
		c.apiShared = *c.API.Port == *c.UI.Port && *c.UI.Port != 0
	}
	if c.API.Bind == "" {
		c.API.Bind = c.Bind
	}
	if c.apiShared {
		c.API.Bind = c.UI.Bind
	}

	if c.Ingress.Port == nil {
		c.Ingress.Port = Int(DefaultIngressPort)
	}
	if c.Ingress.Bind == "" {
		c.Ingress.Bind = c.Bind
	}
	if c.Ingress.H2C == nil {
		c.Ingress.H2C = Bool(DefaultIngressH2C)
	}
	if c.UI.H2C == nil {
		c.UI.H2C = Bool(false)
	}
	if c.API.H2C == nil {
		c.API.H2C = Bool(false)
	}
	c.ingressShared = *c.Ingress.Port == *c.UI.Port && *c.UI.Port != 0
	if c.ingressShared {
		c.Ingress.Bind = c.UI.Bind
	}

	if c.Storage.Capacity == 0 {
		c.Storage.Capacity = DefaultCapacity
	}
	if c.Storage.BlobLimit == 0 {
		c.Storage.BlobLimit = DefaultBlobLimit
	}
	if c.DefaultEnabled == nil {
		c.DefaultEnabled = Bool(true)
	}
	if c.Plugins == nil {
		c.Plugins = map[string]PluginConfig{}
	}
}

// H2C reports whether the physical listener carrying the named core surface
// ("ui", "api" or "ingress") serves cleartext HTTP/2.
//
// It has to be answered per listener rather than per surface: h2c is decided
// from the first bytes of a connection, long before any routing could say which
// surface the request is for. So a listener that several surfaces share speaks
// h2c when any one of them asks for it - which by default is what happens the
// moment the ingress is pointed at the UI port, since the ingress asks for h2c
// and the UI does not. That is safe: browsers never attempt h2c (they require
// TLS for HTTP/2), and an HTTP/1.1 request is unaffected either way.
func (c *Config) H2C(surface string) bool {
	uiListener := func() bool {
		return boolValue(c.UI.H2C) ||
			(c.apiShared && boolValue(c.API.H2C)) ||
			(c.ingressShared && boolValue(c.Ingress.H2C))
	}
	switch surface {
	case "ui":
		return uiListener()
	case "api":
		if c.apiShared {
			return uiListener()
		}
		return boolValue(c.API.H2C)
	case "ingress":
		if c.ingressShared {
			return uiListener()
		}
		return boolValue(c.Ingress.H2C)
	}
	return false
}

func boolValue(p *bool) bool { return p != nil && *p }

// APISharesUIListener reports whether the API is served by the UI listener.
func (c *Config) APISharesUIListener() bool { return c.apiShared }

// IngressSharesUIListener reports whether ingress is served by the UI listener.
func (c *Config) IngressSharesUIListener() bool { return c.ingressShared }

// UIAddr, APIAddr and IngressAddr return the configured host:port to listen on.
func (c *Config) UIAddr() string      { return addr(c.UI, c.Bind) }
func (c *Config) APIAddr() string     { return addr(c.API, c.Bind) }
func (c *Config) IngressAddr() string { return addr(c.Ingress, c.Bind) }

func addr(l ListenerConfig, fallbackBind string) string {
	bind := l.Bind
	if bind == "" {
		bind = fallbackBind
	}
	port := 0
	if l.Port != nil {
		port = *l.Port
	}
	return fmt.Sprintf("%s:%d", bind, port)
}

// PluginEnabled reports whether a plugin should run.
func (c *Config) PluginEnabled(name string) bool {
	if pc, ok := c.Plugins[name]; ok && pc.Enabled != nil {
		return *pc.Enabled
	}
	return c.DefaultEnabled == nil || *c.DefaultEnabled
}

// ProviderEnabled reports whether a provider should run. A provider of a
// disabled plugin is always off.
func (c *Config) ProviderEnabled(plugin, provider string) bool {
	if !c.PluginEnabled(plugin) {
		return false
	}
	if pc, ok := c.Plugins[plugin]; ok {
		if prov, ok := pc.Providers[provider]; ok && prov.Enabled != nil {
			return *prov.Enabled
		}
	}
	return c.DefaultEnabled == nil || *c.DefaultEnabled
}

// Provider returns the configuration section of a provider, zero value when
// the config does not mention it.
func (c *Config) Provider(plugin, provider string) ProviderConfig {
	if pc, ok := c.Plugins[plugin]; ok {
		if prov, ok := pc.Providers[provider]; ok {
			return prov
		}
	}
	return ProviderConfig{}
}

// SetProvider stores a provider section, creating the plugin section when
// needed. This is how the CLI path builds a config.
func (c *Config) SetProvider(plugin, provider string, pc ProviderConfig) {
	if c.Plugins == nil {
		c.Plugins = map[string]PluginConfig{}
	}
	p := c.Plugins[plugin]
	if p.Providers == nil {
		p.Providers = map[string]ProviderConfig{}
	}
	p.Providers[provider] = pc
	c.Plugins[plugin] = p
}

// SetPluginEnabled turns a plugin on or off explicitly.
func (c *Config) SetPluginEnabled(plugin string, enabled bool) {
	if c.Plugins == nil {
		c.Plugins = map[string]PluginConfig{}
	}
	p := c.Plugins[plugin]
	p.Enabled = Bool(enabled)
	c.Plugins[plugin] = p
}

// CapacityFor returns the event retention of a plugin.
func (c *Config) CapacityFor(plugin string) int {
	if n, ok := c.Storage.PluginCapacity[plugin]; ok && n > 0 {
		return n
	}
	return c.Storage.Capacity
}
