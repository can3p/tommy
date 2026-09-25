package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Filesystem keeps each key in a file of its own under one directory. Save
// writes a temporary file, syncs it, renames it over the key and syncs the
// directory, so a snapshot is either wholly replaced or not at all.
type Filesystem struct{ dir string }

// NewFilesystem returns a store rooted at dir. The directory is created on the
// first Save, so a scope that never saves leaves nothing on disk.
func NewFilesystem(dir string) *Filesystem {
	return &Filesystem{dir: dir}
}

// validKey keeps keys to plain file names. A leading dot is refused so a key
// can never name a temporary file or a directory entry like "..".
func validKey(key string) bool {
	if key == "" || key[0] == '.' {
		return false
	}
	for _, r := range key {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && !strings.ContainsRune("-._", r) {
			return false
		}
	}
	return true
}

func (s *Filesystem) path(key string) (string, error) {
	if !validKey(key) {
		return "", fmt.Errorf("state: invalid key %q", key)
	}
	return filepath.Join(s.dir, key), nil
}

func (s *Filesystem) Load(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.path(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("state: load %s: %w", key, err)
	}
	return data, nil
}

func (s *Filesystem) Save(ctx context.Context, key string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("state: create %s: %w", s.dir, err)
	}
	f, err := os.CreateTemp(s.dir, ".state-*")
	if err != nil {
		return fmt.Errorf("state: create temporary file: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("state: write %s: %w", key, err)
	}
	if err := ctx.Err(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("state: sync %s: %w", key, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("state: close %s: %w", key, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("state: install %s: %w", key, err)
	}
	return syncDir(s.dir)
}

func (s *Filesystem) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	} else if err != nil {
		return fmt.Errorf("state: delete %s: %w", key, err)
	}
	return syncDir(s.dir)
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("state: sync %s: %w", path, err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("state: sync %s: %w", path, err)
	}
	return nil
}
