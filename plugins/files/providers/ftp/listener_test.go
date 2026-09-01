package ftp_test

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/files"
	"github.com/can3p/tommy/plugins/files/providers/ftp"
)

// startListener boots a whole tommy with this provider on an ephemeral port,
// so parallel test runs never collide, and returns the address it bound.
func startListener(t *testing.T, values map[string]any) (*testutil.Instance, string) {
	t.Helper()

	settings := map[string]any{"port": 0}
	for k, v := range values {
		settings[k] = v
	}
	prov := ftp.New()
	cfg := config.Ephemeral()
	cfg.SetProvider(files.PluginName, ftp.ProviderName, config.NewProviderConfig(settings))

	inst := testutil.Start(t, cfg, files.New(prov))
	addr, err := prov.Addr(5 * time.Second)
	if err != nil {
		t.Fatalf("listener never bound: %v", err)
	}
	return inst, addr
}

// ---------------------------------------------------------------------------
// A hand-driven FTP control connection, for everything a passive-mode data
// transfer needs - which is all of it, since Go ships no FTP client at all.
// ---------------------------------------------------------------------------

type ctl struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func dial(t *testing.T, addr string) *ctl {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	if err := c.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	cl := &ctl{t: t, conn: c, r: bufio.NewReader(c)}
	cl.expect(220)
	return cl
}

func (c *ctl) close() { _ = c.conn.Close() }

// reply reads one FTP reply, single- or multi-line ("150-..." continuations
// end at a line whose fourth character is a space rather than a dash), and
// returns its status code and full text.
func (c *ctl) reply() (int, string) {
	c.t.Helper()
	var last string
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			c.t.Fatalf("read reply: %v", err)
		}
		last = strings.TrimRight(line, "\r\n")
		if len(line) >= 4 && line[3] == '-' {
			continue
		}
		break
	}
	code, err := strconv.Atoi(last[:3])
	if err != nil {
		c.t.Fatalf("reply %q has no status code", last)
	}
	return code, last
}

func (c *ctl) send(line string) {
	c.t.Helper()
	if _, err := c.conn.Write([]byte(line + "\r\n")); err != nil {
		c.t.Fatalf("write %q: %v", line, err)
	}
}

// expect reads one reply and asserts its code.
func (c *ctl) expect(want int) string {
	c.t.Helper()
	code, text := c.reply()
	if code != want {
		c.t.Fatalf("reply %q, want %d", text, want)
	}
	return text
}

// cmd sends a command and asserts the reply code.
func (c *ctl) cmd(line string, want int) string {
	c.t.Helper()
	c.send(line)
	return c.expect(want)
}

func (c *ctl) login(user, pass string) {
	c.t.Helper()
	c.cmd("USER "+user, 331)
	c.cmd("PASS "+pass, 230)
}

var pasvRe = regexp.MustCompile(`\((\d+),(\d+),(\d+),(\d+),(\d+),(\d+)\)`)

// pasv issues PASV and dials the data connection it advertises - a real
// passive-mode transfer, not a loopback shortcut.
func (c *ctl) pasv() net.Conn {
	c.t.Helper()
	c.send("PASV")
	code, text := c.reply()
	if code != 227 {
		c.t.Fatalf("PASV reply %q, want 227", text)
	}
	m := pasvRe.FindStringSubmatch(text)
	if m == nil {
		c.t.Fatalf("PASV reply %q did not contain an address", text)
	}
	ip := strings.Join(m[1:5], ".")
	p1, _ := strconv.Atoi(m[5])
	p2, _ := strconv.Atoi(m[6])
	port := p1*256 + p2
	addr := net.JoinHostPort(ip, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		c.t.Fatalf("dial passive data connection %s: %v", addr, err)
	}
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		c.t.Fatalf("set data deadline: %v", err)
	}
	return conn
}

// reply2 sends a command and returns its reply, asserting the code.
func (c *ctl) reply2(line string, want int) (int, string) {
	c.t.Helper()
	c.send(line)
	code, text := c.reply()
	if code != want {
		c.t.Fatalf("reply %q, want %d", text, want)
	}
	return code, text
}

// stor uploads data over a passive connection and waits for the transfer to
// be confirmed complete.
func (c *ctl) stor(remote string, data []byte) {
	c.t.Helper()
	dataConn := c.pasv()
	c.send("STOR " + remote)
	code, text := c.reply()
	if code != 150 {
		c.t.Fatalf("STOR %s: reply %q, want 150", remote, text)
	}
	if _, err := dataConn.Write(data); err != nil {
		c.t.Fatalf("write data: %v", err)
	}
	if err := dataConn.Close(); err != nil {
		c.t.Fatalf("close data conn: %v", err)
	}
	c.expect(226)
}

// retr downloads a file over a passive connection.
func (c *ctl) retr(remote string) []byte {
	c.t.Helper()
	dataConn := c.pasv()
	c.send("RETR " + remote)
	code, text := c.reply()
	if code != 150 {
		c.t.Fatalf("RETR %s: reply %q, want 150", remote, text)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(dataConn); err != nil {
		c.t.Fatalf("read data: %v", err)
	}
	_ = dataConn.Close()
	c.expect(226)
	return buf.Bytes()
}

// list runs LIST or NLST over a passive connection and returns the raw text.
func (c *ctl) list(cmd, arg string) string {
	c.t.Helper()
	dataConn := c.pasv()
	line := cmd
	if arg != "" {
		line += " " + arg
	}
	c.send(line)
	code, text := c.reply()
	if code != 150 && code != 125 {
		c.t.Fatalf("%s: reply %q, want 150/125", line, text)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(dataConn); err != nil {
		c.t.Fatalf("read listing: %v", err)
	}
	_ = dataConn.Close()
	c.expect(226)
	return buf.String()
}

func waitForEvent(t *testing.T, inst *testutil.Instance, typ string) *event.Event {
	t.Helper()
	events := inst.WaitForEvents(1, store.Query{Plugin: files.PluginName, Type: typ}, 5*time.Second)
	return events[0]
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestUploadThenDownloadByteExact(t *testing.T) {
	inst, addr := startListener(t, nil)
	c := dial(t, addr)
	defer c.close()
	c.login("any", "any")

	c.cmd("MKD /upload", 257)
	mkdirEv := waitForEvent(t, inst, files.EventMkdir)
	if op, ok := files.OpOf(mkdirEv); !ok || op.Path != "/upload" || !op.Dir {
		t.Errorf("mkdir event = %+v", mkdirEv.Payload)
	}

	payload := bytes.Repeat([]byte("tommy-ftp-payload-"), 1000)
	c.stor("/upload/report.csv", payload)

	uploadEv := waitForEvent(t, inst, files.EventUpload)
	if op, ok := files.OpOf(uploadEv); !ok || op.Path != "/upload/report.csv" || op.Size != int64(len(payload)) {
		t.Errorf("upload event = %+v", uploadEv.Payload)
	}
	if uploadEv.Provider != ftp.ProviderName {
		t.Errorf("event provider = %q, want %q", uploadEv.Provider, ftp.ProviderName)
	}
	if uploadEv.Raw.Transport != "ftp" {
		t.Errorf("Raw.Transport = %q, want ftp", uploadEv.Raw.Transport)
	}
	if uploadEv.Raw.PeerAddr == "" {
		t.Error("Raw.PeerAddr is empty")
	}
	if !strings.Contains(string(uploadEv.Raw.Body), "STOR /upload/report.csv") {
		t.Errorf("Raw.Body = %q, want it to name the STOR command", uploadEv.Raw.Body)
	}
	if got := uploadEv.Meta["user"]; got != "any" {
		t.Errorf("Meta.user = %v, want the presented username", got)
	}

	got := c.retr("/upload/report.csv")
	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded %d bytes, want %d bytes matching the upload", len(got), len(payload))
	}

	// It is also visible over the plugin's own read-back API, proving the FTP
	// upload and the HTTP surface share one tree.
	status, body := inst.GetBody(inst.API("/files/content/upload/report.csv"))
	if status != 200 {
		t.Fatalf("GET content: status %d", status)
	}
	if body != string(payload) {
		t.Error("content served over the API does not match the FTP upload")
	}

	c.cmd("QUIT", 221)
}

func TestListMkdirRenameDelete(t *testing.T) {
	inst, addr := startListener(t, nil)
	c := dial(t, addr)
	defer c.close()
	c.login("any", "any")

	c.cmd("MKD /docs", 257)
	c.stor("/docs/a.txt", []byte("hello"))

	nlst := c.list("NLST", "/docs")
	if !strings.Contains(nlst, "a.txt") {
		t.Errorf("NLST /docs = %q, want it to list a.txt", nlst)
	}
	list := c.list("LIST", "/docs")
	if !strings.Contains(list, "a.txt") {
		t.Errorf("LIST /docs = %q, want it to list a.txt", list)
	}

	c.cmd("RNFR /docs/a.txt", 350)
	c.cmd("RNTO /docs/b.txt", 250)
	renameEv := waitForEvent(t, inst, files.EventRename)
	if op, ok := files.OpOf(renameEv); !ok || op.From != "/docs/a.txt" || op.Path != "/docs/b.txt" {
		t.Errorf("rename event = %+v", renameEv.Payload)
	}

	sizeReply := c.cmd("SIZE /docs/b.txt", 213)
	if !strings.Contains(sizeReply, "5") {
		t.Errorf("SIZE reply = %q, want it to report 5 bytes", sizeReply)
	}
	c.cmd("MDTM /docs/b.txt", 213)

	c.cmd("DELE /docs/b.txt", 250)
	deleteEv := waitForEvent(t, inst, files.EventDelete)
	if op, ok := files.OpOf(deleteEv); !ok || op.Path != "/docs/b.txt" || op.Dir {
		t.Errorf("delete event = %+v", deleteEv.Payload)
	}

	// The file is really gone, not just unlisted: SIZE on it now fails.
	c.cmd("SIZE /docs/b.txt", 550)

	c.cmd("RMD /docs", 250)
	// And the directory is really gone too.
	c.cmd("CWD /docs", 550)
}

func TestPassiveTransferIsReallyDataOverThatPort(t *testing.T) {
	_, addr := startListener(t, map[string]any{"passive_host": "127.0.0.1"})
	c := dial(t, addr)
	defer c.close()
	c.login("any", "any")

	// One passive connection that is opened and immediately dropped without
	// a transfer, and a second, real one right after - proves a fresh PASV
	// is negotiated per transfer rather than reusing a stale listener.
	dropped := c.pasv()
	_ = dropped.Close()

	c.stor("/x.bin", []byte("passive-mode-works"))
	if got := c.retr("/x.bin"); string(got) != "passive-mode-works" {
		t.Errorf("retr = %q", got)
	}
}

func TestEPSV(t *testing.T) {
	_, addr := startListener(t, nil)
	c := dial(t, addr)
	defer c.close()
	c.login("any", "any")

	_, text := c.reply2("EPSV", 229)
	if !strings.Contains(text, "|") {
		t.Errorf("EPSV reply = %q, want the |||port| form", text)
	}
}

func TestCWDPWDCDUPAndTraversalIsClamped(t *testing.T) {
	_, addr := startListener(t, nil)
	c := dial(t, addr)
	defer c.close()
	c.login("any", "any")

	c.cmd("PWD", 257)
	// CWD .. at the root must not escape - there is nowhere to escape to, but
	// the reply must still say the client stayed at the root rather than
	// erroring into an inconsistent state.
	c.cmd("CWD ..", 250)
	pwd := c.cmd("PWD", 257)
	if !strings.Contains(pwd, `"/"`) {
		t.Errorf("PWD after CWD .. at root = %q, want \"/\"", pwd)
	}

	c.cmd("MKD /a", 257)
	c.cmd("CWD /a", 250)
	c.cmd("CDUP", 250)
	pwd = c.cmd("PWD", 257)
	if !strings.Contains(pwd, `"/"`) {
		t.Errorf("PWD after CDUP = %q, want \"/\"", pwd)
	}

	// A traversal attempt must never reach a real file: the path resolves
	// inside the virtual tree, where nothing was ever put at that name, so it
	// must fail exactly as any other nonexistent path would - it must never
	// leak real host content.
	c.send("RETR ../../../../../../etc/passwd")
	code, text := c.reply()
	if code == 150 {
		t.Fatalf("traversal RETR was accepted: %q", text)
	}
}

func TestAuthAcceptedByDefault(t *testing.T) {
	inst, addr := startListener(t, nil)
	c := dial(t, addr)
	defer c.close()
	c.login("whoever", "whatever-password")

	c.cmd("MKD /ok", 257)
	ev := waitForEvent(t, inst, files.EventMkdir)
	if ev.Meta["user"] != "whoever" {
		t.Errorf("Meta.user = %v, want the presented username recorded even though it was never checked", ev.Meta["user"])
	}
}

func TestPinnedCredentials(t *testing.T) {
	_, addr := startListener(t, map[string]any{"username": "right", "password": "alsoright"})

	wrong := dial(t, addr)
	defer wrong.close()
	wrong.cmd("USER right", 331)
	wrong.cmd("PASS wrongpass", 530)

	right := dial(t, addr)
	defer right.close()
	right.login("right", "alsoright")
	right.cmd("NOOP", 200)
}

func TestNoopAndCommandSequencing(t *testing.T) {
	_, addr := startListener(t, nil)
	c := dial(t, addr)
	defer c.close()
	c.login("any", "any")
	c.cmd("NOOP", 200)
	c.cmd("TYPE I", 200)
	c.cmd("QUIT", 221)
}

// TestListenStopsOnContextCancel proves the provider honors the lifecycle the
// core supervises it with - the same contract the SMTP provider's own test
// proves for its listener.
func TestListenStopsOnContextCancel(t *testing.T) {
	prov := ftp.New()
	d := plugin.Deps{Config: config.NewProviderConfig(map[string]any{"port": 0})}.Normalize()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- prov.Listen(ctx, d) }()

	if _, err := prov.Addr(5 * time.Second); err != nil {
		t.Fatalf("listener never bound: %v", err)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Listen returned %v, want nil on a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not return after the context was canceled")
	}
}

// ---------------------------------------------------------------------------
// curl, if it is on PATH, drives the exact snippet this provider ships.
// ---------------------------------------------------------------------------

func TestCurlSnippetWithNestedCreateDirs(t *testing.T) {
	curlPath, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl not found on PATH")
	}

	_, addr := startListener(t, nil)
	url := "ftp://" + addr + "/a/b/c/deep.txt"

	cmd := exec.Command(curlPath, "-sS", "-T", "-", url, "--ftp-create-dirs", "-u", "any:any")
	cmd.Stdin = strings.NewReader("nested upload works\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("curl upload failed: %v\n%s", err, out)
	}

	c := dial(t, addr)
	defer c.close()
	c.login("any", "any")
	got := c.retr("/a/b/c/deep.txt")
	if string(got) != "nested upload works\n" {
		t.Errorf("downloaded %q, want the uploaded content", got)
	}
}
