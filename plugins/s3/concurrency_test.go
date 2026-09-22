package s3_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/can3p/tommy/plugins/s3"
)

// These tests are intentionally most useful under go test -race.
func TestConcurrentObjectOperations(t *testing.T) {
	store, blobs := newStore(t)
	if _, err := store.CreateBucket("bucket"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const workers = 12
	const rounds = 50
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for round := range rounds {
				key := fmt.Sprintf("prefix/%d//.././key-%d", worker, round%8)
				body := fmt.Sprintf("worker=%d round=%d", worker, round)
				if _, err := store.PutObject(ctx, "bucket", key, bytes.NewBufferString(body), s3.PutOptions{Metadata: map[string]string{"worker": fmt.Sprint(worker)}}); err != nil {
					t.Errorf("put: %v", err)
					return
				}
				if _, err := store.HeadObject("bucket", key); err != nil && !errors.Is(err, s3.ErrObjectNotFound) {
					t.Errorf("head: %v", err)
					return
				}
				if _, err := store.ListObjects("bucket", s3.ListOptions{Prefix: "prefix/", Delimiter: "/"}); err != nil {
					t.Errorf("list: %v", err)
					return
				}
				_ = store.Stats()
				if round%3 == 0 {
					if _, _, err := store.DeleteObject(ctx, "bucket", key); err != nil {
						t.Errorf("delete: %v", err)
						return
					}
				}
			}
		}(worker)
	}
	wg.Wait()

	stats := store.Stats()
	if blobs.Len() != stats.Objects {
		t.Fatalf("catalog/blob divergence after concurrent operations: blobs=%d stats=%+v", blobs.Len(), stats)
	}
	listed, err := store.ListObjects("bucket", s3.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(listed.Objects); i++ {
		if listed.Objects[i-1].Key >= listed.Objects[i].Key {
			t.Fatalf("listing is not strictly sorted at %q, %q", listed.Objects[i-1].Key, listed.Objects[i].Key)
		}
	}
}

func TestConcurrentOverwritesInstallOneWholeObject(t *testing.T) {
	store, blobs := newStore(t)
	_, _ = store.CreateBucket("bucket")
	ctx := context.Background()
	const writers = 64
	var wg sync.WaitGroup
	for writer := range writers {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			body := bytes.Repeat([]byte{byte(writer)}, 1024)
			if _, err := store.PutObject(ctx, "bucket", "same//key", bytes.NewReader(body), s3.PutOptions{}); err != nil {
				t.Errorf("put: %v", err)
			}
		}(writer)
	}
	wg.Wait()
	data, _, err := store.GetObject(ctx, "bucket", "same//key")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 1024 || !bytes.Equal(data, bytes.Repeat(data[:1], len(data))) {
		t.Fatal("surviving object contains a mixture of concurrent writers")
	}
	if blobs.Len() != 1 || store.Stats().Objects != 1 {
		t.Fatalf("overwrites left stale state: blobs=%d stats=%+v", blobs.Len(), store.Stats())
	}
}
