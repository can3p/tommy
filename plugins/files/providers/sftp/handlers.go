package sftp

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"

	sftplib "github.com/pkg/sftp"

	"github.com/can3p/tommy/plugins/files"
)

// handler implements the four pkg/sftp request handlers on top of one
// files.Session, which is the whole provider-facing surface of the plugin.
//
// The mapping is close to one-to-one, because FS1 designed the session for it:
//
//	Fileread  Get                              -> Session.Open, an io.ReaderAt
//	Filewrite Put/Open                         -> Session.OpenFile, an io.WriterAt
//	Filecmd   Setstat Rename Rmdir Mkdir       -> Session.Chtimes/Truncate, Rename,
//	          Link Symlink Remove                 Remove, Mkdir
//	Filelist  List Stat Readlink               -> Session.List, Session.Stat
//
// Two things are deliberately not implemented. Symlinks are not modeled by the
// VFS at all, so Symlink, Link and Readlink answer SSH_FX_OP_UNSUPPORTED rather
// than pretending; and ownership and permission bits have no analog, so a
// chmod or chown is accepted and dropped instead of failing an upload that only
// wanted to preserve a mode.
//
// No path is ever interpreted here. Every path goes to the session, which
// resolves it through VFS.Resolve - the single security gate - and there is no
// host filesystem underneath it to escape to.
type handler struct {
	sess *files.Session
	log  *slog.Logger

	// ctx outlives any single request. pkg/sftp cancels a request's context as
	// it closes the handle, and closing a write handle is exactly when the
	// upload is committed and its event appended, so a request-scoped context
	// would lose the last step of every transfer.
	ctx context.Context

	mu sync.Mutex
	// pending maps a resolved path to the write handle that is still open on
	// it. An SFTP client sends fsetstat before it closes an upload - that is
	// how `sftp put -p` preserves timestamps - and at that moment there is no
	// committed node to stat, let alone touch.
	pending map[string]*writeHandle
}

var (
	_ sftplib.FileReader     = (*handler)(nil)
	_ sftplib.FileWriter     = (*handler)(nil)
	_ sftplib.OpenFileWriter = (*handler)(nil)
	_ sftplib.FileCmder      = (*handler)(nil)
	_ sftplib.FileLister     = (*handler)(nil)
)

func newHandler(ctx context.Context, sess *files.Session, log *slog.Logger) *handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return &handler{
		sess:    sess,
		log:     log,
		ctx:     context.WithoutCancel(ctx),
		pending: map[string]*writeHandle{},
	}
}

// unsupported is the answer for an operation the VFS has no meaning for. The
// wrapped sentinel is what makes pkg/sftp send SSH_FX_OP_UNSUPPORTED rather
// than a generic failure, so a client can tell "never going to work" from
// "went wrong".
func unsupported(op string) error {
	return fmt.Errorf("sftp: %s is not supported by tommy's virtual filesystem: %w", op, sftplib.ErrSSHFxOpUnsupported)
}

// Fileread implements sftplib.FileReader: a download.
func (h *handler) Fileread(r *sftplib.Request) (io.ReaderAt, error) {
	f, err := h.sess.Open(h.ctx, r.Filepath)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Filewrite implements sftplib.FileWriter: an upload. The handle buffers and
// commits atomically on close, so a listing never sees a half-written file and
// an abandoned transfer leaves nothing behind at all.
func (h *handler) Filewrite(r *sftplib.Request) (io.WriterAt, error) {
	return h.openWrite(r)
}

// OpenFile implements sftplib.OpenFileWriter, which is what lets a client open
// one handle for reading and writing at once.
func (h *handler) OpenFile(r *sftplib.Request) (sftplib.WriterAtReaderAt, error) {
	return h.openWrite(r)
}

func (h *handler) openWrite(r *sftplib.Request) (*writeHandle, error) {
	// Resolving first both rejects a hostile path before anything is opened
	// and gives the pending map a canonical key.
	clean, err := h.sess.Resolve(r.Filepath)
	if err != nil {
		return nil, err
	}

	pflags := r.Pflags()
	flag := files.OpenWrite
	if pflags.Read {
		flag = files.OpenReadWrite
	}
	if pflags.Creat {
		flag |= files.OpenCreate
	}
	if pflags.Trunc {
		flag |= files.OpenTruncate
	}
	if pflags.Append {
		flag |= files.OpenAppend
	}
	if pflags.Excl {
		flag |= files.OpenExclusive
	}

	f, err := h.sess.OpenFile(h.ctx, clean, flag, files.WithCommand("SSH_FXP_OPEN "+clean))
	if err != nil {
		return nil, err
	}
	w := &writeHandle{File: f, h: h, path: clean, appends: pflags.Append}

	h.mu.Lock()
	h.pending[clean] = w
	h.mu.Unlock()
	return w, nil
}

// forget drops a write handle from the pending map, unless a newer handle on
// the same path has already replaced it.
func (h *handler) forget(w *writeHandle) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending[w.path] == w {
		delete(h.pending, w.path)
	}
}

func (h *handler) pendingFor(path string) *writeHandle {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pending[path]
}

// Filecmd implements sftplib.FileCmder: everything that changes the tree
// without moving bytes.
func (h *handler) Filecmd(r *sftplib.Request) error {
	switch r.Method {
	case "Mkdir":
		_, err := h.sess.Mkdir(h.ctx, r.Filepath, files.WithCommand("SSH_FXP_MKDIR "+r.Filepath))
		return err

	case "Rmdir":
		// SSH_FXP_RMDIR is for directories only, and a non-empty one is an
		// error: a client that means "and everything under it" walks the tree
		// itself, which is what pkg/sftp's RemoveAll does.
		n, err := h.sess.Stat(r.Filepath)
		if err != nil {
			return err
		}
		if !n.Dir {
			return &fs.PathError{Op: "rmdir", Path: r.Filepath, Err: files.ErrNotDir}
		}
		_, err = h.sess.Remove(h.ctx, r.Filepath, files.WithCommand("SSH_FXP_RMDIR "+r.Filepath))
		return err

	case "Remove":
		n, err := h.sess.Stat(r.Filepath)
		if err != nil {
			return err
		}
		if n.Dir {
			// Answering "is a directory" is what makes a client retry with
			// rmdir instead of giving up.
			return &fs.PathError{Op: "remove", Path: r.Filepath, Err: files.ErrIsDir}
		}
		_, err = h.sess.Remove(h.ctx, r.Filepath, files.WithCommand("SSH_FXP_REMOVE "+r.Filepath))
		return err

	case "Rename", "PosixRename":
		_, err := h.sess.Rename(h.ctx, r.Filepath, r.Target,
			files.WithCommand("SSH_FXP_RENAME "+r.Filepath+" -> "+r.Target))
		return err

	case "Setstat":
		return h.setstat(r)

	case "Symlink":
		return unsupported("symlink")

	case "Link":
		return unsupported("hard link")

	default:
		return unsupported(r.Method)
	}
}

// setstat applies what the VFS can represent and drops the rest.
//
// Nothing is recorded: a timestamp or a mode fixup is metadata, not a change to
// what the filesystem holds, and an event per setstat would drown the log that
// the uploads are in.
func (h *handler) setstat(r *sftplib.Request) error {
	attrs := r.Attributes()
	flags := r.AttrFlags()

	// An upload that has not been closed yet has no node to touch, so the
	// change is applied to the open handle instead - a size directly, an mtime
	// once the commit has produced something to stamp.
	if w := h.pendingFor(cleanOrEmpty(h, r.Filepath)); w != nil {
		if flags.Size {
			return w.Truncate(int64(attrs.Size))
		}
		if flags.Acmodtime {
			w.setMtime(time.Unix(int64(attrs.Mtime), 0))
		}
		return nil
	}

	// Everything below needs the entry to exist, and saying so is better than
	// a silent success on a path that is not there.
	if _, err := h.sess.Stat(r.Filepath); err != nil {
		return err
	}
	if flags.Size {
		if err := h.sess.Truncate(h.ctx, r.Filepath, int64(attrs.Size)); err != nil {
			return err
		}
	}
	if flags.Acmodtime {
		if err := h.sess.Chtimes(r.Filepath, time.Unix(int64(attrs.Mtime), 0)); err != nil {
			return err
		}
	}
	// Permissions and ownership are accepted and dropped on purpose: there is
	// no mode and no uid in the VFS, and failing here would break every client
	// that chmods what it just uploaded.
	if flags.Permissions || flags.UidGid {
		h.log.Debug("sftp: ignoring chmod/chown, the virtual filesystem has no modes", "path", r.Filepath)
	}
	return nil
}

// cleanOrEmpty resolves a path for a lookup that must not fail on a bad one.
func cleanOrEmpty(h *handler, name string) string {
	clean, err := h.sess.Resolve(name)
	if err != nil {
		return ""
	}
	return clean
}

// Filelist implements sftplib.FileLister: List for a directory listing, Stat
// and Lstat for one entry - the VFS has no symlinks, so the two are the same
// thing - and Readlink, which cannot mean anything here.
func (h *handler) Filelist(r *sftplib.Request) (sftplib.ListerAt, error) {
	switch r.Method {
	case "List":
		entries, err := h.sess.List(r.Filepath)
		if err != nil {
			return nil, err
		}
		infos := make([]os.FileInfo, 0, len(entries))
		for _, e := range entries {
			infos = append(infos, e.FileInfo())
		}
		return listerAt(infos), nil

	case "Stat", "Lstat":
		n, err := h.sess.Stat(r.Filepath)
		if err != nil {
			return nil, err
		}
		return listerAt{n.FileInfo()}, nil

	case "Readlink":
		return nil, unsupported("readlink")

	default:
		return nil, unsupported(r.Method)
	}
}

// listerAt is the []os.FileInfo equivalent of an io.ReaderAt that pkg/sftp
// reads a listing out of, one packet at a time.
type listerAt []os.FileInfo

func (l listerAt) ListAt(f []os.FileInfo, off int64) (int, error) {
	if off < 0 {
		return 0, fs.ErrInvalid
	}
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(f, l[off:])
	if n < len(f) {
		return n, io.EOF
	}
	return n, nil
}

// writeHandle is an upload in flight. It exists to control what Close means:
// the commit runs with the handler's context rather than the request's, which
// pkg/sftp cancels as it closes the handle, and a transfer that failed is
// aborted instead of committed so a broken upload leaves neither a truncated
// file nor an event.
type writeHandle struct {
	*files.File
	h    *handler
	path string
	// appends records SSH_FXF_APPEND. The protocol says a write on an
	// append-mode handle goes to the end of the file whatever offset the
	// packet carries, and clients rely on it: pkg/sftp's own client sends
	// offset 0 for every append.
	appends bool

	mu      sync.Mutex
	mtime   time.Time
	aborted bool
}

// WriteAt honors append mode, where the offset in the packet is meaningless.
func (w *writeHandle) WriteAt(p []byte, off int64) (int, error) {
	if w.appends {
		// Write - not WriteAt - is the File method that moves to the end
		// first for a handle opened to append.
		return w.Write(p)
	}
	return w.File.WriteAt(p, off)
}

func (w *writeHandle) setMtime(t time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.mtime = t
}

// TransferError is called by pkg/sftp when the connection dies with this handle
// still open.
func (w *writeHandle) TransferError(err error) {
	w.mu.Lock()
	w.aborted = true
	w.mu.Unlock()
	w.h.log.Debug("sftp: transfer failed, discarding upload", "path", w.path, "err", err)
}

// Close commits the upload, which is also when files.upload is appended.
func (w *writeHandle) Close() error {
	w.h.forget(w)

	w.mu.Lock()
	aborted, mtime := w.aborted, w.mtime
	w.mu.Unlock()

	if aborted {
		return w.Abort()
	}
	if err := w.CloseContext(w.h.ctx); err != nil {
		return err
	}
	if !mtime.IsZero() {
		// The client asked to preserve the timestamp before it closed the
		// file; now there is something to stamp.
		if err := w.h.sess.Chtimes(w.path, mtime); err != nil {
			w.h.log.Debug("sftp: could not preserve mtime", "path", w.path, "err", err)
		}
	}
	return nil
}
