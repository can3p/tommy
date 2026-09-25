package files_test

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
	blobmem "github.com/can3p/tommy/core/blob/memory"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/state"
	"github.com/can3p/tommy/plugins/files"
)

// persistentScope is what the server hands a filesystem-backed files plugin:
// a state store plus a blob store that can list what it holds.
func persistentScope(t *testing.T) plugin.Storage {
	t.Helper()
	blobs, err := blobfs.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return plugin.Storage{State: state.NewMemory(), Blobs: blobs}
}

// bound returns a plugin bound to st, the way the server binds it at startup.
func bound(t *testing.T, st plugin.Storage) *files.VFS {
	t.Helper()
	p := files.New()
	if err := p.BindStorage(context.Background(), st); err != nil {
		t.Fatalf("BindStorage: %v", err)
	}
	return p.VFS()
}

// dump renders a tree, bytes included, as a comparable string.
func dump(t *testing.T, v *files.VFS) string {
	t.Helper()
	var b strings.Builder
	root := v.Root()
	fmt.Fprintf(&b, "/ %s %q\n", root.ModTime.UTC().Format(time.RFC3339Nano), root.Provider)
	var paths []files.Node
	if err := v.Walk(func(n files.Node) error {
		paths = append(paths, n)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, n := range paths {
		fmt.Fprintf(&b, "%s dir=%v size=%d mod=%s provider=%q ct=%q blob=%s",
			n.Path, n.Dir, n.Size, n.ModTime.UTC().Format(time.RFC3339Nano), n.Provider, n.ContentType, n.Blob.ID)
		if !n.Dir {
			data, err := v.ReadFile(context.Background(), n.Path)
			if err != nil {
				t.Fatalf("read %s: %v", n.Path, err)
			}
			fmt.Fprintf(&b, " bytes=%q", data)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func TestPersistenceRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := persistentScope(t)
	v := bound(t, st)

	if _, err := v.Mkdir("/a/b/c", files.WriteOptions{Provider: "sftp", Parents: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.MkdirAll("/empty/dir", files.WriteOptions{Provider: "nfs"}); err != nil {
		t.Fatal(err)
	}
	put(t, v, "/a/report.csv", "id,name\n1,tommy\n")
	put(t, v, "/a/b/c/deep.bin", "\x00\x01\x02binary")
	if _, err := v.PutBytes(ctx, "/a/typed", []byte("{}"), files.WriteOptions{Provider: "tftp", ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	put(t, v, "/a/replaced.txt", "old")
	put(t, v, "/a/replaced.txt", "new content")
	put(t, v, "/gone.txt", "bye")
	if _, err := v.Remove(ctx, "/gone.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Rename(ctx, "/a/report.csv", "/a/b/renamed.csv"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Rename(ctx, "/a/b/c", "/moved"); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2001, 2, 3, 4, 5, 6, 7, time.UTC)
	if err := v.Chtimes("/a/b/renamed.csv", stamp); err != nil {
		t.Fatal(err)
	}
	if err := v.Chtimes("/empty", stamp.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := v.Truncate(ctx, "/a/typed", 5); err != nil {
		t.Fatal(err)
	}
	w, err := v.Create(ctx, "/a/handle.txt", files.WriteOptions{Provider: "sftp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt([]byte("world"), 6); err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt([]byte("hello "), 0); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// An aborted handle is not a commit and must not reach the snapshot.
	aborted, err := v.Create(ctx, "/a/aborted.txt", files.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = aborted.Write([]byte("never"))
	_ = aborted.Abort()

	before := dump(t, v)
	restored := bound(t, st)
	after := dump(t, restored)
	if before != after {
		t.Fatalf("restored tree differs\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if restored.Exists("/a/aborted.txt") || restored.Exists("/gone.txt") {
		t.Error("an aborted or removed file came back")
	}
	if got := read(t, restored, "/a/typed"); got != "{}\x00\x00\x00" {
		t.Errorf("truncated file = %q", got)
	}
	if got := read(t, restored, "/a/replaced.txt"); got != "new content" {
		t.Errorf("replaced file = %q", got)
	}
	if s := restored.Stats(); s != v.Stats() {
		t.Errorf("stats %+v, want %+v", s, v.Stats())
	}

	// Every blob left in the store belongs to a file in the tree: replaced and
	// removed bytes were freed once the snapshot stopped naming them.
	refs, err := st.Blobs.(blob.Lister).List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(refs), restored.Stats().Files; got != want {
		t.Errorf("blob store holds %d blobs, tree has %d files", got, want)
	}

	// The restored tree is live: a further change is saved too.
	put(t, restored, "/after.txt", "later")
	third := bound(t, st)
	if got := read(t, third, "/after.txt"); got != "later" {
		t.Errorf("change after restore = %q", got)
	}
}

func TestPersistenceSnapshotIsDeterministic(t *testing.T) {
	t.Parallel()
	st := persistentScope(t)
	v := bound(t, st)
	put(t, v, "/z/b.txt", "b")
	put(t, v, "/z/a.txt", "a")
	put(t, v, "/a.txt", "a")
	first, err := st.State.Load(context.Background(), "tree.json")
	if err != nil {
		t.Fatal(err)
	}
	// Binding a fresh VFS and forcing a save of the identical tree must
	// produce identical bytes.
	restored := bound(t, st)
	if err := restored.Chtimes("/a.txt", mustStat(t, restored, "/a.txt").ModTime); err != nil {
		t.Fatal(err)
	}
	second, err := st.State.Load(context.Background(), "tree.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("snapshot bytes differ for the same tree:\n%s\n%s", first, second)
	}
	var decoded struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(first, &decoded); err != nil || decoded.Version != 1 {
		t.Errorf("snapshot version = %d, %v", decoded.Version, err)
	}
}

// quote encodes a string as JSON, so a hostile path reaches the restore
// rather than failing to decode.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func mustStat(t *testing.T, v *files.VFS, name string) files.Node {
	t.Helper()
	n, err := v.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A snapshot that cannot be restored is a startup failure, never a silent
// reset to an empty tree.
func TestPersistenceRejectsBadSnapshots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := blobmem.New(0)
	good, err := blobs.Put(ctx, strings.NewReader("hello"), blob.Ref{Filename: "f.txt"})
	if err != nil {
		t.Fatal(err)
	}
	ref := func(r blob.Ref) string {
		b, _ := json.Marshal(r)
		return string(b)
	}
	const root = `"root":{"path":"/","dir":true,"mod_time":"2020-01-01T00:00:00Z"}`
	file := func(p string, r blob.Ref, size int64) string {
		return fmt.Sprintf(`{"path":%s,"mod_time":"2020-01-01T00:00:00Z","size":%d,"blob":%s}`, quote(p), size, ref(r))
	}
	dir := func(p string) string {
		return fmt.Sprintf(`{"path":%s,"dir":true,"mod_time":"2020-01-01T00:00:00Z"}`, quote(p))
	}
	snap := func(entries ...string) string {
		return `{"version":1,` + root + `,"entries":[` + strings.Join(entries, ",") + `]}`
	}

	cases := map[string]string{
		"corrupt":           `{"version":1,"root":`,
		"not json":          "tree",
		"unknown version":   `{"version":99,` + root + `,"entries":[]}`,
		"no version":        `{` + root + `,"entries":[]}`,
		"bad root":          `{"version":1,"root":{"path":"/x","dir":true},"entries":[]}`,
		"missing blob":      snap(file("/f.txt", blob.Ref{ID: "nope", Size: 5}, 5)),
		"size mismatch":     snap(file("/f.txt", blob.Ref{ID: good.ID, Size: 4}, 4)),
		"ref size mismatch": snap(file("/f.txt", good, 4)),
		"file without blob": snap(file("/f.txt", blob.Ref{}, 0)),
		"traversal":         snap(file("/../etc/passwd", good, 5)),
		"dotdot inside":     snap(dir("/a"), file("/a/../f.txt", good, 5)),
		"relative":          snap(file("f.txt", good, 5)),
		"trailing slash":    snap(dir("/a/")),
		"backslash":         snap(file(`\f.txt`, good, 5)),
		"control char":      snap(file("/f\x01.txt", good, 5)),
		"nul":               snap(file("/f\x00.txt", good, 5)),
		"root entry":        snap(dir("/")),
		"missing parent":    snap(file("/a/f.txt", good, 5)),
		"child before dir":  snap(file("/a/f.txt", good, 5), dir("/a")),
		"duplicate file":    snap(file("/f.txt", good, 5), file("/f.txt", good, 5)),
		"file and dir":      snap(dir("/f.txt"), file("/f.txt", good, 5)),
		"dir with content":  snap(`{"path":"/d","dir":true,"mod_time":"2020-01-01T00:00:00Z","size":5,"blob":` + ref(good) + `}`),
		"name too long":     snap(dir("/" + strings.Repeat("x", 300))),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := state.NewMemory()
			if err := st.Save(ctx, "tree.json", []byte(data)); err != nil {
				t.Fatal(err)
			}
			p := files.New()
			err := p.BindStorage(ctx, plugin.Storage{State: st, Blobs: blobs})
			if err == nil {
				t.Fatalf("BindStorage accepted %q", data)
			}
			if p.VFS().Stats() != (files.Stats{}) {
				t.Error("a rejected snapshot left entries in the tree")
			}
		})
	}

	// The limits in force apply to a restored tree exactly as to a live one.
	t.Run("limits", func(t *testing.T) {
		t.Parallel()
		limitCases := map[string]struct {
			limits files.Limits
			data   string
		}{
			"dir entries": {files.Limits{MaxDirEntries: 1}, snap(dir("/a"), dir("/b"))},
			"nodes":       {files.Limits{MaxNodes: 1}, snap(dir("/a"), dir("/a/b"))},
			"depth":       {files.Limits{MaxDepth: 1}, snap(dir("/a"), dir("/a/b"))},
			"file size":   {files.Limits{MaxFileSize: 4}, snap(file("/f.txt", good, 5))},
		}
		for name, tc := range limitCases {
			st := state.NewMemory()
			if err := st.Save(ctx, "tree.json", []byte(tc.data)); err != nil {
				t.Fatal(err)
			}
			p := files.NewWithVFS(files.NewVFS(files.WithLimits(tc.limits)))
			if err := p.BindStorage(ctx, plugin.Storage{State: st, Blobs: blobs}); err == nil {
				t.Errorf("%s: BindStorage accepted a snapshot over the limit", name)
			}
		}
	})

	// And the well-formed version of the same snapshot is accepted, so the
	// failures above are about what they say they are about.
	t.Run("control", func(t *testing.T) {
		t.Parallel()
		st := state.NewMemory()
		if err := st.Save(ctx, "tree.json", []byte(snap(dir("/a"), file("/a/f.txt", good, 5)))); err != nil {
			t.Fatal(err)
		}
		p := files.New()
		if err := p.BindStorage(ctx, plugin.Storage{State: st, Blobs: blobs}); err != nil {
			t.Fatal(err)
		}
		if got := read(t, p.VFS(), "/a/f.txt"); got != "hello" {
			t.Errorf("restored = %q", got)
		}
	})
}

// Bytes written by a process that died before saving the snapshot naming them
// are reclaimed on the next bind; bytes the snapshot names are kept.
func TestPersistenceSweepsOrphanBlobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := persistentScope(t)
	v := bound(t, st)
	put(t, v, "/kept.txt", "kept")
	kept := mustStat(t, v, "/kept.txt").Blob.ID

	orphan, err := st.Blobs.Put(ctx, strings.NewReader("orphan"), blob.Ref{})
	if err != nil {
		t.Fatal(err)
	}

	restored := bound(t, st)
	if _, err := st.Blobs.Stat(ctx, orphan.ID); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("orphan blob survived the bind: %v", err)
	}
	if _, err := st.Blobs.Stat(ctx, kept); err != nil {
		t.Errorf("referenced blob was swept: %v", err)
	}
	if got := read(t, restored, "/kept.txt"); got != "kept" {
		t.Errorf("kept file = %q", got)
	}
}

// With no state, the plugin behaves exactly as it always has: its store is
// the one attached first and a later bind resets nothing.
func TestPersistenceMemoryScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := blobmem.New(0)
	p := files.New()
	if err := p.BindStorage(ctx, plugin.Storage{Blobs: blobs}); err != nil {
		t.Fatal(err)
	}
	put(t, p.VFS(), "/a.txt", "a")
	if p.VFS().Blobs() != blob.BlobStore(blobs) {
		t.Error("memory scope did not attach its blob store")
	}
	if err := p.BindStorage(ctx, plugin.Storage{Blobs: blobmem.New(0)}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, p.VFS(), "/a.txt"); got != "a" {
		t.Errorf("file after second memory bind = %q", got)
	}
}

// A persistent scope's blob store wins over one attached earlier, since that
// is where the snapshot's bytes are; but not once the tree holds files.
func TestPersistenceBindUsesScopeBlobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := persistentScope(t)
	p := files.New()
	p.VFS().Attach(blobmem.New(0))
	if err := p.BindStorage(ctx, st); err != nil {
		t.Fatal(err)
	}
	if p.VFS().Blobs() != st.Blobs {
		t.Fatal("persistent scope's blob store is not the one in use")
	}

	busy := files.New()
	put(t, busy.VFS(), "/a.txt", "a")
	if err := busy.BindStorage(ctx, persistentScope(t)); err == nil {
		t.Error("binding storage under existing files succeeded")
	}
}

type failingState struct {
	state.Store
	mu   sync.Mutex
	fail bool
}

func (f *failingState) Save(ctx context.Context, key string, data []byte) error {
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if fail {
		return errors.New("disk full")
	}
	return f.Store.Save(ctx, key, data)
}

// A failed save is reported, the change stays visible in this process, and
// the bytes the saved snapshot still names are not freed.
func TestPersistenceSaveFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scope := persistentScope(t)
	fs := &failingState{Store: scope.State}
	scope.State = fs
	v := bound(t, scope)
	put(t, v, "/f.txt", "old")
	oldBlob := mustStat(t, v, "/f.txt").Blob.ID

	fs.mu.Lock()
	fs.fail = true
	fs.mu.Unlock()
	if _, err := v.PutBytes(ctx, "/f.txt", []byte("new"), files.WriteOptions{}); err == nil {
		t.Fatal("put reported success although the snapshot was not saved")
	}
	if got := read(t, v, "/f.txt"); got != "new" {
		t.Errorf("in-process content = %q, want the new content", got)
	}
	if _, err := scope.Blobs.Stat(ctx, oldBlob); err != nil {
		t.Errorf("old bytes were freed while the saved snapshot still names them: %v", err)
	}
	if _, err := v.Remove(ctx, "/f.txt"); err == nil {
		t.Error("remove reported success although the snapshot was not saved")
	}

	// What is on disk is still the last snapshot that was saved, and it
	// restores cleanly - the unsaved changes are lost, not corrupted.
	restored := bound(t, scope)
	if got := read(t, restored, "/f.txt"); got != "old" {
		t.Errorf("restored content = %q, want the last saved content", got)
	}
}

// Under concurrent mutation the saved snapshot must end up equal to the tree
// in memory: saves are serialized and always write the newest revision, so a
// slow save can never land on top of a newer one.
func TestPersistenceConcurrentMutations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// An in-memory blob store keeps this about the snapshot ordering rather
	// than about fsync latency.
	st := plugin.Storage{State: state.NewMemory(), Blobs: blobmem.New(0)}
	v := bound(t, st)

	const workers = 16
	const rounds = 12
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			dir := fmt.Sprintf("/w%02d", w)
			for i := range rounds {
				name := fmt.Sprintf("%s/f%02d.txt", dir, i)
				if _, err := v.PutBytes(ctx, name, []byte(name), files.WriteOptions{Parents: true}); err != nil {
					t.Errorf("put %s: %v", name, err)
					return
				}
				switch i % 4 {
				case 1:
					if _, err := v.Rename(ctx, name, name+".moved"); err != nil {
						t.Errorf("rename %s: %v", name, err)
					}
				case 2:
					if _, err := v.Remove(ctx, name); err != nil {
						t.Errorf("remove %s: %v", name, err)
					}
				case 3:
					if err := v.Chtimes(name, time.Unix(int64(w*1000+i), 0)); err != nil {
						t.Errorf("chtimes %s: %v", name, err)
					}
				}
				// Everyone also rewrites one shared file, so replacements
				// race each other and their superseded blobs.
				if _, err := v.PutBytes(ctx, "/shared.txt", []byte(name), files.WriteOptions{}); err != nil {
					t.Errorf("put shared: %v", err)
				}
				if _, err := v.MkdirAll(fmt.Sprintf("%s/sub%02d", dir, i%3), files.WriteOptions{}); err != nil {
					t.Errorf("mkdirall: %v", err)
				}
			}
			if w%4 == 0 {
				if _, _, err := v.RemoveAll(ctx, dir); err != nil {
					t.Errorf("removeall %s: %v", dir, err)
				}
			}
		}(w)
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	want := dump(t, v)
	restored := bound(t, st)
	if got := dump(t, restored); got != want {
		t.Fatalf("saved snapshot differs from the tree in memory\nmemory:\n%s\nsaved:\n%s", want, got)
	}
}
