package s3_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/can3p/tommy/core/blob"
	blobmem "github.com/can3p/tommy/core/blob/memory"
	"github.com/can3p/tommy/plugins/s3"
)

func newStore(t *testing.T, opts ...s3.Option) (*s3.Store, *blobmem.Store) {
	t.Helper()
	blobs := blobmem.New(16 << 20)
	opts = append([]s3.Option{s3.WithBlobs(blobs)}, opts...)
	return s3.NewStore(opts...), blobs
}

func putObject(t *testing.T, store *s3.Store, bucket, key, body string) s3.Object {
	t.Helper()
	obj, err := store.PutObject(context.Background(), bucket, key, bytes.NewBufferString(body), s3.PutOptions{})
	if err != nil {
		t.Fatalf("PutObject(%q): %v", key, err)
	}
	return obj
}

func TestExactKeysAndLexicographicListing(t *testing.T) {
	store, _ := newStore(t)
	if _, err := store.CreateBucket("bucket"); err != nil {
		t.Fatal(err)
	}
	keys := []string{"a//b", "a/../b", "a/./b", ".", "z", "a/child/one", "a/child/two"}
	for _, key := range keys {
		putObject(t, store, "bucket", key, key)
	}
	for _, key := range keys {
		obj, err := store.HeadObject("bucket", key)
		if err != nil || obj.Key != key {
			t.Errorf("HeadObject(%q) = %#v, %v", key, obj, err)
		}
	}

	page, err := store.ListObjects("bucket", s3.ListOptions{Prefix: "a/", Delimiter: "/", MaxKeys: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !page.IsTruncated || page.NextContinuationToken == "" {
		t.Fatalf("first page = %#v, want a continuation", page)
	}
	if !reflect.DeepEqual(page.CommonPrefixes, []string{"a/../", "a/./"}) {
		t.Fatalf("common prefixes = %#v", page.CommonPrefixes)
	}
	next, err := store.ListObjects("bucket", s3.ListOptions{Prefix: "a/", Delimiter: "/", ContinuationToken: page.NextContinuationToken})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(next.CommonPrefixes, []string{"a//", "a/child/"}) {
		t.Fatalf("next common prefixes = %#v", next.CommonPrefixes)
	}
}

func TestLimitsRejectWithoutLeakingBlobs(t *testing.T) {
	limits := s3.Limits{
		MaxBuckets: 1, MaxObjects: 1, MaxActiveUploads: 1, MaxParts: 1,
		MaxBucketBytes: 4, MaxKeyBytes: 4, MaxMetadataBytes: 4, MaxObjectBytes: 4,
	}
	store, blobs := newStore(t, s3.WithLimits(limits))
	if _, err := store.CreateBucket("one"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBucket("two"); !errors.Is(err, s3.ErrBucketLimit) {
		t.Fatalf("second bucket = %v", err)
	}
	if _, err := store.PutObject(context.Background(), "one", "toolong", bytes.NewBufferString("x"), s3.PutOptions{}); !errors.Is(err, s3.ErrInvalidKey) {
		t.Fatalf("long key = %v", err)
	}
	if _, err := store.PutObject(context.Background(), "one", "meta", bytes.NewBufferString("x"), s3.PutOptions{Metadata: map[string]string{"abc": "de"}}); !errors.Is(err, s3.ErrMetadataTooLarge) {
		t.Fatalf("large metadata = %v", err)
	}
	if _, err := store.PutObject(context.Background(), "one", "big", bytes.NewBufferString("12345"), s3.PutOptions{}); !errors.Is(err, s3.ErrObjectTooLarge) {
		t.Fatalf("large object = %v", err)
	}
	putObject(t, store, "one", "a", "1234")
	if _, err := store.PutObject(context.Background(), "one", "b", bytes.NewBufferString("x"), s3.PutOptions{}); !errors.Is(err, s3.ErrObjectLimit) {
		t.Fatalf("second object = %v", err)
	}
	if blobs.Len() != 1 {
		t.Fatalf("failed writes leaked blobs: Len() = %d", blobs.Len())
	}

	upload, err := store.CreateMultipart("one", "up", s3.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateMultipart("one", "u2", s3.PutOptions{}); !errors.Is(err, s3.ErrUploadLimit) {
		t.Fatalf("second upload = %v", err)
	}
	if _, err := store.PutPart(context.Background(), upload.ID, 1, bytes.NewBufferString("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutPart(context.Background(), upload.ID, 2, bytes.NewBufferString("b")); !errors.Is(err, s3.ErrPartLimit) {
		t.Fatalf("second part = %v", err)
	}
	if blobs.Len() != 2 {
		t.Fatalf("failed part leaked a blob: Len() = %d", blobs.Len())
	}
}

func TestOverwriteDeleteAndAbortCleanUpBlobs(t *testing.T) {
	store, blobs := newStore(t)
	_, _ = store.CreateBucket("bucket")
	first := putObject(t, store, "bucket", "key", "first")
	second := putObject(t, store, "bucket", "key", "second")
	if first.Blob.ID == second.Blob.ID || blobs.Len() != 1 {
		t.Fatalf("overwrite retained stale blob: first=%q second=%q len=%d", first.Blob.ID, second.Blob.ID, blobs.Len())
	}
	if _, err := blobs.Stat(context.Background(), first.Blob.ID); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("old blob still exists: %v", err)
	}
	if _, removed, err := store.DeleteObject(context.Background(), "bucket", "key"); err != nil || !removed {
		t.Fatalf("delete = %v, %v", removed, err)
	}
	if blobs.Len() != 0 {
		t.Fatalf("delete retained %d blobs", blobs.Len())
	}

	upload, _ := store.CreateMultipart("bucket", "multi", s3.PutOptions{})
	_, _ = store.PutPart(context.Background(), upload.ID, 1, bytes.NewBufferString("one"))
	_, _ = store.PutPart(context.Background(), upload.ID, 2, bytes.NewBufferString("two"))
	if err := store.AbortMultipart(context.Background(), upload.ID); err != nil {
		t.Fatal(err)
	}
	if blobs.Len() != 0 || store.Stats().ActiveUploads != 0 {
		t.Fatalf("abort left state: blobs=%d stats=%+v", blobs.Len(), store.Stats())
	}
}

func TestMultipartCompositionAndETag(t *testing.T) {
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	store, blobs := newStore(t, s3.WithClock(func() time.Time { return now }))
	_, _ = store.CreateBucket("bucket")
	upload, err := store.CreateMultipart("bucket", "a//../object", s3.PutOptions{
		Metadata: map[string]string{"origin": "test"},
		Headers:  s3.ContentHeaders{ContentType: "text/plain", CacheControl: "no-cache"},
	})
	if err != nil {
		t.Fatal(err)
	}
	part1, err := store.PutPart(context.Background(), upload.ID, 1, bytes.NewBufferString("hello "))
	if err != nil {
		t.Fatal(err)
	}
	part2, err := store.PutPart(context.Background(), upload.ID, 2, bytes.NewBufferString("world"))
	if err != nil {
		t.Fatal(err)
	}
	obj, err := store.CompleteMultipart(context.Background(), upload.ID, []s3.CompletedPart{
		{Number: 1, ETag: `"` + part1.ETag + `"`}, {Number: 2, ETag: part2.ETag},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := store.GetObject(context.Background(), "bucket", "a//../object")
	if err != nil || string(data) != "hello world" {
		t.Fatalf("composed data = %q, %v", data, err)
	}
	one := md5.Sum([]byte("hello "))
	two := md5.Sum([]byte("world"))
	joined := append(one[:], two[:]...)
	want := md5.Sum(joined)
	if obj.ETag != hex.EncodeToString(want[:])+"-2" {
		t.Fatalf("ETag = %q", obj.ETag)
	}
	if obj.Metadata["origin"] != "test" || obj.Headers.ContentType != "text/plain" || !obj.LastModified.Equal(now) {
		t.Fatalf("completed metadata = %#v", obj)
	}
	if blobs.Len() != 1 || store.Stats().Parts != 0 {
		t.Fatalf("completion did not clean parts: blobs=%d stats=%+v", blobs.Len(), store.Stats())
	}
}

func TestCopyAndForceDelete(t *testing.T) {
	store, blobs := newStore(t)
	_, _ = store.CreateBucket("source")
	_, _ = store.CreateBucket("destination")
	original, err := store.PutObject(context.Background(), "source", "key", bytes.NewBufferString("body"), s3.PutOptions{
		Metadata: map[string]string{"copy": "me"}, Headers: s3.ContentHeaders{ContentType: "text/plain"},
	})
	if err != nil {
		t.Fatal(err)
	}
	copied, err := store.CopyObject(context.Background(), s3.ObjectLocation{Bucket: "source", Key: "key"}, s3.ObjectLocation{Bucket: "destination", Key: "copy"}, s3.CopyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if copied.Metadata["copy"] != "me" || copied.Headers != original.Headers {
		t.Fatalf("copy did not preserve metadata: %#v", copied)
	}
	if _, err := store.DeleteBucket(context.Background(), "destination", false); !errors.Is(err, s3.ErrBucketNotEmpty) {
		t.Fatalf("non-force delete = %v", err)
	}
	if _, err := store.DeleteBucket(context.Background(), "destination", true); err != nil {
		t.Fatal(err)
	}
	if blobs.Len() != 1 {
		t.Fatalf("force delete retained destination blob: %d", blobs.Len())
	}
}

func TestAttachFirstNonNilWins(t *testing.T) {
	store := s3.NewStore()
	first, second := blobmem.New(1024), blobmem.New(1024)
	store.Attach(nil)
	store.Attach(first)
	store.Attach(second)
	if store.Blobs() != first {
		t.Fatal("Attach did not keep the first non-nil blob store")
	}
}

func TestOpenStreamsBlobBytes(t *testing.T) {
	store, _ := newStore(t)
	_, _ = store.CreateBucket("bucket")
	putObject(t, store, "bucket", "key", "stream")
	r, obj, err := store.OpenObject(context.Background(), "bucket", "key")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil || string(data) != "stream" || obj.Size != 6 {
		t.Fatalf("open = %q %#v %v", data, obj, err)
	}
}
