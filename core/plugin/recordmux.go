package plugin

import (
	"net/http"
	"sort"
)

// RecordingMux is a Mux that remembers every pattern registered on it and
// still serves them.
//
// The ingress has always needed this - it has to name both claimants of a
// colliding route - and the API needs it for a different reason: a plugin's
// API routes are the source of truth its OpenAPI description is checked
// against. Handing a plugin a bare *http.ServeMux makes both impossible,
// because a ServeMux cannot be asked what is on it.
type RecordingMux struct {
	mux      *http.ServeMux
	patterns []string
}

// NewRecordingMux returns an empty recording mux.
func NewRecordingMux() *RecordingMux {
	return &RecordingMux{mux: http.NewServeMux()}
}

// Handle records the pattern and mounts the handler.
func (m *RecordingMux) Handle(pattern string, h http.Handler) {
	m.patterns = append(m.patterns, pattern)
	m.mux.Handle(pattern, h)
}

// HandleFunc records the pattern and mounts the handler.
func (m *RecordingMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(h))
}

// Patterns returns what was registered, sorted, in "METHOD /path" form as the
// caller wrote it.
func (m *RecordingMux) Patterns() []string {
	out := append([]string(nil), m.patterns...)
	sort.Strings(out)
	return out
}

// ServeHTTP serves the routes registered on it.
func (m *RecordingMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mux.ServeHTTP(w, r)
}

var _ Mux = (*RecordingMux)(nil)
var _ http.Handler = (*RecordingMux)(nil)
