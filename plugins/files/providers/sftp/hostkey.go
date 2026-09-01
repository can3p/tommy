package sftp

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// HostKey is a loaded SSH host key together with where it came from.
//
// The whole point of this type is the Created flag and the Fingerprint: an SSH
// client remembers the key of every host it has ever talked to, so a server
// that generates a fresh one on each boot makes every second connection fail
// with the alarming REMOTE HOST IDENTIFICATION HAS CHANGED banner - and the
// person debugging it blames tommy, not their known_hosts file. The key is
// therefore written to disk on first use and read back forever after, and the
// fingerprint is logged at startup so it can be compared against what the
// client reports.
type HostKey struct {
	Signer ssh.Signer
	// Path is the file the key lives in.
	Path string
	// Created is true when this run generated the key rather than loading it.
	Created bool
}

// Fingerprint is the SHA256 fingerprint OpenSSH prints, in the same form, so a
// line from the log can be compared with what the client showed.
func (k HostKey) Fingerprint() string {
	if k.Signer == nil {
		return ""
	}
	return ssh.FingerprintSHA256(k.Signer.PublicKey())
}

// AuthorizedKey is the key's public half in authorized_keys form, which is what
// goes into a known_hosts entry.
func (k HostKey) AuthorizedKey() string {
	if k.Signer == nil {
		return ""
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k.Signer.PublicKey())))
}

// DefaultHostKeyPath is where the key lives when the configuration says
// nothing: alongside whatever else tommy keeps in the user's config directory.
// It falls back to the temporary directory only when there is no config
// directory at all (no HOME), because a key that cannot be written anywhere is
// a key that changes on every restart - the exact failure this file exists to
// prevent.
func DefaultHostKeyPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.TempDir(), "tommy-config")
	}
	return filepath.Join(dir, "tommy", DefaultHostKeyName)
}

// ExpandPath resolves a leading ~ against the user's home directory, which is
// how the path is written in tommy.toml. Anything else is returned untouched,
// including a relative path: the process working directory is a legitimate
// place to keep a throwaway key.
func ExpandPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// LoadOrCreateHostKey returns the host key at path, generating and persisting a
// new ed25519 key the first time.
//
// The file is created with 0600 through O_EXCL, so two tommys racing on the
// same path end up agreeing on one key rather than overwriting each other, and
// the key is never left world-readable. A missing parent directory is created
// with 0700.
//
// A file that exists but cannot be parsed is an error naming the path rather
// than a silent regeneration: replacing a key the clients already trust is
// exactly the outcome that must never happen by accident.
func LoadOrCreateHostKey(path string) (HostKey, error) {
	path = ExpandPath(path)
	if path == "" {
		return HostKey{}, errors.New("sftp: host_key_path is empty")
	}

	if signer, err := readHostKey(path); err == nil {
		return HostKey{Signer: signer, Path: path}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return HostKey{}, err
	}

	signer, err := writeHostKey(path)
	if err == nil {
		return HostKey{Signer: signer, Path: path, Created: true}, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return HostKey{}, err
	}

	// Somebody else won the race between our read and our write. Their key is
	// as good as ours would have been, so adopt it.
	signer, err = readHostKey(path)
	if err != nil {
		return HostKey{}, err
	}
	return HostKey{Signer: signer, Path: path}, nil
}

func readHostKey(path string) (ssh.Signer, error) {
	pemBytes, err := os.ReadFile(path) // #nosec G304 - the path is operator configuration
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("sftp: host key %s is not a usable private key (delete it to have a new one generated): %w", path, err)
	}
	return signer, nil
}

func writeHostKey(path string) (ssh.Signer, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("sftp: create host key directory %s: %w", dir, err)
		}
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sftp: generate host key: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "tommy sftp host key")
	if err != nil {
		return nil, fmt.Errorf("sftp: encode host key: %w", err)
	}

	// O_EXCL rather than a plain create: it makes the write lose cleanly
	// against a concurrent one instead of clobbering a key another process has
	// already handed out, and it refuses to follow a symlink planted at path.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(pem.EncodeToMemory(block)); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("sftp: write host key %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("sftp: write host key %s: %w", path, err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("sftp: host key: %w", err)
	}
	return signer, nil
}

// hostKeyPermsWarning returns a warning when the key file is readable by
// anyone but its owner, which is worth saying out loud even though nothing in
// tommy refuses to start over it.
func hostKeyPermsWarning(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Sprintf("host key %s is mode %#o; 0600 is expected", path, perm)
	}
	return ""
}
