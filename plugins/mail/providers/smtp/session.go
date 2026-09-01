package smtp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/mail"
)

// backend hands every connection its own session.
type backend struct {
	d   plugin.Deps
	cfg Config
}

func (b *backend) NewSession(c *gosmtp.Conn) (gosmtp.Session, error) {
	peer := ""
	if conn := c.Conn(); conn != nil && conn.RemoteAddr() != nil {
		peer = conn.RemoteAddr().String()
	}
	return &session{b: b, conn: c, peer: peer}, nil
}

// authRecord is what a client presented to AUTH. It is recorded rather than
// judged: a fake that rejected credentials would fail every application that
// has not been configured yet.
type authRecord struct {
	Mechanism string `json:"mechanism"`
	Identity  string `json:"identity,omitempty"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
	Accepted  bool   `json:"accepted"`
}

// session is one SMTP transaction sequence on one connection.
type session struct {
	b    *backend
	conn *gosmtp.Conn
	peer string

	from     string
	fromSet  bool
	mailOpts *gosmtp.MailOptions
	rcpts    []string
	auth     *authRecord
}

var _ gosmtp.AuthSession = (*session)(nil)

// AuthMechanisms advertises the two mechanisms every mail library can speak.
func (s *session) AuthMechanisms() []string { return []string{sasl.Plain, sasl.Login} }

// Auth returns the SASL server for a mechanism. Credentials are recorded and
// only checked when the provider configuration pins them.
func (s *session) Auth(mech string) (sasl.Server, error) {
	switch strings.ToUpper(mech) {
	case sasl.Plain:
		return sasl.NewPlainServer(func(identity, username, password string) error {
			return s.record(sasl.Plain, identity, username, password)
		}), nil
	case sasl.Login:
		return &loginServer{authenticate: func(username, password string) error {
			return s.record(sasl.Login, "", username, password)
		}}, nil
	default:
		return nil, gosmtp.ErrAuthUnknownMechanism
	}
}

// record keeps what was presented and applies the pinned credentials, if any.
func (s *session) record(mech, identity, username, password string) error {
	rec := &authRecord{Mechanism: mech, Identity: identity, Username: username, Password: password, Accepted: true}
	s.auth = rec
	cfg := s.b.cfg
	if !cfg.RequiresAuth() {
		return nil
	}
	if (cfg.Username != "" && username != cfg.Username) || (cfg.Password != "" && password != cfg.Password) {
		rec.Accepted = false
		return gosmtp.ErrAuthFailed
	}
	return nil
}

// Mail records the envelope sender. The headers of the message may say
// something else entirely, and that difference is often the bug being chased,
// so both are kept.
func (s *session) Mail(from string, opts *gosmtp.MailOptions) error {
	if s.b.cfg.RequiresAuth() && (s.auth == nil || !s.auth.Accepted) {
		return gosmtp.ErrAuthRequired
	}
	s.from = from
	s.fromSet = true
	s.mailOpts = opts
	return nil
}

// Rcpt records one envelope recipient.
func (s *session) Rcpt(to string, opts *gosmtp.RcptOptions) error {
	s.rcpts = append(s.rcpts, to)
	return nil
}

// Reset clears the transaction, keeping the authentication as RFC 5321 does.
func (s *session) Reset() {
	s.from = ""
	s.fromSet = false
	s.mailOpts = nil
	s.rcpts = nil
}

// Logout implements gosmtp.Session.
func (s *session) Logout() error { return nil }

// Data reads the message, parses it and appends one event.
//
// One SMTP transaction is one delivered message even when it carried several
// RCPT TO addresses: the envelope recipients all land in Meta, and the
// canonical To/Cc/Bcc come from the headers, exactly as they were sent.
func (s *session) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		if errors.Is(err, gosmtp.ErrDataTooLarge) {
			return gosmtp.ErrDataTooLarge
		}
		return &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 3, 0},
			Message: "Could not read message data: " + err.Error()}
	}

	ctx := context.Background()
	msg, warns, err := ParseMessage(ctx, raw, s.b.d.Blobs)
	if err != nil {
		s.b.d.Logger.Error("smtp: store message", "err", err)
		return &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 3, 0},
			Message: "Could not store message: " + err.Error()}
	}

	ev := mail.NewEvent(ProviderName, msg)
	ev.Raw = event.Raw{
		Transport: "smtp",
		PeerAddr:  s.peer,
		Headers:   wireHeaders(raw),
		Body:      raw,
		// The raw bytes are served as message/rfc822 and shown in the source
		// view; only an embedded NUL means they are not renderable as text.
		Text: !bytes.ContainsRune(raw, 0),
	}
	ev.Meta = s.meta(raw, warns)

	// The envelope is the only sender a message with no From header has, so it
	// backs the listing summary without ever leaking into the canonical model.
	if ev.Summary.From == "" && s.from != "" {
		ev.Summary.From = s.from
	}
	if len(ev.Summary.To) == 0 && len(s.rcpts) > 0 {
		ev.Summary.To = append([]string(nil), s.rcpts...)
	}

	if err := s.b.d.Append(ctx, ev); err != nil {
		s.b.d.Logger.Error("smtp: append event", "err", err)
		return &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 3, 0},
			Message: "Could not store message: " + err.Error()}
	}
	s.b.d.Logger.Info("smtp: message captured",
		"id", string(ev.ID), "from", s.from, "rcpts", len(s.rcpts), "bytes", len(raw))

	return &gosmtp.SMTPError{Code: 250, EnhancedCode: gosmtp.EnhancedCode{2, 0, 0},
		Message: fmt.Sprintf("OK: queued as %s", ev.ID)}
}

// meta collects everything about the delivery that is not the message itself.
func (s *session) meta(raw []byte, warns []string) map[string]any {
	envelope := map[string]any{
		"mail_from": s.from,
		"rcpt_to":   append([]string(nil), s.rcpts...),
	}
	if s.rcpts == nil {
		envelope["rcpt_to"] = []string{}
	}
	meta := map[string]any{
		"envelope": envelope,
		"helo":     s.conn.Hostname(),
		"peer":     s.peer,
		"size":     len(raw),
	}
	if o := s.mailOpts; o != nil {
		if o.Body != "" {
			meta["body_type"] = string(o.Body)
		}
		if o.Size > 0 {
			meta["declared_size"] = o.Size
		}
		if o.UTF8 {
			meta["smtputf8"] = true
		}
		if o.Auth != nil {
			envelope["auth"] = *o.Auth
		}
	}
	if s.auth != nil {
		meta["auth"] = s.auth
	}
	if len(warns) > 0 {
		meta["parse_warnings"] = warns
	}
	return meta
}

// wireHeaders is the header block exactly as it arrived, names and all, which
// is what event.Raw.Headers is for. The canonical message carries the decoded
// copy.
func wireHeaders(raw []byte) map[string][]string {
	fields, _ := splitMessage(raw)
	if len(fields) == 0 {
		return nil
	}
	out := map[string][]string{}
	for _, f := range fields {
		// Assigned directly rather than through Add, so the casing the sender
		// used survives into the raw view.
		out[f.Name] = append(out[f.Name], f.Value)
	}
	return out
}

// loginServer implements the obsolete-but-universal AUTH LOGIN exchange, which
// go-sasl ships only as a client. Real mail libraries still offer it, and a
// fake that answered "unsupported mechanism" would look broken.
type loginServer struct {
	state        int
	username     string
	authenticate func(username, password string) error
}

const (
	loginInit = iota
	loginWantUsername
	loginWantPassword
	loginDone
)

func (l *loginServer) Next(response []byte) (challenge []byte, done bool, err error) {
	switch l.state {
	case loginInit:
		if response == nil {
			l.state = loginWantUsername
			return []byte("Username:"), false, nil
		}
		// AUTH LOGIN <base64 username> carried the username already.
		l.username = string(response)
		l.state = loginWantPassword
		return []byte("Password:"), false, nil
	case loginWantUsername:
		l.username = string(response)
		l.state = loginWantPassword
		return []byte("Password:"), false, nil
	case loginWantPassword:
		l.state = loginDone
		return nil, true, l.authenticate(l.username, string(response))
	default:
		return nil, false, errors.New("smtp: unexpected AUTH LOGIN exchange")
	}
}
