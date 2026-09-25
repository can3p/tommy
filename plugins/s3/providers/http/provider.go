// Package http provides a path-style, S3-compatible HTTP listener.
package http

import (
	"context"
	"errors"
	"fmt"
	"net"
	stdhttp "net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/s3"
)

const (
	ProviderName = "http"
	DefaultPort  = 9000
	DefaultBind  = "127.0.0.1"

	DefaultMaxObjectBytes    = 64 << 20
	DefaultMaxXMLBytes       = 1 << 20
	DefaultMaxHeaderBytes    = 1 << 20
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultReadTimeout       = 5 * time.Minute
	DefaultWriteTimeout      = 5 * time.Minute
	DefaultIdleTimeout       = 2 * time.Minute
	DefaultShutdownTimeout   = 10 * time.Second
)

// Config is the [plugins.s3.providers.http] section. The catalog limits are
// useful to the provider's private fallback store; a plugin-bound shared store
// remains authoritative, while MaxObjectBytes still bounds every wire body.
type Config struct {
	Bind              string
	Port              int
	Buckets           []string
	Limits            s3.Limits
	MaxObjectBytes    int64
	MaxXMLBytes       int64
	MaxHeaderBytes    int
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

func LoadConfig(pc plugin.ProviderConfig) (Config, error) {
	var raw struct {
		Buckets []string `toml:"buckets"`
	}
	if err := pc.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("s3 http: %w", err)
	}
	buckets := make([]string, 0, len(raw.Buckets))
	seen := make(map[string]struct{}, len(raw.Buckets))
	for i, name := range raw.Buckets {
		name = strings.TrimSpace(name)
		if name == "" {
			return Config{}, fmt.Errorf("s3 http: buckets[%d] is empty", i)
		}
		if _, ok := seen[name]; ok {
			return Config{}, fmt.Errorf("s3 http: bucket %q is configured more than once", name)
		}
		seen[name] = struct{}{}
		buckets = append(buckets, name)
	}

	d := s3.DefaultLimits
	maxObjectBytes := int64(positive(pc.Int("max_object_bytes", DefaultMaxObjectBytes), DefaultMaxObjectBytes))
	return Config{
		Bind:    pc.String("bind", DefaultBind),
		Port:    pc.Int("port", DefaultPort),
		Buckets: buckets,
		Limits: s3.Limits{
			MaxBuckets:       positive(pc.Int("max_buckets", d.MaxBuckets), d.MaxBuckets),
			MaxObjects:       positive(pc.Int("max_objects", d.MaxObjects), d.MaxObjects),
			MaxActiveUploads: positive(pc.Int("max_active_uploads", d.MaxActiveUploads), d.MaxActiveUploads),
			MaxParts:         positive(pc.Int("max_parts", d.MaxParts), d.MaxParts),
			MaxBucketBytes:   positive(pc.Int("max_bucket_bytes", d.MaxBucketBytes), d.MaxBucketBytes),
			MaxKeyBytes:      positive(pc.Int("max_key_bytes", d.MaxKeyBytes), d.MaxKeyBytes),
			MaxMetadataBytes: positive(pc.Int("max_metadata_bytes", d.MaxMetadataBytes), d.MaxMetadataBytes),
			MaxObjectBytes:   maxObjectBytes,
		},
		MaxObjectBytes:    maxObjectBytes,
		MaxXMLBytes:       int64(positive(pc.Int("max_xml_bytes", DefaultMaxXMLBytes), DefaultMaxXMLBytes)),
		MaxHeaderBytes:    positive(pc.Int("max_header_bytes", DefaultMaxHeaderBytes), DefaultMaxHeaderBytes),
		ReadHeaderTimeout: seconds(pc, "read_header_timeout", DefaultReadHeaderTimeout),
		ReadTimeout:       seconds(pc, "read_timeout", DefaultReadTimeout),
		WriteTimeout:      seconds(pc, "write_timeout", DefaultWriteTimeout),
		IdleTimeout:       seconds(pc, "idle_timeout", DefaultIdleTimeout),
		ShutdownTimeout:   seconds(pc, "shutdown_timeout", DefaultShutdownTimeout),
	}, nil
}

func positive(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

func seconds(pc plugin.ProviderConfig, key string, fallback time.Duration) time.Duration {
	return time.Duration(positive(pc.Int(key, int(fallback/time.Second)), int(fallback/time.Second))) * time.Second
}

func (c Config) ListenAddr() string { return net.JoinHostPort(c.Bind, strconv.Itoa(c.Port)) }

// Provider owns the dedicated S3 HTTP listener.
type Provider struct {
	mu      sync.RWMutex
	addr    string
	bound   chan struct{}
	store   *s3.Store
	handler stdhttp.Handler
}

var (
	_ plugin.Provider            = (*Provider)(nil)
	_ plugin.ListenerProvider    = (*Provider)(nil)
	_ plugin.AddressableProvider = (*Provider)(nil)
	_ plugin.PortProvider        = (*Provider)(nil)
	_ s3.StoreBinder             = (*Provider)(nil)
)

func New() *Provider { return &Provider{bound: make(chan struct{})} }

func (p *Provider) Name() string   { return ProviderName }
func (p *Provider) Plugin() string { return s3.PluginName }
func (p *Provider) Description() string {
	return "A dedicated path-style S3-compatible HTTP server for local SDK and transfer-manager traffic. " +
		"It keeps buckets, objects, metadata, checksums and multipart uploads in the shared in-memory S3 catalog and captures successful mutations as inspectable events."
}
func (p *Provider) Endpoints() []plugin.Endpoint            { return nil }
func (p *Provider) RegisterIngress(plugin.Mux, plugin.Deps) {}
func (p *Provider) ListenPort(pc plugin.ProviderConfig) plugin.ListenPort {
	return plugin.ListenPort{Port: pc.Int("port", DefaultPort), Network: "tcp"}
}

func (p *Provider) Snippets() []plugin.Snippet {
	endpoint := `http://{{.Addr "s3" "http"}}`
	return []plugin.Snippet{
		{
			Title: "Use the AWS CLI with path-style S3",
			Lang:  "bash",
			Code: `export AWS_ACCESS_KEY_ID=tommy AWS_SECRET_ACCESS_KEY=tommy AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url ` + endpoint + ` s3api create-bucket --bucket example
printf 'captured by tommy\n' > /tmp/tommy-s3.txt
aws --endpoint-url ` + endpoint + ` s3 cp /tmp/tommy-s3.txt s3://example/hello.txt
aws --endpoint-url ` + endpoint + ` s3 cp s3://example/hello.txt -`,
		},
		{
			Title: "Use the official AWS SDK for Go v2 in path style",
			Lang:  "go",
			Code: `cfg, _ := config.LoadDefaultConfig(context.Background(),
    config.WithRegion("us-east-1"),
    config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("tommy", "tommy", "")),
)
client := s3.NewFromConfig(cfg, func(o *s3.Options) {
    o.BaseEndpoint = aws.String("` + endpoint + `")
    o.UsePathStyle = true
})
_, _ = client.PutObject(context.Background(), &s3.PutObjectInput{
    Bucket: aws.String("example"), Key: aws.String("hello.txt"),
    Body: strings.NewReader("captured by tommy\n"),
})`,
		},
	}
}

func (p *Provider) BindStore(store *s3.Store) {
	if store == nil {
		return
	}
	p.mu.Lock()
	p.store = store
	p.mu.Unlock()
}

func (p *Provider) Addr(timeout time.Duration) (string, error) {
	select {
	case <-p.bound:
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.addr, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("s3 http: listener did not bind within %s", timeout)
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

// ServeHTTP delegates directly to the S3 handler. It deliberately does not use
// ServeMux, whose path canonicalisation would rewrite valid S3 keys.
func (p *Provider) ServeHTTP(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	p.mu.RLock()
	h := p.handler
	p.mu.RUnlock()
	if h == nil {
		stdhttp.Error(w, "S3 listener is not running", stdhttp.StatusServiceUnavailable)
		return
	}
	h.ServeHTTP(w, r)
}

func (p *Provider) Listen(ctx context.Context, d plugin.Deps) error {
	d = d.Normalize().WithLogger("plugin", s3.PluginName, "provider", ProviderName)
	cfg, err := LoadConfig(d.Config)
	if err != nil {
		return err
	}

	p.mu.Lock()
	store := p.store
	if store == nil {
		store = s3.NewStore(s3.WithLimits(cfg.Limits))
		p.store = store
	}
	p.mu.Unlock()
	validation := s3.NewStore(s3.WithLimits(cfg.Limits))
	for _, name := range cfg.Buckets {
		if _, err := validation.CreateBucket(name); err != nil {
			return fmt.Errorf("s3 http: invalid configured bucket %q: %w", name, err)
		}
	}
	for _, name := range cfg.Buckets {
		if _, err := store.CreateBucket(name); err != nil && !errors.Is(err, s3.ErrBucketExists) {
			return fmt.Errorf("s3 http: create configured bucket %q: %w", name, err)
		}
	}
	p.mu.Lock()
	p.handler = newHandler(store, d, cfg)
	p.mu.Unlock()

	ln, err := net.Listen("tcp", cfg.ListenAddr())
	if err != nil {
		return fmt.Errorf("s3 http listener on %s: %w", cfg.ListenAddr(), err)
	}
	p.setAddr(ln.Addr().String())
	d.Logger.Info("s3 http listening", "addr", ln.Addr().String())

	srv := &stdhttp.Server{
		Handler:           p,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			_ = srv.Close()
		}
	}()

	err = srv.Serve(ln)
	if ctx.Err() != nil {
		<-shutdownDone
	}
	if err != nil && !errors.Is(err, stdhttp.ErrServerClosed) {
		return fmt.Errorf("s3 http serve: %w", err)
	}
	return nil
}
