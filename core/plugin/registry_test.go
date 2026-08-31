package plugin_test

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
)

type stubProvider struct {
	name, owner string
	endpoints   []plugin.Endpoint
	snippets    []plugin.Snippet
}

func (p *stubProvider) Name() string                            { return p.name }
func (p *stubProvider) Plugin() string                          { return p.owner }
func (p *stubProvider) Description() string                     { return "A stub provider used by the registry tests." }
func (p *stubProvider) Endpoints() []plugin.Endpoint            { return p.endpoints }
func (p *stubProvider) Snippets() []plugin.Snippet              { return p.snippets }
func (p *stubProvider) RegisterIngress(plugin.Mux, plugin.Deps) {}

type listenerProvider struct{ stubProvider }

func (p *listenerProvider) Listen(ctx context.Context, d plugin.Deps) error {
	<-ctx.Done()
	return nil
}

type stubPlugin struct {
	name      string
	title     string
	providers []plugin.Provider
}

func (p *stubPlugin) Name() string  { return p.name }
func (p *stubPlugin) Title() string { return p.title }
func (p *stubPlugin) Description() string {
	return "A stub plugin used by the registry tests."
}
func (p *stubPlugin) Providers() []plugin.Provider        { return p.providers }
func (p *stubPlugin) RegisterAPI(plugin.Mux, plugin.Deps) {}
func (p *stubPlugin) RegisterUI(plugin.Mux, plugin.Deps)  {}
func (p *stubPlugin) Templates() fs.FS                    { return nil }

func newStub(name string, providerNames ...string) *stubPlugin {
	p := &stubPlugin{name: name, title: strings.ToUpper(name[:1]) + name[1:]}
	for _, pn := range providerNames {
		p.providers = append(p.providers, &stubProvider{
			name:      pn,
			owner:     name,
			endpoints: []plugin.Endpoint{{Method: "POST", Path: "/" + name + "/" + pn, Description: "Send something."}},
			snippets:  []plugin.Snippet{{Title: "send", Lang: "bash", Code: "curl {{.IngressURL}}/" + name + "/" + pn}},
		})
	}
	return p
}

func TestRegistryValidation(t *testing.T) {
	tests := []struct {
		name    string
		plugins []plugin.Plugin
		wantErr string
	}{
		{
			name:    "duplicate plugin names",
			plugins: []plugin.Plugin{newStub("mail", "a"), newStub("mail", "b")},
			wantErr: `plugin "mail" is registered twice`,
		},
		{
			name:    "uppercase plugin name",
			plugins: []plugin.Plugin{newStub("Mail", "a")},
			wantErr: "must be lowercase",
		},
		{
			name: "provider claims another plugin",
			plugins: []plugin.Plugin{&stubPlugin{name: "mail", title: "Mail", providers: []plugin.Provider{
				&stubProvider{name: "mailjet", owner: "sms"},
			}}},
			wantErr: `provider "mailjet" says it belongs to plugin "sms"`,
		},
		{
			name: "duplicate provider inside a plugin",
			plugins: []plugin.Plugin{&stubPlugin{name: "mail", title: "Mail", providers: []plugin.Provider{
				&stubProvider{name: "mailjet", owner: "mail"},
				&stubProvider{name: "mailjet", owner: "mail"},
			}}},
			wantErr: `registers provider "mailjet" twice`,
		},
		{
			name:    "valid",
			plugins: []plugin.Plugin{newStub("mail", "mailjet", "sendgrid"), newStub("sms", "twilio")},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := plugin.New(config.Default(), tc.plugins...)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestRegistryRespectsConfig(t *testing.T) {
	cfg, err := config.Parse([]byte(`
[plugins.sms]
enabled = false

[plugins.mail.providers.sendgrid]
enabled = false
`))
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	reg, err := plugin.New(cfg, newStub("mail", "mailjet", "sendgrid"), newStub("sms", "twilio"))
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	if got := reg.SortedNames(); len(got) != 1 || got[0] != "mail" {
		t.Errorf("enabled plugins = %v, want just mail", got)
	}
	if len(reg.AllPlugins()) != 2 {
		t.Errorf("AllPlugins = %d, want both", len(reg.AllPlugins()))
	}
	if len(reg.DisabledPlugins()) != 1 {
		t.Errorf("DisabledPlugins = %d, want sms", len(reg.DisabledPlugins()))
	}
	if _, ok := reg.Plugin("sms"); ok {
		t.Error("a disabled plugin must not resolve")
	}
	providers := reg.Providers("mail")
	if len(providers) != 1 || providers[0].Name() != "mailjet" {
		t.Errorf("mail providers = %v, want just mailjet", names(providers))
	}
	if got := reg.Providers("sms"); got != nil {
		t.Errorf("providers of a disabled plugin = %v, want none", names(got))
	}
	if len(reg.Refs()) != 1 {
		t.Errorf("Refs = %d, want 1", len(reg.Refs()))
	}
	if got := reg.String(); got != "mail[mailjet]" {
		t.Errorf("String = %q", got)
	}
}

func TestRegistrySplitsListenerProviders(t *testing.T) {
	mail := newStub("mail", "mailjet")
	smtp := &listenerProvider{stubProvider{name: "smtp", owner: "mail"}}
	mail.providers = append(mail.providers, smtp)

	reg, err := plugin.New(config.Default(), mail)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	if got := names(providersOf(reg.IngressRefs())); len(got) != 1 || got[0] != "mailjet" {
		t.Errorf("IngressRefs = %v, want just the HTTP provider", got)
	}
	if got := names(providersOf(reg.ListenerRefs())); len(got) != 1 || got[0] != "smtp" {
		t.Errorf("ListenerRefs = %v, want just the listener provider", got)
	}
}

func TestRegistryDescribeRendersSnippets(t *testing.T) {
	reg, err := plugin.New(config.Default(), newStub("mail", "mailjet"))
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	ctx := plugin.NewSnippetCtx("localhost", "127.0.0.1:8811", "127.0.0.1:8811", "127.0.0.1:9999")

	info, err := reg.Describe(ctx)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if len(info) != 1 || len(info[0].Providers) != 1 {
		t.Fatalf("info = %+v", info)
	}
	snippet := info[0].Providers[0].Snippets[0]
	if want := "curl http://127.0.0.1:9999/mail/mailjet"; snippet.Code != want {
		t.Errorf("snippet = %q, want it rendered against the live port: %q", snippet.Code, want)
	}
	if info[0].Providers[0].Listener {
		t.Error("an HTTP provider must not be reported as a listener")
	}
	if len(info[0].Providers[0].Endpoints) != 1 {
		t.Error("endpoints must be carried through")
	}
}

func TestRegistryDescribeFailsOnABadSnippet(t *testing.T) {
	broken := &stubPlugin{name: "mail", title: "Mail", providers: []plugin.Provider{
		&stubProvider{name: "mailjet", owner: "mail", snippets: []plugin.Snippet{{Title: "bad", Code: "{{.Nope}}"}}},
	}}
	reg, err := plugin.New(config.Default(), broken)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	if _, err := reg.Describe(plugin.SnippetCtx{}); err == nil {
		t.Error("Describe must fail loudly on a snippet that cannot render")
	}
}

func TestDepsFor(t *testing.T) {
	cfg, err := config.Parse([]byte("[plugins.mail.providers.smtp]\nport = 2525\nbanner = \"hi\"\n"))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	reg, err := plugin.New(cfg, newStub("mail", "smtp"))
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	d := reg.DepsFor(plugin.Deps{}, "mail", "smtp")
	if d.Config.Port != 2525 {
		t.Errorf("port = %d, want the provider's own section", d.Config.Port)
	}
	if got := d.Config.String("banner", ""); got != "hi" {
		t.Errorf("banner = %q", got)
	}
	if d.Now == nil || d.NewID == nil || d.Logger == nil {
		t.Error("DepsFor must normalize, so plugins never nil-check")
	}
}

func names(providers []plugin.Provider) []string {
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		out = append(out, p.Name())
	}
	return out
}

func providersOf(refs []plugin.Ref) []plugin.Provider {
	out := make([]plugin.Provider, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Provider)
	}
	return out
}
