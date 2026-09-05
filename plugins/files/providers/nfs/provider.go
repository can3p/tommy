// Package nfs is tommy's NFSv3 listener: a files provider that owns a real
// TCP port and serves the plugin's shared virtual filesystem over the
// protocol every operating system can mount natively, so a file written by
// `cp` into a mounted directory lands in the same tree - and the same event
// log - that ftp, sftp and tftp write into.
//
// It is built on github.com/willscott/go-nfs, whose backend contract is a
// github.com/go-git/go-billy/v5 filesystem. fs.go is the translation layer,
// the same job plugins/files/providers/ftp/fs.go does against afero: every
// method forwards onto a *files.Session and none of them interprets a path,
// because VFS.Resolve is the plugin's one security gate.
//
// # Mount, and why there is no portmapper
//
// An NFS client normally finds the NFS and mountd services by asking rpcbind
// (the portmapper) on port 111. go-nfs implements no portmapper at all, and
// tommy would not want one: 111 is privileged on every OS tommy runs on, and
// registering with a machine's real rpcbind would mean advertising this fake
// to everything on the host. Instead go-nfs answers both RPC programs - MOUNT
// (100005) and NFS (100003) - on the one TCP listener this provider binds,
// dispatching on the program number in each request header.
//
// The consequence is the single thing a user must know: the client has to be
// told both ports explicitly, because with neither given it will go looking
// for rpcbind and fail. Linux nfs(5) and macOS mount_nfs(8) spell the options
// the same way - port= and mountport= - and both are this provider's one
// port. Every Snippet carries the whole command; see also the README.
//
// # What a client sees
//
// NFS is stateless: there is no open or close on the wire, so a file arrives
// as a CREATE followed by one WRITE per chunk the client chose to send, and
// each of those is committed and recorded on its own. A test fixture is one
// CREATE plus one WRITE; a large file is one event per chunk. That is the
// honest translation - unlike FTP's STOR or an SFTP upload there is no
// end-of-transfer signal to hang a single event on.
package nfs

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	gonfs "github.com/willscott/go-nfs"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/files"
)

// ProviderName is the provider's name, which is also how it is addressed in
// configuration: [plugins.files.providers.nfs].
const ProviderName = "nfs"

// DefaultPort is the port the listener binds when the configuration says
// nothing. Unlike ftp, sftp and tftp - which all had to move off 21, 22 and
// 69 because those are privileged - NFS's real port, 2049, is already above
// 1024, so there is no reason to serve this anywhere else and every reason
// not to: a client told `port=2049` is a client told the default. The one
// machine where it collides is one already running a real nfsd, and
// --nfs-port moves it.
//
// Note that this is only the NFS program's traditional port. mountd has no
// fixed port anywhere - it is normally discovered through rpcbind - and
// go-nfs serves it on this same listener, which is why every snippet passes
// mountport= as well.
const DefaultPort = 2049

// Defaults for the rest of the listener.
const (
	DefaultBind = "127.0.0.1"
	// DefaultHandleCache is how many file handles the server keeps live. See
	// newHandler for why the number matters in both directions: too small and
	// a client that is still walking a directory gets NFS3ERR_STALE, since
	// go-nfs also caps one READDIR response at half this value; too large and
	// a long-running instance holds paths no one will ask for again. 16384 is
	// generous next to the VFS's own 4096-entries-per-directory limit.
	DefaultHandleCache = 16384
)

// Config is the [plugins.files.providers.nfs] section.
//
// Every key is optional. port = 0 means "bind an ephemeral port", which is
// what the test harness uses; an absent port means DefaultPort.
type Config struct {
	Bind string
	Port int

	// HandleCache bounds the live file-handle table.
	HandleCache int
}

// LoadConfig reads the provider section, applying every default.
func LoadConfig(pc plugin.ProviderConfig) Config {
	cfg := Config{
		Bind:        pc.String("bind", DefaultBind),
		Port:        pc.Int("port", DefaultPort),
		HandleCache: pc.Int("handle_cache", DefaultHandleCache),
	}
	// go-nfs warns and misbehaves below two, and READDIR hands back
	// HandleLimit()/2 entries at a time, so a tiny cache is never what
	// someone meant.
	if cfg.HandleCache < 64 {
		cfg.HandleCache = 64
	}
	return cfg
}

// ListenAddr is the address Listen binds.
func (c Config) ListenAddr() string { return net.JoinHostPort(c.Bind, strconv.Itoa(c.Port)) }

// Provider is the NFS listener.
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

// New returns the NFS provider. Everything it needs - the port, the shared
// filesystem - arrives later, so one value is safe to construct before
// anything is running.
func New() *Provider { return &Provider{bound: make(chan struct{})} }

// Name implements plugin.Provider.
func (p *Provider) Name() string { return ProviderName }

// Plugin implements plugin.Provider.
func (p *Provider) Plugin() string { return files.PluginName }

// BindVFS implements files.VFSBinder: the plugin hands every provider the one
// tree they all write into, so a file written into a mounted NFS share is
// listed over FTP and SFTP and downloadable from the Files tab, and vice
// versa.
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
	return "A real NFSv3 server (RFC 1813) on its own TCP port, backed by the shared virtual filesystem rather than a disk: an operating system can mount it and a file copied into the mount point is the same tree ftp, sftp and tftp write into, listed and downloadable exactly the same way. " +
		"It answers the MOUNT and NFS RPC programs on one port and runs no portmapper, so a client is pointed at it with port= and mountport= rather than through rpcbind on privileged port 111."
}

// Endpoints implements plugin.Provider. The provider speaks ONC RPC on its
// own TCP port and mounts no HTTP routes.
func (p *Provider) Endpoints() []plugin.Endpoint { return nil }

// ListenPort implements plugin.PortProvider: where this listener would bind
// under pc, resolved without binding anything. It is the same value Listen
// resolves, so `tommy providers` and the running listener cannot disagree.
func (p *Provider) ListenPort(pc plugin.ProviderConfig) plugin.ListenPort {
	return plugin.ListenPort{Port: LoadConfig(pc).Port, Network: "tcp"}
}

// RegisterIngress implements plugin.Provider. Nothing is mounted: this
// provider speaks NFS on a port of its own.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {}

// Snippets implements plugin.Provider.
//
// Mounting is the whole point and the least obvious part, so it leads: a user
// who cannot mount the share has nothing. The port comes from the SnippetCtx,
// which carries this provider's port whether or not anything is listening: the
// core asks ListenPort when nothing has bound.
func (p *Provider) Snippets() []plugin.Snippet {
	port := `{{.Port "files" "nfs"}}`
	host := `{{.Host}}`
	url := `'nfs://` + host + `/tommy?version=3&nfsport=` + port + `&mountport=` + port + `'`

	return []plugin.Snippet{
		{
			Title: "Mount the share and write to it (Linux)",
			Lang:  "bash",
			Code: `# port= and mountport= are both required: tommy runs no portmapper on
# 111, so there is nothing for the client to ask where the services are.
sudo mkdir -p /mnt/tommy
sudo mount -t nfs -o nfsvers=3,tcp,port=` + port + `,mountport=` + port + `,nolock,noacl ` + host + `:/ /mnt/tommy

echo 'it works' | sudo tee /mnt/tommy/hello.txt
ls -l /mnt/tommy
sudo umount /mnt/tommy`,
		},
		{
			Title: "Mount the share and write to it (macOS)",
			Lang:  "bash",
			Code: `sudo mkdir -p /Volumes/tommy
sudo mount -t nfs -o vers=3,tcp,port=` + port + `,mountport=` + port + `,nolocks ` + host + `:/ /Volumes/tommy

echo 'it works' | sudo tee /Volumes/tommy/hello.txt
ls -l /Volumes/tommy
sudo umount /Volumes/tommy`,
		},
		{
			Title: "Read and write without root, with libnfs (nfs-ls, nfs-cp)",
			Lang:  "bash",
			Code: `# Mounting needs root on every OS; libnfs's tools are a userspace client
# and need none. The export name in the URL is ignored - there is one tree
# and every mount request gets it.
echo 'it works' > ./local.txt
nfs-cp ./local.txt ` + url + `
nfs-ls -l ` + url,
		},
		{
			Title: "Read what was written back over HTTP",
			Lang:  "bash",
			Code: `curl -s {{.APIURL}}/files/tree
curl -s {{.APIURL}}/files/content/hello.txt`,
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
		return "", fmt.Errorf("nfs: listener did not bind within %s", timeout)
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

// Listen implements plugin.ListenerProvider. It binds the TCP port from
// d.Config, serves the MOUNT and NFS RPC programs on it until ctx is done,
// and returns nil on a clean shutdown.
func (p *Provider) Listen(ctx context.Context, d plugin.Deps) error {
	// The core already tags the logger with the plugin and provider, so this
	// only fills in the optional Deps fields.
	d = d.Normalize()
	cfg := LoadConfig(d.Config)

	// go-nfs logs through a package-global of its own, straight to the
	// standard log package. Route it into tommy's logger instead, once per
	// process - see log.go.
	installLogger(d.Logger)

	l, err := net.Listen("tcp", cfg.ListenAddr())
	if err != nil {
		return fmt.Errorf("nfs listener on %s: %w", cfg.ListenAddr(), err)
	}
	p.setBound(l.Addr().String())
	d.Logger.Info("nfs listening", "addr", l.Addr().String())

	srv := &gonfs.Server{
		Handler: newHandler(&shared{vfs: p.tree(), deps: d, cfg: cfg}),
		Context: ctx,
	}

	// Server.Serve blocks in Accept and closes the listener on its way out.
	// Closing it from here is what unblocks that Accept on shutdown; the
	// error it then returns is the expected one and is not reported.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = l.Close()
		case <-stop:
		}
	}()

	if err := srv.Serve(l); err != nil && ctx.Err() == nil {
		return fmt.Errorf("nfs serve: %w", err)
	}
	return nil
}
