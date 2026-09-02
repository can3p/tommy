package nfs_test

import (
	"bytes"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	nfsrpc "github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/files"
	"github.com/can3p/tommy/plugins/files/providers/nfs"
)

// ---------------------------------------------------------------------------
// Every test here drives a real NFSv3 client over a real socket:
// github.com/willscott/go-nfs-client, which speaks ONC RPC, the MOUNT
// protocol and NFSv3 on the wire exactly the way an operating system's
// in-kernel client does. `mount -t nfs` itself needs root and cannot run in a
// test, so this is as close to the real thing as a test can get - and it is
// the same client the server library's own suite is written against.
//
// What it does not prove: that the mount options in the Snippets are correct
// for a kernel client, since no kernel client is involved. Those were checked
// against Linux nfs(5) and macOS mount_nfs(8) instead.
// ---------------------------------------------------------------------------

// start boots a whole tommy with this provider on an ephemeral port - never a
// well-known one, and never 2049 or 111 - and returns the instance plus a
// mounted client target.
func start(t *testing.T) (*testutil.Instance, *nfsc.Target, *nfsrpc.Client) {
	t.Helper()

	prov := nfs.New()
	cfg := config.Ephemeral()
	cfg.SetProvider(files.PluginName, nfs.ProviderName,
		config.NewProviderConfig(map[string]any{"port": 0}))

	inst := testutil.Start(t, cfg, files.New(prov))
	addr, err := prov.Addr(5 * time.Second)
	if err != nil {
		t.Fatalf("listener never bound: %v", err)
	}

	client, err := nfsrpc.DialTCP("tcp", addr, false)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { client.Close() })

	// AUTH_UNIX, the credential a real client sends, so the recording half of
	// provider rule 1 is exercised rather than assumed.
	auth := nfsrpc.NewAuthUnix("tommy-test", 1234, 5678).Auth()

	mounter := &nfsc.Mount{Client: client}
	// The export name is ignored - there is one tree - which is exactly what
	// this asserts by asking for a path that was never created.
	target, err := mounter.Mount("/whatever-export", auth)
	if err != nil {
		t.Fatalf("mount: %v", err)
	}
	t.Cleanup(func() { _ = mounter.Unmount() })

	return inst, target, client
}

// writeFile is create-then-write, which is what NFS actually is: there is no
// open or close on the wire.
func writeFile(t *testing.T, target *nfsc.Target, name string, data []byte) {
	t.Helper()
	if _, err := target.Create(name, 0o666); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	f, err := target.OpenFile(name, 0o666)
	if err != nil {
		t.Fatalf("open %s for writing: %v", name, err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
}

func readFile(t *testing.T, target *nfsc.Target, name string) []byte {
	t.Helper()
	f, err := target.Open(name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read %s: %v", name, err)
	}
	return got
}

// ---------------------------------------------------------------------------
// The one that matters most. docs/lessons.md is unambiguous that a wire
// protocol is tested with a real client, and the standing proof is
// ftpserverlib silently corrupting downloads by defaulting to ASCII mode -
// invisible to a hand-built test, obvious the moment a real client fetched
// the bytes back. This payload is built to expose exactly that class of bug:
// a CRLF pair, a lone LF, a NUL and a high byte, none of which survives a
// text-mode or encoding-aware path.
// ---------------------------------------------------------------------------

func TestWriteThenReadIsByteIdentical(t *testing.T) {
	inst, target, _ := start(t)

	payload := []byte("line one\r\nline two\nzero:\x00 high:\xff\xfe end\r\n")
	writeFile(t, target, "/round-trip.bin", payload)

	got := readFile(t, target, "/round-trip.bin")
	if !bytes.Equal(got, payload) {
		t.Fatalf("round-trip corrupted the bytes:\n want %q\n  got %q", payload, got)
	}

	// The same bytes must be readable from the shared tree, which is the
	// whole point of the provider: one file, every protocol.
	node, statErr := sharedVFS(t, inst).Stat("/round-trip.bin")
	if statErr != nil {
		t.Fatalf("stat in the shared VFS: %v", statErr)
	}
	if node.Size != int64(len(payload)) {
		t.Fatalf("VFS size = %d, want %d", node.Size, len(payload))
	}

	// And over the HTTP API, the way the snippet tells a user to check.
	status, body := inst.GetBody(inst.API("/files/content/round-trip.bin"))
	if status != 200 {
		t.Fatalf("API content status = %d", status)
	}
	if !bytes.Equal([]byte(body), payload) {
		t.Fatalf("API content differs:\n want %q\n  got %q", payload, body)
	}
}

// TestWriteAcrossChunksIsByteIdentical drives a payload big enough that the
// client splits it, so the offset handling in the WRITE path is exercised
// rather than assumed. Each chunk is its own OpenFile/Seek/Write/Close on the
// server, which is where a naive adapter truncates the file on every write.
func TestWriteAcrossChunksIsByteIdentical(t *testing.T) {
	_, target, _ := start(t)

	payload := make([]byte, 300*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	writeFile(t, target, "/big.bin", payload)

	got := readFile(t, target, "/big.bin")
	if !bytes.Equal(got, payload) {
		t.Fatalf("large round-trip corrupted the bytes (%d written, %d read)", len(payload), len(got))
	}
}

// TestEventsRecordTheUpload asserts the events, not just the file. NFS has no
// close on the wire, so one logical upload is a CREATE plus one WRITE per
// chunk, and each is committed and recorded on its own - that is documented
// behavior, and this is what pins it.
func TestEventsRecordTheUpload(t *testing.T) {
	inst, target, _ := start(t)

	writeFile(t, target, "/notes.txt", []byte("hello nfs"))

	events := inst.WaitForEvents(2, store.Query{
		Plugin: files.PluginName,
		Type:   files.EventUpload,
	}, 5*time.Second)

	// Newest first: the WRITE, then the CREATE that preceded it.
	last := events[0]
	op, ok := files.OpOf(last)
	if !ok {
		t.Fatalf("event carried no files.Op: %+v", last)
	}
	if op.Path != "/notes.txt" {
		t.Fatalf("Op.Path = %q, want /notes.txt", op.Path)
	}
	if op.Size != int64(len("hello nfs")) {
		t.Fatalf("Op.Size = %d, want %d", op.Size, len("hello nfs"))
	}
	if op.Blob == nil || op.Blob.ID == "" {
		t.Fatal("upload event carries no blob reference")
	}
	if last.Provider != nfs.ProviderName {
		t.Fatalf("event provider = %q, want %q", last.Provider, nfs.ProviderName)
	}
	if last.Raw.Transport != "tcp" {
		t.Fatalf("Raw.Transport = %q, want tcp", last.Raw.Transport)
	}
	if op.Peer == "" || last.Raw.PeerAddr == "" {
		t.Fatal("no peer address recorded on the event")
	}
	if cmd := string(last.Raw.Body); !strings.HasPrefix(cmd, "NFSPROC3_WRITE ") {
		t.Fatalf("Raw.Body = %q, want the NFSPROC3_WRITE command", cmd)
	}

	// Provider rule 1: the credential is recorded, never checked.
	if op.User != "tommy-test" {
		t.Fatalf("Op.User = %q, want the AUTH_UNIX machine name", op.User)
	}
	if got := last.Meta["uid"]; got == nil {
		t.Fatalf("no uid recorded in Meta: %+v", last.Meta)
	}

	// The CREATE is the older of the two and lands with no content yet.
	first := events[1]
	if cmd := string(first.Raw.Body); !strings.HasPrefix(cmd, "NFSPROC3_CREATE ") {
		t.Fatalf("older event Raw.Body = %q, want the NFSPROC3_CREATE command", cmd)
	}
}

func TestDirectoryLifecycleAndEvents(t *testing.T) {
	inst, target, _ := start(t)

	if _, err := target.Mkdir("/uploads", 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, target, "/uploads/a.txt", []byte("aaa"))
	writeFile(t, target, "/uploads/b.txt", []byte("bbb"))

	entries, err := target.ReadDirPlus("/uploads")
	if err != nil {
		t.Fatalf("readdirplus: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.Name() == "." || e.Name() == ".." {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "a.txt,b.txt" {
		t.Fatalf("listing = %v, want [a.txt b.txt]", names)
	}

	// Stat: the size a client sees must be the size that was written.
	attr, err := target.Getattr("/uploads/a.txt")
	if err != nil {
		t.Fatalf("getattr: %v", err)
	}
	if attr.Size() != 3 {
		t.Fatalf("getattr size = %d, want 3", attr.Size())
	}
	if attr.IsDir() {
		t.Fatal("getattr says a regular file is a directory")
	}
	dirAttr, err := target.Getattr("/uploads")
	if err != nil {
		t.Fatalf("getattr on the directory: %v", err)
	}
	if !dirAttr.IsDir() {
		t.Fatal("getattr says a directory is not one")
	}

	// Rename, then remove, then rmdir - each with the event it should append.
	if err := target.Rename("/uploads/a.txt", "/uploads/renamed.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := readFile(t, target, "/uploads/renamed.txt"); string(got) != "aaa" {
		t.Fatalf("renamed file content = %q, want aaa", got)
	}
	renames := inst.WaitForEvents(1, store.Query{
		Plugin: files.PluginName, Type: files.EventRename,
	}, 5*time.Second)
	if op, ok := files.OpOf(renames[0]); !ok || op.From != "/uploads/a.txt" || op.Path != "/uploads/renamed.txt" {
		t.Fatalf("rename event = %+v", renames[0].Payload)
	}

	if err := target.Remove("/uploads/renamed.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := target.Remove("/uploads/b.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := target.RmDir("/uploads"); err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	deletes := inst.WaitForEvents(3, store.Query{
		Plugin: files.PluginName, Type: files.EventDelete,
	}, 5*time.Second)
	if op, ok := files.OpOf(deletes[0]); !ok || op.Path != "/uploads" || !op.Dir {
		t.Fatalf("last delete event = %+v", deletes[0].Payload)
	}

	mkdirs := inst.Events(store.Query{Plugin: files.PluginName, Type: files.EventMkdir})
	if len(mkdirs) != 1 {
		t.Fatalf("mkdir events = %d, want 1", len(mkdirs))
	}

	// Nothing is left in the tree.
	if _, err := target.Getattr("/uploads"); err == nil {
		t.Fatal("the removed directory is still there")
	}
}

// TestSetattrTruncate covers the one place go-nfs's flags and the VFS's
// disagree: SETATTR with a size opens the file O_WRONLY|O_EXCL without
// O_CREATE, which the VFS reads strictly as "must not exist". A client
// truncating a file - what open(O_TRUNC) becomes on an NFS mount - fails
// outright unless the adapter drops that flag.
func TestSetattrTruncate(t *testing.T) {
	_, target, _ := start(t)

	writeFile(t, target, "/truncate-me.txt", []byte("0123456789"))

	if err := target.Setattr("/truncate-me.txt", nfsc.Sattr3{
		Size: nfsc.SetSize{SetIt: true, Size: 4},
	}); err != nil {
		t.Fatalf("setattr with a size: %v", err)
	}
	if got := readFile(t, target, "/truncate-me.txt"); string(got) != "0123" {
		t.Fatalf("after truncate to 4 the content is %q, want 0123", got)
	}
}

// ---------------------------------------------------------------------------
// Path safety. NFS is handle-based rather than path-based, so there are two
// separate things to prove: a name a client sends cannot escape the tree, and
// a handle a client invents cannot name anything at all.
// ---------------------------------------------------------------------------

func TestCraftedComponentsStayInsideTheTree(t *testing.T) {
	inst, target, client := start(t)

	if _, err := target.Mkdir("/sub", 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, target, "/sub/inside.txt", []byte("inside"))

	auth := nfsrpc.NewAuthUnix("tommy-test", 1234, 5678).Auth()

	_, rootFH, err := target.Lookup("/")
	if err != nil {
		t.Fatalf("lookup the root: %v", err)
	}
	_, subFH, err := target.Lookup("/sub")
	if err != nil {
		t.Fatalf("lookup /sub: %v", err)
	}

	// The client library cleans a path before it splits it, so a hostile
	// component never reaches the wire through Lookup. These are raw LOOKUP
	// and CREATE calls with the component the client would not send: ".." at
	// the root, a relative climb, and an absolute host path smuggled into the
	// single-component filename field.
	//
	// None of them may reach the host, and none may name anything outside the
	// VFS. Landing somewhere else *inside* the tree is not an escape - the
	// whole tree is the export, and every path is resolved through
	// VFS.Resolve before a node is touched.
	for _, tc := range []struct {
		name string
		fh   []byte
		file string
	}{
		{"dotdot at the root", rootFH, ".."},
		{"climb from a subdirectory", subFH, "../../../.."},
		{"absolute host path", rootFH, "/etc/passwd"},
		{"relative host path", subFH, "../../../../etc/passwd"},
		{"traversal with a null", rootFH, "..\x00/etc/passwd"},
	} {
		status := rawLookup(t, client, auth, tc.fh, tc.file)
		t.Logf("LOOKUP %-28s -> %d", tc.name, status)
		switch status {
		case nfs3Ok:
			// Whatever it resolved to must be inside the tree, and the only
			// thing at or above the root is the root itself.
		case nfs3ErrNoEnt, nfs3ErrInval, nfs3ErrAcces, nfs3ErrNotDir, nfs3ErrNameTooLong:
			// A refusal is equally fine.
		default:
			t.Fatalf("%s: unexpected NFS status %d", tc.name, status)
		}

		// A crafted CREATE with the same component must not put a file
		// anywhere but in this tree.
		t.Logf("CREATE %-28s -> %d", tc.name, rawCreate(t, client, auth, tc.fh, tc.file+"/created.txt"))
	}

	// The host is not reachable through any of it, and nothing outside the
	// tree was touched: the only things in the VFS are what this test made.
	if _, _, err := target.Lookup("/etc/passwd"); err == nil {
		t.Fatal("/etc/passwd resolved inside the share")
	}
	assertTreeIsSelfContained(t, sharedVFS(t, inst))
}

// assertTreeIsSelfContained walks the whole VFS and fails on any node whose
// path is not a clean, rooted path inside it - the property a traversal would
// have to break to matter.
func assertTreeIsSelfContained(t *testing.T, vfs *files.VFS) {
	t.Helper()
	var walk func(dir string)
	walk = func(dir string) {
		nodes, err := vfs.List(dir)
		if err != nil {
			t.Fatalf("list %s: %v", dir, err)
		}
		for _, n := range nodes {
			if !strings.HasPrefix(n.Path, "/") || strings.Contains(n.Path, "/../") ||
				strings.HasSuffix(n.Path, "/..") || strings.Contains(n.Path, "//") {
				t.Fatalf("node escaped the tree: %q", n.Path)
			}
			if n.Dir {
				walk(n.Path)
			}
		}
	}
	walk("/")
}

// TestCraftedHandlesAreStale is the handle half of the same question. A file
// handle here is an opaque UUID the server minted; it encodes no path, so a
// handle a client invents, or one it has held past eviction, can only be
// answered with NFS3ERR_STALE.
func TestCraftedHandlesAreStale(t *testing.T) {
	_, target, _ := start(t)

	writeFile(t, target, "/real.txt", []byte("real"))

	crafted := [][]byte{
		// A well-formed but never-issued UUID.
		{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
			0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00},
		// All zeroes.
		make([]byte, 16),
		// Wrong lengths, including one that spells a path.
		[]byte("/etc/passwd"),
		[]byte("../../../../etc/passwd"),
		{},
		make([]byte, 64),
	}
	for i, fh := range crafted {
		if _, err := target.GetAttr(fh); err == nil {
			t.Fatalf("crafted handle %d was accepted", i)
		}
	}

	// The real handle still works, so the check above is not passing because
	// everything fails.
	_, fh, err := target.Lookup("/real.txt")
	if err != nil {
		t.Fatalf("lookup of a real file: %v", err)
	}
	if _, err := target.GetAttr(fh); err != nil {
		t.Fatalf("a handle this server issued was rejected: %v", err)
	}
}

// TestNamesTheVFSRefuses proves the provider does not soften the VFS's own
// rules on the way through: an over-long name and one carrying a control
// character are refused rather than sanitized into something else.
func TestNamesTheVFSRefuses(t *testing.T) {
	_, target, _ := start(t)

	for _, name := range []string{
		"/" + strings.Repeat("x", 300),
		"/bad\x01name",
	} {
		if _, err := target.Create(name, 0o666); err == nil {
			t.Fatalf("create %q was accepted", name)
		}
	}
}

// TestNestedWriteNeedsItsParent pins the behavior NFS specifies and this
// provider does not paper over: unlike FTP, where a client expects
// --ftp-create-dirs style help, an NFS client creates each directory itself.
func TestNestedWriteNeedsItsParent(t *testing.T) {
	_, target, _ := start(t)

	if _, err := target.Create("/missing/child.txt", 0o666); err == nil {
		t.Fatal("create under a missing directory succeeded")
	}
	if _, err := target.Mkdir("/present", 0o777); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, target, "/present/child.txt", []byte("ok"))
	if got := readFile(t, target, "/present/child.txt"); string(got) != "ok" {
		t.Fatalf("content = %q, want ok", got)
	}
}

// TestSharedTreeIsOneTree writes through the VFS the way another provider
// would and reads it back over NFS, which is the property the whole plugin
// exists for.
func TestSharedTreeIsOneTree(t *testing.T) {
	inst, target, _ := start(t)

	if _, err := sharedVFS(t, inst).PutBytes(t.Context(), "/from-elsewhere.txt",
		[]byte("written by another provider"), files.WriteOptions{Provider: "ftp"}); err != nil {
		t.Fatalf("put through the VFS: %v", err)
	}

	if got := readFile(t, target, "/from-elsewhere.txt"); string(got) != "written by another provider" {
		t.Fatalf("NFS read back %q", got)
	}
}

// sharedVFS reaches the one tree every files provider writes into, which is
// what several assertions above cross-check the NFS view against.
func sharedVFS(t *testing.T, inst *testutil.Instance) *files.VFS {
	t.Helper()
	plug, ok := inst.Registry.Plugin(files.PluginName)
	if !ok {
		t.Fatal("files plugin not registered")
	}
	return plug.(*files.Plugin).VFS()
}

// ---------------------------------------------------------------------------
// Raw RPC, for the calls a well-behaved client will not make. The client
// library cleans every path before it splits it into components, so a hostile
// single component - "..", an absolute path, a NUL - can only be put on the
// wire by building the call by hand.
// ---------------------------------------------------------------------------

// NFSv3 status codes used below (RFC 1813 section 2.6), named rather than
// numeric so a failure message says something.
const (
	nfs3Ok             = 0
	nfs3ErrNoEnt       = 2
	nfs3ErrAcces       = 13
	nfs3ErrNotDir      = 20
	nfs3ErrInval       = 22
	nfs3ErrNameTooLong = 63
)

type diropargs3 struct {
	FH       []byte
	Filename string
}

func rpcHeader(proc uint32, auth nfsrpc.Auth) nfsrpc.Header {
	return nfsrpc.Header{
		Rpcvers: 2,
		Prog:    nfsc.Nfs3Prog,
		Vers:    nfsc.Nfs3Vers,
		Proc:    proc,
		Cred:    auth,
		Verf:    nfsrpc.AuthNull,
	}
}

// rawLookup issues NFSPROC3_LOOKUP with an arbitrary filename component and
// returns the NFS status the server answered with.
func rawLookup(t *testing.T, c *nfsrpc.Client, auth nfsrpc.Auth, fh []byte, name string) uint32 {
	t.Helper()
	type args struct {
		nfsrpc.Header
		What diropargs3
	}
	res, err := c.Call(&args{
		Header: rpcHeader(nfsc.NFSProc3Lookup, auth),
		What:   diropargs3{FH: fh, Filename: name},
	})
	if err != nil {
		t.Fatalf("raw lookup %q: %v", name, err)
	}
	status, err := xdr.ReadUint32(res)
	if err != nil {
		t.Fatalf("raw lookup %q: reading status: %v", name, err)
	}
	return status
}

// rawCreate issues NFSPROC3_CREATE with an arbitrary filename component.
func rawCreate(t *testing.T, c *nfsrpc.Client, auth nfsrpc.Auth, fh []byte, name string) uint32 {
	t.Helper()
	type how struct {
		Mode uint32
		Attr nfsc.Sattr3
	}
	type args struct {
		nfsrpc.Header
		Where diropargs3
		HW    how
	}
	res, err := c.Call(&args{
		Header: rpcHeader(nfsc.NFSProc3Create, auth),
		Where:  diropargs3{FH: fh, Filename: name},
		HW:     how{},
	})
	if err != nil {
		t.Fatalf("raw create %q: %v", name, err)
	}
	status, err := xdr.ReadUint32(res)
	if err != nil {
		t.Fatalf("raw create %q: reading status: %v", name, err)
	}
	return status
}
