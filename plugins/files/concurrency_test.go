package files_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/plugins/files"
)

// The VFS is the one genuinely concurrency-sensitive component in tommy: two
// providers write to one tree from their own goroutines, and the UI reads it
// from an HTTP handler at the same time. These tests are written to be run
// under -race, which is where they earn their keep.

// Two providers uploading into the same directory at once must both land, with
// no lost entry and no torn listing.
func TestConcurrentUploadsFromTwoProviders(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)
	ctx := context.Background()
	if _, err := v.MkdirAll("/drop", files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	const perProvider = 60
	var wg sync.WaitGroup
	for _, provider := range []string{"ftp", "sftp"} {
		for i := range perProvider {
			wg.Add(1)
			go func(provider string, i int) {
				defer wg.Done()
				name := fmt.Sprintf("/drop/%s-%03d.txt", provider, i)
				if _, err := v.PutBytes(ctx, name, []byte(provider), files.WriteOptions{Provider: provider}); err != nil {
					t.Errorf("put %s: %v", name, err)
				}
			}(provider, i)
		}
	}
	wg.Wait()

	entries, err := v.List("/drop")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2*perProvider {
		t.Fatalf("listed %d files, want %d", len(entries), 2*perProvider)
	}
	for _, e := range entries {
		if e.Provider != strings.SplitN(e.Name, "-", 2)[0] {
			t.Errorf("%s was recorded as written by %q", e.Name, e.Provider)
		}
		if e.Size != int64(len(e.Provider)) {
			t.Errorf("%s has size %d", e.Name, e.Size)
		}
	}
}

// Uploads, listings, stats and deletes all running at once. The assertion is
// that nothing panics, nothing deadlocks and the race detector stays quiet;
// which file wins a race is deliberately not asserted, because that is a real
// race between two clients and the answer is "whoever finished last".
func TestConcurrentMixedOperations(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)
	ctx := context.Background()

	const workers = 8
	const rounds = 40
	var wg sync.WaitGroup

	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range rounds {
				dir := fmt.Sprintf("/w%d", w%3)
				if _, err := v.MkdirAll(dir, files.WriteOptions{Provider: "ftp"}); err != nil {
					t.Errorf("mkdirall: %v", err)
					return
				}
				name := fmt.Sprintf("%s/f%d.txt", dir, i%7)
				if _, err := v.PutBytes(ctx, name, []byte(strings.Repeat("x", i)), files.WriteOptions{Provider: "sftp"}); err != nil {
					t.Errorf("put: %v", err)
					return
				}
				if _, err := v.List(dir); err != nil {
					t.Errorf("list: %v", err)
					return
				}
				_ = v.Stats()
				if _, err := v.Remove(ctx, name); err != nil && !errors.Is(err, files.ErrNotExist) {
					t.Errorf("remove: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

// Overwriting one path from many goroutines: exactly one version survives, and
// its bytes are one of the ones actually written - never a mixture, and never a
// blob that has already been freed.
func TestConcurrentOverwritesOfOnePath(t *testing.T) {
	t.Parallel()
	v, blobs := newVFS(t)
	ctx := context.Background()

	const writers = 32
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := strings.Repeat(string(rune('a'+i%26)), 100)
			if _, err := v.PutBytes(ctx, "/contended.txt", []byte(body), files.WriteOptions{Provider: "ftp"}); err != nil {
				t.Errorf("put: %v", err)
			}
		}(i)
	}
	wg.Wait()

	body, err := v.ReadFile(ctx, "/contended.txt")
	if err != nil {
		t.Fatalf("read the survivor: %v", err)
	}
	if len(body) != 100 || strings.Count(string(body), string(body[0])) != 100 {
		t.Fatalf("content is a mixture of writers: %q", body)
	}
	// Every loser's bytes are freed, so a contended path costs one blob.
	if blobs.Len() != 1 {
		t.Errorf("blob store holds %d blobs after %d overwrites, want 1", blobs.Len(), writers)
	}
	if got := v.Stats(); got.Files != 1 {
		t.Errorf("Stats() = %+v, want one file", got)
	}
}

// A reader that is already streaming a file keeps working while the same path
// is overwritten and deleted underneath it: the blob store hands out a
// snapshot, so an in-flight download is never torn.
func TestOpenHandleSurvivesOverwriteAndDelete(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)
	ctx := context.Background()
	put(t, v, "/live.txt", "original")

	f, err := v.Open(ctx, "/live.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	put(t, v, "/live.txt", "replaced")
	if _, err := v.Remove(ctx, "/live.txt"); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 8)
	if _, err := f.ReadAt(buf, 0); err != nil {
		t.Fatalf("read from an open handle: %v", err)
	}
	if string(buf) != "original" {
		t.Errorf("open handle read %q, want the bytes it was opened on", buf)
	}
}

// Sessions from two providers appending events concurrently, with the whole
// server running: the store, the SSE hub and the VFS are all live.
func TestConcurrentSessionsThroughARunningServer(t *testing.T) {
	h := start(t)
	ctx := context.Background()

	const perProvider = 25
	var wg sync.WaitGroup
	for _, provider := range []string{"ftp", "sftp"} {
		s := h.session(provider)
		if _, err := s.MkdirAll(ctx, "/"+provider); err != nil {
			t.Fatal(err)
		}
		for i := range perProvider {
			wg.Add(1)
			go func(provider string, i int) {
				defer wg.Done()
				name := fmt.Sprintf("/%s/f%02d.txt", provider, i)
				if _, err := s.PutBytes(ctx, name, []byte("body")); err != nil {
					t.Errorf("put %s: %v", name, err)
				}
			}(provider, i)
		}
	}
	// Read the tab while the writes are in flight.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20 {
			if status, _ := h.GetBody(h.UI("/files/")); status != 200 {
				t.Errorf("tab status = %d", status)
				return
			}
		}
	}()
	wg.Wait()

	if got := h.VFS.Stats(); got.Files != 2*perProvider || got.Dirs != 2 {
		t.Fatalf("Stats() = %+v, want %d files in 2 directories", got, 2*perProvider)
	}
	events := h.Events(store.Query{Plugin: files.PluginName, Type: files.EventUpload})
	if len(events) != 2*perProvider {
		t.Fatalf("recorded %d upload events, want %d", len(events), 2*perProvider)
	}
}
