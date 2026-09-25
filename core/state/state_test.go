package state_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/state"
)

// storeFactory returns a fresh, empty Store for one subtest.
type storeFactory func(t *testing.T) state.Store

// TestStoreContract runs the same behavioral contract against every Store
// implementation, so Memory and Filesystem can never quietly disagree about
// what Load, Save and Delete promise.
func TestStoreContract(t *testing.T) {
	factories := map[string]storeFactory{
		"Memory": func(t *testing.T) state.Store {
			return state.NewMemory()
		},
		"Filesystem": func(t *testing.T) state.Store {
			return state.NewFilesystem(t.TempDir())
		},
	}

	for name, newStore := range factories {
		t.Run(name, func(t *testing.T) {
			t.Run("save load overwrite delete", func(t *testing.T) {
				s := newStore(t)
				ctx := context.Background()

				if err := s.Save(ctx, "widget", []byte("v1")); err != nil {
					t.Fatalf("save: %v", err)
				}
				got, err := s.Load(ctx, "widget")
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				if string(got) != "v1" {
					t.Errorf("load = %q, want %q", got, "v1")
				}

				if err := s.Save(ctx, "widget", []byte("v2")); err != nil {
					t.Fatalf("overwrite save: %v", err)
				}
				got, err = s.Load(ctx, "widget")
				if err != nil {
					t.Fatalf("load after overwrite: %v", err)
				}
				if string(got) != "v2" {
					t.Errorf("load after overwrite = %q, want %q", got, "v2")
				}

				if err := s.Delete(ctx, "widget"); err != nil {
					t.Fatalf("delete: %v", err)
				}
				if _, err := s.Load(ctx, "widget"); !errors.Is(err, state.ErrNotFound) {
					t.Errorf("load after delete = %v, want ErrNotFound", err)
				}
			})

			t.Run("load missing key", func(t *testing.T) {
				s := newStore(t)
				if _, err := s.Load(context.Background(), "nope"); !errors.Is(err, state.ErrNotFound) {
					t.Errorf("load missing = %v, want ErrNotFound", err)
				}
			})

			t.Run("delete missing key", func(t *testing.T) {
				s := newStore(t)
				if err := s.Delete(context.Background(), "nope"); !errors.Is(err, state.ErrNotFound) {
					t.Errorf("delete missing = %v, want ErrNotFound", err)
				}
			})

			t.Run("invalid keys rejected", func(t *testing.T) {
				s := newStore(t)
				ctx := context.Background()
				for _, key := range []string{"", ".x", "../x", "A", "a/b", "a\\b"} {
					if err := s.Save(ctx, key, []byte("x")); err == nil {
						t.Errorf("Save(%q) succeeded, want an error rejecting the key", key)
					}
				}
			})

			t.Run("canceled context", func(t *testing.T) {
				s := newStore(t)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				if err := s.Save(ctx, "widget", []byte("x")); err == nil {
					t.Error("Save with a canceled context should fail")
				}
				if _, err := s.Load(ctx, "widget"); err == nil {
					t.Error("Load with a canceled context should fail")
				}
				if err := s.Delete(ctx, "widget"); err == nil {
					t.Error("Delete with a canceled context should fail")
				}
			})
		})
	}
}

func TestFilesystemDirectoryCreatedLazily(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scope")
	s := state.NewFilesystem(dir)

	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory must not exist before NewFilesystem does anything, stat = %v", err)
	}

	// A Load or Delete against a store that never saved must not create the
	// directory either.
	if _, err := s.Load(context.Background(), "key"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("load on empty scope = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load must not create the directory, stat = %v", err)
	}

	if err := s.Save(context.Background(), "key", []byte("data")); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("directory must exist after the first Save: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
}

func TestFilesystemReopenSeesSavedData(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	first := state.NewFilesystem(dir)
	if err := first.Save(ctx, "catalog", []byte("persisted")); err != nil {
		t.Fatalf("save: %v", err)
	}

	second := state.NewFilesystem(dir)
	got, err := second.Load(ctx, "catalog")
	if err != nil {
		t.Fatalf("load from a fresh Filesystem over the same dir: %v", err)
	}
	if string(got) != "persisted" {
		t.Errorf("load = %q, want %q", got, "persisted")
	}
}

func TestFilesystemFileMode(t *testing.T) {
	dir := t.TempDir()
	s := state.NewFilesystem(dir)
	if err := s.Save(context.Background(), "secret", []byte("x")); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "secret"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}
}

func TestFilesystemNoTempFilesLeftBehind(t *testing.T) {
	dir := t.TempDir()
	s := state.NewFilesystem(dir)
	ctx := context.Background()

	if err := s.Save(ctx, "a", []byte("1")); err != nil {
		t.Fatalf("save a: %v", err)
	}
	if err := s.Save(ctx, "b", []byte("2")); err != nil {
		t.Fatalf("save b: %v", err)
	}
	if err := s.Save(ctx, "a", []byte("1-updated")); err != nil {
		t.Fatalf("overwrite a: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".state-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
	if len(entries) != 2 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory has %d entries %v, want exactly [a b]", len(entries), names)
	}
}
