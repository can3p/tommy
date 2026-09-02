package tftp

import (
	"context"
	"io"
	"strings"

	tftplib "github.com/pin/tftp/v3"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/files"
)

// readHandler returns the RRQ handler pin/tftp/v3 calls for a download,
// bound to deps d. It opens name on a *files.Session, which is the only
// thing allowed to interpret a path - VFS.Resolve is the gate, and the raw
// client-supplied name is passed straight through.
//
// The returned *files.File also satisfies io.Seeker, so pin/tftp/v3's own
// automatic tsize handling (RFC 2349) works without any extra code here: the
// library seeks the reader it is handed to learn the size before it starts
// sending, exactly the way it would for an *os.File.
func (p *Provider) readHandler(d plugin.Deps) func(filename string, rf io.ReaderFrom) error {
	return func(filename string, rf io.ReaderFrom) error {
		ctx := context.Background()
		sess := files.NewSession(p.tree(), d,
			files.WithProvider(ProviderName),
			files.WithTransport("udp"),
			files.WithPeer(peerOfOutgoing(rf)))

		f, err := sess.Open(ctx, filename)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()

		_, err = rf.ReadFrom(f)
		return err
	}
}

// writeHandler returns the WRQ handler for an upload, bound to deps d. The
// write goes through Session.OpenFile so it both lands in the tree and
// appends a files.upload event on a successful Close - an abandoned transfer
// (the client vanishes mid-upload, or the far end reports an error) is
// Aborted instead, leaving neither a file nor an event behind, the same
// contract Session gives ftp and sftp.
func (p *Provider) writeHandler(d plugin.Deps) func(filename string, wt io.WriterTo) error {
	return func(filename string, wt io.WriterTo) error {
		ctx := context.Background()
		sess := files.NewSession(p.tree(), d,
			files.WithProvider(ProviderName),
			files.WithTransport("udp"),
			files.WithPeer(peerOfIncoming(wt)))

		// TFTP has no MKD of its own, but a client naming a nested path
		// ("sub/dir/file.bin") should not behave differently over TFTP than
		// it would over ftp, which pre-creates missing parents the same way
		// curl's --ftp-create-dirs does. dirOf is a guess, not a security
		// decision: the write below still resolves through the VFS and can
		// land nowhere that guess did not already allow.
		if dir := dirOf(filename); dir != "/" {
			if _, err := sess.MkdirAll(ctx, dir, files.WithCommand("WRQ "+filename)); err != nil {
				return err
			}
		}

		opts := []files.EventOption{files.WithCommand("WRQ " + filename)}
		// RFC 2349's tsize option, when the client sent one, is available
		// before the transfer starts - recorded for inspection value, not
		// acted on: the VFS learns the real size from what actually arrives.
		if it, ok := wt.(tftplib.IncomingTransfer); ok {
			if n, ok := it.Size(); ok {
				opts = append(opts, files.WithEventMeta("tsize_declared", n))
			}
		}

		f, err := sess.OpenFile(ctx, filename, files.OpenWrite|files.OpenCreate|files.OpenTruncate, opts...)
		if err != nil {
			return err
		}
		if _, err := wt.WriteTo(f); err != nil {
			_ = f.Abort()
			return err
		}
		return f.Close()
	}
}

// peerOfOutgoing extracts the client address pin/tftp/v3 hands a read
// (RRQ/download) handler. OutgoingTransfer is the interface the library's rf
// value actually satisfies; the type assertion only fails if a future
// library version changes that, in which case an empty peer is recorded
// rather than a panic.
func peerOfOutgoing(rf io.ReaderFrom) string {
	ot, ok := rf.(tftplib.OutgoingTransfer)
	if !ok {
		return ""
	}
	addr := ot.RemoteAddr()
	return addr.String()
}

// peerOfIncoming is peerOfOutgoing for a write (WRQ/upload) handler's wt.
func peerOfIncoming(wt io.WriterTo) string {
	it, ok := wt.(tftplib.IncomingTransfer)
	if !ok {
		return ""
	}
	addr := it.RemoteAddr()
	return addr.String()
}

// dirOf returns the parent of a client-supplied, not-yet-resolved filename,
// or "/" when there is none. It is a convenience for the write handler's
// create-parents behavior, not a security boundary - see the comment at its
// one call site.
func dirOf(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	i := strings.LastIndexByte(name, '/')
	if i <= 0 {
		return "/"
	}
	return name[:i]
}
