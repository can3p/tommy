package memory_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/blob/memory"
)

func TestPutOpenStatDelete(t *testing.T) {
	s := memory.New(1 << 20)
	ctx := context.Background()

	ref, err := s.Put(ctx, strings.NewReader("hello world"), blob.Ref{
		ContentType: "text/plain",
		Filename:    "greeting.txt",
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if ref.ID == "" {
		t.Error("Put must generate an id when the caller does not supply one")
	}
	if ref.Size != 11 {
		t.Errorf("Size = %d, want the real byte count 11", ref.Size)
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

	// ReadSeekCloser, so the API can serve range requests.
	if _, err := rc.Seek(6, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	rest, _ := io.ReadAll(rc)
	if string(rest) != "world" {
		t.Errorf("after seek read %q, want %q", rest, "world")
	}

	stat, err := s.Stat(ctx, ref.ID)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stat != ref {
		t.Errorf("Stat = %+v, want %+v", stat, ref)
	}

	if err := s.Delete(ctx, ref.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if s.Used() != 0 {
		t.Errorf("Used = %d after deleting the only blob", s.Used())
	}
	if _, _, err := s.Open(ctx, ref.ID); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Open after delete = %v, want ErrNotFound", err)
	}
	if _, err := s.Stat(ctx, ref.ID); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Stat after delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, ref.ID); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Delete twice = %v, want ErrNotFound", err)
	}
}

func TestPutHonoursCallerID(t *testing.T) {
	s := memory.New(1 << 20)
	ref, err := s.Put(context.Background(), strings.NewReader("x"), blob.Ref{ID: "chosen"})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if ref.ID != "chosen" {
		t.Errorf("ID = %q, want the caller's id", ref.ID)
	}
}

func TestOverwriteReleasesTheOldBytes(t *testing.T) {
	s := memory.New(100)
	ctx := context.Background()

	if _, err := s.Put(ctx, bytes.NewReader(make([]byte, 60)), blob.Ref{ID: "same"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := s.Put(ctx, bytes.NewReader(make([]byte, 60)), blob.Ref{ID: "same"}); err != nil {
		t.Fatalf("overwriting an id must reuse its space, got %v", err)
	}
	if s.Used() != 60 {
		t.Errorf("Used = %d, want 60", s.Used())
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1", s.Len())
	}
}

func TestSizeCap(t *testing.T) {
	s := memory.New(100)
	ctx := context.Background()

	full, err := s.Put(ctx, bytes.NewReader(make([]byte, 100)), blob.Ref{})
	if err != nil {
		t.Fatalf("a blob exactly at the limit must fit: %v", err)
	}
	_, err = s.Put(ctx, bytes.NewReader([]byte{1}), blob.Ref{})
	if !errors.Is(err, blob.ErrCapacityExceeded) {
		t.Fatalf("over the cap = %v, want ErrCapacityExceeded", err)
	}
	if s.Used() != 100 || s.Len() != 1 {
		t.Errorf("a rejected Put must not change the store: used=%d len=%d", s.Used(), s.Len())
	}

	// Freeing space makes room again; blobs are never evicted on our behalf,
	// because the event ring buffer and the blob store have separate lifetimes.
	if err := s.Delete(ctx, full.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Put(ctx, bytes.NewReader(make([]byte, 50)), blob.Ref{}); err != nil {
		t.Errorf("after freeing space: %v", err)
	}
}

func TestPutDoesNotBufferPastTheCap(t *testing.T) {
	s := memory.New(1024)
	// An endless reader: if Put read everything before checking the cap, this
	// would never return.
	counted := &countingReader{}
	_, err := s.Put(context.Background(), counted, blob.Ref{})
	if !errors.Is(err, blob.ErrCapacityExceeded) {
		t.Fatalf("err = %v, want ErrCapacityExceeded", err)
	}
	if got := counted.n.Load(); got > 2048 {
		t.Errorf("read %d bytes from an oversized body; it must stop just past the cap", got)
	}
}

func TestPutReadError(t *testing.T) {
	s := memory.New(1024)
	_, err := s.Put(context.Background(), failingReader{}, blob.Ref{})
	if err == nil || errors.Is(err, blob.ErrCapacityExceeded) {
		t.Errorf("err = %v, want the underlying read error", err)
	}
	if s.Len() != 0 {
		t.Error("a failed Put must not store anything")
	}
}

func TestConcurrentPutsRespectTheCap(t *testing.T) {
	const limit = 10 * 1024
	s := memory.New(limit)

	var wg sync.WaitGroup
	var stored atomic.Int64
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ref, err := s.Put(context.Background(), bytes.NewReader(make([]byte, 1024)), blob.Ref{})
			if err == nil {
				stored.Add(ref.Size)
			} else if !errors.Is(err, blob.ErrCapacityExceeded) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if s.Used() > limit {
		t.Errorf("Used = %d, over the %d cap: concurrent Puts must re-check under the lock", s.Used(), limit)
	}
	if s.Used() != stored.Load() {
		t.Errorf("Used = %d but successful Puts stored %d", s.Used(), stored.Load())
	}
}

func TestContextCancellation(t *testing.T) {
	s := memory.New(1024)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.Put(ctx, strings.NewReader("x"), blob.Ref{}); err == nil {
		t.Error("Put with a canceled context should fail")
	}
	if _, _, err := s.Open(ctx, "x"); err == nil {
		t.Error("Open with a canceled context should fail")
	}
}

func TestInterfaceCompliance(t *testing.T) {
	var _ blob.BlobStore = memory.New(1)
}

type countingReader struct{ n atomic.Int64 }

func (r *countingReader) Read(p []byte) (int, error) {
	r.n.Add(int64(len(p)))
	return len(p), nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("boom") }
