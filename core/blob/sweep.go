package blob

import (
	"context"
	"errors"
	"fmt"
)

// Lister is an optional interface for a store that can enumerate what it
// holds. A persistent store implements it so the owner of its contents can
// reclaim bytes nothing refers to any more.
type Lister interface {
	List(ctx context.Context) ([]Ref, error)
}

// Sweep deletes every blob in s whose id is not in live, and reports how many
// it deleted. A store that is not a Lister is left alone.
//
// It exists for crash recovery: a persistent owner writes bytes before the
// snapshot that refers to them and deletes replaced bytes only after that
// snapshot is saved, so a process that dies in between leaves bytes behind,
// never a reference to bytes that are gone. Sweeping after a restore reclaims
// them. Only the owner of a store knows its live set, so only the owner may
// sweep it, and only once it has restored.
func Sweep(ctx context.Context, s BlobStore, live map[string]struct{}) (int, error) {
	lister, ok := s.(Lister)
	if !ok {
		return 0, nil
	}
	refs, err := lister.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("blob: list for sweep: %w", err)
	}
	deleted := 0
	for _, ref := range refs {
		if _, keep := live[ref.ID]; keep {
			continue
		}
		if err := s.Delete(ctx, ref.ID); err != nil && !errors.Is(err, ErrNotFound) {
			return deleted, fmt.Errorf("blob: sweep %s: %w", ref.ID, err)
		}
		deleted++
	}
	return deleted, nil
}
