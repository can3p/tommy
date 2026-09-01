package smtp

import (
	"bufio"
	"context"
	"encoding/base64"
	"net"
	netsmtp "net/smtp"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/mail"
)

// startListener boots a whole tommy with this provider on an ephemeral port, so
// parallel test runs never collide, and returns the address it bound.
func startListener(t *testing.T, values map[string]any) (*testutil.Instance, string) {
	t.Helper()

	settings := map[string]any{"port": 0}
	for k, v := range values {
		settings[k] = v
	}
	prov := New()
	cfg := config.Ephemeral()
	cfg.SetProvider(mail.PluginName, ProviderName, config.NewProviderConfig(settings))

	inst := testutil.Start(t, cfg, mail.New(prov))
	addr, err := prov.Addr(5 * time.Second)
	if err != nil {
		t.Fatalf("listener never bound: %v", err)
	}
	return inst, addr
}

func waitForMessage(t *testing.T, inst *testutil.Instance) (*event.Event, *mail.Message) {
	t.Helper()
	events := inst.WaitForEvents(1, store.Query{Plugin: mail.PluginName}, 5*time.Second)
	e := events[0]
	msg, ok := mail.MessageOf(e)
	if !ok {
		t.Fatalf("event %s carries no mail message: %+v", e.ID, e.Payload)
	}
	return e, msg
}

const sampleMessage = "From: Alice <alice@example.com>\r\n" +
	"To: Bob <bob@example.com>\r\n" +
	"Subject: =?UTF-8?B?SGVsbG8gZnJvbSB0b21teQ==?=\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"It works.\r\n"

// TestListenerEndToEnd speaks real SMTP over a real socket with the standard
// library's client, then reads the message back through the HTTP API - the
// whole path an application under test takes.
func TestListenerEndToEnd(t *testing.T) {
	inst, addr := startListener(t, nil)

	if err := netsmtp.SendMail(addr, nil, "alice@example.com", []string{"bob@example.com"}, []byte(sampleMessage)); err != nil {
		t.Fatalf("SendMail: %v", err)
	}

	e, msg := waitForMessage(t, inst)

	if msg.Subject != "Hello from tommy" {
		t.Errorf("Subject = %q, want the decoded encoded-word", msg.Subject)
	}
	if msg.From.Email != "alice@example.com" || len(msg.To) != 1 || msg.To[0].Email != "bob@example.com" {
		t.Errorf("addresses = %+v / %+v", msg.From, msg.To)
	}
	if msg.Text != "It works.\n" {
		t.Errorf("Text = %q", msg.Text)
	}

	if e.Provider != ProviderName || e.Type != mail.TypeMessage {
		t.Errorf("event = %s/%s, want mail/%s", e.Provider, e.Type, mail.TypeMessage)
	}
	if e.Raw.Transport != "smtp" {
		t.Errorf("Raw.Transport = %q, want smtp", e.Raw.Transport)
	}
	if e.Raw.PeerAddr == "" {
		t.Error("Raw.PeerAddr is empty")
	}
	if !e.Raw.Text {
		t.Error("Raw.Text is false for a plain text message")
	}
	// The raw body must be the wire bytes, untouched.
	if got := strings.TrimRight(string(e.Raw.Body), "\r\n"); got != strings.TrimRight(sampleMessage, "\r\n") {
		t.Errorf("Raw.Body = %q, want the message as sent", string(e.Raw.Body))
	}
	if got := e.Raw.Headers["Subject"]; len(got) != 1 || !strings.HasPrefix(got[0], "=?UTF-8?B?") {
		t.Errorf("Raw.Headers[Subject] = %v, want the still-encoded wire value", got)
	}

	// The envelope is recorded next to the message, not merged into it.
	envelope, ok := e.Meta["envelope"].(map[string]any)
	if !ok {
		t.Fatalf("Meta.envelope = %#v, want a map", e.Meta["envelope"])
	}
	if envelope["mail_from"] != "alice@example.com" {
		t.Errorf("Meta.envelope.mail_from = %v", envelope["mail_from"])
	}

	// And it is all readable through the plugin API.
	var view struct {
		ID      event.ID `json:"id"`
		Message struct {
			Subject string `json:"subject"`
		} `json:"message"`
	}
	if status := inst.GetJSON(inst.API("/mail/messages/"+string(e.ID)), &view); status != 200 {
		t.Fatalf("GET message: status %d", status)
	}
	if view.Message.Subject != "Hello from tommy" {
		t.Errorf("API subject = %q", view.Message.Subject)
	}

	resp := inst.Get(inst.API("/mail/messages/" + string(e.ID) + "/raw"))
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "message/rfc822" {
		t.Errorf("raw Content-Type = %q, want message/rfc822", ct)
	}
}

// TestListenerEnvelopeDiffersFromHeaders is the case someone actually reaches
// for tommy to see: a bounce-address envelope and a bcc that the headers never
// mention.
func TestListenerEnvelopeDiffersFromHeaders(t *testing.T) {
	inst, addr := startListener(t, nil)

	c, err := netsmtp.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Hello("test.example"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	if err := c.Mail("bounces+token@mailer.example"); err != nil {
		t.Fatalf("MAIL FROM: %v", err)
	}
	for _, rcpt := range []string{"bob@example.com", "archive@example.com", "auditor@example.org"} {
		if err := c.Rcpt(rcpt); err != nil {
			t.Fatalf("RCPT TO %s: %v", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		t.Fatalf("DATA: %v", err)
	}
	if _, err := w.Write([]byte(sampleMessage)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close data: %v", err)
	}
	if err := c.Quit(); err != nil {
		t.Fatalf("QUIT: %v", err)
	}

	e, msg := waitForMessage(t, inst)

	if msg.From.Email != "alice@example.com" {
		t.Errorf("canonical From = %q, want the header address", msg.From.Email)
	}
	if len(msg.To) != 1 || msg.To[0].Email != "bob@example.com" {
		t.Errorf("canonical To = %+v, want only the header recipient", msg.To)
	}

	envelope := e.Meta["envelope"].(map[string]any)
	if envelope["mail_from"] != "bounces+token@mailer.example" {
		t.Errorf("envelope sender = %v", envelope["mail_from"])
	}
	rcpts, _ := envelope["rcpt_to"].([]string)
	if len(rcpts) != 3 || rcpts[2] != "auditor@example.org" {
		t.Errorf("envelope recipients = %v, want all three", envelope["rcpt_to"])
	}
	if e.Meta["helo"] != "test.example" {
		t.Errorf("Meta.helo = %v", e.Meta["helo"])
	}
}

// TestListenerAuthPlain proves credentials are recorded and never demanded.
func TestListenerAuthPlain(t *testing.T) {
	inst, addr := startListener(t, nil)

	host, _, _ := net.SplitHostPort(addr)
	auth := netsmtp.PlainAuth("", "apikey", "s3cret", host)
	if err := netsmtp.SendMail(addr, auth, "alice@example.com", []string{"bob@example.com"}, []byte(sampleMessage)); err != nil {
		t.Fatalf("SendMail with AUTH PLAIN: %v", err)
	}

	e, _ := waitForMessage(t, inst)
	rec, ok := e.Meta["auth"].(*authRecord)
	if !ok {
		t.Fatalf("Meta.auth = %#v, want an auth record", e.Meta["auth"])
	}
	if rec.Mechanism != "PLAIN" || rec.Username != "apikey" || rec.Password != "s3cret" || !rec.Accepted {
		t.Errorf("auth record = %+v", rec)
	}
}

// TestListenerAuthLogin covers the mechanism the standard library has no client
// for and half the mail libraries still use.
func TestListenerAuthLogin(t *testing.T) {
	inst, addr := startListener(t, nil)

	conn := dialRaw(t, addr)
	defer func() { _ = conn.Close() }()

	conn.expect(t, "220")
	conn.cmd(t, "EHLO test.example")
	caps := conn.readMulti(t)
	if !strings.Contains(caps, "AUTH") || !strings.Contains(caps, "LOGIN") {
		t.Fatalf("EHLO did not advertise AUTH LOGIN: %s", caps)
	}
	conn.cmd(t, "AUTH LOGIN")
	conn.expect(t, "334")
	conn.cmd(t, base64Of("apikey"))
	conn.expect(t, "334")
	conn.cmd(t, base64Of("s3cret"))
	conn.expect(t, "235")

	conn.cmd(t, "MAIL FROM:<alice@example.com>")
	conn.expect(t, "250")
	conn.cmd(t, "RCPT TO:<bob@example.com>")
	conn.expect(t, "250")
	conn.cmd(t, "DATA")
	conn.expect(t, "354")
	conn.write(t, sampleMessage+".\r\n")
	conn.expect(t, "250")
	conn.cmd(t, "QUIT")

	e, _ := waitForMessage(t, inst)
	rec, ok := e.Meta["auth"].(*authRecord)
	if !ok {
		t.Fatalf("Meta.auth = %#v", e.Meta["auth"])
	}
	if rec.Mechanism != "LOGIN" || rec.Username != "apikey" || rec.Password != "s3cret" {
		t.Errorf("auth record = %+v", rec)
	}
}

// TestListenerPinnedCredentials proves the one case where credentials are
// judged: the configuration asked for it.
func TestListenerPinnedCredentials(t *testing.T) {
	_, addr := startListener(t, map[string]any{"username": "right", "password": "alsoright"})

	host, _, _ := net.SplitHostPort(addr)
	wrong := netsmtp.PlainAuth("", "right", "wrong", host)
	err := netsmtp.SendMail(addr, wrong, "alice@example.com", []string{"bob@example.com"}, []byte(sampleMessage))
	if err == nil {
		t.Fatal("wrong credentials were accepted even though the config pinned them")
	}

	right := netsmtp.PlainAuth("", "right", "alsoright", host)
	if err := netsmtp.SendMail(addr, right, "alice@example.com", []string{"bob@example.com"}, []byte(sampleMessage)); err != nil {
		t.Fatalf("the pinned credentials were rejected: %v", err)
	}
}

// TestListenerPinnedCredentialsRequireAuth proves an unauthenticated client is
// turned away once credentials are pinned.
func TestListenerPinnedCredentialsRequireAuth(t *testing.T) {
	_, addr := startListener(t, map[string]any{"username": "right", "password": "alsoright"})

	err := netsmtp.SendMail(addr, nil, "alice@example.com", []string{"bob@example.com"}, []byte(sampleMessage))
	if err == nil {
		t.Fatal("an unauthenticated client was accepted even though the config pinned credentials")
	}
}

// TestListenerCommands walks the command set by hand: HELO, NOOP, RSET, an
// out-of-order DATA, and QUIT.
func TestListenerCommands(t *testing.T) {
	inst, addr := startListener(t, nil)

	conn := dialRaw(t, addr)
	defer func() { _ = conn.Close() }()

	conn.expect(t, "220")
	conn.cmd(t, "HELO plain.example")
	conn.expect(t, "250")
	conn.cmd(t, "NOOP")
	conn.expect(t, "250")

	// DATA before RCPT must be refused, and must not kill the connection.
	conn.cmd(t, "DATA")
	conn.expect(t, "502")

	conn.cmd(t, "MAIL FROM:<alice@example.com>")
	conn.expect(t, "250")
	conn.cmd(t, "RSET")
	conn.expect(t, "250")

	// After a reset the transaction really is gone.
	conn.cmd(t, "DATA")
	conn.expect(t, "502")

	conn.cmd(t, "MAIL FROM:<alice@example.com>")
	conn.expect(t, "250")
	conn.cmd(t, "RCPT TO:<bob@example.com>")
	conn.expect(t, "250")
	conn.cmd(t, "DATA")
	conn.expect(t, "354")
	conn.write(t, sampleMessage+".\r\n")
	line := conn.expect(t, "250")
	if !strings.Contains(line, "queued as") {
		t.Errorf("DATA response %q does not name the stored event", line)
	}
	conn.cmd(t, "QUIT")
	conn.expect(t, "221")

	if events := inst.WaitForEvents(1, store.Query{Plugin: mail.PluginName}, 5*time.Second); len(events) != 1 {
		t.Fatalf("got %d events, want exactly one", len(events))
	}
}

// TestListenerOversizedMessage proves the size cap answers with an SMTP error
// rather than eating the memory, and that the listener survives it.
func TestListenerOversizedMessage(t *testing.T) {
	inst, addr := startListener(t, map[string]any{"max_message_bytes": 2048})

	huge := sampleMessage + strings.Repeat("x", 8192) + "\r\n"
	err := netsmtp.SendMail(addr, nil, "alice@example.com", []string{"bob@example.com"}, []byte(huge))
	if err == nil {
		t.Fatal("an oversized message was accepted")
	}
	if !strings.Contains(err.Error(), "552") {
		t.Errorf("error = %v, want a 552", err)
	}

	// The listener is still there.
	if err := netsmtp.SendMail(addr, nil, "alice@example.com", []string{"bob@example.com"}, []byte(sampleMessage)); err != nil {
		t.Fatalf("the listener did not survive an oversized message: %v", err)
	}
	events := inst.WaitForEvents(1, store.Query{Plugin: mail.PluginName}, 5*time.Second)
	if len(events) != 1 {
		t.Errorf("got %d events, want only the message that fit", len(events))
	}
}

// TestListenerMIMEOverTheWire sends a real fixture through the socket and reads
// the attachments back out of the blob store through the API.
func TestListenerMIMEOverTheWire(t *testing.T) {
	inst, addr := startListener(t, nil)

	raw := readFixture(t, "mixed_nested.eml")
	if err := netsmtp.SendMail(addr, nil, "alice@example.com", []string{"bob@example.com"}, raw); err != nil {
		t.Fatalf("SendMail: %v", err)
	}

	e, msg := waitForMessage(t, inst)
	if len(msg.Attachments) != 2 {
		t.Fatalf("got %d attachments, want 2", len(msg.Attachments))
	}
	status, body := inst.GetBody(inst.API("/mail/messages/" + string(e.ID) + "/attachments/0"))
	if status != 200 {
		t.Fatalf("attachment download: status %d", status)
	}
	if body != "id,total\r\n1,42\r\n" {
		t.Errorf("attachment body = %q", body)
	}
}

// TestListenStopsOnContextCancel proves the provider honors the lifecycle the
// core supervises it with.
func TestListenStopsOnContextCancel(t *testing.T) {
	prov := New()
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

// rawConn is a hand-driven SMTP client, for the parts of the protocol the
// standard library's client will not do.
type rawConn struct {
	net.Conn
	r *bufio.Reader
}

func dialRaw(t *testing.T, addr string) *rawConn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	return &rawConn{Conn: c, r: bufio.NewReader(c)}
}

func (c *rawConn) write(t *testing.T, s string) {
	t.Helper()
	if _, err := c.Write([]byte(s)); err != nil {
		t.Fatalf("write %q: %v", s, err)
	}
}

func (c *rawConn) cmd(t *testing.T, line string) {
	t.Helper()
	c.write(t, line+"\r\n")
}

// expect reads one reply, skipping the continuation lines of a multiline one,
// and asserts its status code.
func (c *rawConn) expect(t *testing.T, code string) string {
	t.Helper()
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			t.Fatalf("read reply (wanted %s): %v", code, err)
		}
		if len(line) >= 4 && line[3] == '-' {
			continue // a continuation line
		}
		if !strings.HasPrefix(line, code) {
			t.Fatalf("reply %q, want a %s", strings.TrimSpace(line), code)
		}
		return strings.TrimSpace(line)
	}
}

// readMulti reads a whole multiline reply and returns it.
func (c *rawConn) readMulti(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			t.Fatalf("read multiline reply: %v", err)
		}
		sb.WriteString(line)
		if len(line) >= 4 && line[3] != '-' {
			return sb.String()
		}
	}
}

func base64Of(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
