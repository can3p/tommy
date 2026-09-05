// Package sse writes the server-sent-events stream shared by the API and the
// UI. There is no hub: store.Subscribe already fans out, so a stream is just a
// subscriber plus a formatter.
package sse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/store"
)

// DefaultHeartbeat keeps proxies and idle connections from timing out.
const DefaultHeartbeat = 25 * time.Second

// Options tunes a stream.
type Options struct {
	// Heartbeat is how often a comment frame is sent. Zero means the default.
	Heartbeat time.Duration
	// Filter drops events the client did not ask for.
	Filter store.Query
	// Backlog, when set, is written as the first frames, so a client can paint
	// without a second request.
	Backlog []*event.Event

	// Envelope, when set, replaces the event as the JSON payload of a data
	// frame. The API uses it to add the URL of the event's own page, so a
	// stream consumer gets the same shape the REST routes return; core needs
	// no envelope of its own, and nothing here knows what the wrapper is.
	Envelope func(*event.Event) any
}

// Each appended event produces two frames:
//
//	(1) a default "message" frame carrying the event as JSON, so any plain
//	    EventSource consumer gets everything through onmessage;
//	(2) a frame named after the event Type carrying just the event id, so htmx
//	    can use hx-trigger="sse:mail.message" without parsing anything.
//
// Raw.Body is stripped from the JSON: raw bodies can be megabytes and every
// subscriber would pay for them on every event. Fetch the event by id for it.
func Stream(w http.ResponseWriter, r *http.Request, events <-chan *event.Event, opts Options) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	fmt.Fprint(w, ": tommy event stream\n\n")
	flusher.Flush()

	for _, e := range opts.Backlog {
		writeEvent(w, e, opts.Envelope)
	}
	if len(opts.Backlog) > 0 {
		flusher.Flush()
	}

	heartbeat := opts.Heartbeat
	if heartbeat <= 0 {
		heartbeat = DefaultHeartbeat
	}
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case e, ok := <-events:
			if !ok {
				return
			}
			if !opts.Filter.Matches(e) {
				continue
			}
			writeEvent(w, e, opts.Envelope)
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, e *event.Event, envelope func(*event.Event) any) {
	var body any = e.WithoutRawBody()
	if envelope != nil {
		body = envelope(e.WithoutRawBody())
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %s\ndata: %s\n\n", e.ID, payload)
	if e.Type != "" {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, e.ID)
	}
}

// Handler returns an http.Handler streaming everything appended to s.
// query builds the filter from the request; pass nil for "everything".
func Handler(s store.Store, query func(*http.Request) store.Query) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		var q store.Query
		if query != nil {
			q = query(r)
		}
		// Subscribe before anything else so no event can slip through between
		// the initial render and the first frame.
		ch := s.Subscribe(ctx)
		Stream(w, r, ch, Options{Filter: q})
	})
}
