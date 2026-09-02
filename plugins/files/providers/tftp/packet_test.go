package tftp_test

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/can3p/tommy/plugins/files"
	"github.com/can3p/tommy/plugins/files/providers/tftp"
)

// ---------------------------------------------------------------------------
// A hand-driven TFTP client, built straight from RFC 1350's five opcodes,
// for asserting the exact bytes this provider puts on the wire rather than
// trusting a library (ours or pin/tftp/v3's own client) to interpret them
// the same way a hostile or merely different implementation would.
// ---------------------------------------------------------------------------

const (
	opRRQ   = uint16(1)
	opWRQ   = uint16(2)
	opDATA  = uint16(3)
	opACK   = uint16(4)
	opERROR = uint16(5)
)

// packRQ builds an RRQ or WRQ packet: 2-byte opcode, filename, a NUL, the
// mode, a NUL. No options - the option-negotiation path is exercised by the
// curl tests in listener_test.go, which is what blksize is realistically
// tested with; this file is about the base RFC 1350 wire format.
func packRQ(op uint16, filename, mode string) []byte {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, op)
	buf.WriteString(filename)
	buf.WriteByte(0)
	buf.WriteString(mode)
	buf.WriteByte(0)
	return buf.Bytes()
}

// packDATA builds a DATA packet: 2-byte opcode, 2-byte block number, payload.
func packDATA(block uint16, data []byte) []byte {
	buf := make([]byte, 4+len(data))
	binary.BigEndian.PutUint16(buf[0:2], opDATA)
	binary.BigEndian.PutUint16(buf[2:4], block)
	copy(buf[4:], data)
	return buf
}

// packACK builds an ACK packet: 2-byte opcode, 2-byte block number.
func packACK(block uint16) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint16(buf[0:2], opACK)
	binary.BigEndian.PutUint16(buf[2:4], block)
	return buf
}

// rawClient is an unconnected UDP socket, because the first reply to an RRQ
// or WRQ comes from a brand-new ephemeral port (the transfer's TID) - a
// connected socket dialed at the well-known server address would silently
// drop it.
type rawClient struct {
	t    *testing.T
	conn *net.UDPConn
}

func dialRaw(t *testing.T) *rawClient {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("open raw udp socket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &rawClient{t: t, conn: conn}
}

func (c *rawClient) sendTo(addr *net.UDPAddr, pkt []byte) {
	c.t.Helper()
	if err := c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		c.t.Fatalf("set write deadline: %v", err)
	}
	if _, err := c.conn.WriteToUDP(pkt, addr); err != nil {
		c.t.Fatalf("write to %s: %v", addr, err)
	}
}

// recv reads one datagram and returns its bytes and sender address.
func (c *rawClient) recv() ([]byte, *net.UDPAddr) {
	c.t.Helper()
	if err := c.conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		c.t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 65536)
	n, from, err := c.conn.ReadFromUDP(buf)
	if err != nil {
		c.t.Fatalf("read datagram: %v", err)
	}
	return buf[:n], from
}

func resolveUDP(t *testing.T, addr string) *net.UDPAddr {
	t.Helper()
	a, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("resolve %s: %v", addr, err)
	}
	return a
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestPacketWRQThenRRQExactBytes hand-drives a full upload and a full
// download over one connection each, asserting the exact bytes of every
// packet exchanged: WRQ -> ACK(0), DATA(1) -> ACK(1) for the upload; RRQ ->
// DATA(1) -> ACK(1) for the download that reads the same bytes back. The
// payload is kept under one block (512 bytes) so both transfers finish in a
// single DATA packet each and the byte assertions stay exact and simple.
func TestPacketWRQThenRRQExactBytes(t *testing.T) {
	inst, addr := startListener(t, nil)
	serverAddr := resolveUDP(t, addr)
	const name = "/handmade/upload.bin"
	payload := []byte("hand-rolled TFTP payload, well under one block")

	// --- WRQ: upload ---
	up := dialRaw(t)
	up.sendTo(serverAddr, packRQ(opWRQ, name, "octet"))

	ack0, dataAddr := up.recv()
	if len(ack0) != 4 {
		t.Fatalf("ACK(0) reply is %d bytes, want 4", len(ack0))
	}
	if got := binary.BigEndian.Uint16(ack0[0:2]); got != opACK {
		t.Fatalf("opcode = %d, want ACK(%d)", got, opACK)
	}
	if got := binary.BigEndian.Uint16(ack0[2:4]); got != 0 {
		t.Fatalf("ACK block = %d, want 0", got)
	}
	// The reply came from a fresh TID, not the well-known server port - the
	// defining behavior of TFTP's per-transfer socket.
	if dataAddr.Port == serverAddr.Port {
		t.Fatalf("WRQ ACK(0) came from the server's own port %d, want a new per-transfer TID", serverAddr.Port)
	}

	up.sendTo(dataAddr, packDATA(1, payload))
	ack1, from := up.recv()
	if !from.IP.Equal(dataAddr.IP) || from.Port != dataAddr.Port {
		t.Fatalf("ACK(1) came from %s, want the TID %s", from, dataAddr)
	}
	wantAck1 := packACK(1)
	if !bytes.Equal(ack1, wantAck1) {
		t.Fatalf("ACK(1) = % x, want % x", ack1, wantAck1)
	}

	uploadEv := waitForEvent(t, inst, files.EventUpload)
	if op, ok := files.OpOf(uploadEv); !ok || op.Path != name || op.Size != int64(len(payload)) {
		t.Errorf("upload event = %+v, want path %q size %d", uploadEv.Payload, name, len(payload))
	}
	if uploadEv.Provider != tftp.ProviderName {
		t.Errorf("event provider = %q, want %q", uploadEv.Provider, tftp.ProviderName)
	}

	// --- RRQ: download the same file back ---
	down := dialRaw(t)
	down.sendTo(serverAddr, packRQ(opRRQ, name, "octet"))

	data1, dlAddr := down.recv()
	wantData1 := packDATA(1, payload)
	if !bytes.Equal(data1, wantData1) {
		t.Fatalf("DATA(1) = % x, want % x", data1, wantData1)
	}
	if dlAddr.Port == serverAddr.Port {
		t.Fatalf("RRQ DATA(1) came from the server's own port %d, want a new per-transfer TID", serverAddr.Port)
	}

	down.sendTo(dlAddr, packACK(1))
}

// TestPacketRRQMissingFileIsError proves a request for a name that was never
// written gets a real ERROR packet - opcode 5, a two-byte error code, a NUL
// terminated message - rather than a timeout or a zero-length DATA packet.
func TestPacketRRQMissingFileIsError(t *testing.T) {
	_, addr := startListener(t, nil)
	serverAddr := resolveUDP(t, addr)

	c := dialRaw(t)
	c.sendTo(serverAddr, packRQ(opRRQ, "/this/was/never/uploaded.bin", "octet"))

	reply, _ := c.recv()
	if len(reply) < 5 {
		t.Fatalf("ERROR reply is %d bytes, want at least 5 (opcode + code + a NUL)", len(reply))
	}
	if got := binary.BigEndian.Uint16(reply[0:2]); got != opERROR {
		t.Fatalf("opcode = %d, want ERROR(%d); full reply % x", got, opERROR, reply)
	}
	// pin/tftp/v3 packs every abort with error code 1 regardless of cause
	// (see backoff/receiver/sender abort()), which is still RFC 1350
	// compliant - the message text is what actually names the problem, and
	// it is asserted below.
	code := binary.BigEndian.Uint16(reply[2:4])
	if code != 1 {
		t.Errorf("error code = %d, want 1 (pin/tftp/v3's fixed abort code)", code)
	}
	msg := reply[4:]
	if len(msg) == 0 || msg[len(msg)-1] != 0 {
		t.Fatalf("error message %q is not NUL-terminated", msg)
	}
	text := string(msg[:len(msg)-1])
	if text == "" {
		t.Error("error message is empty, want a reason naming what went wrong")
	}
}
