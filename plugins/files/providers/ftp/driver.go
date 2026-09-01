package ftp

import (
	"crypto/tls"
	"fmt"

	ftpserver "github.com/fclairamb/ftpserverlib"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/files"
)

// mainDriver implements ftpserver.MainDriver. It owns the listener-wide
// settings and turns a successful login into a *files.Session bound to the
// shared VFS - one Session per connection, so the peer address and the
// accepted user land on every event that connection produces.
type mainDriver struct {
	vfs  *files.VFS
	deps plugin.Deps
	cfg  Config
}

var _ ftpserver.MainDriver = (*mainDriver)(nil)

// GetSettings implements ftpserver.MainDriver.
func (d *mainDriver) GetSettings() (*ftpserver.Settings, error) {
	settings := &ftpserver.Settings{
		ListenAddr: d.cfg.ListenAddr(),
		// PublicHost must be a literal IPv4 address - ftpserverlib splits it
		// into four dotted quads for the PASV reply - which is why
		// passive_host defaults to 127.0.0.1 rather than a hostname.
		PublicHost:        d.cfg.PassiveHost,
		Banner:            "tommy FTP server ready",
		IdleTimeout:       d.cfg.IdleTimeoutSeconds,
		ConnectionTimeout: d.cfg.ConnectionTimeoutSeconds,
		// The VFS holds exactly the bytes it was given and nothing about it
		// is line-oriented, so a byte-for-byte transfer is always what an
		// application under test wants back - never vsftpd-style CRLF
		// rewriting sprung on it by a client that happened to default to
		// TYPE A, or never sent TYPE at all. Binary is the default transfer
		// type, and ASCII conversion is disabled outright rather than merely
		// discouraged, which also lets SIZE work regardless of TYPE.
		DefaultTransferType:    ftpserver.TransferTypeBinary,
		DisableASCIIConversion: true,
	}
	if d.cfg.PassiveRange != nil {
		settings.PassiveTransferPortRange = *d.cfg.PassiveRange
	}
	return settings, nil
}

// ClientConnected implements ftpserver.MainDriver.
func (d *mainDriver) ClientConnected(cc ftpserver.ClientContext) (string, error) {
	return "tommy FTP server ready", nil
}

// ClientDisconnected implements ftpserver.MainDriver. Nothing to release: the
// Session created in AuthUser holds no resource beyond the shared VFS.
func (d *mainDriver) ClientDisconnected(cc ftpserver.ClientContext) {}

// AuthUser implements ftpserver.MainDriver.
//
// Credentials are recorded rather than judged, unless the configuration pins
// them - provider rule 1. Every mutation the returned driver goes on to make
// carries the presented username in Event.Meta.user and Op.User (see
// files.Session), which is the "either way" half of the rule: there is
// nothing to attach metadata to for a login that never produces a session, so
// a pinned-credential rejection is refused outright, exactly as the SMTP
// provider refuses an unauthenticated MAIL FROM.
func (d *mainDriver) AuthUser(cc ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	if d.cfg.RequiresAuth() && (user != d.cfg.Username || pass != d.cfg.Password) {
		return nil, fmt.Errorf("ftp: invalid credentials")
	}

	peer := ""
	if addr := cc.RemoteAddr(); addr != nil {
		peer = addr.String()
	}
	sess := files.NewSession(d.vfs, d.deps,
		files.WithProvider(ProviderName),
		files.WithTransport("ftp"),
		files.WithPeer(peer),
		files.WithUser(user),
	)
	return &fsAdapter{sess: sess}, nil
}

// GetTLSConfig implements ftpserver.MainDriver. TLS is never required -
// credentials are not secrets here - so a nil config is fine; it is only
// consulted if a client explicitly asks for AUTH TLS.
func (d *mainDriver) GetTLSConfig() (*tls.Config, error) { return nil, nil }
