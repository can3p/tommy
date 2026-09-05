package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/server/api"
	"github.com/can3p/tommy/core/testutil"
)

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

// A running server describes itself, and the description is served from the
// live route table rather than from a copy that could be stale.
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

	paths, _ := doc["paths"].(map[string]any)
	for _, want := range []string{"/events", "/events/{id}", "/events/stream", "/blobs/{id}", "/openapi.json"} {
		if _, ok := paths[want]; !ok {
			t.Errorf("%s is missing from the description", want)
		}
	}

	// The plugin this instance enabled is described; nothing else is.
	if _, ok := paths["/fake/messages"]; !ok {
		t.Errorf("the enabled plugin's own routes are missing: %v", keys(paths))
	}
	for path := range paths {
		if strings.HasPrefix(path, "/mail/") {
			t.Errorf("%s is described, but this instance runs no mail plugin", path)
		}
	}
}

// The vendor endpoints are somebody else's specification, and describing half
// of one is worse than describing none. The document says so, and does not
// carry them.
func TestOpenAPIExcludesTheIngress(t *testing.T) {
	in := start(t)
	doc := spec(t, in)

	paths, _ := doc["paths"].(map[string]any)
	for path := range paths {
		if strings.HasPrefix(path, "/fake/v1/") {
			t.Errorf("%s is an ingress route and must not be in tommy's own description", path)
		}
	}
	info, _ := doc["info"].(map[string]any)
	desc, _ := info["description"].(string)
	if !strings.Contains(desc, "fake vendor endpoints") {
		t.Errorf("the description does not say what it leaves out: %q", desc)
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

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
