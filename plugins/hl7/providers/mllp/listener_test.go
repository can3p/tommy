package mllp

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/hl7"
)

// startListener boots a whole tommy with this provider on an ephemeral port,
// so parallel test runs never collide, and returns the address it bound.
func startListener(t *testing.T, values map[string]any) (*testutil.Instance, string) {
	t.Helper()

	settings := map[string]any{"port": 0}
	for k, v := range values {
		settings[k] = v
	}
	prov := New()
	cfg := config.Ephemeral()
	cfg.SetProvider(hl7.Name, ProviderName, config.NewProviderConfig(settings))

	inst := testutil.Start(t, cfg, hl7.New(prov))
	addr, err := prov.Addr(5 * time.Second)
	if err != nil {
		t.Fatalf("listener never bound: %v", err)
	}
	return inst, addr
}

// dial connects to addr with a generous deadline, so a bug that hangs a test
// fails fast instead of hanging the suite.
func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	// Nagle's algorithm would happily coalesce the small, deliberately
	// separated writes the split-packet test makes into one read on the
	// server side, defeating the point of the test. Every other test is
	// unaffected by turning it off.
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return c
}

// readACK reads the next MLLP frame off r using the provider's own frame
// reader - the same accumulate-and-find-the-trailer logic a real
// integration engine's client side needs, exercised here as the test's
// client. maxSize 0 means "no limit", which is fine for a test asserting on
// small, well-formed ACKs.
func readACK(t *testing.T, fr *frameReader) []byte {
	t.Helper()
	payload, err := fr.next()
	if err != nil {
		t.Fatalf("reading ack: %v", err)
	}
	return payload
}

// parseACK parses an ACK's wire bytes back into the canonical model, the way
// a real receiving engine would, so assertions read fields by HL7 path
// rather than by hand-splitting strings.
func parseACK(t *testing.T, b []byte) *hl7.Message {
	t.Helper()
	msg, err := hl7.Parse(b)
	if err != nil {
		t.Fatalf("ack does not parse as hl7: %v\nraw: %q", err, b)
	}
	return msg
}

func waitForMessage(t *testing.T, inst *testutil.Instance) (*event.Event, *hl7.Message) {
	t.Helper()
	events := inst.WaitForEvents(1, store.Query{Plugin: hl7.Name}, 5*time.Second)
	e := events[0]
	msg, ok := hl7.MessageOf(e)
	if !ok {
		t.Fatalf("event %s carries no hl7 message: %+v", e.ID, e.Payload)
	}
	return e, msg
}

const sampleMSH = "MSH|^~\\&|SEND_APP|SEND_FAC|RECV_APP|RECV_FAC|20240101120000||ADT^A01|MSG00001|P|2.5"
const samplePID = "PID|1||123456^^^MRN||DOE^JOHN^A||19800101|M"

var sampleMessage = sampleMSH + "\r" + samplePID + "\r"

// TestListenerEndToEnd speaks real MLLP over a real socket, then asserts
// both the canonical model recorded and the exact ACK an integration engine
// would receive back - including that the ACK is itself correctly
// MLLP-framed.
func TestListenerEndToEnd(t *testing.T) {
	inst, addr := startListener(t, nil)

	conn := dial(t, addr)
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(frame([]byte(sampleMessage))); err != nil {
		t.Fatalf("write: %v", err)
	}

	e, msg := waitForMessage(t, inst)

	if got := msg.Header.MessageType(); got != "ADT^A01" {
		t.Errorf("MessageType = %q", got)
	}
	if got := msg.Header.ControlID; got != "MSG00001" {
		t.Errorf("ControlID = %q", got)
	}
	if got := msg.PatientName(); got != "DOE^JOHN^A" {
		t.Errorf("PatientName = %q", got)
	}
	if e.Provider != ProviderName || e.Type != hl7.EventType {
		t.Errorf("event = %s/%s, want %s/%s", e.Provider, e.Type, ProviderName, hl7.EventType)
	}
	if e.Raw.Transport != "tcp" {
		t.Errorf("Raw.Transport = %q, want tcp", e.Raw.Transport)
	}
	if e.Raw.PeerAddr == "" {
		t.Error("Raw.PeerAddr is empty")
	}
	if string(e.Raw.Body) != sampleMessage {
		t.Errorf("Raw.Body = %q, want the message as sent", e.Raw.Body)
	}
	if e.Meta["framing"] != "mllp" {
		t.Errorf("Meta.framing = %v, want mllp", e.Meta["framing"])
	}
	if e.Meta["peer_addr"] != e.Raw.PeerAddr {
		t.Errorf("Meta.peer_addr = %v, want %v", e.Meta["peer_addr"], e.Raw.PeerAddr)
	}

	fr := newFrameReader(conn, 0)
	ack := parseACK(t, readACK(t, fr))

	if got := ack.Value("MSH-3"); got != "RECV_APP" {
		t.Errorf("ACK MSH-3 (sending app) = %q, want the original's receiving app", got)
	}
	if got := ack.Value("MSH-4"); got != "RECV_FAC" {
		t.Errorf("ACK MSH-4 (sending facility) = %q", got)
	}
	if got := ack.Value("MSH-5"); got != "SEND_APP" {
		t.Errorf("ACK MSH-5 (receiving app) = %q, want the original's sending app", got)
	}
	if got := ack.Value("MSH-6"); got != "SEND_FAC" {
		t.Errorf("ACK MSH-6 (receiving facility) = %q", got)
	}
	if got := ack.Value("MSH-9.1"); got != "ACK" {
		t.Errorf("ACK MSH-9.1 = %q", got)
	}
	if got := ack.Value("MSH-9.2"); got != "A01" {
		t.Errorf("ACK MSH-9.2 (trigger event) = %q, want it echoed", got)
	}
	if got := ack.Value("MSH-11"); got != "P" {
		t.Errorf("ACK MSH-11 (processing id) = %q, want it echoed", got)
	}
	if got := ack.Value("MSH-12"); got != "2.5" {
		t.Errorf("ACK MSH-12 (version) = %q, want it echoed", got)
	}
	if got := ack.Value("MSH-10"); got == "" || got == "MSG00001" {
		t.Errorf("ACK MSH-10 (control id) = %q, want a fresh one, not empty or the original's", got)
	}
	if got := ack.Value("MSA-1"); got != "AA" {
		t.Errorf("MSA-1 = %q, want AA", got)
	}
	if got := ack.Value("MSA-2"); got != "MSG00001" {
		t.Errorf("MSA-2 = %q, want the original control id echoed", got)
	}
}

// TestListenerCustomSeparators is the test the plan calls out explicitly:
// using the conventional |^~\& separators for the ACK instead of the
// message's own declared set would be the exact bug this plugin exists to
// expose.
func TestListenerCustomSeparators(t *testing.T) {
	inst, addr := startListener(t, nil)

	// Field ';', component '!', repetition '@', escape '#', subcomponent '$'
	// - nothing here is the conventional set.
	msg := "MSH;!@#$;SEND_APP;SEND_FAC;RECV_APP;RECV_FAC;20240101120000;;ADT!A01;MSG00002;P;2.5\r" +
		"PID;1\r"

	conn := dial(t, addr)
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(frame([]byte(msg))); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, parsed := waitForMessage(t, inst)
	if parsed.Separators.Standard() {
		t.Fatal("test message unexpectedly parsed with the conventional separators")
	}
	if parsed.Separators.Field != ";" || parsed.Separators.Component != "!" {
		t.Fatalf("test message did not parse with the intended custom separators: %+v", parsed.Separators)
	}

	fr := newFrameReader(conn, 0)
	raw := readACK(t, fr)

	// The raw ACK bytes must themselves use ';' and '!', not '|' and '^' -
	// checked before parsing, since hl7.Parse would recover the message
	// even if the wrong separators were used for its own header, hiding
	// exactly the bug this test exists to catch.
	if !strings.Contains(string(raw), ";!@#$;") {
		t.Fatalf("ack does not carry the original message's encoding characters verbatim: %q", raw)
	}

	ack := parseACK(t, raw)
	if ack.Separators.Field != ";" || ack.Separators.Component != "!" || ack.Separators.Repetition != "@" ||
		ack.Separators.Escape != "#" || ack.Separators.Subcomponent != "$" {
		t.Errorf("ack separators = %+v, want the message's own", ack.Separators)
	}
	if got := ack.Value("MSH-9.1") + "!" + ack.Value("MSH-9.2"); got != "ACK!A01" {
		t.Errorf("ack MSH-9 = %q", got)
	}
	if got := ack.Value("MSA-2"); got != "MSG00002" {
		t.Errorf("MSA-2 = %q", got)
	}
}

// TestListenerMessageSplitAcrossPackets proves the reader accumulates rather
// than assuming one Read gives one message: the frame is written in several
// separate, deliberately spaced-out writes.
func TestListenerMessageSplitAcrossPackets(t *testing.T) {
	inst, addr := startListener(t, nil)

	conn := dial(t, addr)
	defer func() { _ = conn.Close() }()

	whole := frame([]byte(sampleMessage))
	chunks := splitInto(whole, 5)
	for _, c := range chunks {
		if _, err := conn.Write(c); err != nil {
			t.Fatalf("write chunk: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	e, msg := waitForMessage(t, inst)
	if msg.Header.ControlID != "MSG00001" {
		t.Errorf("ControlID = %q", msg.Header.ControlID)
	}
	if string(e.Raw.Body) != sampleMessage {
		t.Errorf("Raw.Body = %q, want the reassembled message", e.Raw.Body)
	}

	fr := newFrameReader(conn, 0)
	ack := parseACK(t, readACK(t, fr))
	if ack.Value("MSA-1") != "AA" || ack.Value("MSA-2") != "MSG00001" {
		t.Errorf("ack = MSA-1 %q MSA-2 %q", ack.Value("MSA-1"), ack.Value("MSA-2"))
	}
}

// splitInto breaks b into n roughly equal, non-empty chunks, in order.
func splitInto(b []byte, n int) [][]byte {
	if n <= 0 || n > len(b) {
		n = len(b)
	}
	size := (len(b) + n - 1) / n
	var out [][]byte
	for i := 0; i < len(b); i += size {
		end := i + size
		if end > len(b) {
			end = len(b)
		}
		out = append(out, b[i:end])
	}
	return out
}

// TestListenerPipelinedMessages proves several messages arriving back to
// back on one connection - here in a single Write, so they are guaranteed to
// land in one underlying Read on the server side - are captured and
// acknowledged as two separate messages, in order.
func TestListenerPipelinedMessages(t *testing.T) {
	inst, addr := startListener(t, nil)

	second := "MSH|^~\\&|SEND_APP|SEND_FAC|RECV_APP|RECV_FAC|20240101120100||ORU^R01|MSG00002|P|2.5\r" +
		"PID|1\r"

	conn := dial(t, addr)
	defer func() { _ = conn.Close() }()

	both := append(frame([]byte(sampleMessage)), frame([]byte(second))...)
	if _, err := conn.Write(both); err != nil {
		t.Fatalf("write: %v", err)
	}

	events := inst.WaitForEvents(2, store.Query{Plugin: hl7.Name}, 5*time.Second)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	// Newest first; the second message sent is the first one listed.
	m0, ok0 := hl7.MessageOf(events[0])
	m1, ok1 := hl7.MessageOf(events[1])
	if !ok0 || !ok1 {
		t.Fatalf("events did not carry hl7 messages")
	}
	if m0.Header.ControlID != "MSG00002" || m1.Header.ControlID != "MSG00001" {
		t.Errorf("control ids = %q, %q (newest first)", m0.Header.ControlID, m1.Header.ControlID)
	}

	fr := newFrameReader(conn, 0)
	ack1 := parseACK(t, readACK(t, fr))
	ack2 := parseACK(t, readACK(t, fr))
	if got := ack1.Value("MSA-2"); got != "MSG00001" {
		t.Errorf("first ack MSA-2 = %q, want MSG00001", got)
	}
	if got := ack2.Value("MSA-2"); got != "MSG00002" {
		t.Errorf("second ack MSA-2 = %q, want MSG00002", got)
	}
}

// TestListenerLeadingJunkBeforeStart proves bytes before the first 0x0B are
// discarded rather than mangling the first segment id.
func TestListenerLeadingJunkBeforeStart(t *testing.T) {
	inst, addr := startListener(t, nil)

	conn := dial(t, addr)
	defer func() { _ = conn.Close() }()

	junk := []byte("\x00\x01garbage-before-the-header\xff")
	payload := append(append([]byte{}, junk...), frame([]byte(sampleMessage))...)
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, msg := waitForMessage(t, inst)
	if msg.Header.ControlID != "MSG00001" {
		t.Errorf("ControlID = %q, leading junk corrupted the first segment", msg.Header.ControlID)
	}

	fr := newFrameReader(conn, 0)
	ack := parseACK(t, readACK(t, fr))
	if ack.Value("MSA-1") != "AA" {
		t.Errorf("MSA-1 = %q", ack.Value("MSA-1"))
	}
}

// TestListenerJunkBetweenTrailerAndNextHeader proves bytes between one
// frame's trailer and the next frame's start byte are discarded, not
// mistaken for part of either message.
func TestListenerJunkBetweenTrailerAndNextHeader(t *testing.T) {
	inst, addr := startListener(t, nil)

	second := "MSH|^~\\&|SEND_APP|SEND_FAC|RECV_APP|RECV_FAC|20240101120100||ORU^R01|MSG00002|P|2.5\r" +
		"PID|1\r"

	conn := dial(t, addr)
	defer func() { _ = conn.Close() }()

	var payload []byte
	payload = append(payload, frame([]byte(sampleMessage))...)
	payload = append(payload, []byte("\r\n   stray keepalive noise   \r\n")...)
	payload = append(payload, frame([]byte(second))...)
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	events := inst.WaitForEvents(2, store.Query{Plugin: hl7.Name}, 5*time.Second)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	m0, _ := hl7.MessageOf(events[0])
	m1, _ := hl7.MessageOf(events[1])
	if m0.Header.ControlID != "MSG00002" || m1.Header.ControlID != "MSG00001" {
		t.Errorf("control ids = %q, %q (newest first)", m0.Header.ControlID, m1.Header.ControlID)
	}
}

// TestListenerMissingTrailerBounded proves a frame that never terminates is
// bounded rather than buffered forever, and the connection is closed rather
// than left hanging.
func TestListenerMissingTrailerBounded(t *testing.T) {
	// The limit must stay comfortably above sampleMessage's own length (128
	// bytes), since assertListenerAlive below reuses it to prove the
	// listener survived - a limit tight enough to also reject that message
	// would conflate "the oversized frame was bounded" with "the config is
	// now too small for anything".
	inst, addr := startListener(t, map[string]any{"max_message_bytes": 256})

	conn := dial(t, addr)
	defer func() { _ = conn.Close() }()

	payload := append([]byte{startByte}, []byte(repeat("X", 2048))...) // no trailer, ever
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, 16)
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("expected the connection to be closed once the frame exceeded the configured limit")
	}

	if events := inst.Events(store.Query{Plugin: hl7.Name}); len(events) != 0 {
		t.Errorf("got %d events, want 0 - an unterminated oversized frame must not be captured", len(events))
	}

	// The listener itself must have survived: a fresh connection works.
	assertListenerAlive(t, inst, addr)
}

// TestListenerConnectionClosesMidFrame proves a client that hangs up after a
// start byte but before a trailer neither hangs the server nor produces a
// half-formed event, and that the listener keeps serving other connections.
func TestListenerConnectionClosesMidFrame(t *testing.T) {
	inst, addr := startListener(t, nil)

	conn := dial(t, addr)
	if _, err := conn.Write(append([]byte{startByte}, []byte("MSH|^~\\&|A")...)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Give the server a moment to notice the close before asserting nothing
	// was recorded from the partial frame.
	time.Sleep(100 * time.Millisecond)
	if events := inst.Events(store.Query{Plugin: hl7.Name}); len(events) != 0 {
		t.Errorf("got %d events, want 0 - a connection closed mid-frame must not be captured", len(events))
	}

	assertListenerAlive(t, inst, addr)
}

// assertListenerAlive opens a fresh connection and proves a complete,
// well-formed message on it still works - the proof that an earlier
// misbehaving connection did not take the whole listener down with it.
func assertListenerAlive(t *testing.T, inst *testutil.Instance, addr string) {
	t.Helper()
	before := len(inst.Events(store.Query{Plugin: hl7.Name}))

	conn := dial(t, addr)
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(frame([]byte(sampleMessage))); err != nil {
		t.Fatalf("write on fresh connection: %v", err)
	}

	events := inst.WaitForEvents(before+1, store.Query{Plugin: hl7.Name}, 5*time.Second)
	if len(events) != before+1 {
		t.Fatalf("listener did not accept a message on a fresh connection after the previous one misbehaved")
	}

	fr := newFrameReader(conn, 0)
	ack := parseACK(t, readACK(t, fr))
	if ack.Value("MSA-1") != "AA" {
		t.Errorf("MSA-1 = %q on the fresh connection's ack", ack.Value("MSA-1"))
	}
}

// TestListenerEmptyFrameProducesNoEventOrAck proves a frame with nothing
// between its control bytes - the one case hl7.Parse actually fails on - is
// dropped rather than crashing the connection or queuing a stray ack ahead
// of the next real message.
func TestListenerEmptyFrameProducesNoEventOrAck(t *testing.T) {
	inst, addr := startListener(t, nil)

	conn := dial(t, addr)
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write([]byte{startByte, endByte1, endByte2}); err != nil {
		t.Fatalf("write empty frame: %v", err)
	}
	if _, err := conn.Write(frame([]byte(sampleMessage))); err != nil {
		t.Fatalf("write real message: %v", err)
	}

	_, msg := waitForMessage(t, inst)
	if msg.Header.ControlID != "MSG00001" {
		t.Errorf("ControlID = %q", msg.Header.ControlID)
	}

	// If the empty frame had produced an ack, this would be it instead of
	// the real message's - and its MSA-2 would not match.
	fr := newFrameReader(conn, 0)
	ack := parseACK(t, readACK(t, fr))
	if got := ack.Value("MSA-2"); got != "MSG00001" {
		t.Errorf("MSA-2 = %q, want MSG00001 with nothing queued ahead of it", got)
	}
}

// TestListenerNoHeaderGetsAR proves a frame with no MSH at all gets AR, with
// the conventional separators (there is nothing to take the real ones from)
// and an empty MSA-2 (there is no control id to echo).
func TestListenerNoHeaderGetsAR(t *testing.T) {
	inst, addr := startListener(t, nil)

	headerless := "PID|1||123456^^^MRN||DOE^JOHN^A||19800101|M\r"

	conn := dial(t, addr)
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(frame([]byte(headerless))); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, msg := waitForMessage(t, inst)
	if msg.HasHeader() {
		t.Fatal("test message unexpectedly has a header")
	}

	fr := newFrameReader(conn, 0)
	ack := parseACK(t, readACK(t, fr))
	if got := ack.Value("MSA-1"); got != "AR" {
		t.Errorf("MSA-1 = %q, want AR for a headerless message", got)
	}
	if got := ack.Value("MSA-2"); got != "" {
		t.Errorf("MSA-2 = %q, want empty - there is no control id to echo", got)
	}
	if ack.Separators.Field != "|" || ack.Separators.Component != "^" {
		t.Errorf("ack separators = %+v, want the conventional set", ack.Separators)
	}
}

// TestListenerBadEncodingCharactersGetsAE proves a message with an MSH that
// is present but does not declare usable encoding characters gets AE, not AA
// or AR: there is a header, but it cannot be trusted.
func TestListenerBadEncodingCharactersGetsAE(t *testing.T) {
	inst, addr := startListener(t, nil)

	broken := "MSH|\r" + "PID|1\r"

	conn := dial(t, addr)
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(frame([]byte(broken))); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, msg := waitForMessage(t, inst)
	if !msg.HasHeader() {
		t.Fatal("test message unexpectedly has no header")
	}
	if !msg.HasIssue(hl7.IssueNoEncodingCharacters) {
		t.Fatalf("test message did not trigger IssueNoEncodingCharacters: %+v", msg.Issues)
	}

	fr := newFrameReader(conn, 0)
	ack := parseACK(t, readACK(t, fr))
	if got := ack.Value("MSA-1"); got != "AE" {
		t.Errorf("MSA-1 = %q, want AE for a header with no usable encoding characters", got)
	}
}

// TestListenStopsOnContextCancel proves the provider honors the lifecycle
// the core supervises it with.
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

func repeat(s string, n int) string {
	b := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		b = append(b, s...)
	}
	return string(b)
}
