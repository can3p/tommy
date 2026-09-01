package files_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	blobmem "github.com/can3p/tommy/core/blob/memory"
	"github.com/can3p/tommy/plugins/files"
)

// newVFS is a filesystem with its own blob store, so a test can assert on what
// the bytes did as well as on what the tree did.
func newVFS(t *testing.T, opts ...files.Option) (*files.VFS, *blobmem.Store) {
	t.Helper()
	blobs := blobmem.New(8 << 20)
	opts = append([]files.Option{files.WithBlobs(blobs)}, opts...)
	return files.NewVFS(opts...), blobs
}

func put(t *testing.T, v *files.VFS, path, body string) files.Node {
	t.Helper()
	n, err := v.PutBytes(context.Background(), path, []byte(body), files.WriteOptions{Provider: "ftp", Parents: true})
	if err != nil {
		t.Fatalf("put %s: %v", path, err)
	}
	return n
}

func read(t *testing.T, v *files.VFS, path string) string {
	t.Helper()
	b, err := v.ReadFile(context.Background(), path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestVFSStartsWithOnlyARoot(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)

	root := v.Root()
	if !root.Dir || root.Path != "/" || root.Name != "/" {
		t.Fatalf("root = %+v", root)
	}
	entries, err := v.List("/")
	if err != nil || len(entries) != 0 {
		t.Fatalf("List(/) = %v, %v; want an empty listing", entries, err)
	}
	if got := v.Stats(); got.Dirs != 0 || got.Files != 0 || got.Bytes != 0 {
		t.Errorf("Stats() = %+v, want all zero", got)
	}
}

func TestVFSPutStatAndList(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)

	n := put(t, v, "/upload/report.csv", "a,b\n1,2\n")
	if n.Path != "/upload/report.csv" || n.Name != "report.csv" || n.Dir {
		t.Fatalf("node = %+v", n)
	}
	if n.Size != 8 {
		t.Errorf("size = %d, want 8", n.Size)
	}
	if n.Provider != "ftp" {
		t.Errorf("provider = %q, want ftp", n.Provider)
	}
	if !strings.HasPrefix(n.ContentType, "text/csv") {
		t.Errorf("content type = %q, want text/csv", n.ContentType)
	}
	if n.Blob.ID == "" {
		t.Error("the node must point at a blob; bytes never live in the node")
	}

	got, err := v.Stat("upload/report.csv") // a relative path resolves to the same file
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got.Path != n.Path || got.Size != n.Size {
		t.Errorf("stat = %+v, want %+v", got, n)
	}
	if !v.Exists("/upload/report.csv") || v.IsDir("/upload/report.csv") {
		t.Error("Exists/IsDir disagree with Stat")
	}
	if !v.IsDir("/upload") {
		t.Error("Parents did not create /upload as a directory")
	}
	if body := read(t, v, "/upload/report.csv"); body != "a,b\n1,2\n" {
		t.Errorf("content = %q", body)
	}

	// Listing a file is an error, not a one-entry listing: providers rely on
	// telling a directory from a file.
	if _, err := v.List("/upload/report.csv"); !errors.Is(err, files.ErrNotDir) {
		t.Errorf("List(file) = %v, want ErrNotDir", err)
	}
}

func TestVFSListSortsDirectoriesFirst(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)
	put(t, v, "/b.txt", "b")
	put(t, v, "/a.txt", "a")
	put(t, v, "/zdir/x", "x")
	if _, err := v.Mkdir("/adir", files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	entries, err := v.List("/")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	want := []string{"adir", "zdir", "a.txt", "b.txt"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("List = %v, want %v", names, want)
	}
}

func TestVFSOverwriteReplacesContentAndFreesTheOldBlob(t *testing.T) {
	t.Parallel()
	v, blobs := newVFS(t)

	first := put(t, v, "/data.bin", "old content")
	second := put(t, v, "/data.bin", "new")

	if second.Blob.ID == first.Blob.ID {
		t.Error("an overwrite must store new bytes, not mutate the old blob")
	}
	if body := read(t, v, "/data.bin"); body != "new" {
		t.Errorf("content after overwrite = %q", body)
	}
	if got := v.Stats(); got.Files != 1 || got.Bytes != 3 {
		t.Errorf("Stats() = %+v, want one 3-byte file", got)
	}
	// The replaced bytes are freed: nothing in the tree points at them any
	// more, and the blob store never evicts on its own.
	if blobs.Len() != 1 {
		t.Errorf("blob store holds %d blobs, want 1", blobs.Len())
	}
	if blobs.Used() != 3 {
		t.Errorf("blob store holds %d bytes, want 3", blobs.Used())
	}
}

func TestVFSMkdirAndMkdirAll(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)

	if _, err := v.Mkdir("/a/b", files.WriteOptions{}); !errors.Is(err, files.ErrNotExist) {
		t.Errorf("Mkdir with a missing parent = %v, want ErrNotExist", err)
	}
	if _, err := v.Mkdir("/a", files.WriteOptions{Provider: "sftp"}); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, err := v.Mkdir("/a", files.WriteOptions{}); !errors.Is(err, files.ErrExist) {
		t.Errorf("Mkdir over an existing directory = %v, want ErrExist", err)
	}

	n, err := v.MkdirAll("/a/b/c/d", files.WriteOptions{Provider: "sftp"})
	if err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if n.Path != "/a/b/c/d" || !n.Dir || n.Provider != "sftp" {
		t.Fatalf("node = %+v", n)
	}
	for _, p := range []string{"/a", "/a/b", "/a/b/c", "/a/b/c/d"} {
		if !v.IsDir(p) {
			t.Errorf("%s was not created", p)
		}
	}
	// MkdirAll is idempotent.
	if _, err := v.MkdirAll("/a/b/c/d", files.WriteOptions{}); err != nil {
		t.Errorf("MkdirAll twice: %v", err)
	}
	// A file in the way is still an error.
	put(t, v, "/a/file", "x")
	if _, err := v.MkdirAll("/a/file/deeper", files.WriteOptions{}); !errors.Is(err, files.ErrNotDir) {
		t.Errorf("MkdirAll through a file = %v, want ErrNotDir", err)
	}
}

func TestVFSPutNeedsAnExistingParent(t *testing.T) {
	t.Parallel()
	v, blobs := newVFS(t)

	_, err := v.PutBytes(context.Background(), "/nope/file.txt", []byte("x"), files.WriteOptions{})
	if !errors.Is(err, files.ErrNotExist) {
		t.Fatalf("Put into a missing directory = %v, want ErrNotExist", err)
	}
	// The bytes that were already streamed are not left behind.
	if blobs.Len() != 0 {
		t.Errorf("a failed upload left %d blobs behind", blobs.Len())
	}
}

func TestVFSRemove(t *testing.T) {
	t.Parallel()
	v, blobs := newVFS(t)
	put(t, v, "/dir/a.txt", "aaa")
	put(t, v, "/dir/sub/b.txt", "bbb")

	if _, err := v.Remove(context.Background(), "/dir"); !errors.Is(err, files.ErrNotEmpty) {
		t.Errorf("Remove of a non-empty directory = %v, want ErrNotEmpty", err)
	}
	if _, err := v.Remove(context.Background(), "/dir/a.txt"); err != nil {
		t.Fatalf("Remove file: %v", err)
	}
	if v.Exists("/dir/a.txt") {
		t.Error("the file is still there after Remove")
	}
	if blobs.Used() != 3 {
		t.Errorf("removing a file must free its bytes; %d bytes still held", blobs.Used())
	}
	if _, err := v.Remove(context.Background(), "/dir/a.txt"); !errors.Is(err, files.ErrNotExist) {
		t.Errorf("Remove twice = %v, want ErrNotExist", err)
	}
	if _, err := v.Remove(context.Background(), "/"); err == nil {
		t.Error("removing the root must be refused")
	}
}

func TestVFSRemoveAllIsRecursive(t *testing.T) {
	t.Parallel()
	v, blobs := newVFS(t)
	put(t, v, "/dir/a.txt", "aaa")
	put(t, v, "/dir/sub/b.txt", "bbb")
	put(t, v, "/dir/sub/deep/c.txt", "ccc")
	put(t, v, "/keep.txt", "keep")

	n, count, err := v.RemoveAll(context.Background(), "/dir")
	if err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if !n.Dir || n.Path != "/dir" {
		t.Errorf("removed node = %+v", n)
	}
	// /dir, /dir/a.txt, /dir/sub, /dir/sub/b.txt, /dir/sub/deep, /dir/sub/deep/c.txt
	if count != 6 {
		t.Errorf("removed %d entries, want 6", count)
	}
	if v.Exists("/dir") || v.Exists("/dir/sub/deep/c.txt") {
		t.Error("the subtree survived RemoveAll")
	}
	if !v.Exists("/keep.txt") {
		t.Error("RemoveAll took a sibling with it")
	}
	if blobs.Used() != 4 {
		t.Errorf("only /keep.txt's bytes should remain; %d held", blobs.Used())
	}
	if got := v.Stats(); got.Files != 1 || got.Dirs != 0 {
		t.Errorf("Stats() = %+v", got)
	}
}

func TestVFSClear(t *testing.T) {
	t.Parallel()
	v, blobs := newVFS(t)
	put(t, v, "/a/b/c.txt", "x")
	put(t, v, "/d.txt", "y")

	count, err := v.Clear(context.Background())
	if err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if count != 4 { // /a, /a/b, /a/b/c.txt, /d.txt
		t.Errorf("Clear removed %d entries, want 4", count)
	}
	if got := v.Stats(); got != (files.Stats{}) {
		t.Errorf("Stats() = %+v, want empty", got)
	}
	if blobs.Used() != 0 {
		t.Errorf("Clear left %d bytes behind", blobs.Used())
	}
}

func TestVFSRename(t *testing.T) {
	t.Parallel()
	v, blobs := newVFS(t)
	put(t, v, "/a/one.txt", "one")
	put(t, v, "/a/two.txt", "two")
	if _, err := v.Mkdir("/b", files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	// A plain move.
	n, err := v.Rename(context.Background(), "/a/one.txt", "/b/renamed.txt")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if n.Path != "/b/renamed.txt" || n.Size != 3 {
		t.Fatalf("node = %+v", n)
	}
	if v.Exists("/a/one.txt") {
		t.Error("the source survived the rename")
	}
	if body := read(t, v, "/b/renamed.txt"); body != "one" {
		t.Errorf("content after rename = %q", body)
	}

	// Renaming over an existing file replaces it and frees its bytes.
	if _, err := v.Rename(context.Background(), "/a/two.txt", "/b/renamed.txt"); err != nil {
		t.Fatalf("Rename over a file: %v", err)
	}
	if body := read(t, v, "/b/renamed.txt"); body != "two" {
		t.Errorf("content after replacing rename = %q", body)
	}
	if blobs.Len() != 1 {
		t.Errorf("blob store holds %d blobs, want 1", blobs.Len())
	}

	// A whole directory moves with its contents.
	put(t, v, "/tree/x/y.txt", "y")
	if _, err := v.Rename(context.Background(), "/tree", "/moved"); err != nil {
		t.Fatalf("Rename directory: %v", err)
	}
	if body := read(t, v, "/moved/x/y.txt"); body != "y" {
		t.Errorf("content after moving a directory = %q", body)
	}
	if v.Exists("/tree") {
		t.Error("the source directory survived")
	}
}

func TestVFSRenameRefusals(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)
	put(t, v, "/a/f.txt", "f")
	if _, err := v.Mkdir("/b", files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, from, to string
		want           error
	}{
		{"missing source", "/nope", "/x", files.ErrNotExist},
		{"missing destination parent", "/a/f.txt", "/nope/x", files.ErrNotExist},
		{"file onto a directory", "/a/f.txt", "/b", files.ErrIsDir},
		{"directory onto a directory", "/a", "/b", files.ErrExist},
		{"directory into itself", "/a", "/a/deeper", fs.ErrInvalid},
		{"the root", "/", "/x", fs.ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := v.Rename(context.Background(), c.from, c.to)
			if !errors.Is(err, c.want) {
				t.Errorf("Rename(%q, %q) = %v, want %v", c.from, c.to, err, c.want)
			}
		})
	}

	// Renaming onto itself is a no-op, not an error: clients do it.
	if _, err := v.Rename(context.Background(), "/a/f.txt", "/a/./f.txt"); err != nil {
		t.Errorf("self-rename: %v", err)
	}
	if !v.Exists("/a/f.txt") {
		t.Error("a self-rename must not delete the file")
	}
}

func TestVFSChtimes(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)
	put(t, v, "/a.txt", "a")

	when := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := v.Chtimes("/a.txt", when); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	n, err := v.Stat("/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !n.ModTime.Equal(when) {
		t.Errorf("mtime = %v, want %v", n.ModTime, when)
	}
	if err := v.Chtimes("/nope", when); !errors.Is(err, files.ErrNotExist) {
		t.Errorf("Chtimes on a missing file = %v", err)
	}
}

func TestVFSFileHandleWritesAtOffsets(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)

	// SFTP writes arrive out of order at arbitrary offsets; nothing is visible
	// in the tree until Close.
	f, err := v.Create(context.Background(), "/sparse.bin", files.WriteOptions{Provider: "sftp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.WriteAt([]byte("world"), 6); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if _, err := f.WriteAt([]byte("hello"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if v.Exists("/sparse.bin") {
		t.Error("a file must not appear in the tree before its handle is closed")
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := read(t, v, "/sparse.bin"); got != "hello\x00world" {
		t.Errorf("content = %q", got)
	}
	if n, _ := v.Stat("/sparse.bin"); n.Provider != "sftp" {
		t.Errorf("provider = %q", n.Provider)
	}
}

func TestVFSFileHandleAppendAndTruncate(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)
	put(t, v, "/log.txt", "one\n")

	f, err := v.OpenFile(context.Background(), "/log.txt", files.OpenWrite|files.OpenAppend, files.WriteOptions{})
	if err != nil {
		t.Fatalf("OpenFile append: %v", err)
	}
	if _, err := f.Write([]byte("two\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := read(t, v, "/log.txt"); got != "one\ntwo\n" {
		t.Errorf("after append = %q", got)
	}

	if err := v.Truncate(context.Background(), "/log.txt", 3); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if got := read(t, v, "/log.txt"); got != "one" {
		t.Errorf("after truncate = %q", got)
	}
}

func TestVFSFileHandleAbortLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	v, blobs := newVFS(t)

	f, err := v.Create(context.Background(), "/half.bin", files.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	if err := f.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if v.Exists("/half.bin") {
		t.Error("an aborted transfer must leave no entry")
	}
	if blobs.Len() != 0 {
		t.Errorf("an aborted transfer left %d blobs", blobs.Len())
	}
	// Writing after the handle is done is refused rather than silently lost.
	if _, err := f.Write([]byte("more")); !errors.Is(err, files.ErrClosed) {
		t.Errorf("write after abort = %v, want ErrClosed", err)
	}
}

func TestVFSOpenFileFlags(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)
	put(t, v, "/there.txt", "there")

	if _, err := v.OpenFile(context.Background(), "/missing.txt", files.OpenWrite, files.WriteOptions{}); !errors.Is(err, files.ErrNotExist) {
		t.Errorf("write without OpenCreate = %v, want ErrNotExist", err)
	}
	if _, err := v.OpenFile(context.Background(), "/there.txt", files.OpenWrite|files.OpenCreate|files.OpenExclusive, files.WriteOptions{}); !errors.Is(err, files.ErrExist) {
		t.Errorf("OpenExclusive over an existing file = %v, want ErrExist", err)
	}
	// Read-write without truncation starts from the current content.
	f, err := v.OpenFile(context.Background(), "/there.txt", files.OpenReadWrite, files.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("T"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := read(t, v, "/there.txt"); got != "There" {
		t.Errorf("content = %q, want There", got)
	}
}

func TestVFSReadHandleSupportsReadAtAndSeek(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)
	put(t, v, "/abc.txt", "abcdefghij")

	f, err := v.Open(context.Background(), "/abc.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	buf := make([]byte, 3)
	if _, err := f.ReadAt(buf, 4); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != "efg" {
		t.Errorf("ReadAt = %q, want efg", string(buf))
	}
	// ReadAt must not disturb the streaming position.
	first := make([]byte, 3)
	if _, err := io.ReadFull(f, first); err != nil {
		t.Fatal(err)
	}
	if string(first) != "abc" {
		t.Errorf("Read after ReadAt = %q, want abc", string(first))
	}
	if _, err := f.Seek(8, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != "ij" {
		t.Errorf("after Seek = %q, want ij", string(rest))
	}
}

func TestVFSOpenDirectoryLists(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)
	put(t, v, "/d/a.txt", "a")
	put(t, v, "/d/b.txt", "b")

	f, err := v.Open(context.Background(), "/d")
	if err != nil {
		t.Fatalf("Open directory: %v", err)
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(0)
	if err != nil {
		t.Fatalf("Readdirnames: %v", err)
	}
	if strings.Join(names, ",") != "a.txt,b.txt" {
		t.Errorf("Readdirnames = %v", names)
	}
	if _, err := io.ReadAll(f); !errors.Is(err, files.ErrIsDir) {
		t.Errorf("reading a directory = %v, want ErrIsDir", err)
	}
}

func TestNodeFileInfo(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)
	n := put(t, v, "/d/a.txt", "hello")
	dir, err := v.Stat("/d")
	if err != nil {
		t.Fatal(err)
	}

	fi := n.FileInfo()
	if fi.Name() != "a.txt" || fi.Size() != 5 || fi.IsDir() || fi.Mode().IsDir() {
		t.Errorf("file info = %v %d %v %v", fi.Name(), fi.Size(), fi.IsDir(), fi.Mode())
	}
	if got, ok := fi.Sys().(files.Node); !ok || got.Path != "/d/a.txt" {
		t.Errorf("Sys() = %#v, want the Node", fi.Sys())
	}
	di := dir.FileInfo()
	if !di.IsDir() || !di.Mode().IsDir() {
		t.Errorf("directory info = %v %v", di.IsDir(), di.Mode())
	}
}

func TestVFSLimits(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t, files.WithLimits(files.Limits{
		MaxDepth:      3,
		MaxNameLen:    8,
		MaxDirEntries: 3,
		MaxNodes:      6,
		MaxFileSize:   16,
	}))

	t.Run("name length", func(t *testing.T) {
		_, err := v.Mkdir("/123456789", files.WriteOptions{})
		if !errors.Is(err, files.ErrNameTooLong) {
			t.Errorf("= %v, want ErrNameTooLong", err)
		}
	})
	t.Run("depth", func(t *testing.T) {
		_, err := v.MkdirAll("/a/b/c/d", files.WriteOptions{})
		if !errors.Is(err, files.ErrTooDeep) {
			t.Errorf("= %v, want ErrTooDeep", err)
		}
	})
	t.Run("file size", func(t *testing.T) {
		_, err := v.PutBytes(context.Background(), "/big", []byte(strings.Repeat("x", 17)), files.WriteOptions{})
		if !errors.Is(err, files.ErrFileTooLarge) {
			t.Errorf("= %v, want ErrFileTooLarge", err)
		}
		if v.Exists("/big") {
			t.Error("a rejected upload must leave no entry")
		}
		// Exactly at the limit is fine.
		if _, err := v.PutBytes(context.Background(), "/ok", []byte(strings.Repeat("x", 16)), files.WriteOptions{}); err != nil {
			t.Errorf("a file exactly at the limit was rejected: %v", err)
		}
	})
	t.Run("directory entries", func(t *testing.T) {
		v2, _ := newVFS(t, files.WithLimits(files.Limits{MaxDirEntries: 2}))
		put(t, v2, "/a", "a")
		put(t, v2, "/b", "b")
		if _, err := v2.PutBytes(context.Background(), "/c", []byte("c"), files.WriteOptions{}); !errors.Is(err, files.ErrDirFull) {
			t.Errorf("= %v, want ErrDirFull", err)
		}
		// Replacing an existing entry still works when the directory is full.
		if _, err := v2.PutBytes(context.Background(), "/a", []byte("A"), files.WriteOptions{}); err != nil {
			t.Errorf("overwriting inside a full directory: %v", err)
		}
	})
	t.Run("total nodes", func(t *testing.T) {
		v2, _ := newVFS(t, files.WithLimits(files.Limits{MaxNodes: 3, MaxDirEntries: 100}))
		put(t, v2, "/a", "a")
		put(t, v2, "/b", "b")
		put(t, v2, "/c", "c")
		if _, err := v2.PutBytes(context.Background(), "/d", []byte("d"), files.WriteOptions{}); !errors.Is(err, files.ErrTreeFull) {
			t.Errorf("= %v, want ErrTreeFull", err)
		}
	})
	t.Run("write handle refuses to grow past the limit", func(t *testing.T) {
		f, err := v.Create(context.Background(), "/handle", files.WriteOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Abort() }()
		if _, err := f.WriteAt([]byte("x"), 1<<20); !errors.Is(err, files.ErrFileTooLarge) {
			t.Errorf("= %v, want ErrFileTooLarge", err)
		}
	})
}

// A VFS built with no blob store of its own still works, which is what makes it
// usable in isolation; Attach then swaps in the core's store.
func TestVFSAttachBlobStore(t *testing.T) {
	t.Parallel()
	v := files.NewVFS()
	if v.Blobs() == nil {
		t.Fatal("a standalone VFS must have a blob store")
	}
	blobs := blobmem.New(1 << 20)
	v.Attach(blobs)
	if v.Blobs() != blobs {
		t.Fatal("Attach did not take")
	}
	put(t, v, "/a.txt", "a")
	if blobs.Len() != 1 {
		t.Errorf("the attached store holds %d blobs, want 1", blobs.Len())
	}
	// A second Attach is ignored: every surface in a real tommy is handed the
	// same store, and switching under a written file would break its link.
	other := blobmem.New(1 << 20)
	v.Attach(other)
	if v.Blobs() != blobs {
		t.Error("a second Attach must not switch stores")
	}
}

// Moving a directory downwards must not smuggle its contents past MaxDepth,
// which the depth check on the *destination path alone* would let through.
func TestVFSRenameRespectsMaxDepth(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t, files.WithLimits(files.Limits{MaxDepth: 2}))
	if _, err := v.Mkdir("/a", files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	put(t, v, "/a/f.txt", "f")
	if _, err := v.Mkdir("/b", files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}

	if _, err := v.Rename(context.Background(), "/a", "/b/a"); !errors.Is(err, files.ErrTooDeep) {
		t.Errorf("Rename = %v, want ErrTooDeep", err)
	}
	if !v.Exists("/a/f.txt") {
		t.Error("a refused rename must change nothing")
	}
}

// A provider whose per-request context is already done at end-of-transfer can
// still commit, which is the whole reason CloseContext exists.
func TestVFSCloseContextCommitsAfterTheOpenContextIsDone(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)
	ctx, cancel := context.WithCancel(context.Background())

	f, err := v.Create(ctx, "/late.txt", files.WriteOptions{Provider: "sftp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("landed")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := f.CloseContext(context.Background()); err != nil {
		t.Fatalf("CloseContext: %v", err)
	}
	if got := read(t, v, "/late.txt"); got != "landed" {
		t.Errorf("content = %q", got)
	}
}
