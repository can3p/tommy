package files_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/plugins/files"
)

// Path resolution is the security boundary of this plugin: an FTP client is a
// stranger by design, and it must never be able to name anything outside the
// virtual root. The property is enforced in exactly one place - VFS.Resolve -
// and every VFS method funnels through it, so these cases cover every provider
// at once rather than each of them separately.
//
// A hostile path is either rejected outright or normalized to something inside
// the tree. Neither outcome may reach the host filesystem, and in fact nothing
// here can: the tree is a map in memory and the VFS never opens a real file.
func TestResolveHostilePaths(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)

	cases := []struct {
		name  string
		input string
		// want is the resolved path, or "" when the path must be rejected.
		want string
		err  error
	}{
		// Traversal, in every spelling. All of it clamps at the root the way a
		// chroot does, so the worst a client can do is name a file that does
		// not exist inside the tree.
		{"parent of the root", "..", "/", nil},
		{"parent with a slash", "../", "/", nil},
		{"deep traversal", "../../../../etc/passwd", "/etc/passwd", nil},
		{"absolute unix path", "/etc/passwd", "/etc/passwd", nil},
		{"traversal in the middle", "a/../../b", "/b", nil},
		{"traversal back inside", "/a/b/../c", "/a/c", nil},
		{"traversal to the root and back", "/a/../..", "/", nil},
		{"dot segments", "/a/./b/./c", "/a/b/c", nil},
		{"trailing dot", "/a/.", "/a", nil},
		{"windows separators", `..\..\windows\system32`, "/windows/system32", nil},
		{"mixed separators", `a\../..\b`, "/b", nil},
		{"windows drive", `C:\secrets.txt`, "/C:/secrets.txt", nil},
		{"unc path", `\\server\share\f`, "/server/share/f", nil},

		// Normalisation of the boring cases.
		{"empty", "", "/", nil},
		{"just a slash", "/", "/", nil},
		{"dot", ".", "/", nil},
		{"double slashes", "//a///b//", "/a/b", nil},
		{"trailing slash", "/a/b/", "/a/b", nil},
		{"relative", "a/b", "/a/b", nil},

		// Outright rejections.
		{"embedded NUL", "/a\x00b", "", files.ErrInvalidPath},
		{"NUL only", "\x00", "", files.ErrInvalidPath},
		{"newline", "/a\nb", "", files.ErrInvalidPath},
		{"carriage return", "/a\rb", "", files.ErrInvalidPath},
		{"escape character", "/a\x1bb", "", files.ErrInvalidPath},
		{"delete character", "/a\x7fb", "", files.ErrInvalidPath},
		{"invalid utf-8", "/a\xffb", "", files.ErrInvalidPath},
		{"name too long", "/" + strings.Repeat("n", 256), "", files.ErrNameTooLong},
		{"path too long", "/" + strings.Repeat("a/", 3000), "", files.ErrPathTooLong},
		{"too deep", "/" + strings.Repeat("d/", 40), "", files.ErrTooDeep},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := v.Resolve(c.input)
			if c.err != nil {
				if !errors.Is(err, c.err) {
					t.Fatalf("Resolve(%q) = %q, %v; want error %v", c.input, got, err, c.err)
				}
				// Every rejection is an fs.ErrInvalid, which is what a provider
				// maps onto its own "bad filename" status.
				if !errors.Is(err, fs.ErrInvalid) {
					t.Errorf("Resolve(%q) error %v does not wrap fs.ErrInvalid", c.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q) = %v, want %q", c.input, err, c.want)
			}
			if got != c.want {
				t.Fatalf("Resolve(%q) = %q, want %q", c.input, got, c.want)
			}
			assertSafe(t, c.input, got)
		})
	}
}

// assertSafe restates the invariant every resolved path must hold, so a future
// change to the cleaning rules cannot quietly weaken it.
func assertSafe(t *testing.T, input, got string) {
	t.Helper()
	if !strings.HasPrefix(got, "/") {
		t.Errorf("Resolve(%q) = %q: not rooted", input, got)
	}
	if strings.Contains(got, `\`) {
		t.Errorf("Resolve(%q) = %q: a separator survived unnormalized", input, got)
	}
	if strings.Contains(got, "//") {
		t.Errorf("Resolve(%q) = %q: empty segment", input, got)
	}
	if got != "/" && strings.HasSuffix(got, "/") {
		t.Errorf("Resolve(%q) = %q: trailing slash", input, got)
	}
	for _, seg := range strings.Split(strings.TrimPrefix(got, "/"), "/") {
		if seg == "." || seg == ".." {
			t.Errorf("Resolve(%q) = %q: %q survived", input, got, seg)
		}
	}
}

// Traversal is refused at every entry point, not only at Resolve, because a
// provider calls the operations rather than the resolver.
func TestEveryOperationResolvesItsPath(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)
	ctx := context.Background()
	const hostile = "/a\x00b"

	ops := map[string]func() error{
		"Stat":      func() error { _, err := v.Stat(hostile); return err },
		"List":      func() error { _, err := v.List(hostile); return err },
		"Mkdir":     func() error { _, err := v.Mkdir(hostile, files.WriteOptions{}); return err },
		"MkdirAll":  func() error { _, err := v.MkdirAll(hostile, files.WriteOptions{}); return err },
		"Put":       func() error { _, err := v.PutBytes(ctx, hostile, []byte("x"), files.WriteOptions{}); return err },
		"Open":      func() error { _, err := v.Open(ctx, hostile); return err },
		"Create":    func() error { _, err := v.Create(ctx, hostile, files.WriteOptions{}); return err },
		"Remove":    func() error { _, err := v.Remove(ctx, hostile); return err },
		"RemoveAll": func() error { _, _, err := v.RemoveAll(ctx, hostile); return err },
		"RenameSrc": func() error { _, err := v.Rename(ctx, hostile, "/ok"); return err },
		"RenameDst": func() error { _, err := v.Rename(ctx, "/ok", hostile); return err },
		"Chtimes":   func() error { return v.Chtimes(hostile, time0) },
		"Truncate":  func() error { return v.Truncate(ctx, hostile, 0) },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			if err := op(); !errors.Is(err, files.ErrInvalidPath) {
				t.Errorf("%s with a NUL in the path = %v, want ErrInvalidPath", name, err)
			}
		})
	}
	if got := v.Stats(); got != (files.Stats{}) {
		t.Errorf("a rejected path created something: %+v", got)
	}
}

// Traversal normalizes rather than escapes: writing to "../../x" lands inside
// the tree at /x, and nothing above the root is ever named.
func TestTraversalStaysInsideTheRoot(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)
	ctx := context.Background()

	if _, err := v.MkdirAll("/safe", files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	n, err := v.PutBytes(ctx, "/safe/../../../../etc/passwd", []byte("root:x:0:0"), files.WriteOptions{Parents: true})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if n.Path != "/etc/passwd" {
		t.Fatalf("path = %q, want /etc/passwd inside the tree", n.Path)
	}
	// The whole tree is reachable from the root, so nothing was created
	// "above" it - there is no above.
	var seen []string
	if err := v.Walk(func(node files.Node) error {
		seen = append(seen, node.Path)
		if !strings.HasPrefix(node.Path, "/") {
			t.Errorf("node outside the root: %q", node.Path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, " ") != "/etc /etc/passwd /safe" {
		t.Errorf("tree = %v", seen)
	}
}

// A file name that is a plausible attack on whatever renders it is stored and
// listed verbatim - escaping is the renderer's job, not the filesystem's.
func TestHostileButLegalNamesAreKept(t *testing.T) {
	t.Parallel()
	v, _ := newVFS(t)
	ctx := context.Background()

	names := []string{
		`<img src=x onerror=alert(1)>.txt`,
		`<b>not bold`,
		`" onmouseover="alert(1)`,
		`a b c.txt`,
		`ümlaut.txt`,
		`%2e%2e%2fetc%2fpasswd`,
		`-rf`,
		`file.txt `,
	}
	for _, name := range names {
		if _, err := v.PutBytes(ctx, "/"+name, []byte("x"), files.WriteOptions{}); err != nil {
			t.Fatalf("put %q: %v", name, err)
		}
	}
	entries, err := v.List("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(names) {
		t.Fatalf("listed %d entries, want %d", len(entries), len(names))
	}
	for _, name := range names {
		if !v.Exists("/" + name) {
			t.Errorf("%q was not stored under its own name", name)
		}
	}
}

// time0 is the zero time, used where a test only cares that the path was
// rejected before anything else happened.
var time0 = time.Time{}
