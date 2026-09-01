// Package smtp is tommy's SMTP listener: a mail provider that owns a real SMTP
// port and turns everything delivered to it into canonical mail messages.
//
// It is the provider for applications that speak plain SMTP rather than a
// vendor HTTP API - anything pointed at localhost:1025 with no credentials,
// which is how most frameworks are configured in development. Nothing is ever
// relayed: a message is parsed, its attachments are stored in the blob store,
// and the untouched wire bytes are kept in Event.Raw so the exact source is
// always available.
package smtp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/mail"
)

// ProviderName is the provider's name, which is also how it is addressed in
// configuration: [plugins.mail.providers.smtp].
const ProviderName = "smtp"

// DefaultPort is the port the listener binds when the configuration says
// nothing. 1025 is the unprivileged convention every mail catcher uses, so an
// application already pointed at a development mailhog needs no change.
const DefaultPort = 1025

// Defaults for the rest of the listener. They are deliberately generous enough
// for real mail and small enough that a hostile client cannot exhaust memory:
// a connection can hold at most one message of MaxMessageBytes.
const (
	DefaultBind = "127.0.0.1"
	// DefaultDomain is the hostname in the greeting banner.
	DefaultDomain = "tommy"
	// DefaultMaxMessageBytes caps one DATA transaction.
	DefaultMaxMessageBytes = 25 << 20
	// DefaultMaxRecipients caps RCPT TO per transaction.
	DefaultMaxRecipients = 100
	// DefaultMaxLineLength is well above RFC 5321's 1000, because real senders
	// emit long unwrapped HTML lines and a debugging tool that rejected them
	// would be hiding exactly what someone came to look at.
	DefaultMaxLineLength = 64 << 10
	// DefaultReadTimeout and DefaultWriteTimeout stop a stalled connection from
	// holding resources forever.
	DefaultReadTimeout  = 60 * time.Second
	DefaultWriteTimeout = 60 * time.Second
)

// Config is the [plugins.mail.providers.smtp] section.
//
// Every key is optional. port = 0 means "bind an ephemeral port", which is what
// the test harness uses; an absent port means DefaultPort.
type Config struct {
	Bind            string
	Port            int
	Domain          string
	MaxMessageBytes int64
	MaxRecipients   int
	MaxLineLength   int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration

	// Username and Password pin the credentials the listener accepts. They are
	// empty by default, which means any credentials are accepted - and no
	// credentials at all are accepted too, since AUTH is never required. Set
	// either one and AUTH becomes mandatory and is checked, which is how you
	// test your application's error path.
	Username string
	Password string
}

// LoadConfig reads the provider section, applying every default.
func LoadConfig(pc plugin.ProviderConfig) Config {
	return Config{
		Bind:            pc.String("bind", DefaultBind),
		Port:            pc.Int("port", DefaultPort),
		Domain:          pc.String("domain", DefaultDomain),
		MaxMessageBytes: int64(pc.Int("max_message_bytes", DefaultMaxMessageBytes)),
		MaxRecipients:   pc.Int("max_recipients", DefaultMaxRecipients),
		MaxLineLength:   pc.Int("max_line_length", DefaultMaxLineLength),
		ReadTimeout:     time.Duration(pc.Int("read_timeout", int(DefaultReadTimeout/time.Second))) * time.Second,
		WriteTimeout:    time.Duration(pc.Int("write_timeout", int(DefaultWriteTimeout/time.Second))) * time.Second,
		Username:        pc.String("username", ""),
		Password:        pc.String("password", ""),
	}
}

// RequiresAuth reports whether the configuration pins credentials. When it does
// not, AUTH is accepted and recorded but never demanded.
func (c Config) RequiresAuth() bool { return c.Username != "" || c.Password != "" }

// ListenAddr is the address Listen binds.
func (c Config) ListenAddr() string { return net.JoinHostPort(c.Bind, fmt.Sprint(c.Port)) }

// Provider is the SMTP listener.
type Provider struct {
	mu    sync.Mutex
	addr  string
	bound chan struct{}
}

var (
	_ plugin.Provider         = (*Provider)(nil)
	_ plugin.ListenerProvider = (*Provider)(nil)
)

// New returns the SMTP provider. The port comes from the configuration at
// Listen time, so one value is safe to construct before anything is running.
func New() *Provider { return &Provider{bound: make(chan struct{})} }

// Name implements plugin.Provider.
func (p *Provider) Name() string { return ProviderName }

// Plugin implements plugin.Provider.
func (p *Provider) Plugin() string { return mail.PluginName }

// Description implements plugin.Provider.
func (p *Provider) Description() string {
	return "A real SMTP server that accepts mail from any client on its own port and never delivers it anywhere. " +
		"It parses MIME - nested multiparts, attachments, inline images and encoded-word headers - into the canonical message, records the envelope and any AUTH that was offered, and keeps the untouched wire bytes."
}

// Endpoints implements plugin.Provider. The provider speaks SMTP on its own
// port and mounts no HTTP routes.
func (p *Provider) Endpoints() []plugin.Endpoint { return nil }

// Snippets implements plugin.Provider.
func (p *Provider) Snippets() []plugin.Snippet {
	// The listener address is rendered from the live SnippetCtx, with the
	// package default as the fallback. That fallback is not a hardcoded port in
	// disguise: the core can only publish an address it was configured with, so
	// when the configuration says nothing the snippet and Listen fall back to
	// the very same constant.
	addr := fmt.Sprintf(`{{with .SMTPAddr}}{{.}}{{else}}{{.Host}}:%d{{end}}`, DefaultPort)
	port := fmt.Sprintf(`{{with .Port "mail" "smtp"}}{{.}}{{else}}%d{{end}}`, DefaultPort)

	return []plugin.Snippet{
		{
			Title: "Send a message with curl",
			Lang:  "bash",
			Code: `curl -s smtp://` + addr + ` \
  --mail-from alice@example.com --mail-rcpt bob@example.com -T - <<'EOF'
From: Alice <alice@example.com>
To: Bob <bob@example.com>
Subject: Hello from tommy

It works.
EOF`,
		},
		{
			Title: "Send a multipart message with Python",
			Lang:  "python",
			Code: `import smtplib
from email.message import EmailMessage

msg = EmailMessage()
msg["From"] = "Alice <alice@example.com>"
msg["To"] = "Bob <bob@example.com>"
msg["Subject"] = "Hello from tommy"
msg.set_content("It works.")
msg.add_alternative("<p>It <b>works</b>.</p>", subtype="html")
msg.add_attachment(b"id,total\n1,42\n", maintype="text", subtype="csv",
                   filename="invoice.csv")

with smtplib.SMTP("{{.Host}}", ` + port + `) as s:
    s.send_message(msg)`,
		},
	}
}

// RegisterIngress implements plugin.Provider. Nothing is mounted: this provider
// speaks SMTP on a port of its own.
func (p *Provider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {}

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
		return "", fmt.Errorf("smtp: listener did not bind within %s", timeout)
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

// Listen implements plugin.ListenerProvider. It reads its port from d.Config,
// serves SMTP until ctx is done, and returns nil on a clean shutdown.
func (p *Provider) Listen(ctx context.Context, d plugin.Deps) error {
	d = d.Normalize()
	d = d.WithLogger("plugin", mail.PluginName, "provider", ProviderName)
	cfg := LoadConfig(d.Config)

	ln, err := net.Listen("tcp", cfg.ListenAddr())
	if err != nil {
		return fmt.Errorf("smtp listener on %s: %w", cfg.ListenAddr(), err)
	}
	p.setAddr(ln.Addr().String())
	d.Logger.Info("smtp listening", "addr", ln.Addr().String())

	srv := gosmtp.NewServer(&backend{d: d, cfg: cfg})
	srv.Domain = cfg.Domain
	srv.MaxMessageBytes = cfg.MaxMessageBytes
	srv.MaxRecipients = cfg.MaxRecipients
	srv.MaxLineLength = cfg.MaxLineLength
	srv.ReadTimeout = cfg.ReadTimeout
	srv.WriteTimeout = cfg.WriteTimeout
	// Without TLS the library refuses to advertise AUTH at all, and a fake that
	// will not let a client authenticate is a fake nobody can point an
	// application at.
	srv.AllowInsecureAuth = true
	srv.EnableSMTPUTF8 = true
	srv.ErrorLog = &logAdapter{log: d.Logger}

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = srv.Close()
			_ = ln.Close()
		case <-stop:
		}
	}()

	if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
		return fmt.Errorf("smtp serve: %w", err)
	}
	return nil
}

// logAdapter routes go-smtp's per-connection chatter into the injected logger,
// so a client that hangs up mid-transaction does not print to stderr.
type logAdapter struct{ log *slog.Logger }

func (l *logAdapter) Printf(format string, v ...interface{}) {
	l.log.Debug("smtp: " + fmt.Sprintf(format, v...))
}

func (l *logAdapter) Println(v ...interface{}) {
	l.log.Debug("smtp: " + fmt.Sprintln(v...))
}
