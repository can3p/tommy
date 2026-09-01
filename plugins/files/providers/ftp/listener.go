package ftp

import (
	"context"
	"fmt"
	"sync"
	"time"

	ftpserver "github.com/fclairamb/ftpserverlib"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/files"
)

// Provider is the FTP listener.
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
	_ files.VFSBinder            = (*Provider)(nil)
)

// New returns the FTP provider. The port and every other setting come from
// the configuration at Listen time, so one value is safe to construct before
// anything is running.
func New() *Provider { return &Provider{bound: make(chan struct{})} }

// BindVFS receives the filesystem shared with every other files provider.
// Called by files.New before Listen ever runs.
func (p *Provider) BindVFS(v *files.VFS) { p.vfs = v }

// Name implements plugin.Provider.
func (p *Provider) Name() string { return ProviderName }

// Plugin implements plugin.Provider.
func (p *Provider) Plugin() string { return files.PluginName }

// Description implements plugin.Provider.
func (p *Provider) Description() string {
	return "A real FTP server, backed by the shared virtual filesystem rather than a real disk: STOR, RETR, directory listings, rename and delete all work over a genuine control connection, with real passive-mode data transfers, so any FTP client or library can be pointed at it."
}

// Endpoints implements plugin.Provider. The provider speaks FTP on its own
// port and mounts no HTTP routes.
func (p *Provider) Endpoints() []plugin.Endpoint { return nil }

// Snippets implements plugin.Provider.
func (p *Provider) Snippets() []plugin.Snippet {
	// Rendered from the live SnippetCtx, with the package default as the
	// fallback for a configuration that names no port explicitly - the same
	// fallback Listen itself uses, so the two can never disagree.
	addr := fmt.Sprintf(`{{with .FTPAddr}}{{.}}{{else}}{{.Host}}:%d{{end}}`, DefaultPort)

	return []plugin.Snippet{{
		Title: "Upload a file with curl",
		Lang:  "bash",
		Code: `curl -T ./local.txt ftp://` + addr + `/upload/local.txt --ftp-create-dirs -u any:any
curl -s http://localhost:8811/api/v1/files/content/upload/local.txt`,
	}}
}

// RegisterIngress implements plugin.Provider. Nothing is mounted: this
// provider speaks FTP on a port of its own.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {}

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
		return "", fmt.Errorf("ftp: listener did not bind within %s", timeout)
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

// Listen implements plugin.ListenerProvider. It reads its settings from
// d.Config, serves FTP until ctx is done, and returns nil on a clean
// shutdown.
func (p *Provider) Listen(ctx context.Context, d plugin.Deps) error {
	d = d.Normalize()
	d = d.WithLogger("plugin", files.PluginName, "provider", ProviderName)

	cfg, err := LoadConfig(d.Config)
	if err != nil {
		return fmt.Errorf("ftp: %w", err)
	}

	vfs := p.vfs
	if vfs == nil {
		// A provider constructed and Listen-ed without going through
		// files.New (a bespoke test, say) still gets a working, if private,
		// filesystem rather than a nil-pointer panic on first login.
		vfs = files.NewVFS()
	}

	drv := &mainDriver{vfs: vfs, deps: d, cfg: cfg}
	srv := ftpserver.NewFtpServer(drv)
	srv.Logger = d.Logger

	if err := srv.Listen(); err != nil {
		return fmt.Errorf("ftp listen on %s: %w", cfg.ListenAddr(), err)
	}
	p.setAddr(srv.Addr())
	d.Logger.Info("ftp listening", "addr", srv.Addr())

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = srv.Stop()
		case <-stop:
		}
	}()

	if err := srv.Serve(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("ftp serve: %w", err)
	}
	return nil
}
