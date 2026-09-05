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

// specPath is the checked-in description, relative to this package.
const specPath = "../../docs/openapi.json"

func registry(t *testing.T) *plugin.Registry {
	t.Helper()
	reg, err := plugin.New(config.Default(), all.Plugins()...)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return reg
}

// The description and the server must agree in both directions. A route the
// description does not mention is a route no generated client can call and no
// reader knows exists; a described route the server does not mount is a
// promise it will not keep.
func TestOpenAPIDescribesEveryMountedRoute(t *testing.T) {
	reg := registry(t)

	srv, err := api.New(api.Options{Store: storemem.New(10), Registry: reg})
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	mounted := map[string]bool{}
	for _, r := range srv.Routes() {
		mounted[normalize(r)] = true
	}

	spec := api.BuildSpec(api.SpecOptions{Registry: reg})
	described := map[string]bool{}
	for path, item := range spec.Paths {
		for method, op := range map[string]any{
			"GET": item.Get, "POST": item.Post, "PUT": item.Put,
			"PATCH": item.Patch, "DELETE": item.Delete,
		} {
			if !isNil(op) {
				described[normalize(method+" "+path)] = true
			}
		}
	}

	for route := range mounted {
		if !described[route] {
			t.Errorf("%s is mounted but not described; declare it in the plugin's APIEndpoints() (or, for a core route, in coreEndpoints())", route)
		}
	}
	for route := range described {
		if !mounted[route] {
			t.Errorf("%s is described but not mounted; the description promises a route that does not exist", route)
		}
	}
}

// The checked-in document is a build product. It is in the repository so it can
// be linked, diffed and reviewed - and this is what stops it becoming a lie.
func TestCheckedInOpenAPIIsCurrent(t *testing.T) {
	want, err := api.BuildSpec(api.SpecOptions{Registry: registry(t)}).JSON()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	if string(got) != string(want) {
		abs, _ := filepath.Abs(specPath)
		t.Errorf("%s is out of date: run `make openapi` and commit the result.\n%s", abs, firstDifference(string(got), string(want)))
	}
}

// Every plugin route in the description carries prose. A generated document
// whose summaries are empty is a route list, not a description.
func TestOpenAPIOperationsAreDescribed(t *testing.T) {
	spec := api.BuildSpec(api.SpecOptions{Registry: registry(t)})
	for path, item := range spec.Paths {
		for _, op := range []*api.Operation{item.Get, item.Post, item.Put, item.Patch, item.Delete} {
			if op == nil {
				continue
			}
			if strings.TrimSpace(op.Summary) == "" {
				t.Errorf("%s has no summary", path)
			}
			if op.OperationID == "" {
				t.Errorf("%s has no operationId; generated clients name their methods from it", path)
			}
			if len(op.Responses) == 0 {
				t.Errorf("%s describes no response", path)
			}
		}
	}
}

// normalize makes two spellings of one route comparable: the mux writes
// "GET /messages/{id}", the description writes the same path, but a wildcard
// may be named differently on either side and a trailing "..." is not part of
// the identity.
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

func isNil(v any) bool {
	switch op := v.(type) {
	case *api.Operation:
		return op == nil
	default:
		return v == nil
	}
}

// firstDifference points at the line that changed, because a 4000-line diff in
// a test failure helps nobody.
func firstDifference(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) && i < len(w); i++ {
		if g[i] != w[i] {
			return "first difference at line " + strconv.Itoa(i+1) + ":\n  checked in: " + g[i] + "\n  generated:  " + w[i]
		}
	}
	if len(g) != len(w) {
		return "the files differ in length: " + strconv.Itoa(len(g)) + " lines checked in, " + strconv.Itoa(len(w)) + " generated"
	}
	return ""
}
