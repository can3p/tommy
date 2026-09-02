// Package trap is tommy's SNMP trap receiver: a real UDP listener that
// accepts v1 traps, v2c traps and v2c informs, decodes every varbind and
// records it as an snmp.Trap.
//
// It is built on github.com/gosnmp/gosnmp for the BER encode/decode work
// (SnmpPacket.MarshalMsg / GoSNMP.UnmarshalTrap), but deliberately does not
// use the library's own gosnmp.TrapListener: that type owns its socket
// internally and never exposes the bound net.Addr, which makes it impossible
// to implement plugin.AddressableProvider (an ephemeral port = 0 test would
// have no way to learn what it actually bound - see docs/lessons.md, "a
// contract written against one transport is only proven when a different one
// arrives"). Instead this provider opens its own net.PacketConn, exactly the
// way plugins/files/providers/tftp does, and calls gosnmp's exported
// GoSNMP.UnmarshalTrap / SnmpPacket.MarshalMsg directly for the wire-format
// work. See decode.go and reply.go.
package trap

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/gosnmp/gosnmp"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/snmp"
)

// ProviderName is the provider's name, which is also how it is addressed in
// configuration: [plugins.snmp.providers.trap].
const ProviderName = "trap"

// DefaultPort is the port the listener binds when the configuration says
// nothing. SNMP's real trap-receiving port is 162/udp - registered with IANA
// as "SNMPTRAP" (service-names-port-numbers registry, checked live against
// iana.org September 2026) - which is privileged on every OS tommy runs on;
// binding it would mean running the whole binary as root or granting it
// CAP_NET_BIND_SERVICE, and this is a local testing tool, not a network
// management station. 1162 is the conventional unprivileged stand-in (the
// same pattern ftp uses with 2121 instead of 21, and tftp with 6969 instead
// of 69), so tommy never needs elevated permissions to run it. Point a real
// trap sender at :1162 explicitly, or forward 162 to it.
const DefaultPort = 1162

// DefaultBind is the interface the listener binds when the configuration
// says nothing.
const DefaultBind = "127.0.0.1"

// maxDatagramBytes bounds one read: 65507 is the largest UDP payload IPv4 can
// carry at all, and gosnmp's own trap listener buffers the same class of
// size (its default is 4096, ours is generous instead - a real trap sender
// practically never approaches this, but nothing here should silently
// truncate a legitimate one).
const maxDatagramBytes = 65535

// Config is the [plugins.snmp.providers.trap] section.
//
// Every key is optional. port = 0 means "bind an ephemeral port", which is
// what the test harness uses; an absent port means DefaultPort.
type Config struct {
	Bind string
	Port int
}

// LoadConfig reads the provider section, applying every default.
func LoadConfig(pc plugin.ProviderConfig) Config {
	return Config{
		Bind: pc.String("bind", DefaultBind),
		Port: pc.Int("port", DefaultPort),
	}
}

// ListenAddr is the address Listen binds.
func (c Config) ListenAddr() string { return net.JoinHostPort(c.Bind, strconv.Itoa(c.Port)) }

// Provider is the SNMP trap listener.
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

// New returns the trap provider. The port comes from the configuration at
// Listen time, so one value is safe to construct before anything is running.
func New() *Provider { return &Provider{bound: make(chan struct{})} }

// Name implements plugin.Provider.
func (p *Provider) Name() string { return ProviderName }

// Plugin implements plugin.Provider.
func (p *Provider) Plugin() string { return snmp.Name }

// Description implements plugin.Provider.
func (p *Provider) Description() string {
	return "A real SNMP trap receiver on its own UDP port: v1 traps, v2c traps and v2c informs are all accepted, " +
		"decoded varbind by varbind with their real wire types kept, and recorded whichever version they " +
		"arrived as rather than flattened into one shape. An inform gets a GetResponse echoing its request id " +
		"and varbinds back, the one reply SNMP actually requires here; any community string is accepted and " +
		"recorded, never checked."
}

// Endpoints implements plugin.Provider. The provider speaks SNMP on its own
// UDP port and mounts no HTTP routes.
func (p *Provider) Endpoints() []plugin.Endpoint { return nil }

// RegisterIngress implements plugin.Provider. Nothing is mounted: this
// provider speaks SNMP on a port of its own.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {}

// Snippets implements plugin.Provider.
func (p *Provider) Snippets() []plugin.Snippet {
	// The listener address is rendered from the live SnippetCtx, with the
	// package default as the fallback - not a hardcoded port in disguise:
	// when the configuration names no port, Listen binds this very constant.
	addr := fmt.Sprintf(`{{with .Addr "snmp" "trap"}}{{.}}{{else}}{{.Host}}:%d{{end}}`, DefaultPort)

	return []plugin.Snippet{
		{
			Title: "Send a v2c trap with net-snmp's snmptrap, if it's installed",
			Lang:  "bash",
			Code: `snmptrap -v 2c -c public ` + addr + ` '' 1.3.6.1.6.3.1.1.5.3 \
  1.3.6.1.2.1.1.5.0 s "host01"`,
		},
		{
			Title: "Send a v2c trap and an inform with gosnmp itself (Go)",
			Lang:  "go",
			Code: `package main

import (
	"log"
	"net"
	"strconv"
	"time"

	"github.com/gosnmp/gosnmp"
)

func main() {
	host, portStr, err := net.SplitHostPort("` + addr + `")
	if err != nil {
		log.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		log.Fatal(err)
	}

	g := &gosnmp.GoSNMP{
		Target:    host,
		Port:      uint16(port),
		Community: "public",
		Version:   gosnmp.Version2c,
		Timeout:   2 * time.Second,
	}
	if err := g.Connect(); err != nil {
		log.Fatal(err)
	}
	defer g.Conn.Close()

	trapOID := gosnmp.SnmpPDU{Name: "1.3.6.1.6.3.1.1.4.1.0", Type: gosnmp.ObjectIdentifier, Value: "1.3.6.1.6.3.1.1.5.3"}

	// An unconfirmed trap: no reply.
	if _, err := g.SendTrap(gosnmp.SnmpTrap{Variables: []gosnmp.SnmpPDU{trapOID}}); err != nil {
		log.Fatal(err)
	}
	// An inform: gosnmp blocks for the GetResponse tommy sends back.
	if _, err := g.SendTrap(gosnmp.SnmpTrap{Variables: []gosnmp.SnmpPDU{trapOID}, IsInform: true}); err != nil {
		log.Fatal(err)
	}
}`,
		},
		{
			Title: "Read the captured trap back over HTTP",
			Lang:  "bash",
			Code:  `curl -s '{{.APIURL}}/events?plugin=snmp' | head -c 2000`,
		},
	}
}

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
		return "", fmt.Errorf("trap: listener did not bind within %s", timeout)
	}
}

func (p *Provider) setBound(addr string) {
	p.mu.Lock()
	p.addr = addr
	p.mu.Unlock()
	select {
	case <-p.bound:
	default:
		close(p.bound)
	}
}

// Listen implements plugin.ListenerProvider. It binds the UDP port from
// d.Config, serves SNMP traps and informs until ctx is done, and returns nil
// on a clean shutdown.
func (p *Provider) Listen(ctx context.Context, d plugin.Deps) error {
	d = d.Normalize()
	d = d.WithLogger("plugin", snmp.Name, "provider", ProviderName)
	cfg := LoadConfig(d.Config)

	conn, err := net.ListenPacket("udp", cfg.ListenAddr())
	if err != nil {
		return fmt.Errorf("trap listener on %s: %w", cfg.ListenAddr(), err)
	}
	p.setBound(conn.LocalAddr().String())
	d.Logger.Info("snmp trap listening", "addr", conn.LocalAddr().String())

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	decoder := &gosnmp.GoSNMP{}
	for {
		buf := make([]byte, maxDatagramBytes)
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			d.Logger.Warn("read datagram", "err", err)
			continue
		}
		datagram := buf[:n]
		go handleDatagram(ctx, conn, decoder, datagram, peer, d)
	}
}
