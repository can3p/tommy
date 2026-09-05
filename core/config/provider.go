package config

import (
	"fmt"

	toml "github.com/pelletier/go-toml/v2"
)

// ProviderConfig is one `[plugins.<plugin>.providers.<provider>]` section.
//
// Known keys (enabled, port) are decoded eagerly because the core needs them;
// everything else is kept raw and decoded on demand by the provider itself, so
// adding a provider setting never touches the core config struct.
type ProviderConfig struct {
	// Enabled is nil when the section does not say, which means "inherit".
	Enabled *bool
	// Port, when non-zero, sets a listener provider's listen port - SMTP,
	// FTP, SFTP each read it in their own LoadConfig, and the core uses it in
	// listenerAddr only to report where such a provider can be reached.
	//
	// It does nothing for an HTTP provider. Every HTTP provider is path-routed
	// onto the one shared ingress (core/server/ingress), which has no notion
	// of a per-provider port; giving one a port here is validated for range
	// and collisions and then ignored. Wiring a provider onto a dedicated
	// listener is unbuilt work - see docs/implementation-plan.md.
	Port int

	raw    []byte
	values map[string]any
}

// NewProviderConfig builds a ProviderConfig from the raw key/value pairs of a
// TOML section. The CLI path calls it directly, which is what keeps the two
// entry points producing identical configs.
func NewProviderConfig(values map[string]any) ProviderConfig {
	pc := ProviderConfig{values: map[string]any{}}
	for k, v := range values {
		pc.values[k] = v
	}
	if raw, err := toml.Marshal(pc.values); err == nil {
		pc.raw = raw
	}
	pc.readKnown()
	return pc
}

func (p *ProviderConfig) readKnown() {
	if v, ok := p.values["enabled"]; ok {
		if b, ok := v.(bool); ok {
			p.Enabled = &b
		}
	}
	p.Port = p.Int("port", 0)
}

// withInheritedBind returns the section with `bind` filled in from the
// top-level config when the section names none of its own. Called by
// Config.Provider, which is the one place a provider's section is handed out.
func (p ProviderConfig) withInheritedBind(bind string) ProviderConfig {
	if bind == "" {
		return p
	}
	if _, ok := p.values["bind"]; ok {
		return p
	}
	values := p.Values()
	values["bind"] = bind
	return NewProviderConfig(values)
}

// Decode unmarshals the whole section into v, a pointer to a struct with `toml`
// tags. Unknown keys are ignored, so a provider only declares what it needs.
func (p ProviderConfig) Decode(v any) error {
	if len(p.raw) == 0 {
		return nil
	}
	if err := toml.Unmarshal(p.raw, v); err != nil {
		return fmt.Errorf("decode provider config: %w", err)
	}
	return nil
}

// Values returns the raw key/value pairs of the section.
func (p ProviderConfig) Values() map[string]any {
	out := map[string]any{}
	for k, v := range p.values {
		out[k] = v
	}
	return out
}

// Get returns a raw value from the section.
func (p ProviderConfig) Get(key string) (any, bool) {
	v, ok := p.values[key]
	return v, ok
}

// String returns a string setting, or def when absent or of another type.
func (p ProviderConfig) String(key, def string) string {
	if v, ok := p.values[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

// Int returns an integer setting, or def when absent or of another type.
func (p ProviderConfig) Int(key string, def int) int {
	if v, ok := p.values[key]; ok {
		switch n := v.(type) {
		case int64:
			return int(n)
		case int:
			return n
		case float64:
			return int(n)
		}
	}
	return def
}

// Bool returns a boolean setting, or def when absent or of another type.
func (p ProviderConfig) Bool(key string, def bool) bool {
	if v, ok := p.values[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}
