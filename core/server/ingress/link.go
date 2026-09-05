package ingress

import (
	"net/http"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
)

// LinkHeader is the response header the ingress adds naming the page of every
// event a request produced.
//
// It is the one header tommy sends that no vendor does, and it is deliberate:
// the point of a fake mail provider in local development is to look at the
// mail, and this puts the link in the application's own logs without it having
// to call the API back. SDKs ignore response headers they do not know, so the
// vendor's real response shape is untouched. One request may fan out into
// several events, and then the header is repeated once per event.
const LinkHeader = "X-Tommy-Event-URL"

// linkWriter stamps the header just before the response goes out, which is the
// last moment at which the handler has finished appending.
type linkWriter struct {
	http.ResponseWriter
	collector *plugin.EventCollector
	url       func(event.ID) string
	stamped   bool
}

func (l *linkWriter) stamp() {
	if l.stamped {
		return
	}
	l.stamped = true
	for _, id := range l.collector.IDs() {
		if u := l.url(id); u != "" {
			l.Header().Add(LinkHeader, u)
		}
	}
}

func (l *linkWriter) WriteHeader(status int) {
	l.stamp()
	l.ResponseWriter.WriteHeader(status)
}

func (l *linkWriter) Write(p []byte) (int, error) {
	l.stamp()
	return l.ResponseWriter.Write(p)
}

// Flush keeps a streaming response streaming: net/http probes for the
// interface on the writer it is handed, and a wrapper that does not implement
// it silently turns a flush into a no-op.
func (l *linkWriter) Flush() {
	if f, ok := l.ResponseWriter.(http.Flusher); ok {
		l.stamp()
		f.Flush()
	}
}

// Unwrap lets net/http and anything else reach the real writer, so an
// interface this wrapper does not implement is still found.
func (l *linkWriter) Unwrap() http.ResponseWriter { return l.ResponseWriter }

// instrument wraps a provider's handler so what it captures becomes a link in
// its own response. Without an event URL builder - every test that constructs
// an ingress on its own - the handler is returned untouched.
func (i *Ingress) instrument(h http.Handler) http.Handler {
	if i.eventURL == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := plugin.NewEventCollector()
		lw := &linkWriter{ResponseWriter: w, collector: c, url: i.eventURL}
		h.ServeHTTP(lw, r.WithContext(plugin.WithEventCollector(r.Context(), c)))
		// A handler that wrote nothing at all still owes the caller the
		// header, and net/http will send a bare 200 for it.
		lw.stamp()
	})
}
