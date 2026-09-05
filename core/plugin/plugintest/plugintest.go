// Package plugintest holds the conformance checks every plugin and provider
// must pass.
//
// A fake nobody can figure out how to poke is worthless, so discoverability is
// a contract member rather than a docs convention: descriptions must be real,
// snippets must render, and every endpoint a provider advertises must actually
// answer. A task is not done until Conformance passes.
package plugintest

import (
	"fmt"
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
// the contract; anything shorter is a placeholder.
const MinDescription = 24

var boilerplate = []string{
	"todo", "tbd", "fixme", "xxx", "n/a", "none", "description",
	"lorem ipsum", "coming soon", "not implemented", "no description",
}

// NewDeps returns dependencies backed by fresh in-memory stores, with a fixed
// clock and counting ids, so provider tests are deterministic.
func NewDeps() plugin.Deps {
	var n int
	clock := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	return plugin.Deps{
		Store:  storemem.New(100),
		Blobs:  blobmem.New(1 << 20),
		Logger: slog.New(slog.DiscardHandler),
		Now:    func() time.Time { return clock },
		NewID: func() string {
			n++
			return fmt.Sprintf("test-id-%03d", n)
		},
	}
}

// Deps is NewDeps with a testing.TB, for symmetry with the rest of the helpers.
func Deps(t testing.TB) plugin.Deps {
	t.Helper()
	return NewDeps()
}

// SnippetCtx returns a fully populated context to render snippets against.
func SnippetCtx() plugin.SnippetCtx {
	ctx := plugin.NewSnippetCtx("localhost", "127.0.0.1:8811", "127.0.0.1:8811", "127.0.0.1:8822")
	ctx.SetAddr("mail", "smtp", "localhost:1025")
	ctx.SetAddr("files", "ftp", "localhost:2121")
	ctx.SetAddr("files", "sftp", "localhost:2222")
	return ctx
}

// Conformance checks a plugin and every provider it lists, reporting each
// problem through t.
func Conformance(t *testing.T, p plugin.Plugin) {
	t.Helper()
	if p == nil {
		t.Fatal("conformance: nil plugin")
	}

	name := "<nil>"
	if p != nil {
		name = p.Name()
	}
	t.Run("plugin/"+name, func(t *testing.T) {
		report(t, CheckPlugin(p))
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
	if prov == nil {
		t.Fatal("conformance: nil provider")
	}
	t.Run("provider/"+prov.Plugin()+"/"+prov.Name(), func(t *testing.T) {
		report(t, CheckProvider(prov))
	})
}

func report(t *testing.T, errs []error) {
	t.Helper()
	for _, err := range errs {
		t.Error(err)
	}
}

// CheckPlugin returns everything wrong with a plugin's discoverability, without
// needing a *testing.T. I1 uses it to sweep the whole registry.
func CheckPlugin(p plugin.Plugin) []error {
	var errs []error
	if p == nil {
		return []error{fmt.Errorf("nil plugin")}
	}
	name := p.Name()
	errs = append(errs, checkName("plugin name", name)...)
	if strings.TrimSpace(p.Title()) == "" {
		errs = append(errs, fmt.Errorf("plugin %q: Title() is empty; it is the UI tab label", name))
	}
	errs = append(errs, checkDescription("plugin "+name, p.Description())...)

	providers := p.Providers()
	if len(providers) == 0 {
		errs = append(errs, fmt.Errorf("plugin %q: Providers() is empty; a plugin with no provider can never receive anything", name))
	}
	seen := map[string]bool{}
	for i, prov := range providers {
		if prov == nil {
			errs = append(errs, fmt.Errorf("plugin %q: Providers()[%d] is nil", name, i))
			continue
		}
		if seen[prov.Name()] {
			errs = append(errs, fmt.Errorf("plugin %q: provider %q is listed twice", name, prov.Name()))
		}
		seen[prov.Name()] = true
		if prov.Plugin() != name {
			errs = append(errs, fmt.Errorf("provider %q: Plugin() = %q, but it is listed by plugin %q",
				prov.Name(), prov.Plugin(), name))
		}
	}
	errs = append(errs, checkAPIEndpoints(p)...)
	return errs
}

// checkAPIEndpoints is rule 7 applied to a plugin's own API: a plugin that
// mounts routes under /api/v1/<name>/ must describe them, every described
// route must be mounted, and vice versa.
//
// The description is generated from these, and served at
// /api/v1/<name>/openapi.json - so an undeclared route is one no reader knows
// exists and no generated client can call, and a declaration with no route is a
// promise the server does not keep.
func checkAPIEndpoints(p plugin.Plugin) []error {
	var errs []error
	name := p.Name()

	rec := plugin.NewRecordingMux()
	p.RegisterAPI(rec, NewDeps())
	mounted := map[string]string{} // method + normalized path -> as written
	for _, pat := range rec.Patterns() {
		parsed, err := ingress.ParsePattern(pat)
		if err != nil {
			errs = append(errs, fmt.Errorf("plugin %q: RegisterAPI mounts %q, which is not a valid route: %w", name, pat, err))
			continue
		}
		mounted[routeKey(parsed)] = parsed.String()
	}

	describer, ok := p.(plugin.APIDescriber)
	if !ok {
		if len(mounted) > 0 {
			errs = append(errs, fmt.Errorf(
				"plugin %q mounts %d API route(s) but does not implement plugin.APIDescriber; without APIEndpoints() they appear in no description and nobody can discover them",
				name, len(mounted)))
		}
		// A plugin that mounts nothing owes nothing, which is why the
		// interface is optional.
		return errs
	}

	declared := map[string]bool{}
	for _, e := range describer.APIEndpoints() {
		if strings.TrimSpace(e.Path) == "" {
			errs = append(errs, fmt.Errorf("plugin %q: API endpoint %+v has no Path", name, e))
			continue
		}
		errs = append(errs, checkDescription("plugin "+name+" API endpoint "+e.Path, e.Description)...)
		parsed, err := ingress.ParsePattern(strings.TrimSpace(e.Method + " " + e.Path))
		if err != nil {
			errs = append(errs, fmt.Errorf("plugin %q: API endpoint %q %q is not a valid route: %w", name, e.Method, e.Path, err))
			continue
		}
		if declared[routeKey(parsed)] {
			errs = append(errs, fmt.Errorf("plugin %q: API endpoint %s is declared twice", name, parsed.String()))
		}
		declared[routeKey(parsed)] = true
		if _, ok := mounted[routeKey(parsed)]; !ok {
			errs = append(errs, fmt.Errorf("plugin %q declares API endpoint %s but RegisterAPI never mounts it",
				name, parsed.String()))
		}
		for _, q := range e.Query {
			if strings.TrimSpace(q.Name) == "" {
				errs = append(errs, fmt.Errorf("plugin %q: endpoint %s has a query parameter with no name", name, parsed.String()))
			}
			if strings.TrimSpace(q.Description) == "" {
				errs = append(errs, fmt.Errorf("plugin %q: endpoint %s: query parameter %q has no description", name, parsed.String(), q.Name))
			}
		}
	}

	for key, written := range mounted {
		if !declared[key] {
			errs = append(errs, fmt.Errorf("plugin %q mounts API route %s but does not declare it in APIEndpoints(); it would be missing from the plugin's OpenAPI description",
				name, written))
		}
	}
	return errs
}

// routeKey identifies one route for comparison. Pattern.Key() normalizes
// wildcard names but drops the method, which the ingress does not need and this
// does: a plugin routinely serves GET and DELETE on one path.
func routeKey(p ingress.Pattern) string {
	method := p.Method
	if method == "" {
		method = "GET"
	}
	return method + " " + p.Key()
}

// CheckProvider returns everything wrong with a provider's discoverability.
func CheckProvider(prov plugin.Provider) []error {
	if prov == nil {
		return []error{fmt.Errorf("nil provider")}
	}
	var errs []error
	errs = append(errs, checkName("provider name", prov.Name())...)
	errs = append(errs, checkName("provider plugin", prov.Plugin())...)
	errs = append(errs, checkDescription("provider "+prov.Name(), prov.Description())...)
	errs = append(errs, checkSnippets(prov)...)
	errs = append(errs, checkEndpoints(prov)...)
	return errs
}

func checkName(what, name string) []error {
	if strings.TrimSpace(name) == "" {
		return []error{fmt.Errorf("%s is empty", what)}
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return []error{fmt.Errorf("%s %q must be lowercase letters, digits or dashes: it is a URL segment", what, name)}
		}
	}
	return nil
}

func checkDescription(what, desc string) []error {
	trimmed := strings.TrimSpace(desc)
	if trimmed == "" {
		return []error{fmt.Errorf("%s: Description() is empty; say what this fakes and why", what)}
	}
	var errs []error
	if len(trimmed) < MinDescription {
		errs = append(errs, fmt.Errorf("%s: Description() %q is too short (%d < %d chars); one or two real sentences",
			what, trimmed, len(trimmed), MinDescription))
	}
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "todo") {
		return append(errs, fmt.Errorf("%s: Description() %q is boilerplate", what, trimmed))
	}
	for _, b := range boilerplate {
		if lower == b || strings.HasPrefix(lower, b+" ") {
			return append(errs, fmt.Errorf("%s: Description() %q is boilerplate", what, trimmed))
		}
	}
	return errs
}

func checkSnippets(prov plugin.Provider) []error {
	snippets := prov.Snippets()
	if len(snippets) == 0 {
		return []error{fmt.Errorf("provider %q: Snippets() is empty; ship at least one command that puts real data in from a cold start", prov.Name())}
	}
	var errs []error
	ctx := SnippetCtx()
	for i, s := range snippets {
		if strings.TrimSpace(s.Title) == "" {
			errs = append(errs, fmt.Errorf("provider %q: snippet %d has no Title", prov.Name(), i))
		}
		if strings.TrimSpace(s.Lang) == "" {
			errs = append(errs, fmt.Errorf("provider %q: snippet %q has no Lang (bash, go, python, ...)", prov.Name(), s.Title))
		}
		if strings.TrimSpace(s.Code) == "" {
			errs = append(errs, fmt.Errorf("provider %q: snippet %q has no Code", prov.Name(), s.Title))
			continue
		}
		out, err := s.Render(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("provider %q: %w", prov.Name(), err))
			continue
		}
		if strings.TrimSpace(out) == "" {
			errs = append(errs, fmt.Errorf("provider %q: snippet %q renders to nothing", prov.Name(), s.Title))
		}
		if strings.Contains(out, "{{") {
			errs = append(errs, fmt.Errorf("provider %q: snippet %q still contains a template action after rendering: %s",
				prov.Name(), s.Title, out))
		}
	}
	return errs
}

func checkEndpoints(prov plugin.Provider) []error {
	_, isListener := prov.(plugin.ListenerProvider)
	endpoints := prov.Endpoints()
	if isListener && len(endpoints) == 0 {
		// A listener provider speaks its own protocol; it has no HTTP routes.
		return nil
	}

	var errs []error
	if len(endpoints) == 0 {
		errs = append(errs, fmt.Errorf("provider %q: Endpoints() is empty; every mounted route must be declared so /api/v1/plugins, the UI and `tommy providers` can show it", prov.Name()))
	}

	in := ingress.New(nil)
	prov.RegisterIngress(in.For(prov.Plugin(), prov.Name()), NewDeps())
	if err := in.Err(); err != nil {
		return append(errs, fmt.Errorf("provider %q: RegisterIngress: %w", prov.Name(), err))
	}

	declared := map[string]plugin.Endpoint{}
	for _, e := range endpoints {
		if strings.TrimSpace(e.Path) == "" {
			errs = append(errs, fmt.Errorf("provider %q: endpoint %+v has no Path", prov.Name(), e))
			continue
		}
		errs = append(errs, checkDescription("provider "+prov.Name()+" endpoint "+e.Path, e.Description)...)
		pat, err := ingress.ParsePattern(strings.TrimSpace(e.Method + " " + e.Path))
		if err != nil {
			errs = append(errs, fmt.Errorf("provider %q: endpoint %q %q is not a valid route: %w", prov.Name(), e.Method, e.Path, err))
			continue
		}
		declared[pat.Key()] = e
		method := pat.Method
		if method == "" {
			method = "GET"
		}
		if !in.Has(method, pat.Concrete()) {
			errs = append(errs, fmt.Errorf("provider %q declares endpoint %s %s but RegisterIngress never mounts it",
				prov.Name(), method, e.Path))
		}
	}

	for _, r := range in.Routes() {
		if _, ok := declared[r.Pattern.Key()]; !ok {
			errs = append(errs, fmt.Errorf("provider %q mounts route %s but does not declare it in Endpoints(); it would be invisible in /api/v1/plugins, the UI and `tommy providers`",
				prov.Name(), r.Pattern.String()))
		}
	}
	return errs
}
