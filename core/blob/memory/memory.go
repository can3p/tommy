// Package memory implements a size-capped in-memory blob.BlobStore.
package memory

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
)

// DefaultLimit is the total number of bytes held when no limit is configured.
const DefaultLimit int64 = 256 << 20 // 256MB

type entry struct {
	ref  blob.Ref
	data []byte
}

// Store is an in-memory blob store with a hard total-size cap.
//
// It never evicts: Put fails with blob.ErrCapacityExceeded once the cap is
// reached. Silently dropping a blob would break the download link of a message
// that is still listed in the UI, which is worse than a loud error.
type Store struct {
	mu    sync.RWMutex
	items map[string]*entry
	used  int64
	limit int64
	newID func() string
}

var _ blob.BlobStore = (*Store)(nil)

// Option configures a Store.
type Option func(*Store)

// WithIDFunc overrides the blob id generator (deterministic ids in tests).
func WithIDFunc(f func() string) Option {
	return func(s *Store) { s.newID = f }
}

// New returns a store capped at limit bytes. limit <= 0 means DefaultLimit.
func New(limit int64, opts ...Option) *Store {
	if limit <= 0 {
		limit = DefaultLimit
	}
	s := &Store{
		items: map[string]*entry{},
		limit: limit,
		newID: event.NewID,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Limit reports the configured cap in bytes.
func (s *Store) Limit() int64 { return s.limit }

// Used reports how many bytes are currently held.
func (s *Store) Used() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.used
}

// Len reports how many blobs are currently held.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}

// Put stores r. It reads at most limit-used+1 bytes so a hostile or buggy
// producer cannot blow up memory before the cap is checked.
func (s *Store) Put(ctx context.Context, r io.Reader, meta blob.Ref) (blob.Ref, error) {
	if err := ctx.Err(); err != nil {
		return blob.Ref{}, err
	}

	s.mu.RLock()
	headroom := s.limit - s.used
	s.mu.RUnlock()
	if headroom <= 0 {
		return blob.Ref{}, fmt.Errorf("%w: %d bytes in use of %d", blob.ErrCapacityExceeded, s.used, s.limit)
	}

	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, headroom+1))
	if err != nil {
		return blob.Ref{}, fmt.Errorf("blob: read: %w", err)
	}
	if n > headroom {
		return blob.Ref{}, fmt.Errorf("%w: %d bytes free of %d", blob.ErrCapacityExceeded, headroom, s.limit)
	}

	ref := meta
	ref.Size = n
	if ref.ID == "" {
		ref.ID = s.newID()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-check under the write lock: concurrent Puts each saw the same headroom.
	if prev, ok := s.items[ref.ID]; ok {
		s.used -= prev.ref.Size
	}
	if s.used+n > s.limit {
		if prev, ok := s.items[ref.ID]; ok {
			s.used += prev.ref.Size
		}
		return blob.Ref{}, fmt.Errorf("%w: %d bytes in use of %d", blob.ErrCapacityExceeded, s.used, s.limit)
	}
	s.items[ref.ID] = &entry{ref: ref, data: buf.Bytes()}
	s.used += n
	return ref, nil
}

// Open returns a reader over the stored bytes.
func (s *Store) Open(ctx context.Context, id string) (io.ReadSeekCloser, blob.Ref, error) {
	if err := ctx.Err(); err != nil {
		return nil, blob.Ref{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.items[id]
	if !ok {
		return nil, blob.Ref{}, fmt.Errorf("%w: %s", blob.ErrNotFound, id)
	}
	return nopCloser{bytes.NewReader(e.data)}, e.ref, nil
}

// Stat returns the Ref of a stored blob.
func (s *Store) Stat(ctx context.Context, id string) (blob.Ref, error) {
	if err := ctx.Err(); err != nil {
		return blob.Ref{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.items[id]
	if !ok {
		return blob.Ref{}, fmt.Errorf("%w: %s", blob.ErrNotFound, id)
	}
	return e.ref, nil
}

// Delete removes a blob and frees its bytes.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.items[id]
	if !ok {
		return fmt.Errorf("%w: %s", blob.ErrNotFound, id)
	}
	delete(s.items, id)
	s.used -= e.ref.Size
	return nil
}

// Reset drops every blob. Test helper; not part of blob.BlobStore, because
// clearing the event log must never clear blobs (see package doc).
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = map[string]*entry{}
	s.used = 0
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }
