package s3_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/can3p/tommy/core/blob"
	blobfs "github.com/can3p/tommy/core/blob/filesystem"
	"github.com/can3p/tommy/core/state"
	"github.com/can3p/tommy/plugins/s3"
)

// persistentStore is a catalog bound to state and a filesystem blob store in
// dir, the way the server binds a filesystem scope.
func persistentStore(t *testing.T, st state.Store, dir string) *s3.Store {
	t.Helper()
	blobs, err := blobfs.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := s3.NewStore(s3.WithBlobs(blobs))
	if err := store.BindState(context.Background(), st); err != nil {
		t.Fatalf("BindState: %v", err)
	}
	return store
}

// catalogView is everything observable about a catalog, for comparing a
// restored one against the original.
type catalogView struct {
	Buckets []s3.Bucket
	Objects map[string][]s3.Object
	Bytes   map[string]string
	Uploads map[string][]s3.MultipartUpload
	Parts   map[string][]s3.Part
	Stats   s3.Stats
}

func view(t *testing.T, store *s3.Store) catalogView {
	t.Helper()
	v := catalogView{
		Buckets: store.ListBuckets(),
		Objects: map[string][]s3.Object{},
		Bytes:   map[string]string{},
		Uploads: map[string][]s3.MultipartUpload{},
		Parts:   map[string][]s3.Part{},
		Stats:   store.Stats(),
	}
	for _, b := range v.Buckets {
		list, err := store.ListObjects(b.Name, s3.ListOptions{MaxKeys: 1000})
		if err != nil {
			t.Fatal(err)
		}
		v.Objects[b.Name] = list.Objects
		for _, o := range list.Objects {
			data, _, err := store.GetObject(context.Background(), b.Name, o.Key)
			if err != nil {
				t.Fatalf("GetObject %s/%s: %v", b.Name, o.Key, err)
			}
			v.Bytes[b.Name+"/"+o.Key] = string(data)
		}
		uploads, err := store.ListMultipartUploads(b.Name)
		if err != nil {
			t.Fatal(err)
		}
		v.Uploads[b.Name] = uploads
		for _, up := range uploads {
			parts, err := store.ListParts(up.ID)
			if err != nil {
				t.Fatal(err)
			}
			v.Parts[up.ID] = parts
		}
	}
	return v
}

// assertSameCatalog compares through JSON, which is what a snapshot keeps:
// the monotonic clock reading a live time.Time carries is rightly not saved.
func assertSameCatalog(t *testing.T, got, want catalogView) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if !bytes.Equal(g, w) {
		t.Fatalf("restored catalog differs\n got: %s\nwant: %s", g, w)
	}
}

func TestCatalogSurvivesRebind(t *testing.T) {
	ctx := context.Background()
	st, dir := state.NewMemory(), t.TempDir()
	first := persistentStore(t, st, dir)

	for _, name := range []string{"media", "exports", "empty"} {
		if _, err := first.CreateBucket(name); err != nil {
			t.Fatal(err)
		}
	}
	_, err := first.PutObject(ctx, "media", "photos/cat.jpg", strings.NewReader("meow"), s3.PutOptions{
		Metadata:  map[string]string{"owner": "tommy"},
		Headers:   s3.ContentHeaders{ContentType: "image/jpeg", CacheControl: "max-age=60"},
		Checksums: s3.Checksums{SHA256: "ignored-by-store"},
	})
	if err != nil {
		t.Fatal(err)
	}
	putObject(t, first, "media", "replaced", "old")
	putObject(t, first, "media", "replaced", "new")
	putObject(t, first, "exports", "gone", "bye")
	if _, _, err := first.DeleteObject(ctx, "exports", "gone"); err != nil {
		t.Fatal(err)
	}
	putObject(t, first, "exports", "report.csv", "a,b\n1,2\n")
	up, err := first.CreateMultipart("exports", "big.bin", s3.PutOptions{Metadata: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.PutPart(ctx, up.ID, 2, strings.NewReader("second")); err != nil {
		t.Fatal(err)
	}
	if _, err := first.PutPart(ctx, up.ID, 1, strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	want := view(t, first)

	second := persistentStore(t, st, dir)
	got := view(t, second)
	assertSameCatalog(t, got, want)

	// The restored upload is live: it can be completed.
	parts := want.Parts[up.ID]
	obj, err := second.CompleteMultipart(ctx, up.ID, []s3.CompletedPart{{Number: 1, ETag: parts[0].ETag}, {Number: 2, ETag: parts[1].ETag}})
	if err != nil {
		t.Fatalf("CompleteMultipart after restore: %v", err)
	}
	data, _, err := persistentStore(t, st, dir).GetObject(ctx, "exports", obj.Key)
	if err != nil || string(data) != "firstsecond" {
		t.Fatalf("completed object after second restore = %q, %v", data, err)
	}
}

// A replaced or deleted object's bytes must be gone from disk, not merely
// unreferenced: a persistent store that only ever grows is the failure mode.
func TestReplacedAndDeletedBytesAreReclaimed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := persistentStore(t, state.NewMemory(), dir)
	if _, err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	putObject(t, store, "b", "k", "one")
	putObject(t, store, "b", "k", "two")
	putObject(t, store, "b", "doomed", "x")
	if _, _, err := store.DeleteObject(ctx, "b", "doomed"); err != nil {
		t.Fatal(err)
	}
	if n := countBlobs(t, dir); n != 1 {
		t.Fatalf("%d blobs on disk, want 1 (only the current k)", n)
	}
	if _, err := store.DeleteBucket(ctx, "b", true); err != nil {
		t.Fatal(err)
	}
	if n := countBlobs(t, dir); n != 0 {
		t.Fatalf("%d blobs on disk after deleting the bucket, want 0", n)
	}
}

func countBlobs(t *testing.T, dir string) int {
	t.Helper()
	blobs, err := blobfs.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := blobs.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(refs)
}

func TestRestoreSweepsOrphanedBytes(t *testing.T) {
	ctx := context.Background()
	st, dir := state.NewMemory(), t.TempDir()
	store := persistentStore(t, st, dir)
	if _, err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	putObject(t, store, "b", "kept", "live bytes")

	// What a process that died between writing bytes and saving the snapshot
	// naming them leaves behind.
	blobs, _ := blobfs.New(dir)
	orphan, err := blobs.Put(ctx, strings.NewReader("orphan"), blob.Ref{})
	if err != nil {
		t.Fatal(err)
	}

	restored := persistentStore(t, st, dir)
	if _, err := blobs.Stat(ctx, orphan.ID); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("orphan Stat after restore = %v, want ErrNotFound", err)
	}
	if data, _, err := restored.GetObject(ctx, "b", "kept"); err != nil || string(data) != "live bytes" {
		t.Fatalf("live object after sweep = %q, %v", data, err)
	}
}

func TestRestoreRefusesBadSnapshots(t *testing.T) {
	ctx := context.Background()
	valid := func(t *testing.T) (*state.Memory, string, []byte) {
		st, dir := state.NewMemory(), t.TempDir()
		store := persistentStore(t, st, dir)
		if _, err := store.CreateBucket("b"); err != nil {
			t.Fatal(err)
		}
		putObject(t, store, "b", "k", "bytes")
		data, err := st.Load(ctx, "catalog.json")
		if err != nil {
			t.Fatal(err)
		}
		return st, dir, data
	}
	cases := []struct {
		name    string
		breakIt func(t *testing.T, st *state.Memory, dir string, snapshot []byte)
		want    string
	}{
		{"not json", func(t *testing.T, st *state.Memory, _ string, _ []byte) {
			_ = st.Save(ctx, "catalog.json", []byte("{"))
		}, "decode"},
		{"unknown version", func(t *testing.T, st *state.Memory, _ string, s []byte) {
			_ = st.Save(ctx, "catalog.json", bytes.Replace(s, []byte(`"version":1`), []byte(`"version":99`), 1))
		}, "version 99"},
		{"invalid bucket name", func(t *testing.T, st *state.Memory, _ string, s []byte) {
			s = bytes.ReplaceAll(s, []byte(`"name":"b"`), []byte(`"name":""`))
			_ = st.Save(ctx, "catalog.json", bytes.ReplaceAll(s, []byte(`"bucket":"b"`), []byte(`"bucket":""`)))
		}, "invalid bucket"},
		{"missing bytes", func(t *testing.T, _ *state.Memory, dir string, _ []byte) {
			blobs, _ := blobfs.New(dir)
			refs, _ := blobs.List(ctx)
			for _, r := range refs {
				_ = blobs.Delete(ctx, r.ID)
			}
		}, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, dir, snapshot := valid(t)
			tc.breakIt(t, st, dir, snapshot)
			blobs, _ := blobfs.New(dir)
			err := s3.NewStore(s3.WithBlobs(blobs)).BindState(ctx, st)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BindState = %v, want an error mentioning %q", err, tc.want)
			}
			// A refused restore must not have swept the bytes it could not account for.
			if tc.name != "missing bytes" && countBlobs(t, dir) != 1 {
				t.Fatal("a failed restore deleted blobs")
			}
		})
	}
}

// failingState fails every Save after the first n.
type failingState struct {
	*state.Memory
	mu    sync.Mutex
	allow int
}

func (f *failingState) Save(ctx context.Context, key string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.allow <= 0 {
		return errors.New("disk full")
	}
	f.allow--
	return f.Memory.Save(ctx, key, data)
}

// A delete whose snapshot could not be saved must keep the bytes: the
// snapshot on disk still names them, and the next start would otherwise
// restore an object with nothing behind it.
func TestFailedSaveKeepsBytesTheSnapshotStillNames(t *testing.T) {
	ctx := context.Background()
	st := &failingState{Memory: state.NewMemory(), allow: 2}
	dir := t.TempDir()
	store := persistentStore(t, st, dir)
	if _, err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	putObject(t, store, "b", "k", "precious")

	if _, _, err := store.DeleteObject(ctx, "b", "k"); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("DeleteObject with a failing save = %v, want the save error", err)
	}
	st.allow = 1
	restored := persistentStore(t, st, dir)
	if data, _, err := restored.GetObject(ctx, "b", "k"); err != nil || string(data) != "precious" {
		t.Fatalf("object after failed delete and restart = %q, %v", data, err)
	}
}

func TestMemoryScopeSavesNothing(t *testing.T) {
	store, _ := newStore(t)
	if err := store.BindState(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	putObject(t, store, "b", "k", "v")
	if got := len(store.ListBuckets()); got != 1 {
		t.Fatalf("buckets = %d", got)
	}
}

// Concurrent writers must never leave an older snapshot on disk than the
// catalog in memory: after the dust settles, a restore equals the live view.
func TestConcurrentMutationsNeverRegressTheSnapshot(t *testing.T) {
	ctx := context.Background()
	st, dir := state.NewMemory(), t.TempDir()
	store := persistentStore(t, st, dir)
	if _, err := store.CreateBucket("b"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				key := fmt.Sprintf("k%d", (w*7+i)%20)
				if i%4 == 3 {
					_, _, _ = store.DeleteObject(ctx, "b", key)
					continue
				}
				if _, err := store.PutObject(ctx, "b", key, strings.NewReader(fmt.Sprintf("%d-%d", w, i)), s3.PutOptions{ModTime: time.Unix(int64(i), 0)}); err != nil {
					t.Error(err)
				}
			}
		}(w)
	}
	wg.Wait()

	want := view(t, store)
	got := view(t, persistentStore(t, st, dir))
	assertSameCatalog(t, got, want)
	if n := countBlobs(t, dir); n != len(want.Bytes) {
		t.Fatalf("%d blobs on disk for %d objects", n, len(want.Bytes))
	}
}
