// Package mllp is tommy's HL7 listener: a real TCP server speaking MLLP
// (Minimal Lower Layer Protocol), the framing almost every HL7 v2
// integration engine uses on the wire.
//
// Framing is three control bytes around each message: 0x0B (start block)
// before it, 0x1C 0x0D (end block, carriage return) after it. That part is
// trivial. What this package spends its effort on is correctness at the
// edges of a real TCP stream: a message split across several reads, several
// messages pipelined back to back, a frame that never terminates, junk
// before the first start byte or between one trailer and the next header,
// and a connection that closes mid-frame. See framing.go.
//
// Every captured message gets a mechanical acknowledgement - AA, AE or AR -
// built from the request itself, never from a policy decision about the
// message's content. See ack.go.
package mllp

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/hl7"
)

// ProviderName is the provider's name, which is also how it is addressed in
// configuration: [plugins.hl7.providers.mllp].
const ProviderName = "mllp"

// DefaultPort is the port the listener binds when the configuration says
// nothing. 2575/TCP is IANA's registered port for the "hl7" service
// (verified against the IANA Service Name and Transport Protocol Port
// Number Registry, service-names-port-numbers.xhtml, September 2026: TCP
// and UDP 2575 are both listed, service name "hl7", assignee Tim Jacobs) and
// is what every integration engine - Mirth Connect, Rhapsody, Azure Health
// Data Services, Google Cloud Healthcare API - defaults to. It is also
// already unprivileged, unlike SMTP's 25 or TFTP's 69, so unlike those two
// providers this one needs no unprivileged stand-in port.
const DefaultPort = 2575

// Defaults for the rest of the listener.
const (
	DefaultBind = "127.0.0.1"
	// DefaultMaxMessageBytes bounds a single MLLP frame's payload. Real HL7
	// messages are usually a few KB of text, but an OBX segment can carry a
	// base64-encoded PDF or image (ED/RP data types), so the cap is
	// generous; its real job is making sure a frame that never sends a
	// trailer is bounded rather than buffered forever, per the framing
	// requirements above.
	DefaultMaxMessageBytes = 10 << 20
	// DefaultReadTimeout bounds how long a connection may sit idle between
	// frames (and mid-frame) before it is closed.
	DefaultReadTimeout = 60 * time.Second
	// DefaultWriteTimeout bounds writing the acknowledgement back.
	DefaultWriteTimeout = 10 * time.Second
)

// Config is the [plugins.hl7.providers.mllp] section.
//
// Every key is optional. port = 0 means "bind an ephemeral port", which is
// what the test harness uses; an absent port means DefaultPort.
type Config struct {
	Bind            string
	Port            int
	MaxMessageBytes int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
}

// LoadConfig reads the provider section, applying every default.
func LoadConfig(pc plugin.ProviderConfig) Config {
	return Config{
		Bind:            pc.String("bind", DefaultBind),
		Port:            pc.Int("port", DefaultPort),
		MaxMessageBytes: pc.Int("max_message_bytes", DefaultMaxMessageBytes),
		ReadTimeout:     time.Duration(pc.Int("read_timeout", int(DefaultReadTimeout/time.Second))) * time.Second,
		WriteTimeout:    time.Duration(pc.Int("write_timeout", int(DefaultWriteTimeout/time.Second))) * time.Second,
	}
}

// ListenAddr is the address Listen binds.
func (c Config) ListenAddr() string { return net.JoinHostPort(c.Bind, strconv.Itoa(c.Port)) }

// Provider is the MLLP listener.
type Provider struct {
	mu    sync.Mutex
	addr  string
	bound chan struct{}
}

var (
	_ plugin.Provider            = (*Provider)(nil)
	_ plugin.ListenerProvider    = (*Provider)(nil)
	_ plugin.AddressableProvider = (*Provider)(nil)
)

// New returns the MLLP provider. The port comes from the configuration at
// Listen time, so one value is safe to construct before anything is
// running.
func New() *Provider { return &Provider{bound: make(chan struct{})} }

// Name implements plugin.Provider.
func (p *Provider) Name() string { return ProviderName }

// Plugin implements plugin.Provider.
func (p *Provider) Plugin() string { return hl7.Name }

// Description implements plugin.Provider.
func (p *Provider) Description() string {
	return "A real MLLP server (the framing almost every HL7 v2 integration engine speaks) on its own TCP port. " +
		"It accumulates and reassembles frames across partial reads and pipelined connections, parses each message with " +
		"whatever separators it declared for itself, and answers with a mechanical AA/AE/AR acknowledgement built from " +
		"the request - never from a decision about what the message means."
}

// Endpoints implements plugin.Provider. The provider speaks MLLP on its own
// port and mounts no HTTP routes.
func (p *Provider) Endpoints() []plugin.Endpoint { return nil }

// Snippets implements plugin.Provider.
func (p *Provider) Snippets() []plugin.Snippet {
	// The listener address is rendered from the live SnippetCtx, with the
	// package default as the fallback. That fallback is not a hardcoded
	// port in disguise: the core can only publish an address it was
	// configured with, so when the configuration says nothing the snippet
	// and Listen fall back to the very same constant.
	addr := fmt.Sprintf(`{{with .Addr "hl7" "mllp"}}{{.}}{{else}}{{.Host}}:%d{{end}}`, DefaultPort)

	return []plugin.Snippet{
		{
			Title: "Send a message and read the ACK with plain Python (stdlib only)",
			Lang:  "python",
			Code: `import socket

MSH = "MSH|^~\\&|SendingApp|SendingFac|ReceivingApp|ReceivingFac|20240101120000||ADT^A01|MSG00001|P|2.5\r"
PID = "PID|1||123456^^^MRN||DOE^JOHN^A||19800101|M\r"
message = MSH + PID

with socket.create_connection(("` + addr + `".split(":")[0], int("` + addr + `".split(":")[1]))) as s:
    s.sendall(b"\x0b" + message.encode() + b"\x1c\r")
    ack = s.recv(65536)
    print(ack.decode(errors="replace"))`,
		},
		{
			Title: "The same thing with netcat, framing bytes spelled out",
			Lang:  "bash",
			Code: `printf '\x0bMSH|^~\\&|SendingApp|SendingFac|ReceivingApp|ReceivingFac|20240101120000||ADT^A01|MSG00001|P|2.5\rPID|1||123456^^^MRN||DOE^JOHN^A||19800101|M\r\x1c\r' \
  | nc ` + addr + ` -w 1`,
		},
		{
			Title: "Read the captured message back over HTTP",
			Lang:  "bash",
			Code:  `curl -s {{.APIURL}}/hl7/messages | head -c 2000`,
		},
	}
}

// RegisterIngress implements plugin.Provider. Nothing is mounted: this
// provider speaks MLLP on a port of its own.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {}

// Addr blocks until the listener is bound and returns its address, which is
// how a test finds the ephemeral port it asked for. The channel is created
// in New, so Addr and Listen never race.
func (p *Provider) Addr(timeout time.Duration) (string, error) {
	select {
	case <-p.bound:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.addr, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("mllp: listener did not bind within %s", timeout)
	}
}

func (p *Provider) setAddr(addr string) {
	p.mu.Lock()
	p.addr = addr
	p.mu.Unlock()
	select {
	case <-p.bound:
	default:
		close(p.bound)
	}
}

// Listen implements plugin.ListenerProvider. It reads its port from
// d.Config, serves MLLP until ctx is done, and returns nil on a clean
// shutdown.
func (p *Provider) Listen(ctx context.Context, d plugin.Deps) error {
	d = d.Normalize()
	d = d.WithLogger("plugin", hl7.Name, "provider", ProviderName)
	cfg := LoadConfig(d.Config)

	ln, err := net.Listen("tcp", cfg.ListenAddr())
	if err != nil {
		return fmt.Errorf("mllp listener on %s: %w", cfg.ListenAddr(), err)
	}
	p.setAddr(ln.Addr().String())
	d.Logger.Info("mllp listening", "addr", ln.Addr().String())

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("mllp accept: %w", err)
		}
		go handleConn(ctx, conn, cfg, d)
	}
}
