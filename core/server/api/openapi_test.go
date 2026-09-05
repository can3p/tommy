package api_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/server/api"
	storemem "github.com/can3p/tommy/core/store/memory"
	"github.com/can3p/tommy/core/testutil"
)

// specPath is the checked-in description, relative to this package.
const specPath = "../../../docs/openapi.json"

// eventRoutes is the whole promise of the document, written out here rather
// than derived from the generator: a test that asks the generator what it
// generates proves nothing.
var eventRoutes = []string{
	"GET /events",
	"GET /events/{id}",
	"GET /events/stream",
	"DELETE /events",
	"DELETE /events/{id}",
	"GET /blobs/{id}",
}

// spec is the document as a running server serves it.
func spec(t *testing.T, in *testutil.Instance) map[string]any {
	t.Helper()
	var doc map[string]any
	resp, err := http.Get(in.API("/openapi.json"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return doc
}

// A running server describes itself, and the description is generated from the
// live route table rather than served from a copy that could be stale.
func TestOpenAPIRoute(t *testing.T) {
	in := start(t)
	doc := spec(t, in)

	if doc["openapi"] != "3.1.0" {
		t.Errorf("openapi = %v", doc["openapi"])
	}
	info, _ := doc["info"].(map[string]any)
	if info["version"] != api.APIVersion {
		t.Errorf("info.version = %v, want the API version rather than the build version", info["version"])
	}

	// The server names itself, so a request can be pasted out of a rendered
	// page and run.
	servers, _ := doc["servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("servers = %v", servers)
	}
	first, _ := servers[0].(map[string]any)
	url, _ := first["url"].(string)
	if !strings.HasPrefix(url, "http://") || !strings.HasSuffix(url, "/api/v1") {
		t.Errorf("server url = %q, want this instance's absolute API URL", url)
	}
}

// The document is the events API and nothing else. Every route it promises,
// and no route it does not.
func TestOpenAPIDescribesTheEventsAPI(t *testing.T) {
	s := api.BuildSpec(api.SpecOptions{})

	described := map[string]bool{}
	for path, item := range s.Paths {
		for method, op := range map[string]*api.Operation{
			"GET": item.Get, "POST": item.Post, "PUT": item.Put,
			"PATCH": item.Patch, "DELETE": item.Delete,
		} {
			if op != nil {
				described[normalize(method+" "+path)] = true
			}
		}
	}

	for _, want := range eventRoutes {
		if !described[normalize(want)] {
			t.Errorf("%s is missing from the description", want)
		}
	}
	if len(described) != len(eventRoutes) {
		t.Errorf("the description carries %d operations, want the %d of the events API: %v",
			len(described), len(eventRoutes), keys(described))
	}

	// The rest of the server is deliberately out of scope, and the document
	// says why rather than leaving a reader to wonder.
	for path := range s.Paths {
		for _, unwanted := range []string{"/health", "/plugins", "/openapi.json", "/mail/", "/sms/", "/v3.1/"} {
			if strings.HasPrefix(path, unwanted) {
				t.Errorf("%s is described, but the document is the events API", path)
			}
		}
	}
	if desc := s.Info.Description; !strings.Contains(desc, "fake vendor endpoints") ||
		!strings.Contains(desc, "read-back routes") {
		t.Errorf("the description does not say what it leaves out: %q", desc)
	}
}

// Everything the document describes is really mounted, compared against the
// mux rather than against the declarations it was generated from.
func TestOpenAPIDescribesOnlyMountedRoutes(t *testing.T) {
	srv, err := api.New(api.Options{Store: storemem.New(10)})
	if err != nil {
		t.Fatalf("api: %v", err)
	}
	mounted := map[string]bool{}
	for _, r := range srv.Routes() {
		mounted[normalize(r)] = true
	}

	s := api.BuildSpec(api.SpecOptions{})
	for path, item := range s.Paths {
		for method, op := range map[string]*api.Operation{
			"GET": item.Get, "POST": item.Post, "PUT": item.Put,
			"PATCH": item.Patch, "DELETE": item.Delete,
		} {
			if op == nil {
				continue
			}
			if !mounted[normalize(method+" "+path)] {
				t.Errorf("%s %s is described but not mounted; the document promises a route that does not exist", method, path)
			}
			if strings.TrimSpace(op.Summary) == "" {
				t.Errorf("%s %s has no summary", method, path)
			}
			if op.OperationID == "" {
				t.Errorf("%s %s has no operationId; generated clients name their methods from it", method, path)
			}
			if len(op.Responses) == 0 {
				t.Errorf("%s %s describes no response", method, path)
			}
		}
	}
}

// The checked-in document is a build product. It is in the repository so it can
// be linked, diffed and reviewed - and this is what stops it becoming a lie.
func TestCheckedInOpenAPIIsCurrent(t *testing.T) {
	want, err := api.BuildSpec(api.SpecOptions{}).JSON()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	if string(got) != string(want) {
		abs, _ := filepath.Abs(specPath)
		t.Errorf("%s is out of date: run `make openapi` and commit the result.\n%s",
			abs, firstDifference(string(got), string(want)))
	}
}

// The schemas come from the Go types, so a field added to a response cannot be
// missing from the document. This pins the one that matters most.
func TestOpenAPISchemasComeFromTheTypes(t *testing.T) {
	s := api.BuildSpec(api.SpecOptions{})
	event := s.Components.Schemas["ui.EventJSON"]
	if event == nil {
		t.Fatal("the event schema is missing")
	}
	for _, field := range []string{"id", "plugin", "provider", "type", "received_at", "summary", "raw", "url"} {
		if _, ok := event.Properties[field]; !ok {
			t.Errorf("event schema has no %q", field)
		}
	}
	// The envelope embeds *event.Event, and encoding/json inlines those
	// fields; a schema that referenced the embedded struct instead would
	// describe a shape the API never sends.
	if raw := event.Properties["raw"]; raw == nil || raw.Ref != "#/components/schemas/event.Raw" {
		t.Errorf("raw = %+v, want a reference to the shared component", raw)
	}
	if body := s.Components.Schemas["event.Raw"].Properties["body"]; body == nil || body.ContentEncoding != "base64" {
		t.Errorf("raw body = %+v, want base64-encoded bytes", body)
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

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
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
