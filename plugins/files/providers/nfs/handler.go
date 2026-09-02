package nfs

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"unicode/utf8"

	billy "github.com/go-git/go-billy/v5"
	gonfs "github.com/willscott/go-nfs"
	nfsrpc "github.com/willscott/go-nfs-client/nfs/rpc"
	nfshelpers "github.com/willscott/go-nfs/helpers"
)

// handler is the gonfs.Handler this provider serves. It answers the MOUNT
// procedure and describes the filesystem; the file-handle half of the
// interface is supplied by gonfs's own caching handler, which wraps this one
// (see newHandler).
type handler struct {
	srv *shared
}

var _ gonfs.Handler = (*handler)(nil)

// newHandler builds the handler chain the server runs: this handler for
// mounts and filesystem metadata, wrapped in the library's caching handler
// for file handles.
//
// The wrapper is not optional. Every NFS procedure addresses its target by an
// opaque file handle, and the caching handler is what mints them: a random
// UUID per (filesystem, path) pair, kept in an LRU, with FromHandle answering
// NFS3ERR_STALE for anything it did not mint or has since evicted. That is
// also the security property. A handle carries no path - it cannot, it is a
// UUID - so a crafted or replayed handle can only ever name a path this
// server itself produced from a successful lookup, and that path is resolved
// through VFS.Resolve again on every single operation.
//
// The cache size is therefore both a resource bound and a correctness knob:
// evicting a handle a client is still using yields a spurious ESTALE, and
// go-nfs additionally caps one READDIR/READDIRPLUS response at half the
// limit.
func newHandler(s *shared) gonfs.Handler {
	return nfshelpers.NewCachingHandler(&handler{srv: s}, s.cfg.HandleCache)
}

// Mount answers the MOUNT procedure. Any dirpath is accepted and the whole
// shared tree is returned - there is one export and pinning its name would
// only give a user something else to get wrong (provider rule 1). What the
// client asked for, the credential it presented and its address are recorded
// instead.
//
// The billy.Filesystem returned is built fresh per mounting connection so
// that everything that connection goes on to write is attributed to it. It is
// the only hook the library offers for this: billy's methods take neither a
// context nor a connection, so a single shared filesystem could not tell one
// client from another.
func (h *handler) Mount(_ context.Context, conn net.Conn, req gonfs.MountRequest) (gonfs.MountStatus, billy.Filesystem, []gonfs.AuthFlavor) {
	peer := ""
	if conn != nil && conn.RemoteAddr() != nil {
		peer = conn.RemoteAddr().String()
	}
	cred := parseAuthUnix(req.Cred)

	fs := &billyFS{
		peer: peer,
		user: cred.user(),
		uid:  cred.UID,
		gid:  cred.GID,
		srv:  h.srv,
	}

	h.srv.deps.Logger.Info("nfs mount",
		"peer", peer,
		"dirpath", sanitize(string(req.Dirpath)),
		"auth", cred.Flavor,
		"uid", cred.UID,
		"gid", cred.GID)

	// AUTH_NULL and AUTH_UNIX are both advertised as acceptable, and neither
	// is ever checked: a fake that refuses a mount teaches nobody anything.
	return gonfs.MountStatusOk, fs, []gonfs.AuthFlavor{gonfs.AuthFlavorNull, gonfs.AuthFlavorUnix}
}

// Change reports the filesystem's attribute-changing half. Returning nil here
// would make go-nfs answer NFS3ERR_NOTSUPP to any CREATE or SETATTR carrying
// attributes, which is most of them.
func (h *handler) Change(fs billy.Filesystem) billy.Change {
	if c, ok := fs.(billy.Change); ok {
		return c
	}
	return nil
}

// FSStat answers the FSSTAT procedure - what `df` on a mounted share prints.
// go-nfs seeds it with 2^62 of everything; the real numbers come from the
// VFS's own limits, so a client sees a share the size tommy will actually
// let it fill rather than four exabytes.
func (h *handler) FSStat(_ context.Context, _ billy.Filesystem, s *gonfs.FSStat) error {
	limits := h.srv.vfs.Limits()
	stats := h.srv.vfs.Stats()

	total := uint64(limits.MaxFileSize) * uint64(limits.MaxNodes)
	used := uint64(0)
	if stats.Bytes > 0 {
		used = uint64(stats.Bytes)
	}
	free := uint64(0)
	if total > used {
		free = total - used
	}

	nodes := uint64(limits.MaxNodes)
	usedNodes := uint64(stats.Dirs + stats.Files)
	freeNodes := uint64(0)
	if nodes > usedNodes {
		freeNodes = nodes - usedNodes
	}

	s.TotalSize, s.FreeSize, s.AvailableSize = total, free, free
	s.TotalFiles, s.FreeFiles, s.AvailableFiles = nodes, freeNodes, freeNodes
	s.CacheHint = 0
	return nil
}

// ToHandle, FromHandle, InvalidateHandle and HandleLimit are required by
// gonfs.Handler but never reached: the caching handler this one is wrapped in
// overrides all four. They are here so the interface is satisfied, and they
// deliberately fail closed rather than inventing a handle scheme of their own.
func (h *handler) ToHandle(billy.Filesystem, []string) []byte { return nil }

func (h *handler) FromHandle([]byte) (billy.Filesystem, []string, error) {
	return nil, nil, &gonfs.NFSStatusError{NFSStatus: gonfs.NFSStatusStale}
}

func (h *handler) InvalidateHandle(billy.Filesystem, []byte) error { return nil }

func (h *handler) HandleLimit() int { return h.srv.cfg.HandleCache }

// authUnix is the AUTH_UNIX (AUTH_SYS) credential from RFC 5531 §8.2, as far
// as it is worth recording: who the client says it is. It is never checked.
type authUnix struct {
	Flavor  string
	Machine string
	UID     uint32
	GID     uint32
	GIDs    []uint32
}

// user is what the event list shows as the actor. NFS has no user name on the
// wire, only numbers and the client's own hostname, so the hostname is the
// closest thing to an identity there is; the numbers ride along in Meta.
func (a authUnix) user() string { return a.Machine }

// parseAuthUnix decodes an RPC credential. Anything it cannot read is
// reported as an empty credential rather than an error: a malformed cred must
// not cost a client its mount, and nothing here is a decision, only a record.
//
// The body is decoded by hand because the client library's AuthUnix struct
// hardcodes a single supplementary group, while a real client sends however
// many the user belongs to.
func parseAuthUnix(a nfsrpc.Auth) authUnix {
	switch a.Flavor {
	case 0:
		return authUnix{Flavor: "AUTH_NULL"}
	case 1:
	default:
		return authUnix{Flavor: "AUTH_" + itoa(a.Flavor)}
	}

	out := authUnix{Flavor: "AUTH_UNIX"}
	b := a.Body
	read := func() (uint32, bool) {
		if len(b) < 4 {
			return 0, false
		}
		v := binary.BigEndian.Uint32(b[:4])
		b = b[4:]
		return v, true
	}

	if _, ok := read(); !ok { // stamp
		return out
	}
	n, ok := read()
	if !ok {
		return out
	}
	// XDR strings are 4-byte aligned and the machine name is capped at 255
	// octets by RFC 5531; anything longer is a malformed or hostile cred.
	if n > 255 || int(n) > len(b) {
		return out
	}
	out.Machine = sanitize(string(b[:n]))
	pad := (4 - n%4) % 4
	if int(n+pad) > len(b) {
		return out
	}
	b = b[n+pad:]

	if out.UID, ok = read(); !ok {
		return out
	}
	if out.GID, ok = read(); !ok {
		return out
	}
	count, ok := read()
	if !ok {
		return out
	}
	// RFC 5531 allows 16; a few clients send more. The cap is here so a
	// bogus length cannot make us allocate.
	if count > 64 {
		count = 64
	}
	for i := uint32(0); i < count; i++ {
		g, ok := read()
		if !ok {
			break
		}
		out.GIDs = append(out.GIDs, g)
	}
	return out
}

// sanitize keeps a hostile string out of a log line and out of the event
// meta. Everything a client sends is untrusted; this one is short and
// free-form, so it is bounded and stripped of control characters at the door.
func sanitize(s string) string {
	const max = 255
	if len(s) > max {
		s = s[:max]
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// itoa avoids pulling strconv in for one label.
func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
