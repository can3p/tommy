package nfs

import (
	"context"
	"hash/fnv"
	"io/fs"
	"os"
	"path"
	"time"

	billy "github.com/go-git/go-billy/v5"
	nfsfile "github.com/willscott/go-nfs/file"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/files"
)

// billyFS adapts the plugin's shared virtual filesystem to
// github.com/go-git/go-billy/v5, which is the backend interface
// github.com/willscott/go-nfs expects - the same job fsAdapter does for
// ftpserverlib's afero.Fs, against a different interface.
//
// Like that adapter it is deliberately thin: every method forwards onto a
// *files.Session, which is the only thing allowed to interpret a path.
// VFS.Resolve is the plugin's single security gate and this file never
// second-guesses it. Join below is the one place a path is touched at all,
// and it only joins and cleans - it decides nothing about what is reachable,
// because whatever it produces is resolved again before the tree is touched.
//
// One value is created per mounted client connection (see handler.Mount), so
// the peer address and the RPC credential that connection presented land on
// every event that connection produces. That is also why the discriminating
// fields are declared first: go-nfs's handle cache compares filesystems with
// reflect.DeepEqual, which short-circuits on an identical pointer and
// otherwise walks the struct field by field, so a cheap difference up front
// keeps it from ever descending into the shared tree.
type billyFS struct {
	// peer is the client address of the connection that mounted. Two live
	// connections never share one, which makes this the cheap discriminator
	// reflect.DeepEqual needs.
	peer string
	user string
	uid  uint32
	gid  uint32

	// srv is shared by every connection, so comparing two billyFS values
	// never walks into the filesystem itself.
	srv *shared
}

var (
	_ billy.Filesystem = (*billyFS)(nil)
	_ billy.Change     = (*billyFS)(nil)
	_ billy.Capable    = (*billyFS)(nil)
)

// shared is everything a billyFS needs that does not vary per connection.
type shared struct {
	vfs  *files.VFS
	deps plugin.Deps
	cfg  Config
}

// session binds the shared tree to this connection's identity. It is built
// per operation rather than held, because billy hands us no place to keep one
// and constructing it is a struct literal.
func (b *billyFS) session() *files.Session {
	return files.NewSession(b.srv.vfs, b.srv.deps,
		files.WithProvider(ProviderName),
		files.WithTransport("tcp"),
		files.WithPeer(b.peer),
		files.WithUser(b.user),
		files.WithSessionMeta(map[string]any{
			"uid": b.uid,
			"gid": b.gid,
		}))
}

// Join implements billy.Basic. go-nfs builds every path by joining the
// components stored behind a file handle with the single filename a client
// sent in the request, and this is where that happens.
//
// It is path.Join, not filepath.Join: the VFS is slash-separated on every
// platform and a Windows-flavored join here would produce paths the rest of
// the plugin cannot read. Cleaning "." and ".." out is all it does; it grants
// nothing, because the result is handed to VFS.Resolve before any node is
// touched, and the VFS has no host filesystem underneath it for a traversal
// to escape onto.
func (b *billyFS) Join(elem ...string) string {
	joined := path.Join(elem...)
	if joined == "" {
		// The root handle carries an empty component list. "/" keeps the
		// library's own path-keyed caches (reverse handles, readdir cookie
		// verifiers) from filing the root under the empty string.
		return "/"
	}
	return joined
}

// Root implements billy.Chroot. The share is the whole tree.
func (b *billyFS) Root() string { return "/" }

// Chroot implements billy.Chroot. go-nfs never calls it - a mount always gets
// the root - and the VFS has no sub-tree view to hand back, so this reports
// the same "not supported" billy uses everywhere else rather than pretending.
func (b *billyFS) Chroot(string) (billy.Filesystem, error) {
	return nil, billy.ErrNotSupported
}

// Stat implements billy.Basic.
func (b *billyFS) Stat(filename string) (os.FileInfo, error) {
	n, err := b.session().Stat(filename)
	if err != nil {
		return nil, err
	}
	return b.info(n), nil
}

// Lstat implements billy.Symlink. The VFS has no symlinks, so it is Stat.
func (b *billyFS) Lstat(filename string) (os.FileInfo, error) { return b.Stat(filename) }

// Symlink implements billy.Symlink. There are no symlinks in the tree and
// inventing them would mean a second way to name a node, which is exactly the
// kind of thing the one-resolver rule exists to prevent.
func (b *billyFS) Symlink(string, string) error { return billy.ErrNotSupported }

// Readlink implements billy.Symlink.
func (b *billyFS) Readlink(string) (string, error) { return "", billy.ErrNotSupported }

// TempFile implements billy.TempFile. go-nfs never calls it.
func (b *billyFS) TempFile(string, string) (billy.File, error) {
	return nil, billy.ErrNotSupported
}

// Create implements billy.Basic and is what NFSPROC3_CREATE lands on.
func (b *billyFS) Create(filename string) (billy.File, error) {
	return b.OpenFile(filename, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

// Open implements billy.Basic: the read side of NFSPROC3_READ, and the handle
// go-nfs reads a directory listing through.
func (b *billyFS) Open(filename string) (billy.File, error) {
	return b.OpenFile(filename, os.O_RDONLY, 0)
}

// OpenFile implements billy.Basic. Three NFS procedures reach it: CREATE
// (with O_CREATE|O_TRUNC), WRITE (plain O_RDWR, since NFS requires the file
// to exist already) and SETATTR when it carries a new size.
//
// O_EXCL arrives without O_CREATE from go-nfs's SETATTR-with-size path, where
// it evidently means "do not create". POSIX leaves O_EXCL without O_CREAT
// undefined and the VFS reads it strictly - an existing file is ErrExist -
// so it is dropped here. Without that, truncating a file over NFS, which is
// what a client does for open(O_TRUNC), fails outright.
func (b *billyFS) OpenFile(filename string, flag int, _ os.FileMode) (billy.File, error) {
	ctx := context.Background()
	sess := b.session()

	writing := flag&(os.O_WRONLY|os.O_RDWR) != 0
	if !writing {
		f, err := sess.Open(ctx, filename)
		if err != nil {
			return nil, err
		}
		return billyFile{f}, nil
	}
	if flag&os.O_CREATE == 0 {
		flag &^= os.O_EXCL
	}

	f, err := sess.OpenFile(ctx, filename, flag, files.WithCommand(commandFor(flag, filename)))
	if err != nil {
		return nil, err
	}
	return billyFile{f}, nil
}

// ReadDir implements billy.Dir: NFSPROC3_READDIR and READDIRPLUS. A listing
// is a read, so nothing is recorded.
func (b *billyFS) ReadDir(name string) ([]os.FileInfo, error) {
	nodes, err := b.session().List(name)
	if err != nil {
		return nil, err
	}
	out := make([]os.FileInfo, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, b.info(n))
	}
	return out, nil
}

// MkdirAll implements billy.Dir and is what NFSPROC3_MKDIR lands on. go-nfs
// has already checked that the parent exists and that the name is free, so in
// practice this creates exactly one directory and appends one files.mkdir.
func (b *billyFS) MkdirAll(filename string, _ os.FileMode) error {
	_, err := b.session().MkdirAll(context.Background(), filename,
		files.WithCommand("NFSPROC3_MKDIR "+filename))
	return err
}

// Remove implements billy.Basic. Both NFSPROC3_REMOVE and NFSPROC3_RMDIR
// arrive here - go-nfs serves RMDIR by delegating to its REMOVE handler - so
// the recorded command names the pair rather than guessing which one it was.
func (b *billyFS) Remove(filename string) error {
	_, err := b.session().Remove(context.Background(), filename,
		files.WithCommand("NFSPROC3_REMOVE "+filename))
	return err
}

// Rename implements billy.Basic: NFSPROC3_RENAME.
func (b *billyFS) Rename(oldpath, newpath string) error {
	_, err := b.session().Rename(context.Background(), oldpath, newpath,
		files.WithCommand("NFSPROC3_RENAME "+oldpath+" -> "+newpath))
	return err
}

// Chmod implements billy.Change. There are no permission bits in the VFS to
// change, and a client that cannot chmod cannot finish a CREATE carrying
// attributes, so this accepts and discards - provider rule 1.
func (b *billyFS) Chmod(string, os.FileMode) error { return nil }

// Lchown implements billy.Change: no owners in the tree, so accepted and
// discarded like Chmod.
func (b *billyFS) Lchown(string, int, int) error { return nil }

// Chown implements billy.Change.
func (b *billyFS) Chown(string, int, int) error { return nil }

// Chtimes implements billy.Change: NFSPROC3_SETATTR with a new mtime. The VFS
// records it and, like FTP's MFMT and SFTP's setstat, appends no event - a
// timestamp fixup is metadata, not a change to what the filesystem holds.
func (b *billyFS) Chtimes(name string, _ time.Time, mtime time.Time) error {
	return b.session().Chtimes(name, mtime)
}

// Capabilities implements billy.Capable. go-nfs asks for WriteCapability
// before every mutating procedure and answers NFS3ERR_ROFS without it; it
// also uses the answer to decide whether to advertise settable times in
// FSINFO. Lock is left out because the VFS has no locking to offer.
func (b *billyFS) Capabilities() billy.Capability {
	return billy.WriteCapability | billy.ReadCapability |
		billy.ReadAndWriteCapability | billy.SeekCapability | billy.TruncateCapability
}

// info wraps a VFS node in the os.FileInfo go-nfs turns into an NFS fattr3.
func (b *billyFS) info(n files.Node) os.FileInfo {
	return nfsInfo{FileInfo: n.FileInfo(), path: n.Path, uid: b.uid, gid: b.gid}
}

// nfsInfo is the FileInfo an NFS client sees. It exists for two reasons the
// plain VFS node cannot cover.
//
// Ownership and mode: the VFS has no owners and fixed 0644/0755 bits, and a
// mounted NFS share is policed by the *client* kernel against whatever the
// server reports. Reporting uid 0 and 0644 would leave an ordinary process on
// the client unable to write to a fake that accepts everything. So the caller
// is reported as the owner and the modes are world-writable: there is no
// access control to model here, and a fake that answers "permission denied"
// is useless (provider rule 1).
//
// Fileid: NFS needs a stable, non-zero unique id per object. go-nfs reads one
// out of Sys() when it finds a *file.FileInfo there and otherwise falls back
// to hashing the path - but the moment Sys() returns a file.FileInfo at all,
// its Fileid is used verbatim, so leaving it zero would give every object in
// the tree the same inode number and confuse any client that caches by it.
type nfsInfo struct {
	fs.FileInfo
	path     string
	uid, gid uint32
}

func (i nfsInfo) Mode() fs.FileMode {
	if i.IsDir() {
		return fs.ModeDir | 0o777
	}
	return 0o666
}

func (i nfsInfo) Sys() any {
	// A directory reports two links, the way a real one does: some clients
	// and utilities (find's leaf optimisation, most famously) treat a link
	// count below two as "this directory has no subdirectories".
	nlink := uint32(1)
	if i.IsDir() {
		nlink = 2
	}
	return nfsfile.FileInfo{
		Nlink:  nlink,
		UID:    i.uid,
		GID:    i.gid,
		Fileid: fileID(i.path),
	}
}

// fileID derives a stable inode number from a path. It is never zero, which
// some clients read as "no id".
func fileID(p string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(p))
	id := h.Sum64()
	if id == 0 {
		return 1
	}
	return id
}

// billyFile adapts a *files.File to billy.File, which is the same set of
// methods plus advisory locking. *files.File already satisfies Name, Read,
// ReadAt, Write, Seek, Truncate and Close; there is nothing to lock against
// in an in-memory tree, so Lock and Unlock succeed and do nothing rather than
// failing a client that always calls them.
type billyFile struct{ *files.File }

var _ billy.File = billyFile{}

func (f billyFile) Lock() error   { return nil }
func (f billyFile) Unlock() error { return nil }

// commandFor labels the event with the NFS procedure that plausibly caused
// it, for a readable Event.Raw.Body. The operation never depends on the
// guess. NFS is stateless and has no close on the wire, so a client's single
// logical upload is a CREATE followed by one WRITE per chunk it chose to
// send, and each one commits and is recorded on its own.
func commandFor(flag int, name string) string {
	if flag&os.O_CREATE != 0 {
		return "NFSPROC3_CREATE " + name
	}
	return "NFSPROC3_WRITE " + name
}
