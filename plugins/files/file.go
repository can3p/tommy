package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"path"
	"sync"

	"github.com/can3p/tommy/core/blob"
)

// File is an open handle onto the VFS. It is what both providers hand to their
// protocol library: it satisfies io.Reader, io.ReaderAt, io.Writer,
// io.WriterAt, io.Seeker and io.Closer, which between them cover afero.File
// and pkg/sftp's FileGet and FilePut handlers.
//
// Reads stream straight out of the blob store, so downloading a large file
// costs no memory. Writes are buffered and committed as one atomic step in
// Close: a listing never sees a half-written file, and a transfer abandoned
// halfway leaves no entry behind at all. SFTP writes arrive out of order at
// arbitrary offsets, which is why the buffer exists rather than a stream.
//
// A handle is safe for concurrent use, but two handles writing to one path
// still race the way two writers always do - last Close wins, and the loser's
// bytes are freed.
type File struct {
	v    *VFS
	path string
	opt  WriteOptions

	// ctx is the context the handle was opened with; Close commits with it.
	// A provider whose request context dies at end-of-transfer should call
	// CloseContext instead so the commit still lands.
	ctx context.Context

	mu       sync.Mutex
	closed   bool
	writable bool
	appends  bool

	// read side
	rc   io.ReadSeekCloser
	node Node

	// directory side: a read handle on a directory lists instead of reading
	entries []Node

	// write side
	buf       []byte
	pos       int64
	committed bool
	onCommit  func(context.Context, Node) error
}

// OpenFile is the general open. flag takes the os.O_* values a provider
// already has in hand (OpenRead, OpenWrite, OpenReadWrite, OpenCreate,
// OpenTruncate, OpenAppend, OpenExclusive), and opt is ignored for a read.
//
// Opening a directory for reading succeeds and yields a handle whose Readdir
// works; reading its bytes is ErrIsDir.
func (v *VFS) OpenFile(ctx context.Context, name string, flag int, opt WriteOptions) (*File, error) {
	clean, err := v.Resolve(name)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	writable := flag&OpenWrite != 0 || flag&OpenReadWrite != 0
	if !writable {
		return v.openRead(ctx, clean)
	}
	if clean == "/" {
		return nil, pathErr("open", clean, ErrIsDir)
	}
	return v.openWrite(ctx, clean, flag, opt)
}

func (v *VFS) openRead(ctx context.Context, clean string) (*File, error) {
	v.mu.RLock()
	n, err := v.nodeAt(clean)
	v.mu.RUnlock()
	if err != nil {
		return nil, pathErr("open", clean, err)
	}
	if n.Dir {
		entries, err := v.List(clean)
		if err != nil {
			return nil, err
		}
		return &File{v: v, path: clean, ctx: ctx, node: n, entries: entries}, nil
	}
	rc, _, err := v.Blobs().Open(ctx, n.Blob.ID)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return nil, pathErr("open", clean, ErrNotExist)
		}
		return nil, pathErr("open", clean, err)
	}
	return &File{v: v, path: clean, ctx: ctx, node: n, rc: rc}, nil
}

func (v *VFS) openWrite(ctx context.Context, clean string, flag int, opt WriteOptions) (*File, error) {
	if opt.Parents {
		dirPath, _ := split(clean)
		if _, err := v.MkdirAll(dirPath, opt); err != nil {
			return nil, err
		}
	}

	dirPath, base := split(clean)

	v.mu.RLock()
	parent, perr := v.dirAt(dirPath)
	var (
		exists bool
		ref    blob.Ref
	)
	if perr == nil {
		if _, isDir := parent.dirs[base]; isDir {
			perr = ErrIsDir
		} else if f, ok := parent.files[base]; ok {
			exists, ref = true, f.ref
		}
	}
	v.mu.RUnlock()
	if perr != nil {
		return nil, pathErr("open", clean, perr)
	}
	switch {
	case !exists && flag&OpenCreate == 0:
		return nil, pathErr("open", clean, ErrNotExist)
	case exists && flag&OpenExclusive != 0:
		return nil, pathErr("open", clean, ErrExist)
	}

	f := &File{
		v:        v,
		path:     clean,
		ctx:      ctx,
		opt:      opt,
		writable: true,
		appends:  flag&OpenAppend != 0,
	}
	// Anything but a truncating open starts from the current content, so an
	// SFTP client that writes one block at offset 4096 does not blank the rest.
	if exists && flag&OpenTruncate == 0 {
		data, err := v.readRef(ctx, ref)
		if err != nil {
			return nil, pathErr("open", clean, err)
		}
		f.buf = data
	}
	if f.appends {
		f.pos = int64(len(f.buf))
	}
	return f, nil
}

func (v *VFS) readRef(ctx context.Context, ref blob.Ref) ([]byte, error) {
	if ref.ID == "" {
		return nil, nil
	}
	rc, _, err := v.Blobs().Open(ctx, ref.ID)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// OnCommit registers a callback run inside Close, once the new content is
// visible in the tree, with the context Close was given. It is how Session
// turns a finished upload into an event; an error from it fails the Close.
func (f *File) OnCommit(fn func(context.Context, Node) error) { f.onCommit = fn }

// Name returns the file's path in the VFS, the way os.File.Name does.
func (f *File) Name() string { return f.path }

// Info returns what is known about the entry: the node as it was opened for a
// read, and the node as committed after a successful Close for a write.
func (f *File) Info() Node {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.node
}

// Stat satisfies the fs.File and afero.File contract. Before a write handle is
// closed it reports the size written so far.
func (f *File) Stat() (fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.node
	if f.writable && !f.committed {
		n.Path = f.path
		n.Name = path.Base(f.path)
		n.Size = int64(len(f.buf))
		n.Provider = f.opt.Provider
	}
	return n.FileInfo(), nil
}

// Sync is a no-op: nothing is written until Close.
func (f *File) Sync() error { return nil }

// Readdir lists a directory handle. n <= 0 returns everything, matching
// os.File. It is only ever non-empty for a handle opened on a directory.
func (f *File) Readdir(n int) ([]fs.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, pathErr("readdir", f.path, ErrClosed)
	}
	if !f.node.Dir {
		return nil, pathErr("readdir", f.path, ErrNotDir)
	}
	if n > 0 && n < len(f.entries) {
		f.entries = f.entries[:n]
	}
	out := make([]fs.FileInfo, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e.FileInfo())
	}
	return out, nil
}

// Readdirnames is Readdir reduced to the names.
func (f *File) Readdirnames(n int) ([]string, error) {
	infos, err := f.Readdir(n)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.Name())
	}
	return out, nil
}

// Read implements io.Reader.
func (f *File) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, pathErr("read", f.path, ErrClosed)
	}
	if f.node.Dir {
		return 0, pathErr("read", f.path, ErrIsDir)
	}
	if f.rc != nil {
		return f.rc.Read(p)
	}
	if f.pos >= int64(len(f.buf)) {
		return 0, io.EOF
	}
	n := copy(p, f.buf[f.pos:])
	f.pos += int64(n)
	return n, nil
}

// ReadAt implements io.ReaderAt: what SFTP's FileGet and afero both want.
func (f *File) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, pathErr("readat", f.path, ErrClosed)
	}
	if f.node.Dir {
		return 0, pathErr("readat", f.path, ErrIsDir)
	}
	if off < 0 {
		return 0, pathErr("readat", f.path, ErrInvalidPath)
	}
	if f.rc != nil {
		// The blob reader is this handle's alone, so seeking it is safe; the
		// stream position is restored so an interleaved Read still works.
		cur, err := f.rc.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, err
		}
		if _, err := f.rc.Seek(off, io.SeekStart); err != nil {
			return 0, err
		}
		n, readErr := io.ReadFull(f.rc, p)
		if errors.Is(readErr, io.ErrUnexpectedEOF) {
			readErr = io.EOF
		}
		if _, err := f.rc.Seek(cur, io.SeekStart); err != nil {
			return n, err
		}
		return n, readErr
	}
	if off >= int64(len(f.buf)) {
		return 0, io.EOF
	}
	n := copy(p, f.buf[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Write implements io.Writer.
func (f *File) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.writableLocked("write"); err != nil {
		return 0, err
	}
	if f.appends {
		f.pos = int64(len(f.buf))
	}
	n, err := f.writeAtLocked(p, f.pos)
	f.pos += int64(n)
	return n, err
}

// WriteAt implements io.WriterAt, which is the interface pkg/sftp's FilePut
// handler returns and the one FTP's REST offset lands on.
func (f *File) WriteAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.writableLocked("writeat"); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, pathErr("writeat", f.path, ErrInvalidPath)
	}
	return f.writeAtLocked(p, off)
}

// WriteString is Write for a string, completing the afero.File surface.
func (f *File) WriteString(s string) (int, error) { return f.Write([]byte(s)) }

func (f *File) writableLocked(op string) error {
	if f.closed {
		return pathErr(op, f.path, ErrClosed)
	}
	if !f.writable {
		return pathErr(op, f.path, ErrReadOnly)
	}
	return nil
}

// writeAtLocked grows the buffer, zero-filling any gap, and refuses to grow it
// past the file-size limit - the bound that keeps a hostile upload from
// exhausting memory before the blob store's own cap is even consulted.
func (f *File) writeAtLocked(p []byte, off int64) (int, error) {
	end := off + int64(len(p))
	if end > f.v.limits.MaxFileSize {
		return 0, pathErr("write", f.path, ErrFileTooLarge)
	}
	if end > int64(len(f.buf)) {
		grown := make([]byte, end)
		copy(grown, f.buf)
		f.buf = grown
	}
	copy(f.buf[off:], p)
	return len(p), nil
}

// Seek implements io.Seeker.
func (f *File) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, pathErr("seek", f.path, ErrClosed)
	}
	if f.rc != nil {
		return f.rc.Seek(offset, whence)
	}
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = f.pos + offset
	case io.SeekEnd:
		next = int64(len(f.buf)) + offset
	default:
		return 0, pathErr("seek", f.path, ErrInvalidPath)
	}
	if next < 0 {
		return 0, pathErr("seek", f.path, ErrInvalidPath)
	}
	f.pos = next
	return next, nil
}

// Truncate resizes a write handle's content, zero-filling when it grows.
func (f *File) Truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.writableLocked("truncate"); err != nil {
		return err
	}
	if size < 0 || size > f.v.limits.MaxFileSize {
		return pathErr("truncate", f.path, ErrFileTooLarge)
	}
	grown := make([]byte, size)
	copy(grown, f.buf)
	f.buf = grown
	if f.pos > size {
		f.pos = size
	}
	return nil
}

// Size reports the bytes a read handle will yield, or the bytes a write handle
// has buffered so far.
func (f *File) Size() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rc != nil || f.node.Dir {
		return f.node.Size
	}
	return int64(len(f.buf))
}

// Close commits a write and releases a read. Closing twice is not an error.
func (f *File) Close() error { return f.CloseContext(f.ctx) }

// CloseContext is Close with a context of the caller's choosing, for a
// provider whose per-request context is already canceled by the time the
// transfer ends - which would otherwise lose the upload at the last step.
func (f *File) CloseContext(ctx context.Context) error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	rc, writable, buf, opt := f.rc, f.writable, f.buf, f.opt
	f.mu.Unlock()

	if rc != nil {
		return rc.Close()
	}
	if !writable {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	base := path.Base(f.path)
	ref, err := f.v.store(ctx, base, bytes.NewReader(buf), opt)
	if err != nil {
		return pathErr("close", f.path, err)
	}
	n, err := f.v.install(ctx, f.path, ref, opt)
	if err != nil {
		f.v.discard(ctx, ref)
		return err
	}

	f.mu.Lock()
	f.node = n
	f.committed = true
	f.mu.Unlock()

	if f.onCommit != nil {
		return f.onCommit(ctx, n)
	}
	return nil
}

// Abort closes the handle without committing, which is what an FTP transfer
// canceled with ABOR - or one whose data connection dropped - should do. The
// tree is left exactly as it was.
func (f *File) Abort() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	f.buf = nil
	if f.rc != nil {
		return f.rc.Close()
	}
	return nil
}
