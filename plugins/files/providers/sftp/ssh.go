package sftp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	sftplib "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/files"
)

// Extension keys used to carry what a client authenticated with from the
// ssh.ServerConfig callbacks - the only place that sees it - through to the
// session that records it on every event.
const (
	extMethod      = "tommy-auth-method"
	extUser        = "tommy-auth-user"
	extPassword    = "tommy-auth-password"
	extKeyType     = "tommy-auth-key-type"
	extFingerprint = "tommy-auth-key-fingerprint"
)

// sshServer is the transport half of the provider: it accepts TCP connections,
// runs the SSH handshake, and hands the "sftp" subsystem of each session
// channel to a RequestServer backed by the VFS.
type sshServer struct {
	cfg        Config
	deps       plugin.Deps
	vfs        *files.VFS
	hostKey    HostKey
	authorized *authorizedKeys
}

// serve accepts until ctx is done, then waits for the connections in flight.
func (s *sshServer) serve(ctx context.Context, ln net.Listener) error {
	var conns sync.WaitGroup
	defer conns.Wait()

	// Closing the listener is what unblocks Accept; the deferred close in
	// Listen would only run after this function returns.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = ln.Close()
		case <-done:
		}
	}()

	limit := s.cfg.MaxConnections
	if limit <= 0 {
		limit = DefaultMaxConnections
	}
	slots := make(chan struct{}, limit)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || closedErr(err) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return fmt.Errorf("sftp accept: %w", err)
		}

		select {
		case slots <- struct{}{}:
		default:
			// Refusing is better than queueing: a client gets a closed
			// connection immediately instead of a hang it has to guess at.
			s.deps.Logger.Warn("sftp: connection refused, too many open", "peer", conn.RemoteAddr().String(), "limit", limit)
			_ = conn.Close()
			continue
		}

		conns.Add(1)
		go func() {
			defer conns.Done()
			defer func() { <-slots }()
			s.serveConn(ctx, conn)
		}()
	}
}

// serverConfig builds the SSH configuration. It is rebuilt per connection so a
// callback can stash what it saw on that connection alone.
func (s *sshServer) serverConfig() *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		ServerVersion: s.cfg.ServerVersion,
		MaxAuthTries:  s.cfg.MaxAuthTries,
		// Anyone gets in unless the configuration pins a password or an
		// authorized_keys file - provider rule 1: credentials are recorded,
		// not judged. Accepting the "none" method as well is what makes
		// `sftp -P <port> any@localhost` work without a password prompt.
		NoClientAuth: !s.cfg.RequiresAuth() && !s.authorized.enabled(),
		NoClientAuthCallback: func(conn ssh.ConnMetadata) (*ssh.Permissions, error) {
			return permissions(map[string]string{
				extMethod: "none",
				extUser:   conn.User(),
			}), nil
		},
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			perms := permissions(map[string]string{
				extMethod:   "password",
				extUser:     conn.User(),
				extPassword: string(password),
			})
			if s.authorized.enabled() && !s.cfg.RequiresAuth() {
				// An allowlist that a password walks straight past is not an
				// allowlist, so a configured authorized_keys turns the
				// password method off unless credentials are pinned too.
				return nil, errors.New("sftp: this server requires public-key authentication")
			}
			if !s.cfg.RequiresAuth() {
				return perms, nil
			}
			if s.cfg.Username != "" && conn.User() != s.cfg.Username {
				return nil, fmt.Errorf("sftp: user %q is not accepted", conn.User())
			}
			if s.cfg.Password != "" && string(password) != s.cfg.Password {
				return nil, errors.New("sftp: wrong password")
			}
			return perms, nil
		},
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			perms := permissions(map[string]string{
				extMethod:      "publickey",
				extUser:        conn.User(),
				extKeyType:     key.Type(),
				extFingerprint: ssh.FingerprintSHA256(key),
			})
			if s.authorized.enabled() && !s.authorized.has(key) {
				return nil, fmt.Errorf("sftp: public key %s is not in %s", ssh.FingerprintSHA256(key), s.cfg.AuthorizedKeysPath)
			}
			// A pinned username still applies to key auth; a pinned password
			// cannot, since none was offered.
			if s.cfg.Username != "" && conn.User() != s.cfg.Username {
				return nil, fmt.Errorf("sftp: user %q is not accepted", conn.User())
			}
			if !s.authorized.enabled() && s.cfg.Password != "" {
				// Credentials are pinned but this client offered a key
				// instead: fall through to the password method rather than
				// letting an unknown key past the check.
				return nil, errors.New("sftp: a password is required")
			}
			return perms, nil
		},
		AuthLogCallback: func(conn ssh.ConnMetadata, method string, err error) {
			if err != nil {
				s.deps.Logger.Debug("sftp: auth failed",
					"peer", conn.RemoteAddr().String(), "user", conn.User(), "method", method, "err", err)
			}
		},
	}
	cfg.AddHostKey(s.hostKey.Signer)
	return cfg
}

func permissions(ext map[string]string) *ssh.Permissions {
	return &ssh.Permissions{Extensions: ext}
}

// serveConn runs one client connection: handshake, then every session channel.
func (s *sshServer) serveConn(ctx context.Context, nc net.Conn) {
	peer := nc.RemoteAddr().String()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The handshake gets a short deadline of its own; once a client is
	// authenticated the connection falls back to the idle timeout, which every
	// read and write pushes forward, so a long upload is never cut off.
	conn := newIdleConn(nc, s.cfg.HandshakeTimeout)
	defer func() { _ = conn.Close() }()
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	sconn, chans, reqs, err := ssh.NewServerConn(conn, s.serverConfig())
	if err != nil {
		// A port scanner, a client that gave up, a wrong password: all of them
		// are normal traffic for a fake, and none is worth an error line.
		s.deps.Logger.Debug("sftp: handshake failed", "peer", peer, "err", err)
		return
	}
	defer func() { _ = sconn.Close() }()
	conn.setIdle(s.cfg.IdleTimeout)

	auth := authRecord(sconn)
	s.deps.Logger.Info("sftp: client connected",
		"peer", peer, "user", sconn.User(), "method", auth["method"],
		"client", string(sconn.ClientVersion()))

	// Global requests (keepalives and the like) are answered, never acted on.
	go ssh.DiscardRequests(reqs)

	var channels sync.WaitGroup
	defer channels.Wait()

	for nch := range chans {
		if t := nch.ChannelType(); t != "session" {
			_ = nch.Reject(ssh.UnknownChannelType, "tommy serves the sftp subsystem only; channel type "+t+" is not available")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			s.deps.Logger.Debug("sftp: accept channel", "peer", peer, "err", err)
			continue
		}
		channels.Add(1)
		go func() {
			defer channels.Done()
			s.serveSession(ctx, ch, chReqs, sconn, peer, auth)
		}()
	}
}

// serveSession answers the requests on one session channel. Exactly one of them
// is interesting - "subsystem" naming sftp - and every other one is refused
// with a sentence saying why, because a client that asks for a shell and gets
// silence looks like a hung server.
func (s *sshServer) serveSession(ctx context.Context, ch ssh.Channel, reqs <-chan *ssh.Request, sconn *ssh.ServerConn, peer string, auth map[string]string) {
	defer func() { _ = ch.Close() }()

	for req := range reqs {
		switch req.Type {
		case "subsystem":
			name := subsystemName(req.Payload)
			if name != "sftp" {
				s.refuse(ch, req, fmt.Sprintf("tommy serves the sftp subsystem only; %q is not available", name))
				continue
			}
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			s.serveSFTP(ctx, ch, sconn, peer, auth)
			sendExitStatus(ch, 0)
			return

		case "exec":
			s.refuse(ch, req, "tommy is an SFTP endpoint, not a shell: exec is not available. Use sftp, scp or an SFTP library instead.")
			sendExitStatus(ch, 1)
			return

		case "shell":
			s.refuse(ch, req, "tommy is an SFTP endpoint, not a shell: there is no shell to open. Use sftp, scp or an SFTP library instead.")
			sendExitStatus(ch, 1)
			return

		default:
			// pty-req, env, window-change and friends: harmless to ignore, and
			// an OpenSSH client carries on regardless of the answer.
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

// refuse tells the client what happened on stderr and then says no, which is
// what turns "the connection just sits there" into a readable error.
func (s *sshServer) refuse(ch ssh.Channel, req *ssh.Request, msg string) {
	_, _ = fmt.Fprintln(ch.Stderr(), msg)
	if req.WantReply {
		_ = req.Reply(false, nil)
	}
	s.deps.Logger.Debug("sftp: request refused", "type", req.Type, "reason", msg)
}

// serveSFTP runs the request server for one subsystem request.
func (s *sshServer) serveSFTP(ctx context.Context, ch ssh.Channel, sconn *ssh.ServerConn, peer string, auth map[string]string) {
	sess := files.NewSession(s.vfs, s.deps,
		files.WithProvider(ProviderName),
		files.WithTransport("ssh"),
		files.WithPeer(peer),
		files.WithUser(sconn.User()),
		files.WithSessionMeta(sessionMeta(sconn, peer, auth)))

	h := newHandler(ctx, sess, s.deps.Logger)
	server := sftplib.NewRequestServer(ch, sftplib.Handlers{
		FileGet:  h,
		FilePut:  h,
		FileCmd:  h,
		FileList: h,
	})

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-done:
		}
	}()

	// Serve returns when the client closes the channel. The channel itself is
	// deliberately left open here: the caller still has an exit status to
	// send on it, and closing it first would silently drop that - which is
	// enough to make scp report a failed transfer that in fact succeeded.
	err := server.Serve()
	switch {
	case err == nil, errors.Is(err, io.EOF), closedErr(err):
	default:
		s.deps.Logger.Debug("sftp: session ended", "peer", peer, "err", err)
	}
}

// sessionMeta is what every event from this connection carries: who connected,
// from where, with which client and which credentials. Recorded, never judged.
func sessionMeta(sconn *ssh.ServerConn, peer string, auth map[string]string) map[string]any {
	record := map[string]any{"accepted": true}
	for k, v := range auth {
		record[k] = v
	}
	return map[string]any{
		"auth":           record,
		"peer":           peer,
		"client_version": string(sconn.ClientVersion()),
		"session_id":     fmt.Sprintf("%x", sconn.SessionID()),
	}
}

// authRecord pulls what the auth callback stashed back out of the connection.
func authRecord(sconn *ssh.ServerConn) map[string]string {
	out := map[string]string{"method": "none", "user": sconn.User()}
	if sconn.Permissions == nil {
		return out
	}
	for key, field := range map[string]string{
		extMethod:      "method",
		extUser:        "user",
		extPassword:    "password",
		extKeyType:     "key_type",
		extFingerprint: "key_fingerprint",
	} {
		if v, ok := sconn.Permissions.Extensions[key]; ok && v != "" {
			out[field] = v
		}
	}
	return out
}

// subsystemName decodes an RFC 4254 subsystem request payload, which is one
// SSH string: a four-byte big-endian length followed by the name.
func subsystemName(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	n := binary.BigEndian.Uint32(payload[:4])
	if uint64(n) > uint64(len(payload)-4) {
		return ""
	}
	return string(payload[4 : 4+n])
}

// sendExitStatus closes out a channel the way an SSH server is expected to, so
// a client learns the command finished instead of waiting for a timeout.
func sendExitStatus(ch ssh.Channel, code uint32) {
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], code)
	_, _ = ch.SendRequest("exit-status", false, payload[:])
}

// authorizedKeys is the optional public-key allowlist.
type authorizedKeys struct {
	path string
	keys map[string]struct{}
}

func (a *authorizedKeys) enabled() bool { return a != nil && len(a.keys) > 0 }

func (a *authorizedKeys) has(key ssh.PublicKey) bool {
	if !a.enabled() {
		return false
	}
	_, ok := a.keys[string(key.Marshal())]
	return ok
}

// loadAuthorizedKeys reads an OpenSSH authorized_keys file. An unset path means
// the feature is off; a set path that cannot be read is a startup error, since
// silently accepting everyone would be the opposite of what was asked for.
func loadAuthorizedKeys(path string) (*authorizedKeys, error) {
	path = ExpandPath(path)
	if path == "" {
		return &authorizedKeys{}, nil
	}
	data, err := os.ReadFile(path) // #nosec G304 - the path is operator configuration
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("sftp: authorized_keys %s does not exist", path)
		}
		return nil, fmt.Errorf("sftp: read authorized_keys %s: %w", path, err)
	}
	out := &authorizedKeys{path: path, keys: map[string]struct{}{}}
	rest := data
	for len(rest) > 0 {
		key, _, _, remainder, err := ssh.ParseAuthorizedKey(rest)
		if err != nil {
			break
		}
		out.keys[string(key.Marshal())] = struct{}{}
		rest = remainder
	}
	if len(out.keys) == 0 {
		return nil, fmt.Errorf("sftp: authorized_keys %s contains no usable key", path)
	}
	return out, nil
}

// idleConn is a net.Conn that pushes its deadline forward on every read and
// write, so an idle connection is dropped while a slow but live transfer is
// not. The handshake runs with a shorter value, swapped for the idle one the
// moment authentication succeeds.
type idleConn struct {
	net.Conn
	nanos atomic.Int64
}

func newIdleConn(c net.Conn, timeout time.Duration) *idleConn {
	ic := &idleConn{Conn: c}
	ic.setIdle(timeout)
	ic.touch()
	return ic
}

func (c *idleConn) setIdle(timeout time.Duration) {
	c.nanos.Store(int64(timeout))
	c.touch()
}

func (c *idleConn) touch() {
	if d := time.Duration(c.nanos.Load()); d > 0 {
		_ = c.SetDeadline(time.Now().Add(d))
	} else {
		_ = c.SetDeadline(time.Time{})
	}
}

func (c *idleConn) Read(b []byte) (int, error) {
	c.touch()
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	c.touch()
	return c.Conn.Write(b)
}
