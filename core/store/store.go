// Package store defines the event log every plugin writes to and every read
// surface (API, UI, SSE) reads from.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/can3p/tommy/core/event"
)

// ErrNotFound is returned by Get and Delete for an unknown id.
var ErrNotFound = errors.New("store: event not found")

// Query filters a listing. Zero values mean "no filter".
//
// Plugin, Provider and Type match exactly. Search is a case-insensitive
// substring match over the Summary fields (From, To, Title, Snippet) and the
// event Type - the same fields a generic list view shows. Since is exclusive of
// events older than it. Limit <= 0 means "no limit".
type Query struct {
	Plugin, Provider, Type, Search string
	Since                          time.Time
	Limit, Offset                  int
}

// Store is the event log. Implementations must be safe for concurrent use.
type Store interface {
	// Append records an event, assigning ID and ReceivedAt when they are empty.
	// The assignment is visible on the caller's event.
	Append(ctx context.Context, e *event.Event) error
	// List returns matching events, newest first.
	List(ctx context.Context, q Query) ([]*event.Event, error)
	// Get returns a single event, or ErrNotFound.
	Get(ctx context.Context, id event.ID) (*event.Event, error)
	// Delete removes a single event, or returns ErrNotFound.
	Delete(ctx context.Context, id event.ID) error
	// Clear removes every event of a plugin; "" clears everything.
	Clear(ctx context.Context, plugin string) error
	// Subscribe returns a channel of newly appended events. The channel is
	// closed when ctx is done. Delivery is best effort: a subscriber that
	// cannot keep up misses events, because Append must never block.
	Subscribe(ctx context.Context) <-chan *event.Event
}
