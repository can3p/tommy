package nfs

import (
	"testing"

	nfsrpc "github.com/willscott/go-nfs-client/nfs/rpc"
)

// TestJoinNeverLeavesTheSlashWorld pins the one place this provider touches a
// path at all. Join only joins and cleans - it decides nothing about what is
// reachable, because everything it produces is resolved through VFS.Resolve
// before a node is touched - but it must produce slash-separated paths on
// every platform and must name the root as "/" rather than the empty string,
// which is what go-nfs's own path-keyed caches file the root under.
func TestJoinNeverLeavesTheSlashWorld(t *testing.T) {
	b := &billyFS{}
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, "/"},
		{[]string{}, "/"},
		{[]string{""}, "/"},
		{[]string{"a"}, "a"},
		{[]string{"a", "b.txt"}, "a/b.txt"},
		{[]string{"a", "..", "b.txt"}, "b.txt"},
		{[]string{"a", "../../..", "b.txt"}, "../../b.txt"},
		{[]string{"a", "b/c"}, "a/b/c"},
	} {
		if got := b.Join(tc.in...); got != tc.want {
			t.Fatalf("Join(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestParseAuthUnix covers the credential this provider records and never
// checks. Everything here arrives from a client, so the malformed cases
// matter as much as the good one: none of them may cost a client its mount.
func TestParseAuthUnix(t *testing.T) {
	t.Run("null", func(t *testing.T) {
		got := parseAuthUnix(nfsrpc.AuthNull)
		if got.Flavor != "AUTH_NULL" || got.UID != 0 || got.Machine != "" {
			t.Fatalf("AUTH_NULL parsed as %+v", got)
		}
	})

	t.Run("unix", func(t *testing.T) {
		got := parseAuthUnix(nfsrpc.NewAuthUnix("build-box", 1000, 50).Auth())
		if got.Flavor != "AUTH_UNIX" {
			t.Fatalf("flavor = %q", got.Flavor)
		}
		if got.Machine != "build-box" || got.user() != "build-box" {
			t.Fatalf("machine = %q", got.Machine)
		}
		if got.UID != 1000 || got.GID != 50 {
			t.Fatalf("uid/gid = %d/%d, want 1000/50", got.UID, got.GID)
		}
	})

	t.Run("unknown flavor is named, not dropped", func(t *testing.T) {
		got := parseAuthUnix(nfsrpc.Auth{Flavor: 6, Body: []byte("rpcsec_gss")})
		if got.Flavor != "AUTH_6" {
			t.Fatalf("flavor = %q, want AUTH_6", got.Flavor)
		}
	})

	t.Run("truncated and hostile bodies never panic", func(t *testing.T) {
		full := nfsrpc.NewAuthUnix("build-box", 1000, 50).Auth()
		for i := 0; i <= len(full.Body); i++ {
			_ = parseAuthUnix(nfsrpc.Auth{Flavor: 1, Body: full.Body[:i]})
		}
		// A machine-name length that runs off the end, and one that claims
		// more supplementary groups than could possibly follow.
		_ = parseAuthUnix(nfsrpc.Auth{Flavor: 1, Body: []byte{
			0, 0, 0, 1, // stamp
			0, 0, 0, 200, // machine name length, far past the body
			'x', 'y', 'z', 0,
		}})
		_ = parseAuthUnix(nfsrpc.Auth{Flavor: 1, Body: []byte{
			0, 0, 0, 1, // stamp
			0, 0, 0, 0, // empty machine name
			0, 0, 0, 7, // uid
			0, 0, 0, 8, // gid
			0xff, 0xff, 0xff, 0xff, // gid count
		}})
	})
}

// TestSanitize keeps a hostile machine name out of a log line and out of the
// event metadata: everything a client sends is untrusted, and this one is
// short, free-form and goes straight into both.
func TestSanitize(t *testing.T) {
	if got := sanitize("build\x00box\n"); got != "buildbox" {
		t.Fatalf("control characters survived: %q", got)
	}
	if got := sanitize(string([]byte{0xff, 0xfe})); got != "" {
		t.Fatalf("invalid utf-8 survived: %q", got)
	}
	long := make([]byte, 4000)
	for i := range long {
		long[i] = 'a'
	}
	if got := sanitize(string(long)); len(got) != 255 {
		t.Fatalf("length = %d, want it bounded to 255", len(got))
	}
}

// TestFileIDIsStableAndNeverZero pins the inode numbers an NFS client caches
// by. Zero reads as "no id" to some clients, and an id that changes between
// two stats of the same path invalidates every cache the client holds.
func TestFileIDIsStableAndNeverZero(t *testing.T) {
	seen := map[uint64]string{}
	for _, p := range []string{"/", "/a", "/a/b.txt", "/b", "/a/c"} {
		id := fileID(p)
		if id == 0 {
			t.Fatalf("fileID(%q) is zero", p)
		}
		if id != fileID(p) {
			t.Fatalf("fileID(%q) is not stable", p)
		}
		if other, dup := seen[id]; dup {
			t.Fatalf("fileID collision between %q and %q", p, other)
		}
		seen[id] = p
	}
}
