package config_test

import (
	"strings"
	"testing"

	"github.com/can3p/tommy/core/config"
)

const sample = `
[ui]
port = 8811

[ingress]
port = 8822

[storage]
capacity = 10
blob_limit = "4MB"

[plugins.mail]
enabled = true

[plugins.mail.providers.mailjet]
enabled = true

[plugins.mail.providers.smtp]
enabled = true
port = 1025

[plugins.sms]
enabled = false

[plugins.sms.providers.twilio]
enabled = true
account_sid = "AC0000"
`

func TestParse(t *testing.T) {
	c, err := config.Parse([]byte(sample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := *c.UI.Port; got != 8811 {
		t.Errorf("ui port = %d", got)
	}
	if !c.APISharesUIListener() {
		t.Error("api should share the ui listener when api.port is unset")
	}
	if got := *c.API.Port; got != 8811 {
		t.Errorf("api port = %d, want the ui port", got)
	}
	if got := c.Storage.BlobLimit.Bytes(); got != 4_000_000 {
		t.Errorf("blob_limit = %d", got)
	}
	if c.Storage.Capacity != 10 {
		t.Errorf("capacity = %d", c.Storage.Capacity)
	}
	if !c.PluginEnabled("mail") {
		t.Error("mail should be enabled")
	}
	if c.PluginEnabled("sms") {
		t.Error("sms should be disabled")
	}
	if !c.PluginEnabled("files") {
		t.Error("unmentioned plugins default to enabled")
	}
	if c.ProviderEnabled("sms", "twilio") {
		t.Error("a provider of a disabled plugin must be off")
	}
	if got := c.Provider("mail", "smtp").Port; got != 1025 {
		t.Errorf("smtp port = %d", got)
	}

	var twilio struct {
		AccountSID string `toml:"account_sid"`
	}
	if err := c.Provider("sms", "twilio").Decode(&twilio); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if twilio.AccountSID != "AC0000" {
		t.Errorf("account_sid = %q", twilio.AccountSID)
	}
	if got := c.Provider("sms", "twilio").String("account_sid", ""); got != "AC0000" {
		t.Errorf("String() = %q", got)
	}
}

func TestProgrammaticMatchesTOML(t *testing.T) {
	fromTOML, err := config.Parse([]byte(sample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	built := &config.Config{
		UI:      config.ListenerConfig{Port: config.Int(8811)},
		Ingress: config.ListenerConfig{Port: config.Int(8822)},
		Storage: config.StorageConfig{Capacity: 10, BlobLimit: 4_000_000},
	}
	built.ApplyDefaults()
	built.SetPluginEnabled("mail", true)
	built.SetProvider("mail", "mailjet", config.NewProviderConfig(map[string]any{"enabled": true}))
	built.SetProvider("mail", "smtp", config.NewProviderConfig(map[string]any{"enabled": true, "port": int64(1025)}))
	built.SetPluginEnabled("sms", false)
	built.SetProvider("sms", "twilio", config.NewProviderConfig(map[string]any{"enabled": true, "account_sid": "AC0000"}))
	if err := built.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	for _, tc := range []struct{ plugin, provider string }{
		{"mail", "mailjet"}, {"mail", "smtp"}, {"sms", "twilio"},
	} {
		if a, b := fromTOML.ProviderEnabled(tc.plugin, tc.provider), built.ProviderEnabled(tc.plugin, tc.provider); a != b {
			t.Errorf("%s/%s enabled: toml=%v built=%v", tc.plugin, tc.provider, a, b)
		}
		if a, b := fromTOML.Provider(tc.plugin, tc.provider).Port, built.Provider(tc.plugin, tc.provider).Port; a != b {
			t.Errorf("%s/%s port: toml=%d built=%d", tc.plugin, tc.provider, a, b)
		}
	}
	var twilio struct {
		AccountSID string `toml:"account_sid"`
	}
	if err := built.Provider("sms", "twilio").Decode(&twilio); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if twilio.AccountSID != "AC0000" {
		t.Errorf("account_sid = %q", twilio.AccountSID)
	}
}

func TestDefaultEnabledFalse(t *testing.T) {
	c := &config.Config{DefaultEnabled: config.Bool(false)}
	c.ApplyDefaults()
	c.SetPluginEnabled("mail", true)
	c.SetProvider("mail", "mailjet", config.NewProviderConfig(map[string]any{"enabled": true}))

	if !c.ProviderEnabled("mail", "mailjet") {
		t.Error("explicitly enabled provider must run")
	}
	if c.ProviderEnabled("mail", "sendgrid") {
		t.Error("unmentioned provider must stay off when default_enabled = false")
	}
	if c.PluginEnabled("sms") {
		t.Error("unmentioned plugin must stay off when default_enabled = false")
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "port collision between core listeners",
			toml: "[ui]\nport = 9000\n[api]\nport = 9001\n[ingress]\nport = 9001\n",
			want: "already used by api",
		},
		{
			name: "provider port collides with ingress",
			toml: "[ingress]\nport = 8822\n[plugins.mail.providers.smtp]\nport = 8822\n",
			want: "already used by ingress",
		},
		{
			name: "capacity must be positive",
			toml: "[storage]\ncapacity = -1\n",
			want: "capacity must be > 0",
		},
		{
			name: "port out of range",
			toml: "[ui]\nport = 70000\n",
			want: "out of range",
		},
		{
			name: "bad byte size",
			toml: "[storage]\nblob_limit = \"lots\"\n",
			want: "byte size",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tc.toml))
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestDisabledProviderPortDoesNotCollide(t *testing.T) {
	// A disabled provider never binds, so its port must not fail validation.
	c, err := config.Parse([]byte("[ingress]\nport = 8822\n[plugins.mail.providers.smtp]\nenabled = false\nport = 8822\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.ProviderEnabled("mail", "smtp") {
		t.Error("smtp should be disabled")
	}
}

func TestByteSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		err  bool
	}{
		{in: "1024", want: 1024},
		{in: "1KiB", want: 1024},
		{in: "256MB", want: 256_000_000},
		{in: "1.5GiB", want: 1610612736},
		{in: "10 M", want: 10 << 20},
		{in: "", err: true},
		{in: "-1", err: true},
		{in: "MB", err: true},
	}
	for _, tc := range tests {
		got, err := config.ParseByteSize(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseByteSize(%q) = %d, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseByteSize(%q): %v", tc.in, err)
			continue
		}
		if int64(got) != tc.want {
			t.Errorf("ParseByteSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestApplyDefaultsIsIdempotent(t *testing.T) {
	// The CLI applies defaults, then the bootstrap applies them again. A second
	// pass must not quietly split the shared UI/API listener - with ephemeral
	// ports that would hand the caller two different ports.
	for _, tc := range []struct {
		name       string
		build      func() *config.Config
		wantShared bool
	}{
		{"defaults", func() *config.Config { return &config.Config{} }, true},
		{"ephemeral", config.Ephemeral, true},
		{
			name: "explicit api port",
			build: func() *config.Config {
				c := config.Ephemeral()
				c.API.Port = config.Int(9001)
				return c
			},
			wantShared: false,
		},
		{
			name: "explicit ephemeral api port",
			build: func() *config.Config {
				c := config.Ephemeral()
				c.API.Port = config.Int(0)
				return c
			},
			wantShared: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.build()
			c.ApplyDefaults()
			first := *c.API.Port
			sharedFirst := c.APISharesUIListener()

			c.ApplyDefaults()
			c.ApplyDefaults()

			if *c.API.Port != first || c.APISharesUIListener() != sharedFirst {
				t.Errorf("second pass changed the config: port %d -> %d, shared %v -> %v",
					first, *c.API.Port, sharedFirst, c.APISharesUIListener())
			}
			if c.APISharesUIListener() != tc.wantShared {
				t.Errorf("shared = %v, want %v", c.APISharesUIListener(), tc.wantShared)
			}
			if err := c.Validate(); err != nil {
				t.Errorf("validate: %v", err)
			}
		})
	}
}

func TestH2CDefaults(t *testing.T) {
	c := config.Default()
	if !c.H2C("ingress") {
		t.Error("the ingress should serve cleartext HTTP/2 by default")
	}
	if c.H2C("ui") || c.H2C("api") {
		t.Error("the ui and api listeners must not serve h2c by default")
	}
	if c.H2C("nonsense") {
		t.Error("an unknown surface must not report h2c")
	}
}

func TestH2CFromTOML(t *testing.T) {
	c, err := config.Parse([]byte("[ingress]\nport = 8822\nh2c = false\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.H2C("ingress") {
		t.Error("[ingress] h2c = false was ignored")
	}

	c, err = config.Parse([]byte("[ui]\nport = 8811\nh2c = true\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !c.H2C("ui") {
		t.Error("[ui] h2c = true was ignored")
	}
	// The API rides the UI listener here, so it inherits the decision.
	if !c.H2C("api") {
		t.Error("the api shares the ui listener and must inherit its protocols")
	}
}

// TestH2CFollowsTheListener: h2c is settled from the first bytes of a
// connection, so it belongs to a listener rather than to a surface. A listener
// several surfaces share speaks h2c when any of them asks for it.
func TestH2CFollowsTheListener(t *testing.T) {
	shared := &config.Config{
		UI:      config.ListenerConfig{Port: config.Int(9411)},
		Ingress: config.ListenerConfig{Port: config.Int(9411)},
	}
	shared.ApplyDefaults()
	if !shared.IngressSharesUIListener() {
		t.Fatal("the ingress should share the ui listener")
	}
	for _, surface := range []string{"ui", "api", "ingress"} {
		if !shared.H2C(surface) {
			t.Errorf("%s: a shared listener carrying the ingress serves h2c", surface)
		}
	}

	off := &config.Config{
		UI:      config.ListenerConfig{Port: config.Int(9411)},
		Ingress: config.ListenerConfig{Port: config.Int(9411), H2C: config.Bool(false)},
	}
	off.ApplyDefaults()
	for _, surface := range []string{"ui", "api", "ingress"} {
		if off.H2C(surface) {
			t.Errorf("%s: turning the ingress setting off must clear the shared listener", surface)
		}
	}

	// A dedicated API listener is its own decision either way.
	own := &config.Config{
		UI:      config.ListenerConfig{Port: config.Int(9411)},
		API:     config.ListenerConfig{Port: config.Int(9412), H2C: config.Bool(true)},
		Ingress: config.ListenerConfig{Port: config.Int(9411)},
	}
	own.ApplyDefaults()
	if !own.H2C("api") {
		t.Error("an api listener of its own honors its own h2c setting")
	}
}

func TestH2CSurvivesRepeatedDefaults(t *testing.T) {
	c := config.Ephemeral()
	c.Ingress.H2C = config.Bool(false)
	c.ApplyDefaults()
	c.ApplyDefaults()
	if c.H2C("ingress") {
		t.Error("a second ApplyDefaults pass turned h2c back on")
	}
}

func TestH2CRoundTripsThroughTOML(t *testing.T) {
	c := config.Default()
	c.Ingress.H2C = config.Bool(false)
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), "h2c = false") {
		t.Fatalf("marshaled config does not carry the h2c key:\n%s", data)
	}
	back, err := config.Parse(data)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if back.H2C("ingress") {
		t.Error("h2c = false did not survive a marshal/parse round trip")
	}
}

const storageSample = `
[ui]
port = 8811

[ingress]
port = 8822

[storage]
backend = "filesystem"
path = "/var/lib/tommy"

[storage.plugins.s3]
backend = "memory"

[storage.plugins.mail]
backend = "filesystem"

[storage.plugins.mail.providers.smtp]
backend = "memory"
`

func TestStorageParse(t *testing.T) {
	c, err := config.Parse([]byte(storageSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Storage.Backend != config.StorageFilesystem {
		t.Errorf("storage.backend = %q, want %q", c.Storage.Backend, config.StorageFilesystem)
	}
	if c.Storage.Path != "/var/lib/tommy" {
		t.Errorf("storage.path = %q", c.Storage.Path)
	}
	if got := c.Storage.BackendFor("s3", ""); got != config.StorageMemory {
		t.Errorf("s3 backend = %q, want %q", got, config.StorageMemory)
	}
	if got := c.Storage.BackendFor("mail", ""); got != config.StorageFilesystem {
		t.Errorf("mail backend = %q, want %q", got, config.StorageFilesystem)
	}
	if got := c.Storage.BackendFor("mail", "smtp"); got != config.StorageMemory {
		t.Errorf("mail/smtp backend = %q, want %q", got, config.StorageMemory)
	}
	if got := c.Storage.BackendFor("mail", "mailjet"); got != config.StorageFilesystem {
		t.Errorf("mail/mailjet (no override) backend = %q, want the plugin's %q", got, config.StorageFilesystem)
	}
	if got := c.Storage.BackendFor("files", ""); got != config.StorageFilesystem {
		t.Errorf("unmentioned plugin backend = %q, want the global %q", got, config.StorageFilesystem)
	}
	if !c.Storage.UsesFilesystem() {
		t.Error("UsesFilesystem should be true when the global backend is filesystem")
	}
}

func TestStorageRoundTripsThroughTOML(t *testing.T) {
	c, err := config.Parse([]byte(storageSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	data, err := c.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := config.Parse(data)
	if err != nil {
		t.Fatalf("re-parse marshaled config: %v\n%s", err, data)
	}
	for _, tc := range []struct{ plugin, provider string }{
		{"s3", ""}, {"mail", ""}, {"mail", "smtp"}, {"mail", "mailjet"}, {"files", ""},
	} {
		got, want := back.Storage.BackendFor(tc.plugin, tc.provider), c.Storage.BackendFor(tc.plugin, tc.provider)
		if got != want {
			t.Errorf("%s/%s: round-tripped backend = %q, want %q", tc.plugin, tc.provider, got, want)
		}
	}
	if back.Storage.Path != c.Storage.Path {
		t.Errorf("round-tripped path = %q, want %q", back.Storage.Path, c.Storage.Path)
	}
}

func TestStorageProgrammaticMatchesTOML(t *testing.T) {
	fromTOML, err := config.Parse([]byte(storageSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	built := &config.Config{
		UI:      config.ListenerConfig{Port: config.Int(8811)},
		Ingress: config.ListenerConfig{Port: config.Int(8822)},
		Storage: config.StorageConfig{
			Backend: config.StorageFilesystem,
			Path:    "/var/lib/tommy",
		},
	}
	built.ApplyDefaults()
	if err := built.SetStorageOverride("s3", config.StorageMemory); err != nil {
		t.Fatalf("set s3 override: %v", err)
	}
	if err := built.SetStorageOverride("mail", config.StorageFilesystem); err != nil {
		t.Fatalf("set mail override: %v", err)
	}
	if err := built.SetStorageOverride("mail/smtp", config.StorageMemory); err != nil {
		t.Fatalf("set mail/smtp override: %v", err)
	}
	if err := built.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	for _, tc := range []struct{ plugin, provider string }{
		{"s3", ""}, {"mail", ""}, {"mail", "smtp"}, {"mail", "mailjet"},
	} {
		got, want := built.Storage.BackendFor(tc.plugin, tc.provider), fromTOML.Storage.BackendFor(tc.plugin, tc.provider)
		if got != want {
			t.Errorf("%s/%s: built backend = %q, toml backend = %q", tc.plugin, tc.provider, got, want)
		}
	}
}

func TestStorageDefaults(t *testing.T) {
	c := config.Default()
	if c.Storage.Backend != config.StorageMemory {
		t.Errorf("default storage.backend = %q, want %q", c.Storage.Backend, config.StorageMemory)
	}
	if c.Storage.Path != "" {
		t.Errorf("default storage.path = %q, want empty", c.Storage.Path)
	}
	if c.Storage.UsesFilesystem() {
		t.Error("a default config must not use the filesystem backend")
	}
	if c.Storage.Plugins == nil {
		t.Error("ApplyDefaults must initialize an empty Plugins map, not leave it nil")
	}
}

func TestStorageValidationMessages(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "unknown global backend",
			toml: "[storage]\nbackend = \"bogus\"\n",
			want: "storage.backend:",
		},
		{
			name: "unknown plugin backend names the exact setting",
			toml: "[storage.plugins.s3]\nbackend = \"bogus\"\n",
			want: "storage.plugins.s3.backend:",
		},
		{
			name: "unknown provider backend names the exact setting",
			toml: "[storage.plugins.mail.providers.smtp]\nbackend = \"bogus\"\n",
			want: "storage.plugins.mail.providers.smtp.backend:",
		},
		{
			name: "filesystem anywhere without a path",
			toml: "[storage]\nbackend = \"filesystem\"\n",
			want: "storage.path:",
		},
		{
			name: "filesystem on a plugin scope without a global path",
			toml: "[storage.plugins.s3]\nbackend = \"filesystem\"\n",
			want: "storage.path:",
		},
		{
			name: "filesystem on a provider scope without a global path",
			toml: "[storage.plugins.mail.providers.smtp]\nbackend = \"filesystem\"\n",
			want: "storage.path:",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tc.toml))
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestStoragePath(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		source string
		want   string
	}{
		{"empty path stays empty", "", "/etc/tommy/tommy.toml", ""},
		{"relative path with no source stays relative", "data", "", "data"},
		{"relative path resolves against the config file's directory", "data", "/etc/tommy/tommy.toml", "/etc/tommy/data"},
		{"absolute path is untouched even with a source", "/var/lib/tommy", "/etc/tommy/tommy.toml", "/var/lib/tommy"},
		{"nested relative path resolves fully", "../data", "/etc/tommy/conf/tommy.toml", "/etc/tommy/data"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &config.Config{Storage: config.StorageConfig{Path: tc.path}, Source: tc.source}
			if got := c.StoragePath(); got != tc.want {
				t.Errorf("StoragePath() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSetStorageOverride(t *testing.T) {
	t.Run("global via empty string", func(t *testing.T) {
		c := &config.Config{}
		if err := c.SetStorageOverride("", config.StorageFilesystem); err != nil {
			t.Fatalf("set: %v", err)
		}
		if c.Storage.Backend != config.StorageFilesystem {
			t.Errorf("global backend = %q", c.Storage.Backend)
		}
	})
	t.Run("global via the word global", func(t *testing.T) {
		c := &config.Config{}
		if err := c.SetStorageOverride("global", config.StorageFilesystem); err != nil {
			t.Fatalf("set: %v", err)
		}
		if c.Storage.Backend != config.StorageFilesystem {
			t.Errorf("global backend = %q", c.Storage.Backend)
		}
	})
	t.Run("plugin scope", func(t *testing.T) {
		c := &config.Config{}
		if err := c.SetStorageOverride("s3", config.StorageFilesystem); err != nil {
			t.Fatalf("set: %v", err)
		}
		if got := c.Storage.Plugins["s3"].Backend; got != config.StorageFilesystem {
			t.Errorf("plugins.s3.backend = %q", got)
		}
	})
	t.Run("plugin/provider scope", func(t *testing.T) {
		c := &config.Config{}
		if err := c.SetStorageOverride("mail/smtp", config.StorageMemory); err != nil {
			t.Fatalf("set: %v", err)
		}
		if got := c.Storage.Plugins["mail"].Providers["smtp"].Backend; got != config.StorageMemory {
			t.Errorf("plugins.mail.providers.smtp.backend = %q", got)
		}
		// Setting a provider must not implicitly set the plugin's own backend.
		if got := c.Storage.Plugins["mail"].Backend; got != "" {
			t.Errorf("plugins.mail.backend = %q, want empty (unset)", got)
		}
	})
	t.Run("malformed scopes are rejected", func(t *testing.T) {
		for _, scope := range []string{"a/b/c", "/x", "x/", "//"} {
			c := &config.Config{}
			if err := c.SetStorageOverride(scope, config.StorageFilesystem); err == nil {
				t.Errorf("SetStorageOverride(%q) succeeded, want an error", scope)
			}
		}
	})
}
