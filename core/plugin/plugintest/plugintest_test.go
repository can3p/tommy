package plugintest_test

import (
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/core/testutil/fakeplugin"
)

// TestFakePluginConforms is the positive case, and doubles as the proof that a
// well-formed plugin passes the real reporting path.
func TestFakePluginConforms(t *testing.T) {
	plugintest.Conformance(t, fakeplugin.New())
}

type prov struct {
	name        string
	owner       string
	description string
	endpoints   []plugin.Endpoint
	snippets    []plugin.Snippet
	mount       func(mux plugin.Mux)
}

func (p prov) Name() string                 { return p.name }
func (p prov) Plugin() string               { return p.owner }
func (p prov) Description() string          { return p.description }
func (p prov) Endpoints() []plugin.Endpoint { return p.endpoints }
func (p prov) Snippets() []plugin.Snippet   { return p.snippets }
func (p prov) RegisterIngress(mux plugin.Mux, _ plugin.Deps) {
	if p.mount != nil {
		p.mount(mux)
	}
}

func good() prov {
	return prov{
		name:        "vendor",
		owner:       "mail",
		description: "Imitates the vendor send API so an SDK can post real messages at it.",
		endpoints: []plugin.Endpoint{
			{Method: "POST", Path: "/v9/send", Description: "Accept a message and record it."},
		},
		snippets: []plugin.Snippet{{Title: "Send", Lang: "bash", Code: "curl {{.IngressURL}}/v9/send"}},
		mount: func(mux plugin.Mux) {
			mux.HandleFunc("POST /v9/send", func(w http.ResponseWriter, r *http.Request) {})
		},
	}
}

func TestCheckProvider(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*prov)
		wantErr string
	}{
		{name: "well formed", mutate: func(*prov) {}},
		{
			name:    "empty description",
			mutate:  func(p *prov) { p.description = "" },
			wantErr: "Description() is empty",
		},
		{
			name:    "boilerplate description",
			mutate:  func(p *prov) { p.description = "TODO: write a description here" },
			wantErr: "boilerplate",
		},
		{
			name:    "one word description",
			mutate:  func(p *prov) { p.description = "mailjet" },
			wantErr: "too short",
		},
		{
			name:    "no snippets",
			mutate:  func(p *prov) { p.snippets = nil },
			wantErr: "Snippets() is empty",
		},
		{
			name: "snippet that does not parse",
			mutate: func(p *prov) {
				p.snippets = []plugin.Snippet{{Title: "x", Lang: "bash", Code: "curl {{.IngressURL"}}
			},
			wantErr: "parse",
		},
		{
			name: "snippet referencing a field that does not exist",
			mutate: func(p *prov) {
				p.snippets = []plugin.Snippet{{Title: "x", Lang: "bash", Code: "curl {{.NoSuchPort}}"}}
			},
			wantErr: "render",
		},
		{
			name: "snippet without a title",
			mutate: func(p *prov) {
				p.snippets = []plugin.Snippet{{Lang: "bash", Code: "curl x"}}
			},
			wantErr: "no Title",
		},
		{
			name: "snippet without a language",
			mutate: func(p *prov) {
				p.snippets = []plugin.Snippet{{Title: "x", Code: "curl x"}}
			},
			wantErr: "no Lang",
		},
		{
			name:    "declared endpoint is never mounted",
			mutate:  func(p *prov) { p.mount = nil },
			wantErr: "never mounts it",
		},
		{
			name: "endpoint declared with the wrong method",
			mutate: func(p *prov) {
				p.endpoints = []plugin.Endpoint{{Method: "GET", Path: "/v9/send", Description: "Accept a message and record it."}}
			},
			wantErr: "never mounts it",
		},
		{
			name: "mounted route is not declared",
			mutate: func(p *prov) {
				p.mount = func(mux plugin.Mux) {
					mux.HandleFunc("POST /v9/send", func(w http.ResponseWriter, r *http.Request) {})
					mux.HandleFunc("GET /v9/secret", func(w http.ResponseWriter, r *http.Request) {})
				}
			},
			wantErr: "does not declare it in Endpoints()",
		},
		{
			name:    "no endpoints at all",
			mutate:  func(p *prov) { p.endpoints = nil; p.mount = nil },
			wantErr: "Endpoints() is empty",
		},
		{
			name:    "uppercase name",
			mutate:  func(p *prov) { p.name = "Vendor" },
			wantErr: "must be lowercase",
		},
		{
			name: "endpoint without a real description",
			mutate: func(p *prov) {
				p.endpoints = []plugin.Endpoint{{Method: "POST", Path: "/v9/send", Description: "todo"}}
			},
			wantErr: "boilerplate",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := good()
			tc.mutate(&p)
			errs := plugintest.CheckProvider(p)

			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("expected no problems, got %v", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("expected a problem containing %q, got none", tc.wantErr)
			}
			joined := joinErrs(errs)
			if !strings.Contains(joined, tc.wantErr) {
				t.Fatalf("problems = %s, want one containing %q", joined, tc.wantErr)
			}
		})
	}
}

type plug struct {
	name      string
	title     string
	desc      string
	providers []plugin.Provider
}

func (p plug) Name() string                        { return p.name }
func (p plug) Title() string                       { return p.title }
func (p plug) Description() string                 { return p.desc }
func (p plug) Providers() []plugin.Provider        { return p.providers }
func (p plug) RegisterAPI(plugin.Mux, plugin.Deps) {}
func (p plug) RegisterUI(plugin.Mux, plugin.Deps)  {}
func (p plug) Templates() fs.FS                    { return nil }

func TestCheckPlugin(t *testing.T) {
	base := func() plug {
		return plug{
			name:      "mail",
			title:     "Mail",
			desc:      "Captures outgoing mail from vendor APIs and SMTP so you can read it locally.",
			providers: []plugin.Provider{good()},
		}
	}
	tests := []struct {
		name    string
		mutate  func(*plug)
		wantErr string
	}{
		{name: "well formed", mutate: func(*plug) {}},
		{name: "no title", mutate: func(p *plug) { p.title = "" }, wantErr: "Title() is empty"},
		{name: "no description", mutate: func(p *plug) { p.desc = "" }, wantErr: "Description() is empty"},
		{name: "no providers", mutate: func(p *plug) { p.providers = nil }, wantErr: "Providers() is empty"},
		{
			name: "provider claims another plugin",
			mutate: func(p *plug) {
				other := good()
				other.owner = "sms"
				p.providers = []plugin.Provider{other}
			},
			wantErr: "but it is listed by plugin",
		},
		{
			name: "duplicate provider",
			mutate: func(p *plug) {
				p.providers = []plugin.Provider{good(), good()}
			},
			wantErr: "listed twice",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := base()
			tc.mutate(&p)
			errs := plugintest.CheckPlugin(p)
			if tc.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("expected no problems, got %v", errs)
				}
				return
			}
			if !strings.Contains(joinErrs(errs), tc.wantErr) {
				t.Fatalf("problems = %s, want one containing %q", joinErrs(errs), tc.wantErr)
			}
		})
	}
}

func TestListenerProviderNeedsNoEndpoints(t *testing.T) {
	// A provider that owns its own port has no HTTP routes to declare, and must
	// not be failed for it.
	for _, prov := range fakeplugin.New().Providers() {
		if lp, ok := prov.(plugin.ListenerProvider); ok {
			if errs := plugintest.CheckProvider(lp); len(errs) != 0 {
				t.Fatalf("listener provider reported problems: %v", errs)
			}
			return
		}
	}
	t.Fatal("the fake plugin should ship a listener provider")
}

func TestNewDepsIsDeterministic(t *testing.T) {
	d := plugintest.NewDeps().Normalize()
	first, second := d.NewID(), d.NewID()
	if first == second {
		t.Errorf("ids must still be unique inside one run, got %q twice", first)
	}
	if !d.Now().Equal(d.Now()) {
		t.Error("the clock must not move")
	}
	if d.Store == nil || d.Blobs == nil {
		t.Error("Deps must come with working stores")
	}
}

func joinErrs(errs []error) string {
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, " | ")
}
