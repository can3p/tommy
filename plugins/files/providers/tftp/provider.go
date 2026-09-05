// Package tftp is tommy's TFTP listener: a files provider that owns a real
// UDP port and reads and writes the plugin's shared virtual filesystem for
// every RRQ (download) and WRQ (upload) it receives.
//
// It is built on github.com/pin/tftp/v3, whose server contract is two plain
// functions - a read handler and a write handler, each handed the filename
// the client asked for and an io.ReaderFrom or io.WriterTo to move bytes
// through. handlers.go is the thin translation layer: it opens or creates the
// name on a *files.Session and never interprets the path itself, exactly the
// way fsAdapter does for FTP - VFS.Resolve is the one place that is allowed
// to.
//
// TFTP (RFC 1350) has no login step at all, so there is nothing to accept or
// pin the way FTP and SFTP do; the peer address is recorded on every event
// instead, which is the only thing a TFTP client ever identifies itself by.
package tftp

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	tftplib "github.com/pin/tftp/v3"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/files"
)

// ProviderName is the provider's name, which is also how it is addressed in
// configuration: [plugins.files.providers.tftp].
const ProviderName = "tftp"

// DefaultPort is the port the listener binds when the configuration says
// nothing. TFTP's real port is 69, which is privileged on every OS tommy
// runs on; binding it would mean either running the whole binary as root or
// granting it CAP_NET_BIND_SERVICE, and this is a local testing tool, not a
// PXE server. 6969 is the conventional unprivileged stand-in (the same
// pattern ftp uses with 2121 instead of 21, and sftp with 2222 instead of
// 22), so tommy never needs elevated permissions to run it. Point a real
// client at :6969 explicitly, or NAT/forward 69 to it if a boot ROM that
// cannot be told a different port needs to reach it.
const DefaultPort = 6969

// Defaults for the rest of the listener.
const (
	DefaultBind = "127.0.0.1"
	// DefaultTimeout is how long the server waits for an ACK or DATA packet
	// before retransmitting - pin/tftp/v3's own default, made explicit here
	// so LoadConfig has one place that states it.
	DefaultTimeout = 5 * time.Second
	// DefaultRetries bounds retransmission attempts before a transfer is
	// abandoned - also pin/tftp/v3's own default.
	DefaultRetries = 5
)

// Config is the [plugins.files.providers.tftp] section.
//
// Every key is optional. port = 0 means "bind an ephemeral port", which is
// what the test harness uses; an absent port means DefaultPort.
type Config struct {
	Bind string
	Port int

	// Timeout bounds a single retransmission round-trip. TFTP has no option
	// negotiation for this on the wire that pin/tftp/v3 exposes per-request
	// (see the package README for the "timeout" RFC2349 option, which the
	// library parses but does not currently echo back in an OACK - a real
	// client that asked for a specific timeout falls back to its own
	// default, exactly as a client is required to do when an option is not
	// granted); this is the server-wide value used for every transfer.
	Timeout time.Duration
	// Retries bounds retransmission attempts before a transfer is abandoned.
	Retries int
}

// LoadConfig reads the provider section, applying every default.
func LoadConfig(pc plugin.ProviderConfig) Config {
	return Config{
		Bind:    pc.String("bind", DefaultBind),
		Port:    pc.Int("port", DefaultPort),
		Timeout: time.Duration(pc.Int("timeout_seconds", int(DefaultTimeout/time.Second))) * time.Second,
		Retries: pc.Int("retries", DefaultRetries),
	}
}

// ListenAddr is the address Listen binds.
func (c Config) ListenAddr() string { return net.JoinHostPort(c.Bind, strconv.Itoa(c.Port)) }

// Provider is the TFTP listener.
type Provider struct {
	mu    sync.Mutex
	addr  string
	bound chan struct{}

	vfs *files.VFS
}

var (
	_ plugin.Provider            = (*Provider)(nil)
	_ plugin.ListenerProvider    = (*Provider)(nil)
	_ plugin.AddressableProvider = (*Provider)(nil)
	_ plugin.PortProvider        = (*Provider)(nil)
	_ files.VFSBinder            = (*Provider)(nil)
)

// New returns the TFTP provider. Everything it needs - the port, the shared
// filesystem - arrives later, so one value is safe to construct before
// anything is running.
func New() *Provider { return &Provider{bound: make(chan struct{})} }

// Name implements plugin.Provider.
func (p *Provider) Name() string { return ProviderName }

// Plugin implements plugin.Provider.
func (p *Provider) Plugin() string { return files.PluginName }

// BindVFS implements files.VFSBinder: the plugin hands every provider the one
// tree they all write into, so a file uploaded over TFTP is listed over FTP
// and SFTP and downloadable from the Files tab, and vice versa.
func (p *Provider) BindVFS(v *files.VFS) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.vfs = v
}

// tree returns the shared filesystem, creating a private one when the
// provider is run outside the plugin - which only happens in a test that
// constructs it alone.
func (p *Provider) tree() *files.VFS {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.vfs == nil {
		p.vfs = files.NewVFS()
	}
	return p.vfs
}

// Description implements plugin.Provider.
func (p *Provider) Description() string {
	return "A real TFTP server (RFC 1350) on its own UDP port - RRQ downloads and WRQ uploads both work against the shared virtual filesystem, so a file put by a PXE client or a curl one-liner is the same tree ftp and sftp write into. " +
		"It listens on 6969, not TFTP's traditional 69, because 69 is a privileged port and this is a local testing tool rather than something that should run as root."
}

// Endpoints implements plugin.Provider. The provider speaks TFTP on its own
// UDP port and mounts no HTTP routes.
func (p *Provider) Endpoints() []plugin.Endpoint { return nil }

// ListenPort implements plugin.PortProvider: where this listener would bind
// under pc, resolved without binding anything. It is the same value Listen
// resolves, so `tommy providers` and the running listener cannot disagree.
func (p *Provider) ListenPort(pc plugin.ProviderConfig) plugin.ListenPort {
	return plugin.ListenPort{Port: LoadConfig(pc).Port, Network: "udp"}
}

// RegisterIngress implements plugin.Provider. Nothing is mounted: this
// provider speaks TFTP on a port of its own.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {}

// Snippets implements plugin.Provider.
func (p *Provider) Snippets() []plugin.Snippet {
	// The address comes from the SnippetCtx, which carries this provider's
	// port whether or not anything is listening: the core asks ListenPort
	// when nothing has bound. There is nothing left to hardcode here.
	addr := `{{.Addr "files" "tftp"}}`

	return []plugin.Snippet{
		{
			Title: "Round-trip a file with curl (curl speaks tftp natively)",
			Lang:  "bash",
			Code: `echo 'it works' > ./local.txt
curl -sS -T ./local.txt tftp://` + addr + `/upload/local.txt
curl -sS tftp://` + addr + `/upload/local.txt -o ./downloaded.txt
diff ./local.txt ./downloaded.txt && echo "round-trip ok"`,
		},
		{
			Title: "The same upload with the tftp client",
			Lang:  "bash",
			Code: `tftp ` + addr + ` <<'EOF'
mode binary
put ./local.txt upload/local.txt
get upload/local.txt ./downloaded.txt
EOF`,
		},
		{
			Title: "Read the upload back over HTTP",
			Lang:  "bash",
			Code: `curl -s {{.APIURL}}/files/tree
curl -s {{.APIURL}}/files/content/upload/local.txt`,
		},
	}
}

// Addr blocks until the listener is bound and returns its address, which is
// how a test finds the ephemeral port it asked for. The channel is created in
// New, so Addr and Listen never race.
func (p *Provider) Addr(timeout time.Duration) (string, error) {
	select {
	case <-p.bound:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.addr, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("tftp: listener did not bind within %s", timeout)
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
// d.Config, serves TFTP until ctx is done, and returns nil on a clean
// shutdown.
//
// Unlike every other listener provider in this codebase, this one is UDP:
// net.ListenPacket rather than net.Listen, and pin/tftp/v3's Server.Serve
// takes a net.PacketConn rather than a net.Listener. Nothing about
// plugin.ListenerProvider assumed TCP anywhere - Listen just needs to bind,
// report its address through AddressableProvider, and block until ctx is
// done - so no core contract changed to accommodate this.
func (p *Provider) Listen(ctx context.Context, d plugin.Deps) error {
	// The core already tags the logger with the plugin and provider, so this
	// only fills in the optional Deps fields.
	d = d.Normalize()
	cfg := LoadConfig(d.Config)

	conn, err := net.ListenPacket("udp", cfg.ListenAddr())
	if err != nil {
		return fmt.Errorf("tftp listener on %s: %w", cfg.ListenAddr(), err)
	}
	p.setBound(conn.LocalAddr().String())
	d.Logger.Info("tftp listening", "addr", conn.LocalAddr().String())

	srv := tftplib.NewServer(p.readHandler(d), p.writeHandler(d))
	srv.SetTimeout(cfg.Timeout)
	srv.SetRetries(cfg.Retries)

	// Serve blocks until Shutdown is called (or a startup-time error, which
	// cannot happen here since the socket is already open); Shutdown itself
	// closes conn and waits for every in-flight transfer to finish, so a
	// canceled context never truncates an upload or download in progress.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			srv.Shutdown()
		case <-stop:
		}
	}()

	if err := srv.Serve(conn); err != nil && ctx.Err() == nil {
		return fmt.Errorf("tftp serve: %w", err)
	}
	return nil
}
