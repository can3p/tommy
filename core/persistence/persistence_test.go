package persistence_test

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/blob"
	blobmem "github.com/can3p/tommy/core/blob/memory"
	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/persistence"
)

func TestPrecedence(t *testing.T) {
	tests := []struct {
		name        string
		cfg         func(path string) config.StorageConfig
		plugin      string
		provider    string
		wantBackend config.StorageBackend
	}{
		{
			name: "global memory, plugin filesystem override",
			cfg: func(path string) config.StorageConfig {
				return config.StorageConfig{
					Backend: config.StorageMemory,
					Path:    path,
					Plugins: map[string]config.StorageScopeConfig{
						"s3": {Backend: config.StorageFilesystem},
					},
				}
			},
			plugin:      "s3",
			wantBackend: config.StorageFilesystem,
		},
		{
			name: "global memory, unrelated plugin stays memory",
			cfg: func(path string) config.StorageConfig {
				return config.StorageConfig{
					Backend: config.StorageMemory,
					Path:    path,
					Plugins: map[string]config.StorageScopeConfig{
						"s3": {Backend: config.StorageFilesystem},
					},
				}
			},
			plugin:      "mail",
			wantBackend: config.StorageMemory,
		},
		{
			name: "global filesystem, plugin memory override",
			cfg: func(path string) config.StorageConfig {
				return config.StorageConfig{
					Backend: config.StorageFilesystem,
					Path:    path,
					Plugins: map[string]config.StorageScopeConfig{
						"s3": {Backend: config.StorageMemory},
					},
				}
			},
			plugin:      "s3",
			wantBackend: config.StorageMemory,
		},
		{
			name: "global filesystem, unrelated plugin stays filesystem",
			cfg: func(path string) config.StorageConfig {
				return config.StorageConfig{
					Backend: config.StorageFilesystem,
					Path:    path,
					Plugins: map[string]config.StorageScopeConfig{
						"s3": {Backend: config.StorageMemory},
					},
				}
			},
			plugin:      "mail",
			wantBackend: config.StorageFilesystem,
		},
		{
			name: "provider override beats plugin override",
			cfg: func(path string) config.StorageConfig {
				return config.StorageConfig{
					Backend: config.StorageMemory,
					Path:    path,
					Plugins: map[string]config.StorageScopeConfig{
						"mail": {
							Backend: config.StorageFilesystem,
							Providers: map[string]config.StorageScopeConfig{
								"smtp": {Backend: config.StorageMemory},
							},
						},
					},
				}
			},
			plugin:      "mail",
			provider:    "smtp",
			wantBackend: config.StorageMemory,
		},
		{
			name: "provider with no override inherits the plugin override",
			cfg: func(path string) config.StorageConfig {
				return config.StorageConfig{
					Backend: config.StorageMemory,
					Path:    path,
					Plugins: map[string]config.StorageScopeConfig{
						"mail": {
							Backend: config.StorageFilesystem,
							Providers: map[string]config.StorageScopeConfig{
								"smtp": {Backend: config.StorageMemory},
							},
						},
					},
				}
			},
			plugin:      "mail",
			provider:    "mailjet",
			wantBackend: config.StorageFilesystem,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			m, err := persistence.New(tc.cfg(dir), blobmem.New(0))
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			s, err := m.Scope(tc.plugin, tc.provider)
			if err != nil {
				t.Fatalf("scope: %v", err)
			}
			isMemory := s.State == nil
			wantMemory := tc.wantBackend == config.StorageMemory
			if isMemory != wantMemory {
				t.Errorf("scope(%q, %q): State == nil is %v, want backend %v (memory=%v)",
					tc.plugin, tc.provider, isMemory, tc.wantBackend, wantMemory)
			}
		})
	}
}

func TestMemoryScopeHasNilStateAndTheSharedBlobStore(t *testing.T) {
	mem := blobmem.New(0)
	m, err := persistence.New(config.StorageConfig{Backend: config.StorageMemory}, mem)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	s, err := m.Scope("mail", "")
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	if s.State != nil {
		t.Errorf("memory scope State = %#v, want nil", s.State)
	}
	if s.Blobs != blob.BlobStore(mem) {
		t.Error("memory scope Blobs must be the core in-memory blob store instance")
	}
}

func TestLayoutOnDisk(t *testing.T) {
	dir := t.TempDir()
	cfg := config.StorageConfig{Backend: config.StorageFilesystem, Path: dir}
	m, err := persistence.New(cfg, blobmem.New(0))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()

	pluginScope, err := m.Scope("s3", "")
	if err != nil {
		t.Fatalf("scope s3: %v", err)
	}
	if err := pluginScope.State.Save(ctx, "catalog", []byte("plugin state")); err != nil {
		t.Fatalf("save plugin state: %v", err)
	}
	if _, err := pluginScope.Blobs.Put(ctx, strings.NewReader("plugin blob"), blob.Ref{ID: "obj1"}); err != nil {
		t.Fatalf("put plugin blob: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "plugins", "s3", "state", "catalog")); err != nil {
		t.Errorf("plugin state file missing: %v", err)
	}
	blobName := base64.RawURLEncoding.EncodeToString([]byte("obj1")) + ".blob"
	if _, err := os.Stat(filepath.Join(dir, "plugins", "s3", "blobs", blobName)); err != nil {
		t.Errorf("plugin blob file missing: %v", err)
	}

	providerScope, err := m.Scope("mail", "smtp")
	if err != nil {
		t.Fatalf("scope mail/smtp: %v", err)
	}
	if err := providerScope.State.Save(ctx, "cursor", []byte("provider state")); err != nil {
		t.Fatalf("save provider state: %v", err)
	}
	if _, err := providerScope.Blobs.Put(ctx, strings.NewReader("provider blob"), blob.Ref{ID: "obj2"}); err != nil {
		t.Fatalf("put provider blob: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "plugins", "mail", "providers", "smtp", "state", "cursor")); err != nil {
		t.Errorf("provider state file missing: %v", err)
	}
	blobName2 := base64.RawURLEncoding.EncodeToString([]byte("obj2")) + ".blob"
	if _, err := os.Stat(filepath.Join(dir, "plugins", "mail", "providers", "smtp", "blobs", blobName2)); err != nil {
		t.Errorf("provider blob file missing: %v", err)
	}
}

func TestSameScopeTwiceReturnsIdenticalStorage(t *testing.T) {
	dir := t.TempDir()
	cfg := config.StorageConfig{Backend: config.StorageFilesystem, Path: dir}
	m, err := persistence.New(cfg, blobmem.New(0))
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	for _, sc := range [][2]string{{"s3", ""}, {"mail", "smtp"}} {
		first, err := m.Scope(sc[0], sc[1])
		if err != nil {
			t.Fatalf("scope %v: %v", sc, err)
		}
		second, err := m.Scope(sc[0], sc[1])
		if err != nil {
			t.Fatalf("scope %v (again): %v", sc, err)
		}
		if first.State != second.State {
			t.Errorf("scope %v: State differs across calls", sc)
		}
		if first.Blobs != second.Blobs {
			t.Errorf("scope %v: Blobs differs across calls", sc)
		}
	}
}

func TestChainPutLandsInMemoryOnly(t *testing.T) {
	dir := t.TempDir()
	mem := blobmem.New(0)
	cfg := config.StorageConfig{Backend: config.StorageFilesystem, Path: dir}
	m, err := persistence.New(cfg, mem)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// Register a persistent scope so the chain has somewhere else to check.
	if _, err := m.Scope("s3", ""); err != nil {
		t.Fatalf("scope: %v", err)
	}

	ctx := context.Background()
	ref, err := m.Blobs().Put(ctx, strings.NewReader("event attachment"), blob.Ref{})
	if err != nil {
		t.Fatalf("put via chain: %v", err)
	}
	if _, err := mem.Stat(ctx, ref.ID); err != nil {
		t.Errorf("chain Put must land in the memory store directly: %v", err)
	}
}

func TestChainFindsBlobInPersistentScope(t *testing.T) {
	dir := t.TempDir()
	mem := blobmem.New(0)
	cfg := config.StorageConfig{Backend: config.StorageFilesystem, Path: dir}
	m, err := persistence.New(cfg, mem)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	scope, err := m.Scope("s3", "")
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	ctx := context.Background()
	ref, err := scope.Blobs.Put(ctx, strings.NewReader("bucket object"), blob.Ref{ID: "obj-in-scope"})
	if err != nil {
		t.Fatalf("put directly into the scope: %v", err)
	}

	rc, gotRef, err := m.Blobs().Open(ctx, ref.ID)
	if err != nil {
		t.Fatalf("chain Open must fall through to the persistent scope: %v", err)
	}
	_ = rc.Close()
	if gotRef != ref {
		t.Errorf("ref = %+v, want %+v", gotRef, ref)
	}

	statRef, err := m.Blobs().Stat(ctx, ref.ID)
	if err != nil {
		t.Fatalf("chain Stat must fall through to the persistent scope: %v", err)
	}
	if statRef != ref {
		t.Errorf("stat ref = %+v, want %+v", statRef, ref)
	}
}

func TestChainDeleteDoesNotDeletePersistentBlob(t *testing.T) {
	dir := t.TempDir()
	mem := blobmem.New(0)
	cfg := config.StorageConfig{Backend: config.StorageFilesystem, Path: dir}
	m, err := persistence.New(cfg, mem)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	scope, err := m.Scope("s3", "")
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	ctx := context.Background()
	ref, err := scope.Blobs.Put(ctx, strings.NewReader("bucket object"), blob.Ref{ID: "untouchable"})
	if err != nil {
		t.Fatalf("put directly into the scope: %v", err)
	}

	err = m.Blobs().Delete(ctx, ref.ID)
	if !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("chain Delete of a blob only the persistent scope owns = %v, want ErrNotFound (memory has no such id)", err)
	}

	if _, err := scope.Blobs.Stat(ctx, ref.ID); err != nil {
		t.Errorf("the persistent blob must survive a chain Delete: %v", err)
	}
	if _, err := m.Blobs().Stat(ctx, ref.ID); err != nil {
		t.Errorf("the blob must still be reachable through the chain after the failed delete: %v", err)
	}
}

func TestNewRejectsPathThatIsARegularFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	cfg := config.StorageConfig{Backend: config.StorageFilesystem, Path: filePath}
	if _, err := persistence.New(cfg, blobmem.New(0)); err == nil {
		t.Error("New with a Path pointing at a regular file should error")
	}
}

func TestNewWithNonexistentPathCreatesNothingUntilAWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist-yet")
	cfg := config.StorageConfig{Backend: config.StorageFilesystem, Path: dir}
	m, err := persistence.New(cfg, blobmem.New(0))
	if err != nil {
		t.Fatalf("new with a nonexistent path should succeed: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("New must not create the path itself, stat = %v", err)
	}

	// Even resolving a scope must not create anything until it is written to.
	if _, err := m.Scope("s3", ""); err != nil {
		t.Fatalf("scope: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("resolving a scope must not create its directory before a write, stat = %v", err)
	}
}

func TestUnknownBackendErrors(t *testing.T) {
	cfg := config.StorageConfig{Backend: "nonsense"}
	m, err := persistence.New(cfg, blobmem.New(0))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := m.Scope("mail", ""); err == nil {
		t.Error("Scope with an unknown backend should error")
	}
}
