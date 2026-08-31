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
	// Port, when non-zero, gives the provider its own listener instead of the
	// shared ingress (HTTP providers) or sets its listen port (SMTP, FTP, ...).
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
