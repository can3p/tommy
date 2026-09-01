// Package ftp is tommy's FTP listener: a files provider that owns a real FTP
// port and reads and writes the plugin's shared virtual filesystem over the
// wire protocol every FTP client and library already speaks.
//
// It is built on fclairamb/ftpserverlib, whose ClientDriver contract is an
// afero.Fs - which is exactly what plugins/files/vfs.go already looks like
// through a Session. fsAdapter in fs.go is the thin translation layer: it
// forwards every afero call straight onto a *files.Session and never
// interprets a path itself, because VFS.Resolve is the one place that is
// allowed to.
package ftp

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	ftpserver "github.com/fclairamb/ftpserverlib"

	"github.com/can3p/tommy/core/plugin"
)

// ProviderName is the provider's name, which is also how it is addressed in
// configuration: [plugins.files.providers.ftp].
const ProviderName = "ftp"

// DefaultPort is the port the listener binds when the configuration says
// nothing. 2121 is unprivileged, unlike FTP's traditional 21, so tommy never
// needs elevated permissions to run it.
const DefaultPort = 2121

// Defaults for the rest of the listener.
const (
	DefaultBind        = "127.0.0.1"
	DefaultPassiveHost = "127.0.0.1"
	// DefaultIdleTimeoutSeconds is how long a connection may sit with no
	// command before it is dropped.
	DefaultIdleTimeoutSeconds = 900
	// DefaultConnectionTimeoutSeconds bounds establishing a passive or active
	// data connection.
	DefaultConnectionTimeoutSeconds = 30
)

// Config is the [plugins.files.providers.ftp] section.
//
// Every key is optional. port = 0 means "bind an ephemeral port", which is
// what the test harness uses; an absent port means DefaultPort.
type Config struct {
	Bind string
	Port int

	// PassiveHost is the IP address advertised to a client in response to
	// PASV/EPSV so it knows where to dial back for the data connection. It
	// must be a literal IPv4 address - ftpserverlib splits it into four
	// dotted quads - which is why "localhost" will not do.
	PassiveHost string
	// PassiveRange restricts passive data connections to a fixed port range,
	// the way a real FTP server behind a firewall has to. Unset (the
	// default) lets the OS pick an ephemeral port per transfer, which is the
	// more convenient default for a local testing tool and avoids collisions
	// between parallel test runs.
	PassiveRange *ftpserver.PortRange

	// Username and Password pin the credentials the listener accepts. Empty
	// by default, which means any credentials - or none at all - are
	// accepted. Set either one and the opposite becomes true: login is then
	// checked, which is how you test your application's error path.
	Username string
	Password string

	IdleTimeoutSeconds       int
	ConnectionTimeoutSeconds int
}

// LoadConfig reads the provider section, applying every default.
func LoadConfig(pc plugin.ProviderConfig) (Config, error) {
	cfg := Config{
		Bind:                     pc.String("bind", DefaultBind),
		Port:                     pc.Int("port", DefaultPort),
		PassiveHost:              pc.String("passive_host", DefaultPassiveHost),
		Username:                 pc.String("username", ""),
		Password:                 pc.String("password", ""),
		IdleTimeoutSeconds:       pc.Int("idle_timeout", DefaultIdleTimeoutSeconds),
		ConnectionTimeoutSeconds: pc.Int("connection_timeout", DefaultConnectionTimeoutSeconds),
	}
	if raw := strings.TrimSpace(pc.String("passive_ports", "")); raw != "" {
		r, err := parsePortRange(raw)
		if err != nil {
			return Config{}, fmt.Errorf("ftp: passive_ports: %w", err)
		}
		cfg.PassiveRange = &r
	}
	return cfg, nil
}

// parsePortRange parses "START-END" into an *ftpserver.PortRange.
func parsePortRange(s string) (ftpserver.PortRange, error) {
	before, after, ok := strings.Cut(s, "-")
	if !ok {
		return ftpserver.PortRange{}, fmt.Errorf("expected START-END, got %q", s)
	}
	start, err := strconv.Atoi(strings.TrimSpace(before))
	if err != nil {
		return ftpserver.PortRange{}, fmt.Errorf("invalid start port %q: %w", before, err)
	}
	end, err := strconv.Atoi(strings.TrimSpace(after))
	if err != nil {
		return ftpserver.PortRange{}, fmt.Errorf("invalid end port %q: %w", after, err)
	}
	if start <= 0 || end < start || end > 65535 {
		return ftpserver.PortRange{}, fmt.Errorf("invalid range %d-%d", start, end)
	}
	return ftpserver.PortRange{Start: start, End: end}, nil
}

// RequiresAuth reports whether the configuration pins credentials. When it
// does not, any USER/PASS is accepted and recorded.
func (c Config) RequiresAuth() bool { return c.Username != "" || c.Password != "" }

// ListenAddr is the address Listen binds.
func (c Config) ListenAddr() string { return net.JoinHostPort(c.Bind, strconv.Itoa(c.Port)) }
