// Package persistence resolves the configured storage backend for each
// stateful plugin and provider, and hands each one its own storage.
//
// Two kinds of data are kept apart. Captured events and the bytes they carry
// always live in memory - the event store and the core blob store - whatever
// is configured. State a plugin owns beyond its events (the S3 catalog, the
// Files tree) goes where [storage] says: nowhere beyond the process for
// memory, or a directory of its own under storage.path for filesystem.
//
// Keeping each persistent scope in its own directory is what makes cleanup
// safe. A scope's owner is the only thing that knows which of its bytes are
// still referenced, so it sweeps its own directory after restoring and can
// never touch a plugin that is not running this time.
package persistence

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/can3p/tommy/core/blob"
	blobfs "github.com/can3p/tommy/core/blob/filesystem"
	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/state"
)

// Manager hands out storage per scope and fronts every persistent blob store
// that has been handed out, for reads through the core blob routes.
type Manager struct {
	cfg    config.StorageConfig
	memory blob.BlobStore
	root   *chain

	mu     sync.Mutex
	scopes map[string]plugin.Storage
}

// New returns a manager for cfg, whose Path must already be resolved against
// the config file's directory. memory is the core in-memory blob store.
//
// A configured path that exists but is not a directory is refused here, so a
// typo is a startup complaint; nothing is created until a scope first writes.
func New(cfg config.StorageConfig, memory blob.BlobStore) (*Manager, error) {
	if cfg.UsesFilesystem() {
		info, err := os.Stat(cfg.Path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return nil, fmt.Errorf("storage: path %s: %w", cfg.Path, err)
		case !info.IsDir():
			return nil, fmt.Errorf("storage: path %s is not a directory", cfg.Path)
		}
	}
	return &Manager{
		cfg:    cfg,
		memory: memory,
		root:   &chain{memory: memory},
		scopes: map[string]plugin.Storage{},
	}, nil
}

// Blobs is the store the core serves and captured events write to. Writes and
// deletes go to memory; reads fall through to every persistent scope, so an
// event can link to bytes a persistent plugin holds, as S3 events do.
func (m *Manager) Blobs() blob.BlobStore { return m.root }

// Scope returns the storage for a plugin, or for one of its providers when
// provider is non-empty, resolved provider override > plugin override >
// global default. Asking twice for one scope returns the same storage.
func (m *Manager) Scope(pluginName, provider string) (plugin.Storage, error) {
	backend := m.cfg.BackendFor(pluginName, provider)
	if backend == config.StorageMemory {
		return plugin.Storage{Blobs: m.memory}, nil
	}
	if backend != config.StorageFilesystem {
		return plugin.Storage{}, fmt.Errorf("storage: unknown backend %q", backend)
	}

	dir := filepath.Join(m.cfg.Path, "plugins", pluginName)
	if provider != "" {
		dir = filepath.Join(dir, "providers", provider)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.scopes[dir]; ok {
		return s, nil
	}
	blobs, err := blobfs.New(filepath.Join(dir, "blobs"))
	if err != nil {
		return plugin.Storage{}, err
	}
	s := plugin.Storage{State: state.NewFilesystem(filepath.Join(dir, "state")), Blobs: blobs}
	m.scopes[dir] = s
	m.root.add(blobs)
	return s, nil
}

// chain writes to memory and reads from memory first, then from each
// persistent store. It never deletes from a persistent store: those bytes
// belong to the plugin that wrote them, which deletes them itself.
type chain struct {
	memory blob.BlobStore

	mu         sync.RWMutex
	persistent []blob.BlobStore
}

func (c *chain) add(s blob.BlobStore) {
	c.mu.Lock()
	c.persistent = append(c.persistent, s)
	c.mu.Unlock()
}

func (c *chain) stores() []blob.BlobStore {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]blob.BlobStore{c.memory}, c.persistent...)
}

func (c *chain) Put(ctx context.Context, r io.Reader, meta blob.Ref) (blob.Ref, error) {
	return c.memory.Put(ctx, r, meta)
}

func (c *chain) Delete(ctx context.Context, id string) error {
	return c.memory.Delete(ctx, id)
}

func (c *chain) Open(ctx context.Context, id string) (io.ReadSeekCloser, blob.Ref, error) {
	for _, s := range c.stores() {
		r, ref, err := s.Open(ctx, id)
		if errors.Is(err, blob.ErrNotFound) {
			continue
		}
		return r, ref, err
	}
	return nil, blob.Ref{}, fmt.Errorf("%w: %s", blob.ErrNotFound, id)
}

func (c *chain) Stat(ctx context.Context, id string) (blob.Ref, error) {
	for _, s := range c.stores() {
		ref, err := s.Stat(ctx, id)
		if errors.Is(err, blob.ErrNotFound) {
			continue
		}
		return ref, err
	}
	return blob.Ref{}, fmt.Errorf("%w: %s", blob.ErrNotFound, id)
}
