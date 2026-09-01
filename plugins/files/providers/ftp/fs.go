package ftp

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/spf13/afero"

	ftpserver "github.com/fclairamb/ftpserverlib"

	"github.com/can3p/tommy/plugins/files"
)

// fsAdapter adapts a *files.Session to ftpserverlib's ClientDriver, which is
// exactly an afero.Fs. It is deliberately thin: every method forwards straight
// onto the Session, which is the only thing allowed to interpret a path -
// through VFS.Resolve, the plugin's single security gate. dirOf below finds a
// parent directory to pre-create for a write; that is not a security decision
// because the write itself still resolves through the VFS regardless of what
// dirOf guesses, so a hostile name cannot use it to reach anywhere the VFS
// would not have let it reach on its own.
//
// *files.File, returned by every Session method that opens a handle, already
// satisfies afero.File (Read, ReadAt, Write, WriteAt, Seek, Close, Name,
// Readdir, Readdirnames, Stat, Sync, Truncate, WriteString), so no wrapping is
// needed there either.
type fsAdapter struct {
	sess *files.Session
}

var (
	_ afero.Fs                                 = (*fsAdapter)(nil)
	_ ftpserver.ClientDriver                   = (*fsAdapter)(nil)
	_ ftpserver.ClientDriverExtensionRemoveDir = (*fsAdapter)(nil)
)

// Name implements afero.Fs.
func (a *fsAdapter) Name() string { return "tommy-files-vfs" }

// Create implements afero.Fs: open for writing, creating or truncating.
func (a *fsAdapter) Create(name string) (afero.File, error) {
	return a.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
}

// Mkdir implements afero.Fs. MKD is one level: the parent must already exist.
func (a *fsAdapter) Mkdir(name string, _ os.FileMode) error {
	_, err := a.sess.Mkdir(context.Background(), name, files.WithCommand("MKD "+name))
	return err
}

// MkdirAll implements afero.Fs.
func (a *fsAdapter) MkdirAll(name string, _ os.FileMode) error {
	_, err := a.sess.MkdirAll(context.Background(), name, files.WithCommand("MKD "+name))
	return err
}

// Open implements afero.Fs: a read-only handle, or a directory handle whose
// Readdir lists it (LIST/NLST).
func (a *fsAdapter) Open(name string) (afero.File, error) {
	return a.sess.Open(context.Background(), name)
}

// OpenFile implements afero.Fs and is what STOR, RETR and APPE all resolve to
// (ftpserverlib calls it for every file transfer, not just Create).
//
// The VFS itself refuses to write into a directory that does not exist yet -
// "the parent directory must exist", exactly what a real FTP server answers
// to STOR into nowhere - so a write-with-create pre-creates every missing
// parent first. That is --ftp-create-dirs behavior implemented server-side:
// curl's flag of the same name works by retrying STOR after issuing its own
// MKD commands, and doing the same thing here means a client that never
// bothers to retry still gets the same result.
func (a *fsAdapter) OpenFile(name string, flag int, _ os.FileMode) (afero.File, error) {
	ctx := context.Background()
	creating := flag&os.O_CREATE != 0 && flag&(os.O_WRONLY|os.O_RDWR) != 0
	if creating {
		if dir := dirOf(name); dir != "/" {
			if _, err := a.sess.MkdirAll(ctx, dir); err != nil {
				return nil, err
			}
		}
	}
	return a.sess.OpenFile(ctx, name, flag, files.WithCommand(cmdFor(flag, name)))
}

// Remove implements afero.Fs: DELE, when RemoveDir below is not asked for
// instead.
func (a *fsAdapter) Remove(name string) error {
	_, err := a.sess.Remove(context.Background(), name, files.WithCommand("DELE "+name))
	return err
}

// RemoveDir implements ftpserver.ClientDriverExtensionRemoveDir, which is how
// RMD is told apart from DELE. Without it both commands would share Remove
// and the event log would call every directory delete "DELE".
func (a *fsAdapter) RemoveDir(name string) error {
	_, err := a.sess.Remove(context.Background(), name, files.WithCommand("RMD "+name))
	return err
}

// RemoveAll implements afero.Fs. No standard FTP command reaches this - RMD
// only removes an empty directory - but the interface requires it.
func (a *fsAdapter) RemoveAll(name string) error {
	_, _, err := a.sess.RemoveAll(context.Background(), name, files.WithCommand("RMD "+name))
	return err
}

// Rename implements afero.Fs: RNFR followed by RNTO.
func (a *fsAdapter) Rename(oldname, newname string) error {
	_, err := a.sess.Rename(context.Background(), oldname, newname,
		files.WithCommand("RNFR "+oldname+"; RNTO "+newname))
	return err
}

// Stat implements afero.Fs: SIZE, MDTM, STAT, and the CWD/PWD bookkeeping
// ftpserverlib itself does before ever calling into the driver again.
func (a *fsAdapter) Stat(name string) (os.FileInfo, error) {
	n, err := a.sess.Stat(name)
	if err != nil {
		return nil, err
	}
	return n.FileInfo(), nil
}

// Chmod and Chown have no equivalent in the VFS - there are no permission bits
// or owners to change - so they are no-ops rather than errors. A client that
// always sends them (some GUI clients do, unconditionally, after every
// upload) still works end to end.
func (a *fsAdapter) Chmod(name string, mode os.FileMode) error { return nil }
func (a *fsAdapter) Chown(name string, uid, gid int) error     { return nil }

// Chtimes implements afero.Fs: MFMT.
func (a *fsAdapter) Chtimes(name string, _ time.Time, mtime time.Time) error {
	return a.sess.Chtimes(name, mtime)
}

// dirOf returns the parent of an already-absolute, already-clean path (which
// is what ftpserverlib always hands the driver), or "/" for a top-level name.
// It is a convenience for OpenFile's create-parents behavior, not a security
// boundary: whatever it guesses, the actual write still resolves through
// VFS.Resolve and can land nowhere the VFS would not have allowed anyway.
func dirOf(name string) string {
	i := strings.LastIndexByte(name, '/')
	if i <= 0 {
		return "/"
	}
	return name[:i]
}

// cmdFor labels a Session write event with the FTP command that plausibly
// caused it, purely for a readable Event.Raw.Body; the operation it actually
// performs never depends on this guess.
func cmdFor(flag int, name string) string {
	switch {
	case flag&os.O_APPEND != 0:
		return "APPE " + name
	case flag&(os.O_WRONLY|os.O_RDWR) != 0:
		return "STOR " + name
	default:
		return "RETR " + name
	}
}
