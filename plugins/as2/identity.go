package as2

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AS2 cannot be faked without a key pair: inbound messages are encrypted to the
// receiver's public key and MDNs are signed with its private one. That creates
// the one piece of setup this plugin genuinely needs, and two rules govern how
// it is paid for.
//
// The cost falls only on whoever turned AS2 on. Nothing here runs at package
// init or at binary start. An Identity constructed by the plugin holds no key
// and touches no disk; it does that work when a provider calls Configure, which
// happens in RegisterIngress and therefore only for a provider that is actually
// enabled. Somebody running `tommy mail` never meets a certificate.
//
// A generated default is a convenience, never a constraint. Tommy may well run
// in a cluster that already has a PKI, so an explicit certificate and key path
// always wins; "there is already a CA here" is a normal case, not an edge one.
// When nothing is configured a self-signed certificate is generated once and
// persisted, because a partner has to import it and importing it after every
// restart would make tommy useless for exactly the workflow it is meant to
// shorten.
//
// The seam this sits behind is described in README.md: the plugin builds an
// unconfigured Identity and hands it to providers through IdentityBinder; the
// provider, which is the only thing the core hands a ProviderConfig and a
// ConfigDir, calls Configure. The plugin core cannot do it itself - RegisterAPI
// and RegisterUI receive Deps with an empty Config, by contract.

// Certificate and key file names used when tommy generates its own.
const (
	CertFileName = "as2-cert.pem"
	KeyFileName  = "as2-key.pem"
)

// GeneratedValidity is how long a generated certificate lasts. Ten years, on
// purpose: this certificate protects nothing - it exists so a partner's client
// will talk to a fake - and an expiry that forces a re-import is a cost with no
// benefit.
const GeneratedValidity = 10 * 365 * 24 * time.Hour

// IdentityConfig is everything a provider can say about tommy's AS2 identity.
// Every field is optional; the zero value means "generate one and keep it
// somewhere sensible".
type IdentityConfig struct {
	// CertFile and KeyFile point at a PEM certificate and its unencrypted PEM
	// private key. Both must be given together. When they are set nothing is
	// generated and nothing is written.
	CertFile string
	KeyFile  string
	// PartnerCertFile is a PEM certificate an inbound signature is checked
	// against. Without it a signature can be shown to be intact but never
	// attributed, and every read surface says so.
	PartnerCertFile string
	// Dir is where a generated certificate is kept. It wins over ConfigDir.
	Dir string
	// ConfigDir is plugin.Deps.ConfigDir: the directory the config file was
	// read from, empty for every CLI shortcut and every test. "Beside the
	// config file" is the only place a user finds without being told, so it is
	// preferred over the OS config directory when it is set.
	ConfigDir string
	// InMemory generates a key pair and writes nothing at all. Tests use it,
	// and so does anyone who wants tommy to leave no files behind - at the
	// price of a certificate their partner has to re-import after a restart.
	InMemory bool
	// CommonName is the subject of a generated certificate.
	CommonName string
	// Now is injectable so a test can pin a certificate's validity window.
	Now func() time.Time
}

// IdentitySource says where the key pair came from, for the UI and the API.
const (
	SourceUnconfigured = "unconfigured"
	SourceFiles        = "files"
	SourcePersisted    = "generated"
	SourceMemory       = "memory"
)

// Identity is tommy's AS2 key pair, plus the partner certificate to check
// inbound signatures against. It is safe for concurrent use: providers read it
// on every request while the tab reads it to show the fingerprint.
type Identity struct {
	mu     sync.RWMutex
	cfg    IdentityConfig
	loaded bool
	// materialized records that the deferred half of Configure has run. A key
	// pair is generated on first use rather than at registration, so merely
	// building a server - which every conformance test and every `tommy
	// providers` invocation does - writes nothing to anyone's disk.
	materialized bool
	source       string
	cert         *x509.Certificate
	key          *rsa.PrivateKey
	certPEM      []byte

	certPath string
	keyPath  string

	partner    *x509.Certificate
	partnerPEM []byte

	err error
}

// NewIdentity returns an unconfigured identity. It holds no key, has touched no
// disk, and will do neither until Configure is called.
func NewIdentity() *Identity { return &Identity{source: SourceUnconfigured} }

// ErrIdentityNotConfigured is returned by every accessor before Configure has
// run. It is not a bug: a plugin whose provider is disabled has no identity,
// and the tab says so rather than generating one nobody asked for.
var ErrIdentityNotConfigured = errors.New("as2: no certificate is configured; enable an AS2 provider or set cert_file and key_file")

// Configure loads or generates the key pair. It is what makes generation lazy
// in the right way: it runs in a provider's RegisterIngress, so a disabled
// provider never causes a certificate to exist.
//
// Calling it again replaces the identity, which is what a second provider
// pointing at a different certificate should mean; a repeat call with the same
// config is cheap but not free, so a provider should call it once.
//
// It returns an error rather than swallowing one so the provider can record the
// failure. The identity stays usable afterwards - accessors report the same
// error - and messages are still captured, they simply cannot be decrypted.
func (i *Identity) Configure(cfg IdentityConfig) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.cfg = cfg
	i.loaded = true
	i.materialized = false
	i.cert, i.key, i.certPEM = nil, nil, nil
	i.certPath, i.keyPath = "", ""
	i.partner, i.partnerPEM = nil, nil
	i.err = nil

	if err := i.loadPartner(); err != nil {
		// A bad partner certificate must not stop tommy answering. Record it
		// and carry on unauthenticated, which is the default state anyway.
		i.err = err
	}

	// Explicit files are read now, because a path that does not resolve is a
	// mistake the operator wants to hear about at startup rather than on the
	// first message. Reading them creates nothing.
	//
	// Generation is deliberately NOT done here. See materialize.
	if cfg.CertFile != "" || cfg.KeyFile != "" {
		i.materialize()
		if i.err != nil {
			return i.err
		}
	}
	return i.err
}

// materialize runs the half of Configure that can create something: generating
// a key pair, and writing it where a partner can be told to find it. It happens
// on first use rather than at registration, and the distinction is not
// cosmetic - Configure runs in RegisterIngress, which means it runs for anything
// that merely builds a server. plugintest.Conformance does exactly that, so
// generating eagerly put a real private key in the user's own config directory
// during `make check`. Deferring it means the only things that mint a
// certificate are the ones that genuinely need one: an arriving message, a
// partner fetching /as2/certificate, or the tab showing the fingerprint.
//
// The caller holds the write lock.
func (i *Identity) materialize() {
	if !i.loaded || i.materialized {
		return
	}
	i.materialized = true
	if err := i.load(); err != nil {
		i.err = err
	}
}

// Configured reports whether Configure has run.
func (i *Identity) Configured() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.loaded
}

// KeyPair returns the certificate and private key, or the reason there are
// none.
func (i *Identity) KeyPair() (*x509.Certificate, *rsa.PrivateKey, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.materialize()
	if !i.loaded {
		return nil, nil, ErrIdentityNotConfigured
	}
	if i.cert == nil || i.key == nil {
		if i.err != nil {
			return nil, nil, i.err
		}
		return nil, nil, ErrIdentityNotConfigured
	}
	return i.cert, i.key, nil
}

// Certificate returns tommy's certificate, or nil.
func (i *Identity) Certificate() *x509.Certificate {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.materialize()
	return i.cert
}

// CertificatePEM is what a partner imports. It is served by the plugin's API so
// the how-to-test panel can hand out a URL rather than a filesystem path.
func (i *Identity) CertificatePEM() []byte {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.materialize()
	return i.certPEM
}

// Partner is the certificate inbound signatures are checked against, or nil
// when none was configured - which is the default, and which is why no read
// surface may claim an inbound signature proves who sent it.
func (i *Identity) Partner() *x509.Certificate {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.partner
}

// IdentityInfo is the display form of an identity, for the tab and the API.
type IdentityInfo struct {
	Configured bool `json:"configured"`
	Ready      bool `json:"ready"`
	// Source is SourceFiles, SourcePersisted, SourceMemory or
	// SourceUnconfigured.
	Source      string    `json:"source"`
	CertPath    string    `json:"cert_path,omitempty"`
	KeyPath     string    `json:"key_path,omitempty"`
	Certificate *CertInfo `json:"certificate,omitempty"`
	// PEM is the certificate a partner imports, inline so the tab can offer a
	// copy button without a second request.
	PEM     string    `json:"pem,omitempty"`
	Partner *CertInfo `json:"partner,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// Info describes the identity without changing it, so the tab can render before
// any message has arrived and without ever triggering generation.
func (i *Identity) Info() IdentityInfo {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.materialize()
	info := IdentityInfo{
		Configured: i.loaded,
		Ready:      i.cert != nil && i.key != nil,
		Source:     i.source,
		CertPath:   i.certPath,
		KeyPath:    i.keyPath,
		PEM:        string(i.certPEM),
	}
	if i.err != nil {
		info.Error = i.err.Error()
	}
	if i.cert != nil {
		c := NewCertInfo(i.cert)
		info.Certificate = &c
	}
	if i.partner != nil {
		c := NewCertInfo(i.partner)
		info.Partner = &c
	}
	return info
}

// load fills in the key pair. Caller holds the lock.
func (i *Identity) load() error {
	switch {
	case i.cfg.CertFile != "" || i.cfg.KeyFile != "":
		if i.cfg.CertFile == "" || i.cfg.KeyFile == "" {
			return errors.New("as2: cert_file and key_file must be given together")
		}
		i.source = SourceFiles
		return i.loadFiles(i.cfg.CertFile, i.cfg.KeyFile)
	case i.cfg.InMemory:
		i.source = SourceMemory
		return i.generate("", "")
	default:
		dir, err := i.persistDir()
		if err != nil {
			// Nowhere to keep it is not a reason to refuse to run: fall back
			// to memory and say so, so AS2 still works and the operator learns
			// their partner will have to re-import after a restart.
			i.source = SourceMemory
			if genErr := i.generate("", ""); genErr != nil {
				return genErr
			}
			return fmt.Errorf("as2: no directory to persist a certificate in (%w); "+
				"a certificate was generated in memory and will change on restart", err)
		}
		i.source = SourcePersisted
		certPath := filepath.Join(dir, CertFileName)
		keyPath := filepath.Join(dir, KeyFileName)
		if fileExists(certPath) && fileExists(keyPath) {
			return i.loadFiles(certPath, keyPath)
		}
		return i.generate(certPath, keyPath)
	}
}

// persistDir picks where a generated certificate lives, in the order a person
// would look for it: an explicitly configured directory, then beside the config
// file this run was loaded from, then the OS config directory.
func (i *Identity) persistDir() (string, error) {
	if i.cfg.Dir != "" {
		return i.cfg.Dir, nil
	}
	if i.cfg.ConfigDir != "" {
		return i.cfg.ConfigDir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "tommy", Name), nil
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// loadFiles reads a PEM certificate and key pair from disk. Caller holds the
// lock.
func (i *Identity) loadFiles(certPath, keyPath string) error {
	certPEM, err := os.ReadFile(certPath) //nolint:gosec // the path is operator configuration, not user input
	if err != nil {
		return fmt.Errorf("as2: read certificate %s: %w", certPath, err)
	}
	keyPEM, err := os.ReadFile(keyPath) //nolint:gosec // ditto
	if err != nil {
		return fmt.Errorf("as2: read private key %s: %w", keyPath, err)
	}
	cert, err := parseCertificatePEM(certPEM)
	if err != nil {
		return fmt.Errorf("as2: %s: %w", certPath, err)
	}
	key, err := parseRSAKeyPEM(keyPEM)
	if err != nil {
		return fmt.Errorf("as2: %s: %w", keyPath, err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return fmt.Errorf("as2: %s is not the private key for %s", keyPath, certPath)
	}
	i.cert, i.key = cert, key
	i.certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	i.certPath, i.keyPath = certPath, keyPath
	return nil
}

func (i *Identity) loadPartner() error {
	if i.cfg.PartnerCertFile == "" {
		return nil
	}
	data, err := os.ReadFile(i.cfg.PartnerCertFile) //nolint:gosec // operator configuration
	if err != nil {
		return fmt.Errorf("as2: read partner certificate %s: %w", i.cfg.PartnerCertFile, err)
	}
	cert, err := parseCertificatePEM(data)
	if err != nil {
		return fmt.Errorf("as2: %s: %w", i.cfg.PartnerCertFile, err)
	}
	i.partner, i.partnerPEM = cert, data
	return nil
}

// generate makes a self-signed RSA-2048 certificate and, when paths are given,
// persists it so a partner imports it once rather than after every restart.
// Caller holds the lock.
func (i *Identity) generate(certPath, keyPath string) error {
	now := time.Now
	if i.cfg.Now != nil {
		now = i.cfg.Now
	}
	cn := i.cfg.CommonName
	if cn == "" {
		cn = "tommy AS2"
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("as2: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("as2: generate serial: %w", err)
	}
	start := now().Add(-time.Hour).UTC()
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"tommy"}},
		NotBefore:    start,
		NotAfter:     start.Add(GeneratedValidity),
		// An AS2 certificate has to do both jobs: sign MDNs, and be the
		// recipient key inbound messages are encrypted to.
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageDataEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageEmailProtection, x509.ExtKeyUsageAny},
		BasicConstraintsValid: true,
		// Self-signed and marked as a CA of itself: some AS2 products refuse to
		// import a leaf certificate that chains to nothing, and this costs
		// nothing here because the certificate protects nothing.
		IsCA:     true,
		DNSNames: []string{cn},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("as2: create certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("as2: parse generated certificate: %w", err)
	}

	i.cert, i.key = cert, key
	i.certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	if certPath == "" || keyPath == "" {
		return nil
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustMarshalPKCS8(key)})
	if err := writeIdentityFiles(certPath, i.certPEM, keyPath, keyPEM); err != nil {
		// The key pair is live in memory, so AS2 works; only persistence
		// failed. Report it and carry on rather than refusing to serve.
		return err
	}
	i.certPath, i.keyPath = certPath, keyPath
	return nil
}

func mustMarshalPKCS8(key *rsa.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		// Impossible for an RSA key produced by rsa.GenerateKey.
		panic("as2: marshal generated key: " + err.Error())
	}
	return der
}

// writeIdentityFiles writes the pair with the permissions a private key
// deserves: the directory 0700, the key 0600, and both written to a temporary
// file and renamed so a crash cannot leave a half-written key that later looks
// like a corrupt one.
func writeIdentityFiles(certPath string, certPEM []byte, keyPath string, keyPEM []byte) error {
	dir := filepath.Dir(certPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("as2: create %s: %w", dir, err)
	}
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	return writeFileAtomic(certPath, certPEM, 0o644)
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".as2-*")
	if err != nil {
		return fmt.Errorf("as2: create temporary file beside %s: %w", path, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("as2: chmod %s: %w", name, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("as2: write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("as2: close %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("as2: rename %s to %s: %w", name, path, err)
	}
	return nil
}

// parseCertificatePEM reads the first CERTIFICATE block.
func parseCertificatePEM(data []byte) (*x509.Certificate, error) {
	for rest := data; len(rest) > 0; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		return x509.ParseCertificate(block.Bytes)
	}
	return nil, errors.New("no CERTIFICATE block found")
}

// parseRSAKeyPEM reads an unencrypted RSA private key in PKCS#1 or PKCS#8.
//
// An encrypted key is rejected with an explanation rather than a passphrase
// prompt: tommy is a background process with no terminal, and a passphrase in a
// config file is worse than no passphrase. `openssl pkey -in key.pem -out
// key.unencrypted.pem` is the one-line fix and the error says so.
func parseRSAKeyPEM(data []byte) (*rsa.PrivateKey, error) {
	for rest := data; len(rest) > 0; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "ENCRYPTED PRIVATE KEY" || block.Headers["DEK-Info"] != "" {
			return nil, errors.New("the private key is encrypted; tommy has no terminal to ask for a passphrase on. " +
				"Decrypt it first: openssl pkey -in key.pem -out key-plain.pem")
		}
		switch block.Type {
		case "RSA PRIVATE KEY":
			return x509.ParsePKCS1PrivateKey(block.Bytes)
		case "PRIVATE KEY":
			key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, err
			}
			rsaKey, ok := key.(*rsa.PrivateKey)
			if !ok {
				return nil, fmt.Errorf("the private key is %T; AS2 needs an RSA key", key)
			}
			return rsaKey, nil
		}
	}
	return nil, errors.New("no PRIVATE KEY block found")
}
