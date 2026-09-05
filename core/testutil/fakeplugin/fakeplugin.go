// Package fakeplugin is a complete, self-contained plugin used to exercise the
// core: registry validation, ingress routing, listener supervision, the generic
// event view, SSE delivery and plugintest.Conformance.
//
// It exists so the core can be proven end to end without a real mail or sms
// plugin, and so Wave 1 authors have a worked example of the contracts.
package fakeplugin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/store"
)

// Message is the plugin's canonical model, the shape a real plugin would put in
// Event.Payload.
type Message struct {
	From string `json:"from"`
	To   string `json:"to"`
	Text string `json:"text"`
}

// Plugin is the fake content type.
type Plugin struct {
	name      string
	providers []plugin.Provider
}

// Option configures the fake plugin.
type Option func(*Plugin)

// WithName renames the plugin, for tests that need two of them.
func WithName(name string) Option {
	return func(p *Plugin) { p.name = name }
}

// New returns the fake plugin with an HTTP provider and a TCP listener
// provider.
func New(opts ...Option) *Plugin {
	p := &Plugin{name: "fake"}
	for _, o := range opts {
		o(p)
	}
	p.providers = []plugin.Provider{
		&EchoProvider{plugin: p.name},
		&LineProvider{plugin: p.name, addrCh: make(chan string, 1)},
	}
	return p
}

func (p *Plugin) Name() string  { return p.name }
func (p *Plugin) Title() string { return strings.ToUpper(p.name[:1]) + p.name[1:] }
func (p *Plugin) Description() string {
	return "A test-only content type used to exercise tommy's core: it captures whatever you send it and shows it in the generic event view."
}
func (p *Plugin) Providers() []plugin.Provider { return p.providers }
func (p *Plugin) Templates() fs.FS             { return nil }

// RegisterAPI mounts the plugin's own read-back route under /api/v1/<name>/.
// APIEndpoints declares the one route RegisterAPI mounts. A plugin double has
// to satisfy the same contract as a real one, or it stops being a useful
// double - and this one is what the plugin-description tests are written
// against.
func (p *Plugin) APIEndpoints() []plugin.APIEndpoint {
	return []plugin.APIEndpoint{{
		Method:      "GET",
		Path:        "/messages",
		Description: "Every payload this fake plugin captured, newest first.",
		Response:    []any{},
	}}
}

func (p *Plugin) RegisterAPI(mux plugin.Mux, d plugin.Deps) {
	mux.HandleFunc("GET /messages", func(w http.ResponseWriter, r *http.Request) {
		events, err := d.Store.List(r.Context(), store.Query{Plugin: p.name})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out := make([]any, 0, len(events))
		for _, e := range events {
			out = append(out, e.Payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
}

// RegisterUI mounts nothing, on purpose: the plugin must then get the generic
// event view for free.
func (p *Plugin) RegisterUI(mux plugin.Mux, d plugin.Deps) {}

// EchoProvider is a plain HTTP provider on the shared ingress.
type EchoProvider struct{ plugin string }

func (e *EchoProvider) Name() string   { return "echo" }
func (e *EchoProvider) Plugin() string { return e.plugin }
func (e *EchoProvider) Description() string {
	return "Imitates a minimal JSON send API: POST a message and read it back, the way a real vendor SDK would."
}

func (e *EchoProvider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{
		{Method: "POST", Path: "/fake/v1/send", Description: "Accept a message and record it as an event."},
		{Method: "GET", Path: "/fake/v1/messages/{id}", Description: "Read a recorded message back from the store."},
	}
}

func (e *EchoProvider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{{
		Title: "Send a fake message",
		Lang:  "bash",
		Code: `curl -s {{.IngressURL}}/fake/v1/send \
  -H 'Content-Type: application/json' \
  -d '{"from":"a@example.com","to":"b@example.com","text":"It works."}'`,
	}, {
		Title: "Watch events arrive",
		Lang:  "bash",
		Code:  `curl -N {{.APIURL}}/events/stream`,
	}}
}

func (e *EchoProvider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()

	mux.HandleFunc("POST /fake/v1/send", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var msg Message
		if err := json.Unmarshal(body, &msg); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid json"})
			return
		}

		ev := &event.Event{
			Plugin:   e.plugin,
			Provider: e.Name(),
			Type:     e.plugin + ".message",
			Summary: event.Summary{
				From:    msg.From,
				To:      []string{msg.To},
				Title:   firstLine(msg.Text),
				Snippet: msg.Text,
			},
			Meta:    map[string]any{"content_type": r.Header.Get("Content-Type")},
			Payload: &msg,
			Raw: event.Raw{
				Transport: "http",
				PeerAddr:  r.RemoteAddr,
				Method:    r.Method,
				Path:      r.URL.Path,
				Headers:   r.Header.Clone(),
				Body:      body,
				Text:      true,
			},
		}
		if err := d.Append(r.Context(), ev); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": string(ev.ID), "status": "queued"})
	})

	// Read-back is served from the store, so an SDK that writes then fetches
	// sees its own write (provider rule 5).
	mux.HandleFunc("GET /fake/v1/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		ev, err := d.Store.Get(r.Context(), event.ID(r.PathValue("id")))
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ev.Payload)
	})
}

// LineProvider is a ListenerProvider: it owns a TCP port and records one event
// per line, the shape an SMTP or FTP provider has.
type LineProvider struct {
	plugin string
	// addrCh publishes the bound address, since tests use port 0.
	addrCh chan string
}

func (l *LineProvider) Name() string   { return "line" }
func (l *LineProvider) Plugin() string { return l.plugin }
func (l *LineProvider) Description() string {
	return "A line-oriented TCP listener used to prove that providers owning their own port are supervised and shut down cleanly."
}
func (l *LineProvider) Endpoints() []plugin.Endpoint { return nil }

func (l *LineProvider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{{
		Title: "Send a line over TCP",
		Lang:  "bash",
		Code:  `echo 'hello from tommy' | nc {{.Host}} {{.Port "fake" "line"}}`,
	}}
}

func (l *LineProvider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	// Nothing: this provider speaks its own protocol on its own port.
}

// ListenPort implements plugin.PortProvider. Unlike a shipped provider this
// one has no well-known default: a test fixture that grabbed a fixed port
// would collide with whatever else is running, so an unconfigured LineProvider
// is ephemeral and says so by reporting port 0.
func (l *LineProvider) ListenPort(pc plugin.ProviderConfig) plugin.ListenPort {
	return plugin.ListenPort{Port: pc.Int("port", 0), Network: "tcp"}
}

// Addr blocks until the listener is bound and returns its address. The channel
// is created up front rather than lazily, so Addr and Listen never race.
func (l *LineProvider) Addr(timeout time.Duration) (string, error) {
	select {
	case addr := <-l.addrCh:
		// Put it back so several callers can read it.
		select {
		case l.addrCh <- addr:
		default:
		}
		return addr, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("fakeplugin: listener did not bind within %s", timeout)
	}
}

var (
	_ plugin.ListenerProvider    = (*LineProvider)(nil)
	_ plugin.AddressableProvider = (*LineProvider)(nil)
	_ plugin.PortProvider        = (*LineProvider)(nil)
)

// Listen reads its port from the provider config, exactly as an FTP or SMTP
// provider would, and returns when ctx is done.
func (l *LineProvider) Listen(ctx context.Context, d plugin.Deps) error {
	d = d.Normalize()

	addr := fmt.Sprintf("127.0.0.1:%d", d.Config.Int("port", 0))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("fake line listener on %s: %w", addr, err)
	}
	defer func() { _ = ln.Close() }()

	select {
	case l.addrCh <- ln.Addr().String():
	default:
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go l.handle(ctx, d, conn)
	}
}

func (l *LineProvider) handle(ctx context.Context, d plugin.Deps, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		ev := &event.Event{
			Plugin:   l.plugin,
			Provider: l.Name(),
			Type:     l.plugin + ".line",
			Summary:  event.Summary{Title: firstLine(line), Snippet: line},
			Payload:  &Message{Text: line},
			Raw: event.Raw{
				Transport: "tcp",
				PeerAddr:  conn.RemoteAddr().String(),
				Body:      []byte(line),
				Text:      true,
			},
		}
		if err := d.Append(ctx, ev); err != nil {
			d.Logger.Warn("append failed", "err", err)
			return
		}
		_, _ = io.WriteString(conn, "OK "+string(ev.ID)+"\n")
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
