package plugin

import (
	"context"
	"sync"

	"github.com/can3p/tommy/core/event"
)

// EventCollector records what a provider appended while handling one request.
//
// It exists so the ingress can answer a send with a link to what it captured
// without every provider having to build one: the middleware puts a collector
// in the request context, Deps.Append drops the id it just assigned into it,
// and the response carries the URL. A provider that passes the request's own
// context to Append - which is how they are all written - needs no change, and
// one that does not simply gets no link.
type EventCollector struct {
	mu  sync.Mutex
	ids []event.ID
}

// NewEventCollector returns an empty collector.
func NewEventCollector() *EventCollector { return &EventCollector{} }

// IDs returns the events appended so far, in the order they were appended. One
// request may produce several: a Mailjet Messages[] or a SendGrid
// personalizations[] fans out into one event per delivered message.
func (c *EventCollector) IDs() []event.ID {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]event.ID(nil), c.ids...)
}

func (c *EventCollector) add(id event.ID) {
	if c == nil || id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ids = append(c.ids, id)
}

type collectorKey struct{}

// WithEventCollector attaches a collector to a context.
func WithEventCollector(ctx context.Context, c *EventCollector) context.Context {
	return context.WithValue(ctx, collectorKey{}, c)
}

// EventCollectorFrom returns the collector attached to a context, or nil.
func EventCollectorFrom(ctx context.Context) *EventCollector {
	c, _ := ctx.Value(collectorKey{}).(*EventCollector)
	return c
}
