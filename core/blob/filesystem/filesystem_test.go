package filesystem_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/blob/filesystem"
)

func encodedPath(dir, id string) string {
	return filepath.Join(dir, base64.RawURLEncoding.EncodeToString([]byte(id))+".blob")
}

func TestPutOpenStatDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := filesystem.New(dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()

	ref, err := s.Put(ctx, strings.NewReader("hello world"), blob.Ref{ContentType: "text/plain", Filename: "greeting.txt"})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if ref.ID == "" {
		t.Error("Put must generate an id when the caller does not supply one")
	}
	if ref.Size != 11 {
		t.Errorf("Size = %d, want 11", ref.Size)
	}

	rc, got, err := s.Open(ctx, ref.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("read %q", data)
	}
	if got.ContentType != "text/plain" || got.Filename != "greeting.txt" {
		t.Errorf("metadata lost: %+v", got)
	}

	stat, err := s.Stat(ctx, ref.ID)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stat != ref {
		t.Errorf("stat = %+v, want %+v", stat, ref)
	}

	if err := s.Delete(ctx, ref.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := s.Open(ctx, ref.ID); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("open after delete = %v, want ErrNotFound", err)
	}
	if _, err := s.Stat(ctx, ref.ID); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("stat after delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, ref.ID); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("delete twice = %v, want ErrNotFound", err)
	}
}

func TestPutHonoursCallerID(t *testing.T) {
	s, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ref, err := s.Put(context.Background(), strings.NewReader("x"), blob.Ref{ID: "chosen"})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if ref.ID != "chosen" {
		t.Errorf("id = %q, want %q", ref.ID, "chosen")
	}
}

func TestPutReplacesExplicitID(t *testing.T) {
	s, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()

	if _, err := s.Put(ctx, strings.NewReader("v1"), blob.Ref{ID: "same"}); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if _, err := s.Put(ctx, strings.NewReader("v2-longer"), blob.Ref{ID: "same"}); err != nil {
		t.Fatalf("second put: %v", err)
	}
	rc, ref, err := s.Open(ctx, "same")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	data, _ := io.ReadAll(rc)
	if string(data) != "v2-longer" {
		t.Errorf("data = %q, want the replaced payload", data)
	}
	if ref.Size != int64(len("v2-longer")) {
		t.Errorf("size = %d, want %d", ref.Size, len("v2-longer"))
	}
}

func TestReopenSeesSavedBlobs(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	first, err := filesystem.New(dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ref, err := first.Put(ctx, strings.NewReader("persisted"), blob.Ref{ID: "keepsake"})
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	second, err := filesystem.New(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	rc, gotRef, err := second.Open(ctx, ref.ID)
	if err != nil {
		t.Fatalf("open on reopened store: %v", err)
	}
	defer func() { _ = rc.Close() }()
	data, _ := io.ReadAll(rc)
	if string(data) != "persisted" {
		t.Errorf("data = %q, want %q", data, "persisted")
	}
	if gotRef != ref {
		t.Errorf("ref = %+v, want %+v", gotRef, ref)
	}
}

func TestSeekAndRangeReads(t *testing.T) {
	s, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()
	payload := "0123456789ABCDEFGHIJ"
	ref, err := s.Put(ctx, strings.NewReader(payload), blob.Ref{})
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	rc, _, err := s.Open(ctx, ref.ID)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = rc.Close() }()

	if _, err := rc.Seek(10, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	rest, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read after seek: %v", err)
	}
	if string(rest) != "ABCDEFGHIJ" {
		t.Errorf("read after seek = %q, want %q", rest, "ABCDEFGHIJ")
	}

	// A ReaderAt-style range read via Seek+Read from the start again.
	if _, err := rc.Seek(2, io.SeekStart); err != nil {
		t.Fatalf("seek 2: %v", err)
	}
	buf := make([]byte, 5)
	n, err := io.ReadFull(rc, buf)
	if err != nil {
		t.Fatalf("range read: %v", err)
	}
	if n != 5 || string(buf) != "23456" {
		t.Errorf("range read = %q, want %q", buf[:n], "23456")
	}
}

func TestHostileIDsStayInsideTheDirectory(t *testing.T) {
	dir := t.TempDir()
	s, err := filesystem.New(dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()

	parent := filepath.Dir(dir)
	before, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read parent before: %v", err)
	}

	ids := []string{
		"../../etc/passwd",
		"../../../../../../etc/passwd",
		"a/b",
		"..",
		".",
		"/etc/passwd",
		"a\x00b",
		"héllo-\u200bunicode", // zero-width space, an "empty-looking" unicode id
	}
	for _, id := range ids {
		ref, err := s.Put(ctx, strings.NewReader("payload-"+id), blob.Ref{ID: id})
		if err != nil {
			t.Fatalf("put %q: %v", id, err)
		}
		if ref.ID != id {
			t.Errorf("put %q: ref id = %q", id, ref.ID)
		}
		rc, _, err := s.Open(ctx, id)
		if err != nil {
			t.Fatalf("open %q: %v", id, err)
		}
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(data) != "payload-"+id {
			t.Errorf("open %q: data = %q", id, data)
		}
		if err := s.Delete(ctx, id); err != nil {
			t.Fatalf("delete %q: %v", id, err)
		}
	}

	after, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read parent after: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("hostile ids changed the parent directory's contents: before=%d after=%d", len(before), len(after))
	}
}

func TestStaleTempFilesRemovedByNew(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".tmp-leftover"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("seed stale temp file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real.blob"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("seed unrelated file: %v", err)
	}

	if _, err := filesystem.New(dir); err != nil {
		t.Fatalf("new: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".tmp-leftover")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale temp file should be removed by New, stat = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "real.blob")); err != nil {
		t.Errorf("New must not touch unrelated files: %v", err)
	}
}

func TestOpenCorruptOrTruncatedFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := filesystem.New(dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()

	tests := []struct {
		name string
		id   string
		data []byte
	}{
		{"empty file", "empty", []byte{}},
		{"too short for trailer", "short", []byte("hi")},
		{"wrong magic", "wrongmagic", append([]byte("payload"), append(make([]byte, 4), []byte("XXXXXXXX")...)...)},
		{"garbage bytes", "garbage", []byte("this is not a blob file at all, way too long for the trailer to parse as anything sane")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := encodedPath(dir, tc.id)
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatalf("write corrupt file: %v", err)
			}
			if _, _, err := s.Open(ctx, tc.id); err == nil {
				t.Errorf("open over corrupt file %q succeeded, want an error", tc.id)
			}
		})
	}
}

func TestListReturnsAllRefs(t *testing.T) {
	dir := t.TempDir()
	s, err := filesystem.New(dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()

	want := map[string]bool{}
	for _, id := range []string{"one", "two", "three"} {
		ref, err := s.Put(ctx, strings.NewReader("data-"+id), blob.Ref{ID: id})
		if err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
		want[ref.ID] = true
	}

	refs, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(refs) != len(want) {
		t.Fatalf("list returned %d refs, want %d", len(refs), len(want))
	}
	for _, ref := range refs {
		if !want[ref.ID] {
			t.Errorf("list returned unexpected id %q", ref.ID)
		}
		delete(want, ref.ID)
	}
	if len(want) != 0 {
		t.Errorf("list is missing ids: %v", want)
	}
}

func TestListErrorsOnGarbageBlobFile(t *testing.T) {
	dir := t.TempDir()
	s, err := filesystem.New(dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// A filename that is not valid base64 (RawURLEncoding never emits "!"),
	// so List's id-decode step must fail on it rather than skip it silently.
	if err := os.WriteFile(filepath.Join(dir, "not-base64!!.blob"), []byte("not a blob"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}
	if _, err := s.List(context.Background()); err == nil {
		t.Error("List over a garbage .blob file should error, not silently skip it")
	}
}

func TestListIgnoresTempFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := filesystem.New(dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()
	if _, err := s.Put(ctx, strings.NewReader("x"), blob.Ref{ID: "kept"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Simulate a temp file left mid-write by another process, without going
	// through New (which sweeps them at startup).
	if err := os.WriteFile(filepath.Join(dir, ".tmp-inflight"), []byte("half-written"), 0o600); err != nil {
		t.Fatalf("seed temp file: %v", err)
	}

	refs, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(refs) != 1 || refs[0].ID != "kept" {
		t.Errorf("list = %+v, want only the one real blob", refs)
	}
}

func TestConcurrentPutNeverYieldsAMixedRead(t *testing.T) {
	dir := t.TempDir()
	s, err := filesystem.New(dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()

	payloadA := strings.Repeat("A", 4096)
	payloadB := strings.Repeat("B", 8192)
	if _, err := s.Put(ctx, strings.NewReader(payloadA), blob.Ref{ID: "hot"}); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var mismatches int
	var mu sync.Mutex

	wg.Add(1)
	go func() {
		defer wg.Done()
		toggle := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			p := payloadA
			if toggle {
				p = payloadB
			}
			toggle = !toggle
			if _, err := s.Put(ctx, strings.NewReader(p), blob.Ref{ID: "hot"}); err != nil {
				mu.Lock()
				mismatches++
				mu.Unlock()
			}
		}
	}()

	for range 200 {
		rc, _, err := s.Open(ctx, "hot")
		if err != nil {
			continue // a rename may transiently race a fresh Open; not itself a bug we assert on
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Errorf("read: %v", err)
			continue
		}
		if string(data) != payloadA && string(data) != payloadB {
			mu.Lock()
			mismatches++
			mu.Unlock()
		}
	}
	close(stop)
	wg.Wait()

	if mismatches != 0 {
		t.Errorf("%d reads did not equal one full payload: a concurrent Put mixed old and new bytes", mismatches)
	}
}

func TestPutWithCanceledContextLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	s, err := filesystem.New(dir)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.Put(ctx, strings.NewReader("x"), blob.Ref{ID: "canceled"}); err == nil {
		t.Fatal("Put with a canceled context should fail")
	}

	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory has leftover entries after a canceled Put: %v", names)
	}
}

func TestInterfaceCompliance(t *testing.T) {
	s, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	var _ blob.BlobStore = s
	var _ blob.Lister = s
}
