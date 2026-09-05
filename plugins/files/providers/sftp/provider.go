// Package sftp is tommy's SFTP listener: a files provider that owns a real SSH
// port and writes everything uploaded to it into the plugin's shared virtual
// filesystem.
//
// SFTP is an SSH subsystem rather than FTP with TLS bolted on, so this provider
// runs two layers: an x/crypto/ssh transport that performs the handshake,
// records whatever credentials were presented and honors a request for the
// "sftp" subsystem, and a pkg/sftp RequestServer whose four handlers are backed
// by a files.Session. Nothing is ever written to the host filesystem - the one
// file this provider owns on disk is its SSH host key, which must survive a
// restart or every client fails with a changed-host-key error.
package sftp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/files"
)

// ProviderName is the provider's name, which is also how it is addressed in
// configuration: [plugins.files.providers.sftp].
const ProviderName = "sftp"

// DefaultPort is the port the listener binds when the configuration says
// nothing. 2222 is the unprivileged SSH convention, and it stays clear of a
// real sshd on 22.
const DefaultPort = 2222

// Defaults for the rest of the listener.
const (
	DefaultBind = "127.0.0.1"
	// DefaultHostKeyName is the file name under the user config directory.
	DefaultHostKeyName = "sftp_host_ed25519"
	// DefaultServerVersion is the identification string sent before the
	// handshake. It must start with "SSH-2.0-" to be a legal SSH banner.
	DefaultServerVersion = "SSH-2.0-tommy"
	// DefaultHandshakeTimeout bounds the time between accepting a connection
	// and completing authentication, so a client that connects and says
	// nothing cannot hold a slot forever.
	DefaultHandshakeTimeout = 30 * time.Second
	// DefaultIdleTimeout drops a connection that has neither read nor written
	// for this long. Uploads refresh it as they go, so a slow transfer is
	// never cut off.
	DefaultIdleTimeout = 10 * time.Minute
	// DefaultMaxConnections caps concurrent SSH connections.
	DefaultMaxConnections = 64
	// DefaultMaxAuthTries caps authentication attempts per connection.
	DefaultMaxAuthTries = 6
)

// Config is the [plugins.files.providers.sftp] section.
//
// Every key is optional. port = 0 means "bind an ephemeral port", which is what
// the test harness uses; an absent port means DefaultPort.
type Config struct {
	Bind string
	Port int

	// HostKeyPath is where the ed25519 host key is kept. It is generated on
	// first use and read back on every later run: an SSH server that changes
	// its identity between restarts breaks every client that remembered it.
	HostKeyPath string

	// AuthorizedKeysPath, when set, is an OpenSSH authorized_keys file that
	// public-key authentication is checked against. Unset means any key is
	// accepted, and only recorded.
	AuthorizedKeysPath string

	// Username and Password pin the credentials the listener accepts. They are
	// empty by default, which means anyone gets in - with a password, with a
	// key, or with no credentials at all - and what they presented is recorded.
	// Set either one and a password becomes mandatory and is checked, which is
	// how you exercise your application's error path.
	Username string
	Password string

	ServerVersion    string
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration
	MaxConnections   int
	MaxAuthTries     int
}

// LoadConfig reads the provider section, applying every default.
func LoadConfig(pc plugin.ProviderConfig) Config {
	return Config{
		Bind:               pc.String("bind", DefaultBind),
		Port:               pc.Int("port", DefaultPort),
		HostKeyPath:        pc.String("host_key_path", DefaultHostKeyPath()),
		AuthorizedKeysPath: pc.String("authorized_keys", ""),
		Username:           pc.String("username", ""),
		Password:           pc.String("password", ""),
		ServerVersion:      pc.String("server_version", DefaultServerVersion),
		HandshakeTimeout:   time.Duration(pc.Int("handshake_timeout", int(DefaultHandshakeTimeout/time.Second))) * time.Second,
		IdleTimeout:        time.Duration(pc.Int("idle_timeout", int(DefaultIdleTimeout/time.Second))) * time.Second,
		MaxConnections:     pc.Int("max_connections", DefaultMaxConnections),
		MaxAuthTries:       pc.Int("max_auth_tries", DefaultMaxAuthTries),
	}
}

// RequiresAuth reports whether the configuration pins credentials. When it does
// not, anything a client offers is accepted and recorded but never demanded.
func (c Config) RequiresAuth() bool { return c.Username != "" || c.Password != "" }

// ListenAddr is the address Listen binds.
func (c Config) ListenAddr() string { return net.JoinHostPort(c.Bind, fmt.Sprint(c.Port)) }

// Provider is the SFTP listener.
type Provider struct {
	mu      sync.Mutex
	addr    string
	hostKey HostKey
	bound   chan struct{}

	vfs *files.VFS
}

var (
	_ plugin.Provider            = (*Provider)(nil)
	_ plugin.ListenerProvider    = (*Provider)(nil)
	_ plugin.AddressableProvider = (*Provider)(nil)
	_ plugin.PortProvider        = (*Provider)(nil)
	_ files.VFSBinder            = (*Provider)(nil)
)

// New returns the SFTP provider. Everything it needs - the port, the host key
// path, the shared filesystem - arrives later, so one value is safe to
// construct before anything is running.
func New() *Provider { return &Provider{bound: make(chan struct{})} }

// Name implements plugin.Provider.
func (p *Provider) Name() string { return ProviderName }

// Plugin implements plugin.Provider.
func (p *Provider) Plugin() string { return files.PluginName }

// BindVFS implements files.VFSBinder: the plugin hands every provider the one
// tree they all write into, so a file uploaded over SFTP is listed over FTP and
// downloadable from the Files tab.
func (p *Provider) BindVFS(v *files.VFS) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.vfs = v
}

// Description implements plugin.Provider.
func (p *Provider) Description() string {
	return "A real SFTP server - SSH transport plus the SFTP subsystem - that accepts uploads from any client on its own port and keeps them in the shared virtual filesystem instead of on disk. " +
		"Uploads, mkdir, rename and delete all become files events tagged with the credentials the client presented, and the ed25519 host key is generated once and persisted, so a client never sees the identity change."
}

// Endpoints implements plugin.Provider. The provider speaks SSH on its own port
// and mounts no HTTP routes.
func (p *Provider) Endpoints() []plugin.Endpoint { return nil }

// ListenPort implements plugin.PortProvider: where this listener would bind
// under pc, resolved without binding anything. It is the same value Listen
// resolves, so `tommy providers` and the running listener cannot disagree.
func (p *Provider) ListenPort(pc plugin.ProviderConfig) plugin.ListenPort {
	return plugin.ListenPort{Port: LoadConfig(pc).Port, Network: "tcp"}
}

// RegisterIngress implements plugin.Provider. Nothing is mounted: this provider
// speaks SFTP on a port of its own.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {}

// Snippets implements plugin.Provider.
func (p *Provider) Snippets() []plugin.Snippet {
	// The port comes from the SnippetCtx, which carries this provider's port
	// whether or not anything is listening: the core asks ListenPort when
	// nothing has bound. There is nothing left to hardcode here.
	port := `{{.Port "files" "sftp"}}`
	// Host-key checking is turned off on purpose: the key is a throwaway
	// identity for a local fake, and prompting would break a copy-pasted
	// command.
	opts := `-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR`

	return []plugin.Snippet{
		{
			Title: "Upload a file with the sftp client",
			Lang:  "bash",
			Code: `echo 'it works' > ./local.txt
sftp -P ` + port + ` ` + opts + ` -b - any@{{.Host}} <<'EOF'
mkdir /upload
put ./local.txt /upload/local.txt
ls -l /upload
EOF`,
		},
		{
			Title: "Upload with scp (OpenSSH 9 speaks SFTP under the hood)",
			Lang:  "bash",
			Code:  `scp -P ` + port + ` ` + opts + ` ./local.txt any@{{.Host}}:/local.txt`,
		},
		{
			Title: "Read the upload back over HTTP",
			Lang:  "bash",
			Code: `curl -s {{.APIURL}}/files/tree
curl -s {{.APIURL}}/files/content/upload/local.txt`,
		},
	}
}

// Addr blocks until the listener is bound and returns its address, which is how
// a test finds the ephemeral port it asked for. The channel is created in New,
// so Addr and Listen never race.
func (p *Provider) Addr(timeout time.Duration) (string, error) {
	select {
	case <-p.bound:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.addr, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("sftp: listener did not bind within %s", timeout)
	}
}

// HostKey reports the key the running listener presents, once it has bound. It
// is empty before that; Addr is the way to wait.
func (p *Provider) HostKey() HostKey {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hostKey
}

func (p *Provider) setBound(addr string, key HostKey) {
	p.mu.Lock()
	p.addr = addr
	p.hostKey = key
	p.mu.Unlock()
	select {
	case <-p.bound:
	default:
		close(p.bound)
	}
}

// tree returns the shared filesystem, creating a private one when the provider
// is run outside the plugin - which only happens in a test that constructs it
// alone.
func (p *Provider) tree() *files.VFS {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.vfs == nil {
		p.vfs = files.NewVFS()
	}
	return p.vfs
}

// Listen implements plugin.ListenerProvider. It loads or generates the host
// key, binds the port from d.Config, serves SSH until ctx is done, and returns
// nil on a clean shutdown.
func (p *Provider) Listen(ctx context.Context, d plugin.Deps) error {
	// The core already tags the logger with the plugin and provider, so this
	// only fills in the optional Deps fields.
	d = d.Normalize()
	cfg := LoadConfig(d.Config)

	hostKey, err := LoadOrCreateHostKey(cfg.HostKeyPath)
	if err != nil {
		return err
	}
	if warn := hostKeyPermsWarning(hostKey.Path); warn != "" {
		d.Logger.Warn("sftp: " + warn)
	}

	authorized, err := loadAuthorizedKeys(cfg.AuthorizedKeysPath)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.ListenAddr())
	if err != nil {
		return fmt.Errorf("sftp listener on %s: %w", cfg.ListenAddr(), err)
	}
	defer func() { _ = ln.Close() }()

	p.setBound(ln.Addr().String(), hostKey)
	// The fingerprint is logged every time, generated or loaded: it is the one
	// value a puzzled client can be compared against.
	d.Logger.Info("sftp listening",
		"addr", ln.Addr().String(),
		"host_key", hostKey.Path,
		"host_key_generated", hostKey.Created,
		"fingerprint", hostKey.Fingerprint())

	server := &sshServer{
		cfg:        cfg,
		deps:       d,
		vfs:        p.tree(),
		hostKey:    hostKey,
		authorized: authorized,
	}
	return server.serve(ctx, ln)
}

// closedErr reports whether err is the "use of closed network connection" that
// a deliberate shutdown produces.
func closedErr(err error) bool {
	return errors.Is(err, net.ErrClosed)
}
