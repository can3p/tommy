package blob_test

import (
	"context"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/blob/filesystem"
	"github.com/can3p/tommy/core/blob/memory"
)

func TestSweepDeletesNonLiveKeepsLive(t *testing.T) {
	s, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()

	keep, err := s.Put(ctx, strings.NewReader("keep me"), blob.Ref{ID: "keep"})
	if err != nil {
		t.Fatalf("put keep: %v", err)
	}
	if _, err := s.Put(ctx, strings.NewReader("orphan one"), blob.Ref{ID: "orphan-1"}); err != nil {
		t.Fatalf("put orphan-1: %v", err)
	}
	if _, err := s.Put(ctx, strings.NewReader("orphan two"), blob.Ref{ID: "orphan-2"}); err != nil {
		t.Fatalf("put orphan-2: %v", err)
	}

	live := map[string]struct{}{keep.ID: {}}
	n, err := blob.Sweep(ctx, s, live)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Errorf("sweep deleted %d, want 2", n)
	}

	if _, err := s.Stat(ctx, keep.ID); err != nil {
		t.Errorf("the live blob must survive the sweep: %v", err)
	}
	for _, id := range []string{"orphan-1", "orphan-2"} {
		if _, err := s.Stat(ctx, id); err == nil {
			t.Errorf("%s should have been swept", id)
		}
	}
}

func TestSweepNoopOnNonListingStore(t *testing.T) {
	// core/blob/memory does not implement blob.Lister, so a live set that
	// would delete everything must be a complete no-op against it.
	s := memory.New(1 << 20)
	ctx := context.Background()

	ref, err := s.Put(ctx, strings.NewReader("payload"), blob.Ref{})
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	n, err := blob.Sweep(ctx, s, map[string]struct{}{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("sweep on a non-Lister store deleted %d, want 0", n)
	}
	if _, err := s.Stat(ctx, ref.ID); err != nil {
		t.Errorf("sweep on a non-Lister store must not touch it: %v", err)
	}
}

func TestSweepEmptyLiveSetDeletesEverything(t *testing.T) {
	s, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if _, err := s.Put(ctx, strings.NewReader(id), blob.Ref{ID: id}); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	n, err := blob.Sweep(ctx, s, map[string]struct{}{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 3 {
		t.Errorf("sweep deleted %d, want 3", n)
	}
	refs, err := s.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("store still has %d blobs after sweeping with an empty live set", len(refs))
	}
}

func TestSweepOnEmptyStoreCountsZero(t *testing.T) {
	s, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	n, err := blob.Sweep(context.Background(), s, map[string]struct{}{"anything": {}})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("sweep on an empty store deleted %d, want 0", n)
	}
}
