// Package memory implements store.Store as a per-plugin ring buffer with
// pub/sub fan-out.
//
// Retention is per plugin on purpose: a chatty FTP plugin must not push a
// morning's worth of emails out of the mail tab.
package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/store"
)

// DefaultCapacity is the number of events retained per plugin when no capacity
// is configured.
const DefaultCapacity = 500

// DefaultSubscriberBuffer is how many events a subscriber may fall behind
// before it starts missing them.
const DefaultSubscriberBuffer = 64

type subscriber struct {
	ch chan *event.Event
}

// Store is an in-memory store.Store. Safe for concurrent use.
type Store struct {
	mu        sync.RWMutex
	rings     map[string]*ring
	index     map[event.ID]*entry
	seq       uint64
	capacity  int
	perPlugin map[string]int

	subsMu sync.RWMutex
	subs   map[*subscriber]struct{}
	subBuf int

	dropped atomic.Uint64

	now   func() time.Time
	newID func() string
}

var _ store.Store = (*Store)(nil)

// Option configures a Store.
type Option func(*Store)

// WithPluginCapacity overrides the retained event count for a single plugin.
func WithPluginCapacity(plugin string, capacity int) Option {
	return func(s *Store) {
		if capacity > 0 {
			s.perPlugin[plugin] = capacity
		}
	}
}

// WithClock injects a clock, so tests get deterministic ReceivedAt values.
func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

// WithIDFunc injects an id generator.
func WithIDFunc(f func() string) Option {
	return func(s *Store) {
		if f != nil {
			s.newID = f
		}
	}
}

// WithSubscriberBuffer sets how many events a subscriber may buffer before
// events start being dropped for it.
func WithSubscriberBuffer(n int) Option {
	return func(s *Store) {
		if n > 0 {
			s.subBuf = n
		}
	}
}

// New returns a store retaining capacity events per plugin.
// capacity <= 0 means DefaultCapacity.
func New(capacity int, opts ...Option) *Store {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	s := &Store{
		rings:     map[string]*ring{},
		index:     map[event.ID]*entry{},
		capacity:  capacity,
		perPlugin: map[string]int{},
		subs:      map[*subscriber]struct{}{},
		subBuf:    DefaultSubscriberBuffer,
		now:       time.Now,
		newID:     event.NewID,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Dropped reports how many event deliveries were dropped because a subscriber
// was not keeping up. Exposed for tests and diagnostics.
func (s *Store) Dropped() uint64 { return s.dropped.Load() }

// Subscribers reports the number of live subscribers.
func (s *Store) Subscribers() int {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	return len(s.subs)
}

// Len reports the number of retained events.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.index)
}

func (s *Store) capacityFor(plugin string) int {
	if c, ok := s.perPlugin[plugin]; ok {
		return c
	}
	return s.capacity
}

// Append records e, assigning ID and ReceivedAt when empty. The values are
// written back to the caller's event; the store keeps its own copy.
func (s *Store) Append(ctx context.Context, e *event.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e == nil {
		return fmt.Errorf("store: append nil event")
	}
	if e.ID == "" {
		e.ID = event.ID(s.newID())
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = s.now()
	}

	stored := e.Clone()

	s.mu.Lock()
	if _, exists := s.index[stored.ID]; exists {
		s.mu.Unlock()
		return fmt.Errorf("store: duplicate event id %q", stored.ID)
	}
	s.seq++
	ent := &entry{ev: stored, seq: s.seq}
	r, ok := s.rings[stored.Plugin]
	if !ok {
		r = newRing(s.capacityFor(stored.Plugin))
		s.rings[stored.Plugin] = r
	}
	if evicted := r.push(ent); evicted != nil {
		delete(s.index, evicted.ev.ID)
	}
	s.index[stored.ID] = ent
	s.mu.Unlock()

	s.broadcast(stored)
	return nil
}

// broadcast fans an event out to subscribers. It never blocks: a subscriber
// whose buffer is full misses the event, which is the right trade for a live
// view - Append is on the hot path of a fake API answering a real SDK.
func (s *Store) broadcast(e *event.Event) {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	for sub := range s.subs {
		select {
		case sub.ch <- e.Clone():
		default:
			s.dropped.Add(1)
		}
	}
}

// List returns matching events, newest first.
func (s *Store) List(ctx context.Context, q store.Query) ([]*event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	var matched []*entry
	for plugin, r := range s.rings {
		if q.Plugin != "" && plugin != q.Plugin {
			continue
		}
		for i := 0; i < r.len(); i++ {
			ent := r.at(i)
			if q.Matches(ent.ev) {
				matched = append(matched, ent)
			}
		}
	}
	s.mu.RUnlock()

	// Newest first. Sequence breaks ties so ordering stays stable even when a
	// provider backdates ReceivedAt or many events share a millisecond.
	sort.Slice(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if !a.ev.ReceivedAt.Equal(b.ev.ReceivedAt) {
			return a.ev.ReceivedAt.After(b.ev.ReceivedAt)
		}
		return a.seq > b.seq
	})

	if q.Offset > 0 {
		if q.Offset >= len(matched) {
			return []*event.Event{}, nil
		}
		matched = matched[q.Offset:]
	}
	if q.Limit > 0 && q.Limit < len(matched) {
		matched = matched[:q.Limit]
	}

	out := make([]*event.Event, len(matched))
	for i, ent := range matched {
		out[i] = ent.ev.Clone()
	}
	return out, nil
}

// Get returns one event.
func (s *Store) Get(ctx context.Context, id event.ID) (*event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ent, ok := s.index[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", store.ErrNotFound, id)
	}
	return ent.ev.Clone(), nil
}

// Delete removes one event.
func (s *Store) Delete(ctx context.Context, id event.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ent, ok := s.index[id]
	if !ok {
		return fmt.Errorf("%w: %s", store.ErrNotFound, id)
	}
	delete(s.index, id)
	if r, ok := s.rings[ent.ev.Plugin]; ok {
		r.remove(id)
	}
	return nil
}

// Clear removes every event of a plugin; "" clears everything.
func (s *Store) Clear(ctx context.Context, plugin string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if plugin == "" {
		s.rings = map[string]*ring{}
		s.index = map[event.ID]*entry{}
		return nil
	}
	r, ok := s.rings[plugin]
	if !ok {
		return nil
	}
	for i := 0; i < r.len(); i++ {
		delete(s.index, r.at(i).ev.ID)
	}
	delete(s.rings, plugin)
	return nil
}

// Subscribe returns a channel of newly appended events, closed when ctx is
// done. Slow consumers miss events rather than stalling Append.
func (s *Store) Subscribe(ctx context.Context) <-chan *event.Event {
	sub := &subscriber{ch: make(chan *event.Event, s.subBuf)}

	s.subsMu.Lock()
	s.subs[sub] = struct{}{}
	s.subsMu.Unlock()

	go func() {
		<-ctx.Done()
		s.subsMu.Lock()
		delete(s.subs, sub)
		close(sub.ch)
		s.subsMu.Unlock()
	}()

	return sub.ch
}
