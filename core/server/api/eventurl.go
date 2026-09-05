package api

import (
	"context"
	"net/http"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/server/ui"
)

// origin is where a link this API hands out should point.
func (a *API) origin(r *http.Request) string {
	return ui.Origin(a.opts.SnippetCtx().UIURL, r)
}

type originKey struct{}

// withOrigin makes the origin available to plugin API handlers, which are
// mounted under this one and have no other way to learn where the UI is: they
// are handed plugin.Deps, which carries a ProviderConfig rather than the
// server's addresses, and those addresses are not known until the listeners
// have bound anyway.
func (a *API) withOrigin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), originKey{}, a.origin(r))))
	})
}

// EventURL is the absolute URL of an event's own page, for a plugin API
// handler serving under /api/v1/<plugin>/. Every read-back API that returns
// event-shaped resources should carry it, so a caller that just sent something
// can open what it sent.
//
// It falls back to a site-relative path for a request that did not come
// through the core API - a plugin handler mounted on a bare mux in a test.
func EventURL(r *http.Request, id event.ID) string {
	origin, _ := r.Context().Value(originKey{}).(string)
	return ui.EventURL(origin, id)
}
