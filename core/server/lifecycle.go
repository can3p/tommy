// Package server boots tommy: it wires the store, the registry, the ingress mux
// and the HTTP listeners together, starts every listener provider, and shuts
// the lot down gracefully.
//
// There is exactly one bootstrap. `tommy serve --config` and the single-plugin
// CLI shortcuts both build a config.Config and end up here.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/can3p/tommy/core/blob"
	blobmem "github.com/can3p/tommy/core/blob/memory"
	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/server/api"
	"github.com/can3p/tommy/core/server/ingress"
	"github.com/can3p/tommy/core/server/ui"
	"github.com/can3p/tommy/core/store"
	storemem "github.com/can3p/tommy/core/store/memory"
)

// DefaultShutdownTimeout bounds a graceful shutdown.
const DefaultShutdownTimeout = 5 * time.Second

// Options configures a Server.
type Options struct {
	Config  *config.Config
	Plugins []plugin.Plugin
	Logger  *slog.Logger
	Version string

	// Store and Blobs override the default in-memory implementations.
	Store store.Store
	Blobs blob.BlobStore

	// Now and NewID are injectable so tests are deterministic.
	Now   func() time.Time
	NewID func() string

	// ShutdownTimeout bounds graceful shutdown; zero means the default.
	ShutdownTimeout time.Duration
}

// Addrs are the resolved addresses of the core listeners. With port 0 in the
// config these are only known after New has bound them, which is what lets the
// test harness run many instances at once.
type Addrs struct {
	UI      string
	API     string
	Ingress string
}

// Server is a running tommy.
type Server struct {
	opts    Options
	cfg     *config.Config
	log     *slog.Logger
	reg     *plugin.Registry
	store   store.Store
	blobs   blob.BlobStore
	ingress *ingress.Ingress
	api     *api.API
	ui      *ui.UI

	addrs   Addrs
	snippet plugin.SnippetCtx

	servers   []*httpListener
	listeners []plugin.Ref

	mu       sync.Mutex
	started  bool
	stopped  bool
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	errsMu   sync.Mutex
	runErrs  []error
	baseDeps plugin.Deps
}

type httpListener struct {
	name string
	ln   net.Listener
	srv  *http.Server
}

// New builds a server and binds the core listeners, so Addrs is valid before
// anything is served. It does not accept connections until Start.
func New(opts Options) (*Server, error) {
	cfg := opts.Config
	if cfg == nil {
		cfg = config.Default()
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	s := &Server{opts: opts, cfg: cfg, log: opts.Logger}

	s.store = opts.Store
	if s.store == nil {
		memOpts := []storemem.Option{}
		for name, capacity := range cfg.Storage.PluginCapacity {
			memOpts = append(memOpts, storemem.WithPluginCapacity(name, capacity))
		}
		if opts.Now != nil {
			memOpts = append(memOpts, storemem.WithClock(opts.Now))
		}
		if opts.NewID != nil {
			memOpts = append(memOpts, storemem.WithIDFunc(opts.NewID))
		}
		s.store = storemem.New(cfg.Storage.Capacity, memOpts...)
	}
	s.blobs = opts.Blobs
	if s.blobs == nil {
		s.blobs = blobmem.New(cfg.Storage.BlobLimit.Bytes())
	}

	reg, err := plugin.New(cfg, opts.Plugins...)
	if err != nil {
		return nil, err
	}
	s.reg = reg

	s.baseDeps = plugin.Deps{
		Store:  s.store,
		Blobs:  s.blobs,
		Logger: s.log,
		Now:    opts.Now,
		NewID:  opts.NewID,
	}.Normalize()

	if err := s.bind(); err != nil {
		s.closeListeners()
		return nil, err
	}
	s.snippet = s.buildSnippetCtx()

	if err := s.build(); err != nil {
		s.closeListeners()
		return nil, err
	}
	return s, nil
}

// bind opens the core listeners and records the resolved addresses.
func (s *Server) bind() error {
	uiLn, err := net.Listen("tcp", s.cfg.UIAddr())
	if err != nil {
		return fmt.Errorf("ui listener on %s: %w", s.cfg.UIAddr(), err)
	}
	s.addrs.UI = uiLn.Addr().String()
	s.servers = append(s.servers, &httpListener{name: "ui", ln: uiLn})

	if s.cfg.APISharesUIListener() {
		s.addrs.API = s.addrs.UI
	} else {
		apiLn, err := net.Listen("tcp", s.cfg.APIAddr())
		if err != nil {
			return fmt.Errorf("api listener on %s: %w", s.cfg.APIAddr(), err)
		}
		s.addrs.API = apiLn.Addr().String()
		s.servers = append(s.servers, &httpListener{name: "api", ln: apiLn})
	}

	if s.cfg.IngressSharesUIListener() {
		s.addrs.Ingress = s.addrs.UI
	} else {
		inLn, err := net.Listen("tcp", s.cfg.IngressAddr())
		if err != nil {
			return fmt.Errorf("ingress listener on %s: %w", s.cfg.IngressAddr(), err)
		}
		s.addrs.Ingress = inLn.Addr().String()
		s.servers = append(s.servers, &httpListener{name: "ingress", ln: inLn})
	}
	return nil
}

// listenerAddrTimeout bounds how long resolving a listener's address may wait
// for it to bind. Once bound the answer is immediate, so this only bites before
// startup finishes or for a listener that never came up.
const listenerAddrTimeout = 250 * time.Millisecond

// buildSnippetCtx resolves the addresses snippets render against.
func (s *Server) buildSnippetCtx() plugin.SnippetCtx {
	ctx := plugin.NewSnippetCtx(s.cfg.Host, s.addrs.UI, s.addrs.API, s.addrs.Ingress)
	for _, ref := range s.reg.ListenerRefs() {
		if addr := s.listenerAddr(ref); addr != "" {
			ctx.SetAddr(ref.Plugin.Name(), ref.Provider.Name(), addr)
		}
	}
	return ctx
}

// listenerAddr reports where a listener provider can actually be reached.
//
// Ask the provider first: one that took an ephemeral port, or that fell back to
// its own default because the configuration named none, is the only thing that
// knows where it ended up. Configuration is the fallback, and on its own it is
// wrong in both of those cases - which is how a snippet ends up telling someone
// to connect to a port nothing is listening on.
func (s *Server) listenerAddr(ref plugin.Ref) string {
	if a, ok := ref.Provider.(plugin.AddressableProvider); ok {
		if addr, err := a.Addr(listenerAddrTimeout); err == nil && addr != "" {
			return addr
		}
	}
	if pc := s.reg.ProviderConfig(ref.Plugin.Name(), ref.Provider.Name()); pc.Port > 0 {
		return net.JoinHostPort(s.cfg.Host, strconv.Itoa(pc.Port))
	}
	return ""
}

func (s *Server) build() error {
	s.ingress = ingress.New(s.log.With("component", "ingress"))
	if err := s.ingress.Mount(s.reg, s.baseDeps); err != nil {
		return err
	}
	s.ingress.SetNotFound(ingress.NotFoundHandler(func() []plugin.PluginInfo {
		info, err := s.reg.Describe(s.SnippetCtx())
		if err != nil {
			return nil
		}
		return info
	}))

	apiSrv, err := api.New(api.Options{
		Store:      s.store,
		Blobs:      s.blobs,
		Registry:   s.reg,
		Deps:       s.baseDeps,
		Logger:     s.log.With("component", "api"),
		Version:    s.opts.Version,
		StartedAt:  time.Now(),
		SnippetCtx: s.SnippetCtx,
	})
	if err != nil {
		return err
	}
	s.api = apiSrv

	apiBase := api.Prefix
	if !s.cfg.APISharesUIListener() {
		apiBase = "http://" + s.addrs.API + api.Prefix
	}
	uiSrv, err := ui.New(ui.Options{
		Store:      s.store,
		Blobs:      s.blobs,
		Registry:   s.reg,
		Deps:       s.baseDeps,
		Logger:     s.log.With("component", "ui"),
		Version:    s.opts.Version,
		APIBase:    apiBase,
		SnippetCtx: s.SnippetCtx,
	})
	if err != nil {
		return err
	}
	s.ui = uiSrv

	// One mux per listener. Sharing a listener is a config decision, so the
	// same handlers get composed differently rather than duplicated.
	uiMux := http.NewServeMux()
	s.ui.Mount(uiMux)
	if s.cfg.APISharesUIListener() {
		s.api.Mount(uiMux)
	}
	if s.cfg.IngressSharesUIListener() {
		uiMux.Handle("/", s.ingress.Handler())
	}

	for _, l := range s.servers {
		var h http.Handler
		switch l.name {
		case "ui":
			h = uiMux
		case "api":
			m := http.NewServeMux()
			s.api.Mount(m)
			m.Handle("GET /{$}", http.RedirectHandler(api.Prefix+"/health", http.StatusFound))
			h = m
		case "ingress":
			h = s.ingress.Handler()
		}
		l.srv = &http.Server{
			Handler:           logRequests(s.log.With("listener", l.name), h),
			ReadHeaderTimeout: 15 * time.Second,
			// No WriteTimeout: SSE responses are long-lived by design.
		}
	}

	s.listeners = s.reg.ListenerRefs()
	return nil
}

func logRequests(log *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Debug("request", "method", r.Method, "path", r.URL.Path, "peer", r.RemoteAddr)
		h.ServeHTTP(w, r)
	})
}

// Start begins serving. It returns as soon as everything is running; use Wait
// or Shutdown to control the lifetime.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("server: already started")
	}
	s.started = true
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	for _, l := range s.servers {
		l := l
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.log.Info("listening", "listener", l.name, "addr", l.ln.Addr().String())
			if err := l.srv.Serve(l.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.recordErr(fmt.Errorf("%s listener: %w", l.name, err))
				cancel()
			}
		}()
	}

	for _, ref := range s.listeners {
		ref := ref
		lp, ok := ref.Provider.(plugin.ListenerProvider)
		if !ok {
			continue
		}
		d := s.reg.DepsFor(s.baseDeps, ref.Plugin.Name(), ref.Provider.Name())
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.log.Info("starting listener provider", "plugin", ref.Plugin.Name(), "provider", ref.Provider.Name())
			if err := lp.Listen(runCtx, d); err != nil && !errors.Is(err, context.Canceled) {
				s.recordErr(fmt.Errorf("%s/%s listener: %w", ref.Plugin.Name(), ref.Provider.Name(), err))
			}
		}()
	}

	// Listener providers bind asynchronously, so pick their addresses up once
	// they are up rather than reporting the empty ones captured at build time.
	if len(s.listeners) > 0 {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.resolveListenerAddrs()
		}()
	}

	return nil
}

func (s *Server) recordErr(err error) {
	s.errsMu.Lock()
	s.runErrs = append(s.runErrs, err)
	s.errsMu.Unlock()
	s.log.Error("server error", "err", err)
}

// Shutdown stops every listener gracefully and waits for them to finish.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	cancel := s.cancel
	s.mu.Unlock()

	if cancel != nil {
		cancel() // tells every ListenerProvider to stop
	}

	timeout := s.opts.ShutdownTimeout
	if timeout <= 0 {
		timeout = DefaultShutdownTimeout
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(ctx, timeout)
	defer cancelShutdown()

	var errs []error
	for _, l := range s.servers {
		if l.srv == nil {
			continue
		}
		if err := l.srv.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", l.name, err))
			_ = l.srv.Close()
		}
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-shutdownCtx.Done():
		errs = append(errs, fmt.Errorf("timed out waiting for listeners after %s", timeout))
	}

	s.errsMu.Lock()
	errs = append(errs, s.runErrs...)
	s.runErrs = nil
	s.errsMu.Unlock()
	return errors.Join(errs...)
}

func (s *Server) closeListeners() {
	for _, l := range s.servers {
		if l.ln != nil {
			_ = l.ln.Close()
		}
	}
}

// Addrs returns the resolved core listener addresses.
func (s *Server) Addrs() Addrs { return s.addrs }

// URLs returns the browser-facing URLs of the core listeners.
func (s *Server) URLs() (uiURL, apiURL, ingressURL string) {
	ctx := s.SnippetCtx()
	return ctx.UIURL + ui.Prefix + "/", ctx.APIURL, ctx.IngressURL
}

// SnippetCtx returns the live addresses snippets render against.
func (s *Server) SnippetCtx() plugin.SnippetCtx {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snippet
}

// resolveListenerAddrs refreshes the listener addresses once the providers have
// bound. They cannot be known when the server is built, because a listener
// provider does not start until Start runs - so without this pass a provider
// that took an ephemeral port, or fell back to its own default, is advertised
// with no address at all.
func (s *Server) resolveListenerAddrs() {
	ctx := s.buildSnippetCtx()
	s.mu.Lock()
	s.snippet = ctx
	s.mu.Unlock()
}

// Store, Blobs and Registry expose the wiring, mostly for tests.
func (s *Server) Store() store.Store         { return s.store }
func (s *Server) Blobs() blob.BlobStore      { return s.blobs }
func (s *Server) Registry() *plugin.Registry { return s.reg }
func (s *Server) Config() *config.Config     { return s.cfg }
func (s *Server) Ingress() *ingress.Ingress  { return s.ingress }
func (s *Server) Describe() ([]plugin.PluginInfo, error) {
	return s.reg.Describe(s.snippet)
}

// Run starts a server and blocks until ctx is done, then shuts it down.
func Run(ctx context.Context, opts Options) error {
	s, err := New(opts)
	if err != nil {
		return err
	}
	if err := s.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	return s.Shutdown(context.WithoutCancel(ctx))
}
