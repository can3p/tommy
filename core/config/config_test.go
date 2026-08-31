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
