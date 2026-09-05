package api

import (
	"net/http"
	"strings"

	"github.com/can3p/tommy/core/plugin"
)

// PluginSpecOptions configures one plugin's description.
type PluginSpecOptions struct {
	// Plugin is the plugin being described. It must implement
	// plugin.APIDescriber; one that does not has nothing to describe.
	Plugin plugin.Plugin
	// ServerURL is what the paths are relative to - the plugin's own base,
	// "/api/v1/mail" for the checked-in document and this server's absolute
	// URL when served.
	ServerURL string
}

// BuildPluginSpec generates the OpenAPI description of one plugin's read-back
// API, or nil for a plugin that mounts none.
//
// One document per surface, rather than one document with everything in it. The
// events API is what every consumer of tommy programs against and it has its
// own; a plugin's routes are shaped by its content type, and someone asserting
// about mail wants the mail document, not 28 paths of which seven are theirs.
func BuildPluginSpec(opts PluginSpecOptions) *Spec {
	describer, ok := opts.Plugin.(plugin.APIDescriber)
	if !ok {
		return nil
	}
	endpoints := describer.APIEndpoints()
	if len(endpoints) == 0 {
		return nil
	}
	if opts.ServerURL == "" {
		opts.ServerURL = Prefix + "/" + opts.Plugin.Name()
	}

	b := newSchemaBuilder()
	spec := &Spec{
		OpenAPI: "3.1.0",
		Info: SpecInfo{
			Title:       "tommy " + opts.Plugin.Name() + " API",
			Version:     APIVersion,
			Description: pluginSpecDescription(opts.Plugin),
		},
		Servers:  []SpecServer{{URL: opts.ServerURL, Description: "This tommy's " + opts.Plugin.Name() + " API."}},
		Security: []map[string][]string{},
		Paths:    map[string]PathItem{},
	}
	for _, e := range endpoints {
		spec.add(e, b)
	}
	spec.Components = SpecComponents{Schemas: b.Components()}
	return spec
}

// pluginSpecDescription opens the document with what the plugin is for, in the
// plugin's own words, and with the one thing a reader needs to know about how
// this API relates to the events API.
func pluginSpecDescription(p plugin.Plugin) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(p.Description()))
	b.WriteString("\n\nThese routes read back what tommy captured, in the shape this content type\n")
	b.WriteString("wants: filters that only mean something here, and the parts that are bytes\n")
	b.WriteString("rather than JSON. They read from the same store as the events API, so a\n")
	b.WriteString("client that sends and immediately fetches sees its own write, and every\n")
	b.WriteString("entry carries the id of the event behind it.\n\n")
	b.WriteString("The events API - /api/v1/events, described in its own document - is the\n")
	b.WriteString("generic view of the same captures, and is the one to use when what you are\n")
	b.WriteString("asserting does not depend on the content type. The fake vendor endpoints are\n")
	b.WriteString("described nowhere here: they are the vendors' specifications, not tommy's.\n\n")
	b.WriteString("This document is generated from the server's route table. It cannot describe\n")
	b.WriteString("a route that does not exist, and CI fails if the checked-in copy stops\n")
	b.WriteString("matching the code.")
	return b.String()
}

// mountPluginSpec gives a plugin's own mux the route serving its description.
//
// Mounted by the core rather than by each plugin: it is the same route on every
// plugin, and a plugin that had to remember to mount it would eventually be a
// plugin that forgot.
func (a *API) mountPluginSpec(p plugin.Plugin, mux plugin.Mux) {
	if BuildPluginSpec(PluginSpecOptions{Plugin: p}) == nil {
		return
	}
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		spec := BuildPluginSpec(PluginSpecOptions{
			Plugin:    p,
			ServerURL: a.serverURL(r) + "/" + p.Name(),
		})
		body, err := spec.JSON()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(body)
	})
}
