// Package state is the save/load contract a stateful plugin persists through.
//
// A plugin that owns state beyond its captured events (the S3 catalog, the
// Files tree) serialises it into opaque snapshots under keys of its own
// choosing. It never learns where they are kept: the configured storage
// backend decides that, which is what lets one setting make every stateful
// plugin persistent at once.
package state

import (
	"context"
	"errors"
)

// ErrNotFound is returned by Load and Delete for a key that was never saved.
var ErrNotFound = errors.New("state: not found")

// Store keeps snapshots by key. Keys are lowercase letters, digits, '.', '-'
// and '_'. Save replaces a key atomically: after a crash, Load returns either
// the previous snapshot or the new one, never a mixture.
type Store interface {
	Load(ctx context.Context, key string) ([]byte, error)
	Save(ctx context.Context, key string, data []byte) error
	Delete(ctx context.Context, key string) error
}
