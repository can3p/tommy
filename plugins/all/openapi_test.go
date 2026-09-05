// Package all_test holds the checks that need every plugin at once.
//
// They live here rather than in core/server/api because a plugin imports the
// core API package - for its error helpers and the event link - so the core
// cannot import the plugins back.
package all_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/server/api"
	storemem "github.com/can3p/tommy/core/store/memory"
	"github.com/can3p/tommy/plugins/all"
)

// describable is every plugin that mounts an API of its own, and so owes a
// description. Written out rather than derived, because "the plugins that have
// documents" is exactly what could silently shrink.
var describable = []string{"as2", "chat", "files", "hl7", "mail", "push", "sms"}

func registry(t *testing.T) *plugin.Registry {
	t.Helper()
	reg, err := plugin.New(config.Default(), all.Plugins()...)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return reg
}

// specFor is the path of a plugin's checked-in document, relative to this
// package.
func specFor(name string) string { return "../../docs/openapi-" + name + ".json" }

// Every plugin with an API has a description, and every plugin without one does
// not - so a new plugin that mounts routes cannot quietly ship undescribed, and
// a plugin that loses its API does not leave a document behind describing
// routes that are gone.
func TestEveryPluginWithAnAPIIsDescribed(t *testing.T) {
	want := map[string]bool{}
	for _, name := range describable {
		want[name] = true
	}

	for _, p := range registry(t).AllPlugins() {
		spec := api.BuildPluginSpec(api.PluginSpecOptions{Plugin: p})
		_, expected := want[p.Name()]
		switch {
		case spec == nil && expected:
			t.Errorf("plugin %q is expected to have an API description and builds none", p.Name())
		case spec != nil && !expected:
			t.Errorf("plugin %q builds an API description but is not in the list this test - and the Makefile - keep; add it to both", p.Name())
		}
		delete(want, p.Name())
	}
	for name := range want {
		t.Errorf("no plugin named %q, but a description is expected for it", name)
	}
}

// Each plugin's description describes exactly what that plugin mounts, compared
// against the mux rather than against the declarations the document came from.
func TestPluginSpecsMatchTheMountedRoutes(t *testing.T) {
	reg := registry(t)
	srv, err := api.New(api.Options{Store: storemem.New(10), Registry: reg})
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	mounted := map[string]bool{}
	for _, r := range srv.Routes() {
		mounted[normalize(r)] = true
	}

	for _, p := range reg.Plugins() {
		spec := api.BuildPluginSpec(api.PluginSpecOptions{Plugin: p})
		if spec == nil {
			continue
		}
		prefix := "/" + p.Name()
		described := map[string]bool{}

		for path, item := range spec.Paths {
			for method, op := range operations(item) {
				if op == nil {
					continue
				}
				route := normalize(method + " " + prefix + path)
				described[route] = true
				if !mounted[route] {
					t.Errorf("%s describes %s %s%s, which is not mounted", p.Name(), method, prefix, path)
				}
				if strings.TrimSpace(op.Summary) == "" {
					t.Errorf("%s: %s %s has no summary", p.Name(), method, path)
				}
				if op.OperationID == "" {
					t.Errorf("%s: %s %s has no operationId", p.Name(), method, path)
				}
			}
		}

		// And the other direction: everything the plugin mounts is described.
		// plugintest.Conformance checks this per plugin against APIEndpoints();
		// this checks it against the document those endpoints produced, which
		// is what a reader actually gets.
		for route := range mounted {
			if !strings.HasPrefix(route, "GET "+prefix+"/") && !strings.HasPrefix(route, "DELETE "+prefix+"/") {
				continue
			}
			if !described[route] {
				t.Errorf("%s mounts %s, which its description does not mention", p.Name(), route)
			}
		}
	}
}

// The checked-in documents are build products. They are in the repository so
// they can be linked, diffed and reviewed - and this is what stops them
// becoming lies.
func TestCheckedInPluginSpecsAreCurrent(t *testing.T) {
	for _, p := range registry(t).AllPlugins() {
		spec := api.BuildPluginSpec(api.PluginSpecOptions{Plugin: p})
		if spec == nil {
			continue
		}
		want, err := spec.JSON()
		if err != nil {
			t.Fatalf("%s: build: %v", p.Name(), err)
		}
		path := specFor(p.Name())
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v — run `make openapi`", p.Name(), err)
			continue
		}
		if string(got) != string(want) {
			abs, _ := filepath.Abs(path)
			t.Errorf("%s is out of date: run `make openapi` and commit the result.\n%s",
				abs, firstDifference(string(got), string(want)))
		}
	}
}

// A plugin's document says what it is for in the plugin's own words, and points
// at the events API rather than pretending to be the whole story.
func TestPluginSpecsExplainThemselves(t *testing.T) {
	for _, p := range registry(t).AllPlugins() {
		spec := api.BuildPluginSpec(api.PluginSpecOptions{Plugin: p})
		if spec == nil {
			continue
		}
		desc := spec.Info.Description
		if !strings.HasPrefix(desc, strings.TrimSpace(p.Description())) {
			t.Errorf("%s: the document does not open with the plugin's own description", p.Name())
		}
		for _, want := range []string{"/api/v1/events", "fake vendor endpoints"} {
			if !strings.Contains(desc, want) {
				t.Errorf("%s: the document does not mention %q", p.Name(), want)
			}
		}
		if spec.Servers[0].URL != "/api/v1/"+p.Name() {
			t.Errorf("%s: server url = %q", p.Name(), spec.Servers[0].URL)
		}
	}
}

func operations(item api.PathItem) map[string]*api.Operation {
	return map[string]*api.Operation{
		"GET": item.Get, "POST": item.Post, "PUT": item.Put,
		"PATCH": item.Patch, "DELETE": item.Delete,
	}
}

// normalize makes two spellings of one route comparable: a wildcard's name is
// not part of the route's identity, and a trailing "..." is Go's spelling
// rather than OpenAPI's.
func normalize(route string) string {
	method, path, ok := strings.Cut(route, " ")
	if !ok {
		method, path = "GET", route
	}
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			segments[i] = "{}"
		}
	}
	return strings.ToUpper(method) + " " + strings.Join(segments, "/")
}

// firstDifference points at the line that changed, because a thousand-line diff
// in a test failure helps nobody.
func firstDifference(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) && i < len(w); i++ {
		if g[i] != w[i] {
			return "first difference at line " + strconv.Itoa(i+1) +
				":\n  checked in: " + g[i] + "\n  generated:  " + w[i]
		}
	}
	if len(g) != len(w) {
		return "the files differ in length: " + strconv.Itoa(len(g)) +
			" lines checked in, " + strconv.Itoa(len(w)) + " generated"
	}
	return ""
}
