package hl7_test

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/plugin/plugintest"
	"github.com/can3p/tommy/plugins/hl7"
)

// The conformance gate: descriptions are real, snippets render, and every
// declared endpoint is actually mounted.
func TestConformance(t *testing.T) {
	plugintest.Conformance(t, hl7.New(fakeProvider{}))
}

// Until the MLLP provider lands, hl7.New() has no providers. That is the only
// thing conformance can hold against it, and this test pins that down so the
// day a provider is added nothing else has quietly rotted.
func TestBarePluginOnlyLacksAProvider(t *testing.T) {
	errs := plugintest.CheckPlugin(hl7.New())
	if len(errs) != 1 {
		t.Fatalf("CheckPlugin(hl7.New()) reported %d problems, want only the missing provider: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "Providers() is empty") {
		t.Errorf("unexpected conformance failure: %v", errs[0])
	}
}

func TestPluginIdentity(t *testing.T) {
	p := hl7.New()
	if p.Name() != "hl7" {
		t.Errorf("Name() = %q, want hl7", p.Name())
	}
	if p.Title() != "HL7" {
		t.Errorf("Title() = %q, want HL7", p.Title())
	}
	if len(p.Description()) < 40 || !strings.Contains(strings.ToLower(p.Description()), "hl7") {
		t.Errorf("Description() = %q, want a couple of real sentences", p.Description())
	}
	if got := p.Providers(); got == nil || len(got) != 0 {
		t.Errorf("Providers() = %v, want an empty non-nil slice until the MLLP provider lands", got)
	}

	names, err := fs.Glob(p.Templates(), "*.html")
	if err != nil {
		t.Fatalf("Templates(): %v", err)
	}
	if len(names) == 0 {
		t.Error("Templates() returned no templates; the tab cannot render")
	}
}

// The whole path, over the real server: a message posted to a provider is
// parsed, stored, and readable back through the plugin's own API.
func TestEndToEnd(t *testing.T) {
	in := start(t)

	resp, err := in.Client.Post(in.Ingress("/fake-hl7/messages"), "text/plain", strings.NewReader(
		string(fixture(t, "adt_a01.hl7"))))
	if err != nil {
		t.Fatalf("post message: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	var created struct {
		ID        string `json:"id"`
		ControlID string `json:"control_id"`
		Segments  int    `json:"segments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.ControlID != "MSG00001" {
		t.Errorf("control id = %q, want MSG00001", created.ControlID)
	}
	if created.Segments != 5 {
		t.Errorf("segments = %d, want 5", created.Segments)
	}

	var envelope struct {
		Title   string `json:"title"`
		Message struct {
			Header struct {
				ControlID string `json:"control_id"`
			} `json:"header"`
		} `json:"message"`
	}
	if status := in.GetJSON(in.API("/hl7/messages/"+created.ID), &envelope); status != http.StatusOK {
		t.Fatalf("read back status = %d", status)
	}
	if envelope.Message.Header.ControlID != "MSG00001" {
		t.Errorf("read back control id = %q", envelope.Message.Header.ControlID)
	}
}
