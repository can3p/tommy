package plugin

// APIDescriber is an optional interface for a plugin that mounts API routes of
// its own. It is what its OpenAPI description is generated from.
//
// Optional rather than a member of Plugin, in the same spirit as
// AddressableProvider: a plugin that mounts nothing under /api/v1/<name>/ -
// snmp, which lives entirely on the generic event routes - has nothing to say
// and should not have to say it. A plugin that *does* mount routes must
// implement this, and plugintest.Conformance fails it otherwise: an
// undocumented route is one no reader knows exists and no generated client can
// call.
//
// Paths are relative to /api/v1/<name>, exactly as RegisterAPI mounts them.
type APIDescriber interface {
	APIEndpoints() []APIEndpoint
}

// APIEndpoint documents one route of an API.
//
// It is deliberately not Endpoint. Endpoint is the *discovery* surface a
// provider advertises - what /api/v1/plugins, the UI panel and `tommy
// providers` render - and it is a three-field struct because that is all a
// human-readable listing needs. This is the input to a generated document, and
// carries what a document needs: the shape of the response, the filters, the
// status. Sharing one struct would put half its fields in front of every user
// of the other.
type APIEndpoint struct {
	Method      string
	Path        string
	Description string

	// Query documents the query parameters the route accepts. Never
	// exhaustive by contract: an undeclared parameter is ignored, not
	// rejected.
	Query []APIParam

	// Response is a zero value of the JSON body. The schema is generated from
	// its type, so it cannot drift from what the handler returns. Leave it nil
	// for a route whose body is not JSON.
	Response any

	// Produces is the response media type when it is not application/json.
	Produces string

	// Status is the success status when it is not 200.
	Status int
}

// APIParam is one query parameter. Three fields is all these APIs need: every
// filter is an optional string, an integer or a flag.
type APIParam struct {
	Name        string
	Description string
	// Type is "string" (the default), "integer" or "boolean".
	Type string
}

// CommonListParams are the filters every listing route inherits from
// api.ParseQuery. A plugin's list endpoint declares these plus its own rather
// than restating them, so eight plugins cannot end up documenting the same six
// filters six different ways.
//
// It omits `plugin`, which ParseQuery also accepts: a route under
// /api/v1/<name>/ is already scoped to one plugin, and offering the filter
// there would describe something that cannot widen the result.
func CommonListParams() []APIParam {
	return []APIParam{
		{Name: "provider", Description: "Only entries captured by this provider, such as mailjet."},
		{Name: "type", Description: "Only entries of this event type, such as mail.message."},
		{Name: "search", Description: "Case-insensitive substring over the summary and type."},
		{Name: "since", Description: "RFC3339 timestamp, a duration such as 5m, or unix milliseconds."},
		{Name: "limit", Description: "Maximum number of entries to return.", Type: "integer"},
		{Name: "offset", Description: "How many entries to skip.", Type: "integer"},
	}
}
