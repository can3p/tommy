package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/can3p/tommy/core/blob"
	blobmem "github.com/can3p/tommy/core/blob/memory"
)

// Errors returned by the VFS. Every one of them is wrapped in an *fs.PathError
// naming the operation and the path, so a provider can both switch on the
// sentinel and report something useful to its client:
//
//	if errors.Is(err, files.ErrNotExist) { return ftpCode550 }
//
// The io/fs sentinels are reused where one exists, which means errors.Is with
// fs.ErrNotExist, fs.ErrExist, fs.ErrInvalid and fs.ErrClosed also works - the
// form the afero and pkg/sftp adapters already speak.
var (
	ErrNotExist = fs.ErrNotExist
	ErrExist    = fs.ErrExist
	ErrClosed   = fs.ErrClosed

	ErrInvalidPath = fmt.Errorf("files: invalid path: %w", fs.ErrInvalid)
	ErrNameTooLong = fmt.Errorf("files: name is too long: %w", fs.ErrInvalid)
	ErrPathTooLong = fmt.Errorf("files: path is too long: %w", fs.ErrInvalid)
	ErrTooDeep     = fmt.Errorf("files: path nests too deeply: %w", fs.ErrInvalid)

	ErrNotDir   = errors.New("files: not a directory")
	ErrIsDir    = errors.New("files: is a directory")
	ErrNotEmpty = errors.New("files: directory not empty")

	ErrDirFull      = errors.New("files: directory is full")
	ErrTreeFull     = errors.New("files: too many entries in the filesystem")
	ErrFileTooLarge = errors.New("files: file is too large")

	ErrReadOnly = errors.New("files: file is not open for writing")
)

// pathErr wraps a sentinel with the operation and path that produced it.
func pathErr(op, name string, err error) error {
	return &fs.PathError{Op: op, Path: name, Err: err}
}

// Limits bound what a client can build in the tree. tommy is pointed at by
// whatever the application under test happens to do, and an FTP client is a
// stranger by design, so none of these are optional: without them a loop that
// keeps calling MKD is an out-of-memory error rather than a failed test.
type Limits struct {
	// MaxDepth is how many segments a path may have. 0 means DefaultLimits'.
	MaxDepth int
	// MaxNameLen is the longest single segment, in bytes.
	MaxNameLen int
	// MaxPathLen is the longest whole path, in bytes.
	MaxPathLen int
	// MaxDirEntries is how many children one directory may hold.
	MaxDirEntries int
	// MaxNodes is how many files and directories the whole tree may hold.
	MaxNodes int
	// MaxFileSize is the largest single file, in bytes. The blob store's own
	// cap still applies on top of it.
	MaxFileSize int64
}

// DefaultLimits are generous enough for any realistic test fixture and small
// enough that a hostile client cannot exhaust memory.
var DefaultLimits = Limits{
	MaxDepth:      32,
	MaxNameLen:    255,
	MaxPathLen:    4096,
	MaxDirEntries: 4096,
	MaxNodes:      50000,
	MaxFileSize:   64 << 20,
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits
	if l.MaxDepth <= 0 {
		l.MaxDepth = d.MaxDepth
	}
	if l.MaxNameLen <= 0 {
		l.MaxNameLen = d.MaxNameLen
	}
	if l.MaxPathLen <= 0 {
		l.MaxPathLen = d.MaxPathLen
	}
	if l.MaxDirEntries <= 0 {
		l.MaxDirEntries = d.MaxDirEntries
	}
	if l.MaxNodes <= 0 {
		l.MaxNodes = d.MaxNodes
	}
	if l.MaxFileSize <= 0 {
		l.MaxFileSize = d.MaxFileSize
	}
	return l
}

// Node is a snapshot of one entry in the tree. It is a value: holding one does
// not pin the tree, and mutating it changes nothing.
type Node struct {
	// Name is the last path segment. The root's name is "/".
	Name string `json:"name"`
	// Path is the cleaned, rooted path: "/", "/a", "/a/b.txt".
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
	// Size is the file's byte count; 0 for a directory.
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	// Provider is the name of the provider that last wrote this entry -
	// "ftp", "sftp" - or "" when it was created outside a provider.
	Provider string `json:"provider,omitempty"`
	// ContentType is the file's media type, sniffed at write time.
	ContentType string `json:"content_type,omitempty"`
	// Blob points at the bytes. Zero for a directory. Bytes never live in the
	// node and never live in an event: they live in the blob store, whose
	// lifetime is independent of the event ring buffer.
	Blob blob.Ref `json:"blob,omitzero"`
}

// FileInfo adapts the node to the fs.FileInfo that ftpserverlib and pkg/sftp
// both want back from a stat. Sys returns the Node itself.
func (n Node) FileInfo() fs.FileInfo { return nodeInfo{n} }

type nodeInfo struct{ n Node }

func (i nodeInfo) Name() string { return i.n.Name }
func (i nodeInfo) Size() int64  { return i.n.Size }
func (i nodeInfo) Mode() fs.FileMode {
	if i.n.Dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (i nodeInfo) ModTime() time.Time { return i.n.ModTime }
func (i nodeInfo) IsDir() bool        { return i.n.Dir }
func (i nodeInfo) Sys() any           { return i.n }

// WriteOptions describe a mutation. Every field is optional.
type WriteOptions struct {
	// Provider is recorded on the node, so the UI can show which protocol
	// wrote which file.
	Provider string
	// ContentType overrides the type sniffed from the name and the bytes.
	ContentType string
	// ModTime overrides the VFS clock.
	ModTime time.Time
	// Parents creates any missing parent directory, the way MkdirAll and
	// curl's --ftp-create-dirs do. Without it a missing parent is ErrNotExist,
	// which is what a real FTP server answers to STOR into nowhere.
	Parents bool
}

// Stats counts what the tree currently holds.
type Stats struct {
	Dirs  int   `json:"dirs"`
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// dirNode and fileNode are the tree itself. Neither is ever handed out; every
// read returns a Node value instead, so a caller can never mutate the tree by
// holding on to something it was given.
type dirNode struct {
	name     string
	parent   *dirNode
	modTime  time.Time
	provider string
	dirs     map[string]*dirNode
	files    map[string]*fileNode
}

type fileNode struct {
	name        string
	modTime     time.Time
	provider    string
	contentType string
	ref         blob.Ref
}

func (d *dirNode) len() int { return len(d.dirs) + len(d.files) }

// path walks back to the root. Nodes are shallow by construction (MaxDepth),
// so this is cheap and avoids storing a path that renaming would invalidate.
func (d *dirNode) path() string {
	if d.parent == nil {
		return "/"
	}
	segs := []string{}
	for cur := d; cur.parent != nil; cur = cur.parent {
		segs = append(segs, cur.name)
	}
	for i, j := 0, len(segs)-1; i < j; i, j = i+1, j-1 {
		segs[i], segs[j] = segs[j], segs[i]
	}
	return "/" + strings.Join(segs, "/")
}

func (d *dirNode) node() Node {
	name := d.name
	if d.parent == nil {
		name = "/"
	}
	return Node{Name: name, Path: d.path(), Dir: true, ModTime: d.modTime, Provider: d.provider}
}

func (f *fileNode) node(dir *dirNode) Node {
	return Node{
		Name:        f.name,
		Path:        join(dir.path(), f.name),
		Size:        f.ref.Size,
		ModTime:     f.modTime,
		Provider:    f.provider,
		ContentType: f.contentType,
		Blob:        f.ref,
	}
}

// join is path.Join for one already-clean directory plus one segment.
func join(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}

// VFS is the in-memory filesystem every files provider shares: FTP, SFTP and
// anything added later all mutate the same tree, so a file uploaded over one
// protocol is listed and downloadable over the others and in the UI.
//
// It is the one genuinely concurrency-sensitive component in tommy. Two
// providers write to it from their own goroutines, so every operation is
// guarded by a single RWMutex - and blob I/O deliberately happens outside that
// lock, so a slow upload can never stall a listing.
//
// It is also a security boundary. Path resolution lives in exactly one place,
// Resolve, and every method funnels through it; there is no way for a provider
// to reach the tree with an unresolved path, and there is nothing outside the
// tree to reach - the VFS never touches the host filesystem at all.
type VFS struct {
	mu    sync.RWMutex
	root  *dirNode
	nodes int

	limits Limits
	now    func() time.Time

	blobMu   sync.RWMutex
	blobs    blob.BlobStore
	attached bool
}

// Option configures a VFS.
type Option func(*VFS)

// WithLimits replaces the default bounds. Zero fields keep their default.
func WithLimits(l Limits) Option {
	return func(v *VFS) { v.limits = l.withDefaults() }
}

// WithClock injects the clock used for modification times.
func WithClock(now func() time.Time) Option {
	return func(v *VFS) {
		if now != nil {
			v.now = now
		}
	}
}

// WithBlobs sets the blob store the file bytes live in. In a running tommy the
// plugin attaches the core's store instead; this is for a VFS used on its own.
func WithBlobs(b blob.BlobStore) Option {
	return func(v *VFS) {
		if b != nil {
			v.blobs = b
		}
	}
}

// NewVFS returns an empty filesystem holding only its root directory.
//
// With no WithBlobs option it starts with a private in-memory blob store so it
// is usable standalone; Attach then swaps in the core's store before anything
// is written.
func NewVFS(opts ...Option) *VFS {
	v := &VFS{
		limits: DefaultLimits,
		now:    time.Now,
	}
	for _, o := range opts {
		o(v)
	}
	if v.blobs == nil {
		v.blobs = blobmem.New(0)
	} else {
		v.attached = true
	}
	v.root = &dirNode{
		modTime: v.now(),
		dirs:    map[string]*dirNode{},
		files:   map[string]*fileNode{},
	}
	return v
}

// Attach points the VFS at the core's blob store. It is called by the plugin
// when its API and UI are mounted and by NewSession, so a provider never has to
// think about it.
//
// The first non-nil store wins and later calls are ignored: in a running tommy
// every surface is handed the same store, and silently switching stores under a
// file that has already been written would break its download link.
func (v *VFS) Attach(b blob.BlobStore) {
	if b == nil {
		return
	}
	v.blobMu.Lock()
	defer v.blobMu.Unlock()
	if v.attached {
		return
	}
	v.blobs = b
	v.attached = true
}

// Blobs returns the blob store the file bytes live in.
func (v *VFS) Blobs() blob.BlobStore {
	v.blobMu.RLock()
	defer v.blobMu.RUnlock()
	return v.blobs
}

// Limits reports the bounds in force.
func (v *VFS) Limits() Limits { return v.limits }

// ---------------------------------------------------------------------------
// Path resolution - the security boundary
// ---------------------------------------------------------------------------

// Resolve turns a path as a client typed it into the cleaned, rooted path the
// tree is keyed by. Every VFS method calls it; nothing else in the plugin, and
// nothing in any provider, is allowed to interpret a path itself.
//
// What it does, in order:
//
//   - rejects a path containing a NUL byte, a control character, or invalid
//     UTF-8 (all of which exist only to confuse whatever renders the name);
//   - normalizes Windows separators, so "..\.." cannot sneak past a check that
//     only looked for "../";
//   - makes the path absolute and cleans it, which resolves ".", collapses
//     empty segments, and applies ".." - clamped at the root the way a chroot
//     is, so "../../etc/passwd" resolves to "/etc/passwd" *inside the tree*
//     and can never name anything outside it;
//   - enforces the depth, name-length and path-length limits.
//
// The result always starts with "/", never contains "." or ".." and never ends
// with "/" (except the root itself). Note that there is no host filesystem
// underneath any of this: the tree is a map in memory, so even a bug here
// cannot read or write a real file.
func (v *VFS) Resolve(name string) (string, error) {
	if len(name) > v.limits.MaxPathLen {
		return "", pathErr("resolve", truncateForError(name), ErrPathTooLong)
	}
	if !utf8.ValidString(name) {
		return "", pathErr("resolve", truncateForError(name), ErrInvalidPath)
	}
	if strings.ContainsAny(name, "\x00") {
		return "", pathErr("resolve", truncateForError(name), ErrInvalidPath)
	}
	for _, r := range name {
		// Control characters have no business in a filename and are how a
		// hostile name tries to break a log line or a terminal.
		if r < 0x20 || r == 0x7f {
			return "", pathErr("resolve", truncateForError(name), ErrInvalidPath)
		}
	}

	// A Windows client says "dir\file", and "..\.." must not survive a check
	// that only knows about "/".
	normalized := strings.ReplaceAll(name, `\`, "/")
	clean := path.Clean("/" + normalized)
	if clean == "/" {
		return "/", nil
	}
	if len(clean) > v.limits.MaxPathLen {
		return "", pathErr("resolve", truncateForError(name), ErrPathTooLong)
	}

	segs := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(segs) > v.limits.MaxDepth {
		return "", pathErr("resolve", clean, ErrTooDeep)
	}
	for _, s := range segs {
		// path.Clean guarantees this for a rooted path; assert it anyway,
		// because it is the whole security property and it is one comparison.
		if s == "" || s == "." || s == ".." {
			return "", pathErr("resolve", clean, ErrInvalidPath)
		}
		if len(s) > v.limits.MaxNameLen {
			return "", pathErr("resolve", clean, ErrNameTooLong)
		}
	}
	return clean, nil
}

// truncateForError keeps a hostile path from filling a log line.
func truncateForError(name string) string {
	const max = 120
	if len(name) <= max {
		return name
	}
	return name[:max] + "..."
}

// split returns the parent directory and the final segment of a resolved path.
func split(clean string) (dir, name string) {
	i := strings.LastIndexByte(clean, '/')
	if i <= 0 {
		return "/", clean[1:]
	}
	return clean[:i], clean[i+1:]
}

// ---------------------------------------------------------------------------
// Lookups. All of these expect the caller to hold at least a read lock.
// ---------------------------------------------------------------------------

func (v *VFS) dirAt(clean string) (*dirNode, error) {
	cur := v.root
	if clean == "/" {
		return cur, nil
	}
	for _, seg := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		next, ok := cur.dirs[seg]
		if !ok {
			if _, isFile := cur.files[seg]; isFile {
				return nil, ErrNotDir
			}
			return nil, ErrNotExist
		}
		cur = next
	}
	return cur, nil
}

// nodeAt finds either kind of entry.
func (v *VFS) nodeAt(clean string) (Node, error) {
	if clean == "/" {
		return v.root.node(), nil
	}
	dirPath, name := split(clean)
	dir, err := v.dirAt(dirPath)
	if err != nil {
		return Node{}, err
	}
	if d, ok := dir.dirs[name]; ok {
		return d.node(), nil
	}
	if f, ok := dir.files[name]; ok {
		return f.node(dir), nil
	}
	return Node{}, ErrNotExist
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// Root returns the root directory.
func (v *VFS) Root() Node {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.root.node()
}

// Stat returns one entry. It answers FTP's SIZE and MDTM and SFTP's Stat and
// Lstat; there are no symlinks, so the two are the same thing.
func (v *VFS) Stat(name string) (Node, error) {
	clean, err := v.Resolve(name)
	if err != nil {
		return Node{}, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	n, err := v.nodeAt(clean)
	if err != nil {
		return Node{}, pathErr("stat", clean, err)
	}
	return n, nil
}

// Exists reports whether anything is at name. An unresolvable path is false
// rather than an error, because that is the only useful answer to "is it
// there".
func (v *VFS) Exists(name string) bool {
	_, err := v.Stat(name)
	return err == nil
}

// IsDir reports whether name is an existing directory.
func (v *VFS) IsDir(name string) bool {
	n, err := v.Stat(name)
	return err == nil && n.Dir
}

// List returns the children of a directory, directories first and then files,
// each alphabetically - FTP's LIST and NLST and SFTP's List. Listing a file is
// ErrNotDir, not a one-entry listing: providers rely on telling the two apart.
func (v *VFS) List(name string) ([]Node, error) {
	clean, err := v.Resolve(name)
	if err != nil {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	dir, err := v.dirAt(clean)
	if err != nil {
		return nil, pathErr("list", clean, err)
	}
	out := make([]Node, 0, dir.len())
	names := make([]string, 0, len(dir.dirs))
	for n := range dir.dirs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, dir.dirs[n].node())
	}
	names = names[:0]
	for n := range dir.files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, dir.files[n].node(dir))
	}
	return out, nil
}

// Walk visits every entry below the root, parents before children, in the same
// order List uses. fn must not call back into the VFS: the tree is read-locked
// for the whole walk.
func (v *VFS) Walk(fn func(Node) error) error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.walk(v.root, fn)
}

func (v *VFS) walk(dir *dirNode, fn func(Node) error) error {
	names := make([]string, 0, len(dir.dirs))
	for n := range dir.dirs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		child := dir.dirs[n]
		if err := fn(child.node()); err != nil {
			return err
		}
		if err := v.walk(child, fn); err != nil {
			return err
		}
	}
	names = names[:0]
	for n := range dir.files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := fn(dir.files[n].node(dir)); err != nil {
			return err
		}
	}
	return nil
}

// Stats counts what the tree holds, excluding the root itself.
func (v *VFS) Stats() Stats {
	v.mu.RLock()
	defer v.mu.RUnlock()
	var s Stats
	_ = v.walk(v.root, func(n Node) error {
		if n.Dir {
			s.Dirs++
		} else {
			s.Files++
			s.Bytes += n.Size
		}
		return nil
	})
	return s
}

// ---------------------------------------------------------------------------
// Directory mutations
// ---------------------------------------------------------------------------

// Mkdir creates one directory. Its parent must already exist unless
// opt.Parents is set, and an existing entry is ErrExist - which is what MKD
// and SFTP's Mkdir both want.
func (v *VFS) Mkdir(name string, opt WriteOptions) (Node, error) {
	clean, err := v.Resolve(name)
	if err != nil {
		return Node{}, err
	}
	if clean == "/" {
		return Node{}, pathErr("mkdir", clean, ErrExist)
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	dirPath, base := split(clean)
	parent, err := v.dirAt(dirPath)
	if err != nil {
		if !errors.Is(err, ErrNotExist) || !opt.Parents {
			return Node{}, pathErr("mkdir", clean, err)
		}
		if parent, err = v.mkdirAllLocked(dirPath, opt); err != nil {
			return Node{}, err
		}
	}
	if _, ok := parent.dirs[base]; ok {
		return Node{}, pathErr("mkdir", clean, ErrExist)
	}
	if _, ok := parent.files[base]; ok {
		return Node{}, pathErr("mkdir", clean, ErrExist)
	}
	d, err := v.newDirLocked(parent, base, opt)
	if err != nil {
		return Node{}, pathErr("mkdir", clean, err)
	}
	return d.node(), nil
}

// MkdirAll creates a directory and every missing parent, and is happy when the
// directory is already there. An existing *file* in the way is still an error.
func (v *VFS) MkdirAll(name string, opt WriteOptions) (Node, error) {
	clean, err := v.Resolve(name)
	if err != nil {
		return Node{}, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	d, err := v.mkdirAllLocked(clean, opt)
	if err != nil {
		return Node{}, err
	}
	return d.node(), nil
}

func (v *VFS) mkdirAllLocked(clean string, opt WriteOptions) (*dirNode, error) {
	cur := v.root
	if clean == "/" {
		return cur, nil
	}
	for _, seg := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		if next, ok := cur.dirs[seg]; ok {
			cur = next
			continue
		}
		if _, isFile := cur.files[seg]; isFile {
			return nil, pathErr("mkdir", join(cur.path(), seg), ErrNotDir)
		}
		next, err := v.newDirLocked(cur, seg, opt)
		if err != nil {
			return nil, pathErr("mkdir", join(cur.path(), seg), err)
		}
		cur = next
	}
	return cur, nil
}

// newDirLocked adds one child directory, enforcing the bounds.
func (v *VFS) newDirLocked(parent *dirNode, name string, opt WriteOptions) (*dirNode, error) {
	if err := v.roomForLocked(parent); err != nil {
		return nil, err
	}
	d := &dirNode{
		name:     name,
		parent:   parent,
		modTime:  v.stamp(opt.ModTime),
		provider: opt.Provider,
		dirs:     map[string]*dirNode{},
		files:    map[string]*fileNode{},
	}
	parent.dirs[name] = d
	parent.modTime = d.modTime
	v.nodes++
	return d, nil
}

func (v *VFS) roomForLocked(parent *dirNode) error {
	if parent.len() >= v.limits.MaxDirEntries {
		return ErrDirFull
	}
	if v.nodes >= v.limits.MaxNodes {
		return ErrTreeFull
	}
	return nil
}

func (v *VFS) stamp(t time.Time) time.Time {
	if !t.IsZero() {
		return t
	}
	return v.now()
}

// ---------------------------------------------------------------------------
// File writes
// ---------------------------------------------------------------------------

// Put streams r into the blob store and installs it at name, replacing
// whatever was there. This is FTP's STOR and the simple half of SFTP's put.
//
// The bytes are read outside the tree lock, so a slow upload never blocks a
// listing, and the node is installed in one locked step, so a listing never
// sees a half-written file. If the destination's parent disappears while the
// upload is in flight the write fails and its blob is discarded rather than
// left orphaned.
func (v *VFS) Put(ctx context.Context, name string, r io.Reader, opt WriteOptions) (Node, error) {
	clean, err := v.Resolve(name)
	if err != nil {
		return Node{}, err
	}
	if clean == "/" {
		return Node{}, pathErr("put", clean, ErrIsDir)
	}
	if opt.Parents {
		dirPath, _ := split(clean)
		if _, err := v.MkdirAll(dirPath, opt); err != nil {
			return Node{}, err
		}
	}

	_, base := split(clean)
	ref, err := v.store(ctx, base, r, opt)
	if err != nil {
		return Node{}, pathErr("put", clean, err)
	}
	n, err := v.install(ctx, clean, ref, opt)
	if err != nil {
		v.discard(ctx, ref)
		return Node{}, err
	}
	return n, nil
}

// PutBytes is Put for content already in memory.
func (v *VFS) PutBytes(ctx context.Context, name string, data []byte, opt WriteOptions) (Node, error) {
	return v.Put(ctx, name, bytes.NewReader(data), opt)
}

// store copies the content into the blob store, refusing anything over the
// file-size limit. It reads one byte past the limit and then rejects, so an
// oversized upload is an error rather than a silent truncation.
func (v *VFS) store(ctx context.Context, base string, r io.Reader, opt WriteOptions) (blob.Ref, error) {
	limited := &limitReader{r: r, left: v.limits.MaxFileSize + 1}
	ref, err := v.Blobs().Put(ctx, limited, blob.Ref{Filename: base, ContentType: opt.ContentType})
	if err != nil {
		return blob.Ref{}, err
	}
	if ref.Size > v.limits.MaxFileSize {
		v.discard(ctx, ref)
		return blob.Ref{}, ErrFileTooLarge
	}
	return ref, nil
}

// install puts the finished blob into the tree, replacing any previous file at
// the same path and freeing its bytes.
func (v *VFS) install(ctx context.Context, clean string, ref blob.Ref, opt WriteOptions) (Node, error) {
	dirPath, base := split(clean)

	// Sniffing reads from the blob store, so it happens before the tree lock
	// is taken: no blob I/O ever runs while the tree is locked.
	ct := opt.ContentType
	if ct == "" {
		ct = v.sniff(ctx, base, ref)
	}

	v.mu.Lock()
	parent, err := v.dirAt(dirPath)
	if err != nil {
		v.mu.Unlock()
		return Node{}, pathErr("put", clean, err)
	}
	if _, isDir := parent.dirs[base]; isDir {
		v.mu.Unlock()
		return Node{}, pathErr("put", clean, ErrIsDir)
	}
	prev, replacing := parent.files[base]
	if !replacing {
		if err := v.roomForLocked(parent); err != nil {
			v.mu.Unlock()
			return Node{}, pathErr("put", clean, err)
		}
		v.nodes++
	}
	f := &fileNode{
		name:        base,
		modTime:     v.stamp(opt.ModTime),
		provider:    opt.Provider,
		contentType: ct,
		ref:         ref,
	}
	f.ref.ContentType = ct
	f.ref.Filename = base
	parent.files[base] = f
	parent.modTime = f.modTime
	n := f.node(parent)
	v.mu.Unlock()

	// Freeing the replaced bytes happens outside the lock, and after the new
	// node is visible, so no reader is ever pointed at a deleted blob. An
	// already-open download keeps working: the store hands out a snapshot.
	if replacing && prev.ref.ID != "" && prev.ref.ID != ref.ID {
		v.discard(ctx, prev.ref)
	}
	return n, nil
}

// sniff decides a media type from the name, then from the first bytes.
func (v *VFS) sniff(ctx context.Context, name string, ref blob.Ref) string {
	if ext := path.Ext(name); ext != "" {
		if ct := mime.TypeByExtension(ext); ct != "" {
			return ct
		}
	}
	rc, _, err := v.Blobs().Open(ctx, ref.ID)
	if err != nil {
		return "application/octet-stream"
	}
	defer func() { _ = rc.Close() }()
	head := make([]byte, 512)
	n, _ := io.ReadFull(rc, head)
	if n == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(head[:n])
}

// discard drops a blob that never made it into the tree, or no longer belongs
// to it. A failure here is not worth surfacing: it costs memory, not data.
func (v *VFS) discard(ctx context.Context, ref blob.Ref) {
	if ref.ID == "" {
		return
	}
	_ = v.Blobs().Delete(ctx, ref.ID)
}

// ---------------------------------------------------------------------------
// Removal, renaming, metadata
// ---------------------------------------------------------------------------

// Remove deletes a file or an empty directory - DELE and RMD, and SFTP's
// Remove and Rmdir. A non-empty directory is ErrNotEmpty; use RemoveAll.
func (v *VFS) Remove(ctx context.Context, name string) (Node, error) {
	n, _, err := v.remove(ctx, name, false)
	return n, err
}

// RemoveAll deletes a file, or a directory and everything under it, and
// returns the number of entries that went with it.
func (v *VFS) RemoveAll(ctx context.Context, name string) (Node, int, error) {
	return v.remove(ctx, name, true)
}

func (v *VFS) remove(ctx context.Context, name string, recursive bool) (Node, int, error) {
	clean, err := v.Resolve(name)
	if err != nil {
		return Node{}, 0, err
	}
	op := "remove"
	if recursive {
		op = "removeall"
	}
	if clean == "/" {
		// Emptying the whole tree is Clear, which says so at the call site.
		return Node{}, 0, pathErr(op, clean, ErrInvalidPath)
	}

	dirPath, base := split(clean)

	v.mu.Lock()
	parent, err := v.dirAt(dirPath)
	if err != nil {
		v.mu.Unlock()
		return Node{}, 0, pathErr(op, clean, err)
	}
	var (
		removed Node
		refs    []blob.Ref
		count   int
	)
	switch {
	case parent.files[base] != nil:
		f := parent.files[base]
		removed = f.node(parent)
		refs = append(refs, f.ref)
		delete(parent.files, base)
		v.nodes--
		count = 1
	case parent.dirs[base] != nil:
		d := parent.dirs[base]
		if d.len() > 0 && !recursive {
			v.mu.Unlock()
			return Node{}, 0, pathErr(op, clean, ErrNotEmpty)
		}
		removed = d.node()
		count = 1
		_ = v.walk(d, func(n Node) error {
			count++
			if !n.Dir {
				refs = append(refs, n.Blob)
			}
			return nil
		})
		delete(parent.dirs, base)
		v.nodes -= count
	default:
		v.mu.Unlock()
		return Node{}, 0, pathErr(op, clean, ErrNotExist)
	}
	parent.modTime = v.now()
	v.mu.Unlock()

	for _, ref := range refs {
		v.discard(ctx, ref)
	}
	return removed, count, nil
}

// Clear empties the tree, freeing every blob it held, and reports how many
// entries went. It is what the tab's Clear button and DELETE /tree do.
func (v *VFS) Clear(ctx context.Context) (int, error) {
	v.mu.Lock()
	var (
		refs  []blob.Ref
		count int
	)
	_ = v.walk(v.root, func(n Node) error {
		count++
		if !n.Dir {
			refs = append(refs, n.Blob)
		}
		return nil
	})
	v.root.dirs = map[string]*dirNode{}
	v.root.files = map[string]*fileNode{}
	v.root.modTime = v.now()
	v.nodes = 0
	v.mu.Unlock()

	for _, ref := range refs {
		v.discard(ctx, ref)
	}
	return count, nil
}

// Rename moves a file or a whole directory - RNFR/RNTO and SFTP's Rename.
// The destination's parent must exist; an existing destination is replaced
// when both sides are files, and is an error otherwise, which is the behavior
// clients expect from a POSIX rename.
func (v *VFS) Rename(ctx context.Context, oldName, newName string) (Node, error) {
	oldClean, err := v.Resolve(oldName)
	if err != nil {
		return Node{}, err
	}
	newClean, err := v.Resolve(newName)
	if err != nil {
		return Node{}, err
	}
	if oldClean == "/" || newClean == "/" {
		return Node{}, pathErr("rename", "/", ErrInvalidPath)
	}
	if oldClean == newClean {
		return v.Stat(oldClean)
	}
	// Moving a directory inside itself would detach the subtree from the root.
	if strings.HasPrefix(newClean+"/", oldClean+"/") {
		return Node{}, pathErr("rename", newClean, ErrInvalidPath)
	}

	oldDirPath, oldBase := split(oldClean)
	newDirPath, newBase := split(newClean)

	v.mu.Lock()
	srcParent, err := v.dirAt(oldDirPath)
	if err != nil {
		v.mu.Unlock()
		return Node{}, pathErr("rename", oldClean, err)
	}
	dstParent, err := v.dirAt(newDirPath)
	if err != nil {
		v.mu.Unlock()
		return Node{}, pathErr("rename", newClean, err)
	}

	var replaced blob.Ref
	switch {
	case srcParent.files[oldBase] != nil:
		f := srcParent.files[oldBase]
		if _, isDir := dstParent.dirs[newBase]; isDir {
			v.mu.Unlock()
			return Node{}, pathErr("rename", newClean, ErrIsDir)
		}
		prev, replacing := dstParent.files[newBase]
		if !replacing {
			if err := v.roomForLocked(dstParent); err != nil {
				v.mu.Unlock()
				return Node{}, pathErr("rename", newClean, err)
			}
		} else {
			replaced = prev.ref
			v.nodes--
		}
		delete(srcParent.files, oldBase)
		f.name = newBase
		f.modTime = v.now()
		f.ref.Filename = newBase
		dstParent.files[newBase] = f
	case srcParent.dirs[oldBase] != nil:
		d := srcParent.dirs[oldBase]
		if _, taken := dstParent.dirs[newBase]; taken {
			v.mu.Unlock()
			return Node{}, pathErr("rename", newClean, ErrExist)
		}
		if _, taken := dstParent.files[newBase]; taken {
			v.mu.Unlock()
			return Node{}, pathErr("rename", newClean, ErrNotDir)
		}
		if err := v.roomForLocked(dstParent); err != nil {
			v.mu.Unlock()
			return Node{}, pathErr("rename", newClean, err)
		}
		if depth(newClean)+v.depthLocked(d) > v.limits.MaxDepth {
			v.mu.Unlock()
			return Node{}, pathErr("rename", newClean, ErrTooDeep)
		}
		delete(srcParent.dirs, oldBase)
		d.name = newBase
		d.parent = dstParent
		d.modTime = v.now()
		dstParent.dirs[newBase] = d
	default:
		v.mu.Unlock()
		return Node{}, pathErr("rename", oldClean, ErrNotExist)
	}
	srcParent.modTime = v.now()
	dstParent.modTime = v.now()
	n, err := v.nodeAt(newClean)
	v.mu.Unlock()

	if replaced.ID != "" {
		v.discard(ctx, replaced)
	}
	if err != nil {
		return Node{}, pathErr("rename", newClean, err)
	}
	return n, nil
}

// depth counts the segments of a resolved path.
func depth(clean string) int {
	if clean == "/" {
		return 0
	}
	return strings.Count(clean, "/")
}

// depthLocked returns how deep the subtree under d goes, so a rename cannot
// smuggle a deep tree past MaxDepth by moving it downwards.
func (v *VFS) depthLocked(d *dirNode) int {
	deepest := 0
	for _, child := range d.dirs {
		if n := 1 + v.depthLocked(child); n > deepest {
			deepest = n
		}
	}
	if len(d.files) > 0 && deepest == 0 {
		deepest = 1
	}
	return deepest
}

// Chtimes sets an entry's modification time - FTP's MFMT and SFTP's setstat.
func (v *VFS) Chtimes(name string, mtime time.Time) error {
	clean, err := v.Resolve(name)
	if err != nil {
		return err
	}
	if mtime.IsZero() {
		mtime = v.now()
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if clean == "/" {
		v.root.modTime = mtime
		return nil
	}
	dirPath, base := split(clean)
	parent, err := v.dirAt(dirPath)
	if err != nil {
		return pathErr("chtimes", clean, err)
	}
	if d, ok := parent.dirs[base]; ok {
		d.modTime = mtime
		return nil
	}
	if f, ok := parent.files[base]; ok {
		f.modTime = mtime
		return nil
	}
	return pathErr("chtimes", clean, ErrNotExist)
}

// Truncate resizes a file, padding with zeros when it grows. SFTP's setstat
// asks for this; FTP does not.
func (v *VFS) Truncate(ctx context.Context, name string, size int64) error {
	clean, err := v.Resolve(name)
	if err != nil {
		return err
	}
	if size < 0 || size > v.limits.MaxFileSize {
		return pathErr("truncate", clean, ErrFileTooLarge)
	}
	f, err := v.OpenFile(ctx, name, OpenReadWrite, WriteOptions{})
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Abort()
		return err
	}
	return f.Close()
}

// ---------------------------------------------------------------------------
// Reading and writing through a handle
// ---------------------------------------------------------------------------

// Flags for OpenFile. They are aliases of the os flags, not a parallel set:
// ftpserverlib hands its driver an os flag word and pkg/sftp derives one from
// the SSH_FXF_* bits, so a provider passes what it already has straight
// through. The numeric values differ per platform, which is exactly why these
// are aliases rather than literals.
const (
	OpenRead      = os.O_RDONLY
	OpenWrite     = os.O_WRONLY
	OpenReadWrite = os.O_RDWR

	OpenCreate   = os.O_CREATE
	OpenTruncate = os.O_TRUNC
	OpenAppend   = os.O_APPEND
	// OpenExclusive fails when the file already exists.
	OpenExclusive = os.O_EXCL
)

// Open opens a file for reading. The returned handle streams straight out of
// the blob store, so a large download never copies the file into memory.
func (v *VFS) Open(ctx context.Context, name string) (*File, error) {
	return v.OpenFile(ctx, name, OpenRead, WriteOptions{})
}

// Create opens a file for writing, creating it or replacing what is there.
func (v *VFS) Create(ctx context.Context, name string, opt WriteOptions) (*File, error) {
	return v.OpenFile(ctx, name, OpenWrite|OpenCreate|OpenTruncate, opt)
}

// ReadFile reads a whole file into memory. Convenient for a test; a provider
// should stream with Open instead.
func (v *VFS) ReadFile(ctx context.Context, name string) ([]byte, error) {
	f, err := v.Open(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// limitReader fails rather than truncating once the file-size limit is hit, so
// a client is told its upload was rejected instead of quietly losing its tail.
type limitReader struct {
	r    io.Reader
	left int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, ErrFileTooLarge
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}
