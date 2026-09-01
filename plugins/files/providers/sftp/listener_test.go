package sftp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	sftplib "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/files"
)

// startSFTP boots a whole tommy with this provider on an ephemeral port and a
// throwaway host key, so parallel runs never collide and nothing is ever
// written to the real config directory.
func startSFTP(t *testing.T, values map[string]any) (*testutil.Instance, *Provider, string) {
	t.Helper()

	settings := map[string]any{
		"port":          0,
		"host_key_path": filepath.Join(t.TempDir(), "host_key"),
	}
	for k, v := range values {
		settings[k] = v
	}

	prov := New()
	cfg := config.Ephemeral()
	cfg.SetProvider(files.PluginName, ProviderName, config.NewProviderConfig(settings))

	inst := testutil.Start(t, cfg, files.New(prov))
	addr, err := prov.Addr(5 * time.Second)
	if err != nil {
		t.Fatalf("listener never bound: %v", err)
	}
	return inst, prov, addr
}

// dialSSH opens an SSH connection and reports the host key the server
// presented, which is how the persistence test compares two boots.
func dialSSH(t *testing.T, addr, user string, auth ...ssh.AuthMethod) (*ssh.Client, ssh.PublicKey) {
	t.Helper()

	var presented ssh.PublicKey
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User: user,
		Auth: auth,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			presented = key
			return nil
		},
		Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, presented
}

// dialSFTP is the whole client stack a real application uses: an SSH
// connection, the sftp subsystem on a session channel, and pkg/sftp's client.
func dialSFTP(t *testing.T, addr string) *sftplib.Client {
	t.Helper()
	client, _ := dialSSH(t, addr, "any")
	sc, err := sftplib.NewClient(client)
	if err != nil {
		t.Fatalf("start sftp subsystem: %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	return sc
}

func testKeyPair(t *testing.T) (ssh.PublicKey, ssh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer.PublicKey(), signer
}

func writeFile(t *testing.T, c *sftplib.Client, path string, data []byte) {
	t.Helper()
	f, err := c.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

func readFile(t *testing.T, c *sftplib.Client, path string) []byte {
	t.Helper()
	f, err := c.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func eventsOf(t *testing.T, inst *testutil.Instance, typ string) []*event.Event {
	t.Helper()
	return inst.Events(store.Query{Plugin: files.PluginName, Type: typ})
}

// TestUploadDownloadRoundTrip is the headline path: a real SFTP client over a
// real socket puts a file in, reads the identical bytes back, and the upload is
// visible in the tree, over the plugin API and in the event log.
func TestUploadDownloadRoundTrip(t *testing.T) {
	inst, prov, addr := startSFTP(t, nil)
	c := dialSFTP(t, addr)

	if err := c.Mkdir("/upload"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Big enough to arrive as several SSH_FXP_WRITE packets at increasing
	// offsets, which is the case a naive io.Writer implementation gets wrong.
	payload := bytes.Repeat([]byte("tommy sftp payload 0123456789\n"), 12000)
	writeFile(t, c, "/upload/report.csv", payload)

	if got := readFile(t, c, "/upload/report.csv"); !bytes.Equal(got, payload) {
		t.Fatalf("download is not byte-exact: got %d bytes, want %d", len(got), len(payload))
	}

	info, err := c.Stat("/upload/report.csv")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != int64(len(payload)) || info.IsDir() {
		t.Errorf("stat = size %d dir %v", info.Size(), info.IsDir())
	}

	// The same bytes are in the shared VFS, which is what the tab and the API
	// read from.
	stored, err := prov.tree().ReadFile(context.Background(), "/upload/report.csv")
	if err != nil {
		t.Fatalf("read from the VFS: %v", err)
	}
	if !bytes.Equal(stored, payload) {
		t.Error("the VFS copy differs from what was uploaded")
	}

	status, body := inst.GetBody(inst.API("/files/content/upload/report.csv"))
	if status != 200 || body != string(payload) {
		t.Errorf("GET /files/content: status %d, %d bytes", status, len(body))
	}

	// And both operations are in the log, tagged with this provider.
	inst.WaitForEvents(2, store.Query{Plugin: files.PluginName}, 5*time.Second)
	uploads := eventsOf(t, inst, files.EventUpload)
	if len(uploads) != 1 {
		t.Fatalf("upload events = %d, want 1", len(uploads))
	}
	e := uploads[0]
	if e.Provider != ProviderName || e.Raw.Transport != "ssh" {
		t.Errorf("event = provider %q transport %q", e.Provider, e.Raw.Transport)
	}
	if e.Raw.PeerAddr == "" {
		t.Error("no peer address recorded")
	}
	op, ok := files.OpOf(e)
	if !ok {
		t.Fatalf("event carries no files op: %+v", e.Payload)
	}
	if op.Path != "/upload/report.csv" || op.Size != int64(len(payload)) || op.Blob == nil {
		t.Errorf("op = %+v", op)
	}
	if len(eventsOf(t, inst, files.EventMkdir)) != 1 {
		t.Error("the mkdir was not recorded")
	}
}

// TestDirectoryOperations walks the rest of the surface a client uses: nested
// mkdir, listings, rename, remove and a tree walk.
func TestDirectoryOperations(t *testing.T) {
	inst, _, addr := startSFTP(t, nil)
	c := dialSFTP(t, addr)

	if err := c.MkdirAll("/a/b/c"); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	writeFile(t, c, "/a/one.txt", []byte("one"))
	writeFile(t, c, "/a/b/two.txt", []byte("two"))
	writeFile(t, c, "/a/b/c/three.txt", []byte("three"))

	entries, err := c.ReadDir("/a")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, fmt.Sprintf("%s:%v", e.Name(), e.IsDir()))
	}
	sort.Strings(names)
	if want := "b:true one.txt:false"; strings.Join(names, " ") != want {
		t.Errorf("readdir /a = %v, want %q", names, want)
	}

	// Walking is what a recursive sync does, and it needs Stat and List to
	// agree about every level.
	var walked []string
	w := c.Walk("/a")
	for w.Step() {
		if err := w.Err(); err != nil {
			t.Fatalf("walk: %v", err)
		}
		walked = append(walked, w.Path())
	}
	sort.Strings(walked)
	want := []string{"/a", "/a/b", "/a/b/c", "/a/b/c/three.txt", "/a/b/two.txt", "/a/one.txt"}
	sort.Strings(want)
	if strings.Join(walked, ",") != strings.Join(want, ",") {
		t.Errorf("walk = %v, want %v", walked, want)
	}

	if err := c.Rename("/a/one.txt", "/a/renamed.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := c.Stat("/a/one.txt"); err == nil {
		t.Error("the old name still resolves after a rename")
	}
	if got := readFile(t, c, "/a/renamed.txt"); string(got) != "one" {
		t.Errorf("renamed content = %q", got)
	}

	if err := c.Remove("/a/renamed.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := c.Stat("/a/renamed.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat after remove = %v, want a not-exist error", err)
	}

	// Removing a file with rmdir, or a non-empty directory at all, must fail:
	// a client relies on telling the two apart.
	if err := c.RemoveDirectory("/a/b"); err == nil {
		t.Error("rmdir removed a non-empty directory")
	}
	if err := c.Remove("/a/b"); err == nil {
		t.Error("remove deleted a directory")
	}
	if err := c.RemoveAll("/a/b"); err != nil {
		t.Fatalf("removeall: %v", err)
	}
	if _, err := c.Stat("/a/b"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat after removeall = %v", err)
	}

	// mkdir over something that exists is an error, the way a real server
	// answers it.
	if err := c.Mkdir("/a"); err == nil {
		t.Error("mkdir succeeded on an existing directory")
	}

	inst.WaitForEvents(1, store.Query{Plugin: files.PluginName, Type: files.EventRename}, 5*time.Second)
	renames := eventsOf(t, inst, files.EventRename)
	op, ok := files.OpOf(renames[0])
	if !ok || op.From != "/a/one.txt" || op.Path != "/a/renamed.txt" {
		t.Errorf("rename event = %+v", op)
	}
	if len(eventsOf(t, inst, files.EventDelete)) < 2 {
		t.Errorf("deletes = %d, want the file and the subtree", len(eventsOf(t, inst, files.EventDelete)))
	}
}

// TestHostiledPathsAreClampedNotEscaped drives the traversal table through a
// real client. The VFS clamps ".." at its root the way a chroot does, so an
// escape attempt lands inside the tree and can never name a host file - there
// is no host filesystem under the VFS at all.
func TestHostilePathsAreClampedNotEscaped(t *testing.T) {
	_, prov, addr := startSFTP(t, nil)
	c := dialSFTP(t, addr)

	for _, attempt := range []string{
		"/../../../etc/passwd",
		"../../../../etc/passwd",
		`..\..\..\etc\passwd`,
		"/upload/../../../../etc/passwd",
	} {
		if err := c.MkdirAll("/upload"); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		f, err := c.Create(attempt)
		if err != nil {
			// Refusing outright is just as good an answer as clamping.
			continue
		}
		if _, err := f.Write([]byte("owned")); err != nil {
			t.Fatalf("write %s: %v", attempt, err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close %s: %v", attempt, err)
		}

		// Whatever it resolved to must be inside the tree...
		var landed []string
		if err := prov.tree().Walk(func(n files.Node) error {
			landed = append(landed, n.Path)
			return nil
		}); err != nil {
			t.Fatalf("walk: %v", err)
		}
		for _, p := range landed {
			if !strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
				t.Fatalf("%q produced the path %q inside the tree", attempt, p)
			}
		}
		// ...and nothing was written to the machine.
		if _, err := os.Stat("/etc/passwd"); err == nil {
			data, readErr := os.ReadFile("/etc/passwd")
			if readErr == nil && bytes.Contains(data, []byte("owned")) {
				t.Fatalf("%q wrote to the host filesystem", attempt)
			}
		}
		if _, err := prov.tree().Stat("/etc/passwd"); err == nil {
			// Clamped into the tree, which is the documented behavior.
			if _, _, err := prov.tree().RemoveAll(context.Background(), "/etc"); err != nil {
				t.Fatalf("cleanup: %v", err)
			}
		}
	}

	// A path with a NUL or a control character is refused outright: it exists
	// only to confuse whatever renders the name.
	if err := c.Mkdir("/bad\x00name"); err == nil {
		t.Error("a NUL byte in a path was accepted")
	}
	if err := c.Mkdir("/bad\nname"); err == nil {
		t.Error("a control character in a path was accepted")
	}
}

// TestAuthAcceptedByDefault: a fake that rejected credentials would fail every
// application that has not been configured yet, so anything gets in - including
// a client that offers nothing at all, which is what makes the snippet work
// without a password prompt - and what it presented is recorded either way.
func TestAuthAcceptedByDefault(t *testing.T) {
	inst, _, addr := startSFTP(t, nil)

	// No credentials at all, which is what `sftp any@host` does.
	client, _ := dialSSH(t, addr, "deploy")
	sc, err := sftplib.NewClient(client)
	if err != nil {
		t.Fatalf("sftp with no credentials: %v", err)
	}
	defer func() { _ = sc.Close() }()
	writeFile(t, sc, "/anon.txt", []byte("hello"))

	// A client that has a key gets in just as easily. It is never asked for
	// it - the server accepted the "none" method first - which is exactly why
	// a snippet can be copy-pasted without a password prompt, and why the only
	// credential recorded for this session is the user name.
	_, signer := testKeyPair(t)
	keyed, _ := dialSSH(t, addr, "robot", ssh.PublicKeys(signer))
	kc, err := sftplib.NewClient(keyed)
	if err != nil {
		t.Fatalf("sftp with an unlisted key: %v", err)
	}
	defer func() { _ = kc.Close() }()
	writeFile(t, kc, "/robot.txt", []byte("hello"))

	inst.WaitForEvents(2, store.Query{Plugin: files.PluginName, Type: files.EventUpload}, 5*time.Second)

	byPath := map[string]map[string]any{}
	for _, e := range eventsOf(t, inst, files.EventUpload) {
		op, ok := files.OpOf(e)
		if !ok {
			t.Fatalf("event %s carries no files op", e.ID)
		}
		auth, ok := e.Meta["auth"].(map[string]any)
		if !ok {
			t.Fatalf("event %s has no auth metadata: %+v", e.ID, e.Meta)
		}
		byPath[op.Path] = auth
		if e.Meta["client_version"] == "" || e.Meta["peer"] == "" {
			t.Errorf("event %s meta = %+v", e.ID, e.Meta)
		}
		if op.User == "" {
			t.Errorf("op.User is empty on %s", op.Path)
		}
	}

	anon := byPath["/anon.txt"]
	if anon["method"] != "none" || anon["user"] != "deploy" || anon["accepted"] != true {
		t.Errorf("unauthenticated session recorded %+v", anon)
	}
	robot := byPath["/robot.txt"]
	if robot["user"] != "robot" || robot["accepted"] != true {
		t.Errorf("session recorded %+v", robot)
	}
}

// TestPresentedPasswordIsRecorded: when a client does send a password - which
// it only does when the server asks, so credentials have to be pinned for one
// to exist - the password itself lands in the event metadata, the way the SMTP
// provider records an AUTH.
func TestPresentedPasswordIsRecorded(t *testing.T) {
	inst, _, addr := startSFTP(t, map[string]any{"username": "app", "password": "s3cret"})

	client, _ := dialSSH(t, addr, "app", ssh.Password("s3cret"))
	sc, err := sftplib.NewClient(client)
	if err != nil {
		t.Fatalf("sftp: %v", err)
	}
	defer func() { _ = sc.Close() }()
	writeFile(t, sc, "/from-app.txt", []byte("hello"))

	inst.WaitForEvents(1, store.Query{Plugin: files.PluginName, Type: files.EventUpload}, 5*time.Second)
	e := eventsOf(t, inst, files.EventUpload)[0]
	auth, ok := e.Meta["auth"].(map[string]any)
	if !ok {
		t.Fatalf("no auth metadata: %+v", e.Meta)
	}
	if auth["method"] != "password" || auth["user"] != "app" || auth["password"] != "s3cret" || auth["accepted"] != true {
		t.Errorf("auth metadata = %+v", auth)
	}
	if op, _ := files.OpOf(e); op.User != "app" {
		t.Errorf("op.User = %q, want the ssh user", op.User)
	}
}

// TestPinnedCredentials is the other half of provider rule 1: pin them and they
// are enforced, so an application's error path can be exercised.
func TestPinnedCredentials(t *testing.T) {
	_, _, addr := startSFTP(t, map[string]any{"username": "app", "password": "s3cret"})

	if _, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "app",
		Auth:            []ssh.AuthMethod{ssh.Password("wrong")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}); err == nil {
		t.Error("a wrong password was accepted")
	}

	if _, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "someone-else",
		Auth:            []ssh.AuthMethod{ssh.Password("s3cret")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}); err == nil {
		t.Error("a wrong username was accepted")
	}

	// No credentials at all must not get in either, once they are pinned.
	if _, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "app",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}); err == nil {
		t.Error("an unauthenticated client was accepted even though credentials are pinned")
	}

	client, _ := dialSSH(t, addr, "app", ssh.Password("s3cret"))
	sc, err := sftplib.NewClient(client)
	if err != nil {
		t.Fatalf("the pinned credentials were rejected: %v", err)
	}
	defer func() { _ = sc.Close() }()
	writeFile(t, sc, "/ok.txt", []byte("in"))
}

// TestAuthorizedKeys covers the optional public-key allowlist.
func TestAuthorizedKeys(t *testing.T) {
	pub, signer := testKeyPair(t)
	path := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(path, ssh.MarshalAuthorizedKey(pub), 0o600); err != nil {
		t.Fatalf("write authorized_keys: %v", err)
	}
	inst, _, addr := startSFTP(t, map[string]any{"authorized_keys": path})

	_, other := testKeyPair(t)
	if _, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "any",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(other)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}); err == nil {
		t.Error("a key that is not in authorized_keys was accepted")
	}

	// A password must not walk past the allowlist either.
	if _, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            "any",
		Auth:            []ssh.AuthMethod{ssh.Password("anything")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}); err == nil {
		t.Error("a password was accepted even though authorized_keys is configured")
	}

	client, _ := dialSSH(t, addr, "any", ssh.PublicKeys(signer))
	sc, err := sftplib.NewClient(client)
	if err != nil {
		t.Fatalf("the authorized key was rejected: %v", err)
	}
	defer func() { _ = sc.Close() }()
	writeFile(t, sc, "/keyed.txt", []byte("in"))

	// An allowlisted key is a presented credential, so it is recorded like
	// any other: type and fingerprint, next to the user.
	inst.WaitForEvents(1, store.Query{Plugin: files.PluginName, Type: files.EventUpload}, 5*time.Second)
	auth, ok := eventsOf(t, inst, files.EventUpload)[0].Meta["auth"].(map[string]any)
	if !ok {
		t.Fatal("no auth metadata on the upload")
	}
	if auth["method"] != "publickey" || auth["key_type"] != ssh.KeyAlgoED25519 {
		t.Errorf("auth metadata = %+v", auth)
	}
	if fp, _ := auth["key_fingerprint"].(string); fp != ssh.FingerprintSHA256(pub) {
		t.Errorf("key fingerprint recorded as %q, want %q", fp, ssh.FingerprintSHA256(pub))
	}
}

// TestShellAndExecAreRefusedCleanly: a client that asks for a shell must get a
// refusal it can print, not a hang, and refusing must not take the listener
// down with it.
func TestShellAndExecAreRefusedCleanly(t *testing.T) {
	_, _, addr := startSFTP(t, nil)
	client, _ := dialSSH(t, addr, "any")

	// Driven at the channel level, because a high-level Session throws away
	// everything the server said once the request is refused - and the point
	// here is that the refusal carries an explanation a person can read.
	ch, chReqs, err := client.OpenChannel("session", nil)
	if err != nil {
		t.Fatalf("open session channel: %v", err)
	}
	go ssh.DiscardRequests(chReqs)
	ok, err := ch.SendRequest("exec", true, ssh.Marshal(struct{ Command string }{"cat /etc/passwd"}))
	if err != nil {
		t.Fatalf("exec request: %v", err)
	}
	if ok {
		t.Error("exec was accepted")
	}
	explanation, _ := io.ReadAll(ch.Stderr())
	if !strings.Contains(strings.ToLower(string(explanation)), "sftp") {
		t.Errorf("the refusal did not explain itself: %q", explanation)
	}
	_ = ch.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := sess.Run("cat /etc/passwd"); err == nil {
		t.Error("exec was accepted by the high-level client")
	}
	_ = sess.Close()

	shell, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := shell.Shell(); err == nil {
		t.Error("a shell was opened")
	}
	_ = shell.Close()

	// An unknown subsystem is refused the same way.
	sub, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := sub.RequestSubsystem("netconf"); err == nil {
		t.Error("an unknown subsystem was accepted")
	}
	_ = sub.Close()

	// The connection - and the listener - are still perfectly usable.
	sc, err := sftplib.NewClient(client)
	if err != nil {
		t.Fatalf("sftp after a refused exec: %v", err)
	}
	defer func() { _ = sc.Close() }()
	writeFile(t, sc, "/after-exec.txt", []byte("still here"))
	if got := readFile(t, sc, "/after-exec.txt"); string(got) != "still here" {
		t.Errorf("content = %q", got)
	}

	fresh := dialSFTP(t, addr)
	if _, err := fresh.Stat("/after-exec.txt"); err != nil {
		t.Errorf("a new connection after the refusals failed: %v", err)
	}
}

// TestHostKeyIsStableAcrossRestarts is the persistence trap, checked the way a
// client sees it: two separate boots against the same host_key_path must
// present the identical key, or every client fails with a changed-host-key
// error the second time.
func TestHostKeyIsStableAcrossRestarts(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "persisted_host_key")

	var seen []string
	var reported []string
	for i := range 2 {
		t.Run(fmt.Sprintf("boot-%d", i), func(t *testing.T) {
			_, prov, addr := startSFTP(t, map[string]any{"host_key_path": keyPath})
			_, presented := dialSSH(t, addr, "any")
			if presented == nil {
				t.Fatal("no host key was presented")
			}
			seen = append(seen, ssh.FingerprintSHA256(presented))
			reported = append(reported, prov.HostKey().Fingerprint())
		})
	}

	if len(seen) != 2 {
		t.Fatalf("only %d boots completed", len(seen))
	}
	if seen[0] != seen[1] {
		t.Fatalf("the host key changed across a restart: %s then %s\n"+
			"every client that connected once would now fail with REMOTE HOST IDENTIFICATION HAS CHANGED", seen[0], seen[1])
	}
	if reported[0] != seen[0] || reported[1] != seen[1] {
		t.Errorf("the logged fingerprint %v disagrees with what the client saw %v", reported, seen)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("the host key was never persisted: %v", err)
	}
}

// TestSetstatRecordsNothing: a chmod or a timestamp fixup is metadata, not a
// change to what the filesystem holds, and an event per setstat would drown the
// log the uploads are in.
func TestSetstatRecordsNothing(t *testing.T) {
	inst, prov, addr := startSFTP(t, nil)
	c := dialSFTP(t, addr)

	writeFile(t, c, "/stamped.txt", []byte("content"))
	inst.WaitForEvents(1, store.Query{Plugin: files.PluginName}, 5*time.Second)
	before := len(inst.Events(store.Query{Plugin: files.PluginName}))

	when := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := c.Chtimes("/stamped.txt", when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	// Modes have no analog in the VFS, so a chmod is accepted and dropped
	// rather than failing an upload that only wanted to preserve one.
	if err := c.Chmod("/stamped.txt", 0o640); err != nil {
		t.Errorf("chmod: %v", err)
	}
	n, err := prov.tree().Stat("/stamped.txt")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !n.ModTime.Equal(when) {
		t.Errorf("mtime = %s, want %s", n.ModTime, when)
	}

	// A resize is the one setstat the VFS can act on for real. It stamps a new
	// modification time, the way writing to a file does.
	if err := c.Truncate("/stamped.txt", 3); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if n, err = prov.tree().Stat("/stamped.txt"); err != nil {
		t.Fatalf("stat: %v", err)
	}
	if n.Size != 3 {
		t.Errorf("size after truncate = %d, want 3", n.Size)
	}

	if after := len(inst.Events(store.Query{Plugin: files.PluginName})); after != before {
		t.Errorf("setstat appended %d events", after-before)
	}

	// Setstat on something that is not there is still an error.
	if err := c.Chtimes("/nope.txt", when, when); err == nil {
		t.Error("setstat succeeded on a missing file")
	}
}

// TestSymlinksAreRefused: the VFS does not model links, and answering
// "unsupported" is more honest than inventing one.
func TestSymlinksAreRefused(t *testing.T) {
	_, _, addr := startSFTP(t, nil)
	c := dialSFTP(t, addr)

	writeFile(t, c, "/target.txt", []byte("x"))
	if err := c.Symlink("/target.txt", "/link.txt"); err == nil {
		t.Error("symlink was accepted")
	}
	if err := c.Link("/target.txt", "/hard.txt"); err == nil {
		t.Error("hard link was accepted")
	}
	if _, err := c.ReadLink("/target.txt"); err == nil {
		t.Error("readlink was answered")
	}
	// Lstat, which has no symlinks to distinguish it from Stat, still works.
	if _, err := c.Lstat("/target.txt"); err != nil {
		t.Errorf("lstat: %v", err)
	}
}

// TestOpenSemantics covers the flag combinations a client sends that are not a
// plain create-and-write.
func TestOpenSemantics(t *testing.T) {
	_, _, addr := startSFTP(t, nil)
	c := dialSFTP(t, addr)

	// A write into a directory that does not exist fails, the way a real
	// server answers it - the client is expected to mkdir first.
	if _, err := c.Create("/missing/file.txt"); err == nil {
		t.Error("a write into a missing directory was accepted")
	}

	writeFile(t, c, "/append.txt", []byte("one\n"))
	f, err := c.OpenFile("/append.txt", os.O_WRONLY|os.O_APPEND)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.Write([]byte("two\n")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := readFile(t, c, "/append.txt"); string(got) != "one\ntwo\n" {
		t.Errorf("after append = %q", got)
	}

	// O_EXCL on something that exists must fail.
	if _, err := c.OpenFile("/append.txt", os.O_WRONLY|os.O_CREATE|os.O_EXCL); err == nil {
		t.Error("O_EXCL succeeded on an existing file")
	}

	// Read-write on one handle: pkg/sftp asks for OpenFileWriter here.
	rw, err := c.OpenFile("/append.txt", os.O_RDWR)
	if err != nil {
		t.Fatalf("open read-write: %v", err)
	}
	buf := make([]byte, 3)
	if _, err := rw.ReadAt(buf, 0); err != nil {
		t.Fatalf("readat: %v", err)
	}
	if string(buf) != "one" {
		t.Errorf("readat = %q", buf)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reading something that is not there is a not-exist error, not a hang.
	if _, err := c.Open("/nope.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("open missing = %v, want a not-exist error", err)
	}
	if _, err := c.ReadDir("/append.txt"); err == nil {
		t.Error("listing a file succeeded")
	}
}

// TestConcurrentSessions: one tommy, several clients, all writing into the one
// shared tree. Run under -race this is the provider's half of the concurrency
// contract.
func TestConcurrentSessions(t *testing.T) {
	inst, prov, addr := startSFTP(t, nil)

	const clients = 4
	done := make(chan error, clients)
	for i := range clients {
		go func() {
			client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
				User:            fmt.Sprintf("user-%d", i),
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
				Timeout:         5 * time.Second,
			})
			if err != nil {
				done <- err
				return
			}
			defer func() { _ = client.Close() }()
			sc, err := sftplib.NewClient(client)
			if err != nil {
				done <- err
				return
			}
			defer func() { _ = sc.Close() }()

			dir := fmt.Sprintf("/client-%d", i)
			if err := sc.Mkdir(dir); err != nil {
				done <- err
				return
			}
			f, err := sc.Create(dir + "/data.bin")
			if err != nil {
				done <- err
				return
			}
			if _, err := f.Write(bytes.Repeat([]byte{byte(i)}, 4096)); err != nil {
				done <- err
				return
			}
			done <- f.Close()
		}()
	}
	for range clients {
		if err := <-done; err != nil {
			t.Fatalf("client: %v", err)
		}
	}

	for i := range clients {
		data, err := prov.tree().ReadFile(context.Background(), fmt.Sprintf("/client-%d/data.bin", i))
		if err != nil {
			t.Fatalf("read client %d: %v", i, err)
		}
		if len(data) != 4096 || data[0] != byte(i) {
			t.Errorf("client %d wrote %d bytes starting %v", i, len(data), data[:1])
		}
	}
	inst.WaitForEvents(clients*2, store.Query{Plugin: files.PluginName}, 5*time.Second)
}
