// Package plugintest holds the conformance test every plugin and provider must
// pass.
//
// A fake nobody can figure out how to poke is worthless, so discoverability is
// a contract member rather than a docs convention: descriptions must be real,
// snippets must render, and every endpoint a provider advertises must actually
// answer. A task is not done until Conformance passes.
package plugintest

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	blobmem "github.com/can3p/tommy/core/blob/memory"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/server/ingress"
	storemem "github.com/can3p/tommy/core/store/memory"
)

// MinDescription is the shortest description accepted. One or two sentences is
// the contract; anything below this is a placeholder.
const MinDescription = 24

var boilerplate = []string{
	"todo", "tbd", "fixme", "xxx", "n/a", "none", "description",
	"lorem ipsum", "coming soon", "not implemented", "no description",
}

// Deps returns dependencies backed by fresh in-memory stores, with a fixed
// clock and counting ids so provider tests are deterministic.
func Deps(t testing.TB) plugin.Deps {
	t.Helper()
	var n int
	clock := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	return plugin.Deps{
		Store:  storemem.New(100),
		Blobs:  blobmem.New(1 << 20),
		Logger: slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return clock },
		NewID: func() string {
			n++
			return "test-id-" + string(rune('a'+n-1))
		},
	}
}

// SnippetCtx returns a fully populated context to render snippets against.
func SnippetCtx() plugin.SnippetCtx {
	ctx := plugin.NewSnippetCtx("localhost", "127.0.0.1:8811", "127.0.0.1:8811", "127.0.0.1:8822")
	ctx.SetAddr("mail", "smtp", "localhost:1025")
	ctx.SetAddr("files", "ftp", "localhost:2121")
	ctx.SetAddr("files", "sftp", "localhost:2222")
	return ctx
}

// Conformance checks a plugin and every provider it lists.
func Conformance(t *testing.T, p plugin.Plugin) {
	t.Helper()

	if p == nil {
		t.Fatal("conformance: nil plugin")
	}
	name := p.Name()
	t.Run("plugin/"+name, func(t *testing.T) {
		checkName(t, "plugin name", name)
		if strings.TrimSpace(p.Title()) == "" {
			t.Errorf("plugin %q: Title() is empty; it is the UI tab label", name)
		}
		checkDescription(t, "plugin "+name, p.Description())

		providers := p.Providers()
		if len(providers) == 0 {
			t.Errorf("plugin %q: Providers() is empty; a plugin with no provider can never receive anything", name)
		}
		seen := map[string]bool{}
		for _, prov := range providers {
			if prov == nil {
				t.Fatalf("plugin %q: Providers() contains a nil provider", name)
			}
			if seen[prov.Name()] {
				t.Errorf("plugin %q: provider %q listed twice", name, prov.Name())
			}
			seen[prov.Name()] = true
			if prov.Plugin() != name {
				t.Errorf("provider %q: Plugin() = %q, but it is listed by plugin %q",
					prov.Name(), prov.Plugin(), name)
			}
		}
	})

	for _, prov := range p.Providers() {
		if prov != nil {
			ConformanceProvider(t, prov)
		}
	}
}

// ConformanceProvider checks a single provider. Conformance calls it for every
// provider a plugin lists; call it directly from a provider's own package test.
func ConformanceProvider(t *testing.T, prov plugin.Provider) {
	t.Helper()

	t.Run("provider/"+prov.Plugin()+"/"+prov.Name(), func(t *testing.T) {
		checkName(t, "provider name", prov.Name())
		checkName(t, "provider plugin", prov.Plugin())
		checkDescription(t, "provider "+prov.Name(), prov.Description())
		checkSnippets(t, prov)
		checkEndpoints(t, prov)
	})
}

func checkName(t *testing.T, what, name string) {
	t.Helper()
	if strings.TrimSpace(name) == "" {
		t.Errorf("%s is empty", what)
		return
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			t.Errorf("%s %q must be lowercase letters, digits or dashes: it is a URL segment", what, name)
			return
		}
	}
}

func checkDescription(t *testing.T, what, desc string) {
	t.Helper()
	trimmed := strings.TrimSpace(desc)
	if trimmed == "" {
		t.Errorf("%s: Description() is empty; say what this fakes and why", what)
		return
	}
	if len(trimmed) < MinDescription {
		t.Errorf("%s: Description() %q is too short (%d < %d chars); one or two real sentences",
			what, trimmed, len(trimmed), MinDescription)
	}
	lower := strings.ToLower(trimmed)
	for _, b := range boilerplate {
		if lower == b || strings.HasPrefix(lower, b+" ") || strings.Contains(lower, "todo") {
			t.Errorf("%s: Description() %q is boilerplate", what, trimmed)
			return
		}
	}
}

func checkSnippets(t *testing.T, prov plugin.Provider) {
	t.Helper()
	snippets := prov.Snippets()
	if len(snippets) == 0 {
		t.Errorf("provider %q: Snippets() is empty; ship at least one command that puts real data in from a cold start", prov.Name())
		return
	}
	ctx := SnippetCtx()
	for i, s := range snippets {
		if strings.TrimSpace(s.Title) == "" {
			t.Errorf("provider %q: snippet %d has no Title", prov.Name(), i)
		}
		if strings.TrimSpace(s.Lang) == "" {
			t.Errorf("provider %q: snippet %q has no Lang (bash, go, python, ...)", prov.Name(), s.Title)
		}
		if strings.TrimSpace(s.Code) == "" {
			t.Errorf("provider %q: snippet %q has no Code", prov.Name(), s.Title)
			continue
		}
		out, err := s.Render(ctx)
		if err != nil {
			t.Errorf("provider %q: %v", prov.Name(), err)
			continue
		}
		if strings.TrimSpace(out) == "" {
			t.Errorf("provider %q: snippet %q renders to nothing", prov.Name(), s.Title)
		}
		if strings.Contains(out, "{{") {
			t.Errorf("provider %q: snippet %q still contains a template action after rendering: %s",
				prov.Name(), s.Title, out)
		}
	}
}

func checkEndpoints(t *testing.T, prov plugin.Provider) {
	t.Helper()

	_, isListener := prov.(plugin.ListenerProvider)
	endpoints := prov.Endpoints()
	if isListener && len(endpoints) == 0 {
		// A listener provider speaks its own protocol; it has no HTTP routes.
		return
	}
	if len(endpoints) == 0 {
		t.Errorf("provider %q: Endpoints() is empty; every mounted route must be declared so /api/v1/plugins and the UI can show it", prov.Name())
	}

	in := ingress.New(nil)
	prov.RegisterIngress(in.For(prov.Plugin(), prov.Name()), Deps(t))
	if err := in.Err(); err != nil {
		t.Errorf("provider %q: RegisterIngress: %v", prov.Name(), err)
		return
	}

	declared := map[string]plugin.Endpoint{}
	for _, e := range endpoints {
		if strings.TrimSpace(e.Path) == "" {
			t.Errorf("provider %q: endpoint %+v has no Path", prov.Name(), e)
			continue
		}
		checkDescription(t, "provider "+prov.Name()+" endpoint "+e.Path, e.Description)
		pat, err := ingress.ParsePattern(strings.TrimSpace(e.Method + " " + e.Path))
		if err != nil {
			t.Errorf("provider %q: endpoint %q %q is not a valid route: %v", prov.Name(), e.Method, e.Path, err)
			continue
		}
		declared[pat.Key()] = e
		method := pat.Method
		if method == "" {
			method = "GET"
		}
		if !in.Has(method, pat.Concrete()) {
			t.Errorf("provider %q declares endpoint %s %s but RegisterIngress never mounts it",
				prov.Name(), method, e.Path)
		}
	}

	for _, r := range in.Routes() {
		if _, ok := declared[r.Pattern.Key()]; !ok {
			t.Errorf("provider %q mounts route %s but does not declare it in Endpoints(); it would be invisible in /api/v1/plugins, the UI and `tommy providers`",
				prov.Name(), r.Pattern.String())
		}
	}
}
