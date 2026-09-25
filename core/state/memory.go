package state

import (
	"context"
	"fmt"
	"sync"
)

// Memory is a Store that lives only as long as the process. The server never
// hands it to a plugin - a memory scope gets no State at all - so it exists for
// tests that exercise a plugin's save and restore without touching disk.
type Memory struct {
	mu    sync.RWMutex
	items map[string][]byte
}

// NewMemory returns an empty Memory store.
func NewMemory() *Memory { return &Memory{items: map[string][]byte{}} }

func (s *Memory) Load(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	data, ok := s.items[key]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return append([]byte(nil), data...), nil
}

func (s *Memory) Save(ctx context.Context, key string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validKey(key) {
		return fmt.Errorf("state: invalid key %q", key)
	}
	s.mu.Lock()
	s.items[key] = append([]byte(nil), data...)
	s.mu.Unlock()
	return nil
}

func (s *Memory) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[key]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	delete(s.items, key)
	return nil
}
