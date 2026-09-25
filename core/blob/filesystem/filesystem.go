// Package filesystem is a blob.BlobStore that keeps each blob in a file of its
// own under one directory, so a persistent plugin's bytes outlive the process.
//
// A blob file is the payload followed by a trailer: the blob.Ref as JSON, its
// length as a big-endian uint32, and an 8-byte magic. Writing the trailer last
// lets Put stream the payload in one pass without knowing its size up front,
// and each file is written to a temporary name and renamed into place, so a
// reader sees either the whole previous blob or the whole new one.
//
// There is no byte cap here. storage.blob_limit bounds what tommy holds in
// memory; what a persistent plugin keeps on disk is bounded by that plugin's
// own limits (object count and size for S3, file count and size for Files).
package filesystem

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
)

const (
	ext           = ".blob"
	tempPrefix    = ".tmp-"
	maxHeaderSize = 1 << 20
)

var magic = [8]byte{'T', 'O', 'M', 'M', 'Y', 'B', 'L', 'B'}

// trailerSize is the fixed part of the trailer: the length word and the magic.
const trailerSize = 4 + len(magic)

// Store is a directory of blob files.
type Store struct {
	dir   string
	newID func() string

	// mu orders a Put's rename against a Delete of the same id; reads open the
	// file once and need no lock, since a rename never disturbs an open file.
	mu sync.Mutex
}

// Option configures a Store.
type Option func(*Store)

// WithIDFunc overrides id generation, for deterministic tests.
func WithIDFunc(f func() string) Option {
	return func(s *Store) {
		if f != nil {
			s.newID = f
		}
	}
}

// New returns a store rooted at dir. Nothing is created until the first Put,
// so opening a store for a scope that never writes leaves no trace on disk.
// Temporary files left by a process that died mid-write are removed here.
func New(dir string, opts ...Option) (*Store, error) {
	s := &Store{dir: dir, newID: event.NewID}
	for _, o := range opts {
		o(s)
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("blob filesystem: read %s: %w", dir, err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return s, nil
}

// path maps an id to its file. Ids are opaque and may be chosen by a caller,
// so they are encoded rather than trusted as file names.
func (s *Store) path(id string) string {
	return filepath.Join(s.dir, base64.RawURLEncoding.EncodeToString([]byte(id))+ext)
}

// Put stores r under meta.ID, or a fresh id when it is empty, replacing any
// blob already stored under that id.
func (s *Store) Put(ctx context.Context, r io.Reader, meta blob.Ref) (blob.Ref, error) {
	if err := ctx.Err(); err != nil {
		return blob.Ref{}, err
	}
	if meta.ID == "" {
		meta.ID = s.newID()
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return blob.Ref{}, fmt.Errorf("blob filesystem: create %s: %w", s.dir, err)
	}
	f, err := os.CreateTemp(s.dir, tempPrefix+"*")
	if err != nil {
		return blob.Ref{}, fmt.Errorf("blob filesystem: create temporary file: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // a no-op once renamed

	n, err := io.Copy(f, r)
	if err != nil {
		_ = f.Close()
		return blob.Ref{}, fmt.Errorf("blob filesystem: read: %w", err)
	}
	meta.Size = n
	if err := writeTrailer(f, meta); err != nil {
		_ = f.Close()
		return blob.Ref{}, err
	}
	if err := ctx.Err(); err != nil {
		_ = f.Close()
		return blob.Ref{}, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return blob.Ref{}, fmt.Errorf("blob filesystem: sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return blob.Ref{}, fmt.Errorf("blob filesystem: close: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Rename(tmp, s.path(meta.ID)); err != nil {
		return blob.Ref{}, fmt.Errorf("blob filesystem: install %s: %w", meta.ID, err)
	}
	if err := syncDir(s.dir); err != nil {
		return blob.Ref{}, err
	}
	return meta, nil
}

// Open returns a seekable reader over the payload. Header and payload come
// from one open file, so a concurrent replacement of the same id cannot mix
// the old trailer with the new bytes.
func (s *Store) Open(ctx context.Context, id string) (io.ReadSeekCloser, blob.Ref, error) {
	if err := ctx.Err(); err != nil {
		return nil, blob.Ref{}, err
	}
	f, err := os.Open(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, blob.Ref{}, fmt.Errorf("%w: %s", blob.ErrNotFound, id)
	}
	if err != nil {
		return nil, blob.Ref{}, fmt.Errorf("blob filesystem: open %s: %w", id, err)
	}
	ref, err := readTrailer(f)
	if err != nil {
		_ = f.Close()
		return nil, blob.Ref{}, fmt.Errorf("blob filesystem: %s: %w", id, err)
	}
	return &reader{SectionReader: io.NewSectionReader(f, 0, ref.Size), f: f}, ref, nil
}

// Stat returns the ref stored with id.
func (s *Store) Stat(ctx context.Context, id string) (blob.Ref, error) {
	r, ref, err := s.Open(ctx, id)
	if err != nil {
		return blob.Ref{}, err
	}
	_ = r.Close()
	return ref, nil
}

// Delete removes id.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", blob.ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("blob filesystem: delete %s: %w", id, err)
	}
	return syncDir(s.dir)
}

// List returns the ref of every blob in the store, in no particular order. A
// file that is not a readable blob is an error rather than something skipped,
// because a sweep that cannot see a blob cannot tell whether it is live.
func (s *Store) List(ctx context.Context) ([]blob.Ref, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("blob filesystem: read %s: %w", s.dir, err)
	}
	var refs []blob.Ref
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, tempPrefix) || filepath.Ext(name) != ext {
			continue
		}
		id, err := base64.RawURLEncoding.DecodeString(strings.TrimSuffix(name, ext))
		if err != nil {
			return nil, fmt.Errorf("blob filesystem: unexpected file %s", name)
		}
		ref, err := s.Stat(ctx, string(id))
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func writeTrailer(w io.Writer, ref blob.Ref) error {
	header, err := json.Marshal(ref)
	if err != nil {
		return fmt.Errorf("blob filesystem: encode ref: %w", err)
	}
	buf := make([]byte, 0, len(header)+trailerSize)
	buf = append(buf, header...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(header)))
	buf = append(buf, magic[:]...)
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("blob filesystem: write: %w", err)
	}
	return nil
}

func readTrailer(f *os.File) (blob.Ref, error) {
	info, err := f.Stat()
	if err != nil {
		return blob.Ref{}, err
	}
	size := info.Size()
	if size < int64(trailerSize) {
		return blob.Ref{}, errors.New("truncated blob file")
	}
	var fixed [trailerSize]byte
	if _, err := f.ReadAt(fixed[:], size-int64(trailerSize)); err != nil {
		return blob.Ref{}, err
	}
	if [8]byte(fixed[4:]) != magic {
		return blob.Ref{}, errors.New("not a blob file")
	}
	headerLen := int64(binary.BigEndian.Uint32(fixed[:4]))
	if headerLen > maxHeaderSize || headerLen > size-int64(trailerSize) {
		return blob.Ref{}, errors.New("corrupt blob trailer")
	}
	header := make([]byte, headerLen)
	if _, err := f.ReadAt(header, size-int64(trailerSize)-headerLen); err != nil {
		return blob.Ref{}, err
	}
	var ref blob.Ref
	if err := json.Unmarshal(header, &ref); err != nil {
		return blob.Ref{}, fmt.Errorf("corrupt blob trailer: %w", err)
	}
	if ref.ID == "" || ref.Size != size-int64(trailerSize)-headerLen {
		return blob.Ref{}, errors.New("blob size does not match its trailer")
	}
	return ref, nil
}

type reader struct {
	*io.SectionReader
	f *os.File
}

func (r *reader) Close() error { return r.f.Close() }

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("blob filesystem: sync %s: %w", path, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("blob filesystem: sync %s: %w", path, err)
	}
	return nil
}
