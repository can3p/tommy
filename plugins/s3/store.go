package s3

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/can3p/tommy/core/blob"
	blobmem "github.com/can3p/tommy/core/blob/memory"
	"github.com/can3p/tommy/core/event"
)

var (
	ErrBucketNotFound   = errors.New("s3: bucket not found")
	ErrBucketExists     = errors.New("s3: bucket already exists")
	ErrBucketNotEmpty   = errors.New("s3: bucket is not empty")
	ErrObjectNotFound   = errors.New("s3: object not found")
	ErrUploadNotFound   = errors.New("s3: multipart upload not found")
	ErrUploadBusy       = errors.New("s3: multipart upload is being completed")
	ErrInvalidBucket    = errors.New("s3: invalid bucket name")
	ErrInvalidKey       = errors.New("s3: invalid object key")
	ErrInvalidPart      = errors.New("s3: invalid multipart part")
	ErrBucketLimit      = errors.New("s3: bucket limit exceeded")
	ErrObjectLimit      = errors.New("s3: object limit exceeded")
	ErrUploadLimit      = errors.New("s3: active upload limit exceeded")
	ErrPartLimit        = errors.New("s3: multipart part limit exceeded")
	ErrMetadataTooLarge = errors.New("s3: object metadata is too large")
	ErrObjectTooLarge   = errors.New("s3: object is too large")
)

// Limits bound the catalog and every upload. Zero values use DefaultLimits.
type Limits struct {
	MaxBuckets       int
	MaxObjects       int
	MaxActiveUploads int
	MaxParts         int
	MaxBucketBytes   int
	MaxKeyBytes      int
	MaxMetadataBytes int
	MaxObjectBytes   int64
}

// DefaultLimits keep a runaway client bounded while leaving ample room for test fixtures.
var DefaultLimits = Limits{
	MaxBuckets:       1000,
	MaxObjects:       50000,
	MaxActiveUploads: 1000,
	MaxParts:         10000,
	MaxBucketBytes:   255,
	MaxKeyBytes:      1024,
	MaxMetadataBytes: 16 << 10,
	MaxObjectBytes:   64 << 20,
}

func (l Limits) withDefaults() Limits {
	d := DefaultLimits
	if l.MaxBuckets <= 0 {
		l.MaxBuckets = d.MaxBuckets
	}
	if l.MaxObjects <= 0 {
		l.MaxObjects = d.MaxObjects
	}
	if l.MaxActiveUploads <= 0 {
		l.MaxActiveUploads = d.MaxActiveUploads
	}
	if l.MaxParts <= 0 {
		l.MaxParts = d.MaxParts
	}
	if l.MaxBucketBytes <= 0 {
		l.MaxBucketBytes = d.MaxBucketBytes
	}
	if l.MaxKeyBytes <= 0 {
		l.MaxKeyBytes = d.MaxKeyBytes
	}
	if l.MaxMetadataBytes <= 0 {
		l.MaxMetadataBytes = d.MaxMetadataBytes
	}
	if l.MaxObjectBytes <= 0 {
		l.MaxObjectBytes = d.MaxObjectBytes
	}
	return l
}

// Checksums carries the checksum headers S3 clients can supply.
type Checksums struct {
	CRC32  string `json:"crc32,omitempty"`
	CRC32C string `json:"crc32c,omitempty"`
	SHA1   string `json:"sha1,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// ContentHeaders are the representation headers retained with an object.
type ContentHeaders struct {
	ContentType        string    `json:"content_type,omitempty"`
	ContentEncoding    string    `json:"content_encoding,omitempty"`
	ContentLanguage    string    `json:"content_language,omitempty"`
	ContentDisposition string    `json:"content_disposition,omitempty"`
	CacheControl       string    `json:"cache_control,omitempty"`
	Expires            time.Time `json:"expires,omitzero"`
}

// PutOptions are the metadata installed with a completed object.
type PutOptions struct {
	Metadata  map[string]string
	Headers   ContentHeaders
	Checksums Checksums
	ModTime   time.Time
}

// Bucket is a snapshot of one bucket.
type Bucket struct {
	Name         string    `json:"name"`
	CreationTime time.Time `json:"creation_time"`
	Objects      int       `json:"objects"`
	Bytes        int64     `json:"bytes"`
}

// Object is a snapshot of one exact S3 key and its metadata.
type Object struct {
	Bucket       string            `json:"bucket"`
	Key          string            `json:"key"`
	Size         int64             `json:"size"`
	ETag         string            `json:"etag"`
	Checksums    Checksums         `json:"checksums,omitzero"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Headers      ContentHeaders    `json:"headers,omitzero"`
	LastModified time.Time         `json:"last_modified"`
	Blob         blob.Ref          `json:"blob"`
}

// ObjectLocation identifies an object without interpreting its key as a path.
type ObjectLocation struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

// CopyOptions selects COPY (the default) or REPLACE metadata behavior.
type CopyOptions struct {
	MetadataDirective string
	PutOptions        PutOptions
}

// ListOptions are the S3 ListObjects inputs. ContinuationToken is the last
// returned key or common prefix; MaxKeys <= 0 uses 1000.
type ListOptions struct {
	Prefix            string
	Delimiter         string
	StartAfter        string
	ContinuationToken string
	MaxKeys           int
}

// ListResult is a stable lexicographic page of objects and common prefixes.
type ListResult struct {
	Objects               []Object `json:"objects"`
	CommonPrefixes        []string `json:"common_prefixes"`
	IsTruncated           bool     `json:"is_truncated"`
	NextContinuationToken string   `json:"next_continuation_token,omitempty"`
}

// MultipartUpload is a snapshot of one active multipart upload.
type MultipartUpload struct {
	ID        string    `json:"id"`
	Bucket    string    `json:"bucket"`
	Key       string    `json:"key"`
	Initiated time.Time `json:"initiated"`
}

// Part is a stored multipart part. Its bytes live only in the blob store.
type Part struct {
	Number       int       `json:"number"`
	Size         int64     `json:"size"`
	ETag         string    `json:"etag"`
	LastModified time.Time `json:"last_modified"`
	Blob         blob.Ref  `json:"blob"`
}

// CompletedPart names and verifies a part used by CompleteMultipart.
type CompletedPart struct {
	Number int    `json:"number"`
	ETag   string `json:"etag,omitempty"`
}

// Stats counts current catalog state independently of event retention.
type Stats struct {
	Buckets       int   `json:"buckets"`
	Objects       int   `json:"objects"`
	Bytes         int64 `json:"bytes"`
	ActiveUploads int   `json:"active_uploads"`
	Parts         int   `json:"parts"`
	PartBytes     int64 `json:"part_bytes"`
}

type bucketState struct {
	created time.Time
	objects map[string]Object
}

type uploadState struct {
	MultipartUpload
	opts       PutOptions
	parts      map[int]Part
	completing bool
}

// Store is the concurrency-safe S3 catalog shared by every provider.
// Catalog locks never cover blob I/O.
type Store struct {
	mu      sync.RWMutex
	buckets map[string]*bucketState
	uploads map[string]*uploadState
	objects int

	limits Limits
	now    func() time.Time
	newID  func() string

	blobMu   sync.RWMutex
	blobs    blob.BlobStore
	attached bool
}

// Option configures a Store.
type Option func(*Store)

func WithLimits(l Limits) Option { return func(s *Store) { s.limits = l.withDefaults() } }

func WithClock(now func() time.Time) Option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

func WithIDFunc(f func() string) Option {
	return func(s *Store) {
		if f != nil {
			s.newID = f
		}
	}
}

// WithBlobs installs a blob store for standalone use and counts as the first attachment.
func WithBlobs(b blob.BlobStore) Option {
	return func(s *Store) {
		if b != nil {
			s.blobs = b
			s.attached = true
		}
	}
}

// NewStore returns an empty catalog with a private fallback blob store.
func NewStore(opts ...Option) *Store {
	s := &Store{
		buckets: map[string]*bucketState{},
		uploads: map[string]*uploadState{},
		limits:  DefaultLimits,
		now:     time.Now,
		newID:   event.NewID,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.blobs == nil {
		s.blobs = blobmem.New(0)
	}
	return s
}

// Attach replaces the fallback with the first non-nil shared blob store. Later calls are ignored.
func (s *Store) Attach(b blob.BlobStore) {
	if b == nil {
		return
	}
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	if s.attached {
		return
	}
	s.blobs = b
	s.attached = true
}

func (s *Store) Blobs() blob.BlobStore {
	s.blobMu.RLock()
	defer s.blobMu.RUnlock()
	return s.blobs
}

// blobStore claims the current store before the first blob I/O. That prevents a
// concurrent late Attach from switching stores after a fallback ref is created.
func (s *Store) blobStore() blob.BlobStore {
	s.blobMu.Lock()
	defer s.blobMu.Unlock()
	s.attached = true
	return s.blobs
}

func (s *Store) Limits() Limits { return s.limits }

func (s *Store) CreateBucket(name string) (Bucket, error) {
	if err := s.validateBucket(name); err != nil {
		return Bucket{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buckets[name]; ok {
		return Bucket{}, fmt.Errorf("%w: %s", ErrBucketExists, name)
	}
	if len(s.buckets) >= s.limits.MaxBuckets {
		return Bucket{}, ErrBucketLimit
	}
	created := s.now()
	s.buckets[name] = &bucketState{created: created, objects: map[string]Object{}}
	return Bucket{Name: name, CreationTime: created}, nil
}

func (s *Store) HeadBucket(name string) (Bucket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.buckets[name]
	if !ok {
		return Bucket{}, fmt.Errorf("%w: %s", ErrBucketNotFound, name)
	}
	return bucketSnapshot(name, b), nil
}

func (s *Store) ListBuckets() []Bucket {
	s.mu.RLock()
	out := make([]Bucket, 0, len(s.buckets))
	for name, b := range s.buckets {
		out = append(out, bucketSnapshot(name, b))
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func bucketSnapshot(name string, b *bucketState) Bucket {
	out := Bucket{Name: name, CreationTime: b.created, Objects: len(b.objects)}
	for _, obj := range b.objects {
		out.Bytes += obj.Size
	}
	return out
}

// DeleteBucket removes an empty bucket. With force it atomically clears objects
// and inactive uploads first, then frees all of their blobs outside the lock.
func (s *Store) DeleteBucket(ctx context.Context, name string, force bool) (Bucket, error) {
	s.mu.Lock()
	b, ok := s.buckets[name]
	if !ok {
		s.mu.Unlock()
		return Bucket{}, fmt.Errorf("%w: %s", ErrBucketNotFound, name)
	}
	var bucketUploads []*uploadState
	for _, up := range s.uploads {
		if up.Bucket == name {
			bucketUploads = append(bucketUploads, up)
		}
	}
	if !force && (len(b.objects) > 0 || len(bucketUploads) > 0) {
		s.mu.Unlock()
		return Bucket{}, fmt.Errorf("%w: %s", ErrBucketNotEmpty, name)
	}
	for _, up := range bucketUploads {
		if up.completing {
			s.mu.Unlock()
			return Bucket{}, fmt.Errorf("%w: %s", ErrUploadBusy, up.ID)
		}
	}
	removed := bucketSnapshot(name, b)
	refs := make([]blob.Ref, 0, len(b.objects))
	for _, obj := range b.objects {
		refs = append(refs, obj.Blob)
		s.objects--
	}
	for _, up := range bucketUploads {
		for _, part := range up.parts {
			refs = append(refs, part.Blob)
		}
		delete(s.uploads, up.ID)
	}
	delete(s.buckets, name)
	s.mu.Unlock()
	s.discardAll(ctx, refs)
	return removed, nil
}

// ClearBucket removes every object and inactive upload but keeps the bucket.
func (s *Store) ClearBucket(ctx context.Context, name string) (int, error) {
	s.mu.Lock()
	b, ok := s.buckets[name]
	if !ok {
		s.mu.Unlock()
		return 0, fmt.Errorf("%w: %s", ErrBucketNotFound, name)
	}
	for _, up := range s.uploads {
		if up.Bucket == name && up.completing {
			s.mu.Unlock()
			return 0, fmt.Errorf("%w: %s", ErrUploadBusy, up.ID)
		}
	}
	refs := make([]blob.Ref, 0, len(b.objects))
	count := len(b.objects)
	for _, obj := range b.objects {
		refs = append(refs, obj.Blob)
	}
	b.objects = map[string]Object{}
	s.objects -= count
	for id, up := range s.uploads {
		if up.Bucket != name {
			continue
		}
		for _, part := range up.parts {
			refs = append(refs, part.Blob)
		}
		delete(s.uploads, id)
	}
	s.mu.Unlock()
	s.discardAll(ctx, refs)
	return count, nil
}

func (s *Store) HeadObject(bucket, key string) (Object, error) {
	if err := s.validateKey(key); err != nil {
		return Object{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.buckets[bucket]
	if !ok {
		return Object{}, fmt.Errorf("%w: %s", ErrBucketNotFound, bucket)
	}
	obj, ok := b.objects[key]
	if !ok {
		return Object{}, fmt.Errorf("%w: %s/%s", ErrObjectNotFound, bucket, key)
	}
	return cloneObject(obj), nil
}

func (s *Store) OpenObject(ctx context.Context, bucket, key string) (io.ReadSeekCloser, Object, error) {
	obj, err := s.HeadObject(bucket, key)
	if err != nil {
		return nil, Object{}, err
	}
	r, _, err := s.blobStore().Open(ctx, obj.Blob.ID)
	if err != nil {
		return nil, Object{}, err
	}
	return r, obj, nil
}

func (s *Store) GetObject(ctx context.Context, bucket, key string) ([]byte, Object, error) {
	r, obj, err := s.OpenObject(ctx, bucket, key)
	if err != nil {
		return nil, Object{}, err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, Object{}, err
	}
	return data, obj, nil
}

func (s *Store) PutObject(ctx context.Context, bucket, key string, r io.Reader, opts PutOptions) (Object, error) {
	if err := s.validateKey(key); err != nil {
		return Object{}, err
	}
	if err := s.validateMetadata(opts.Metadata); err != nil {
		return Object{}, err
	}
	ref, etag, err := s.storeBlob(ctx, key, r, opts.Headers.ContentType)
	if err != nil {
		return Object{}, err
	}
	obj := Object{
		Bucket: bucket, Key: key, Size: ref.Size, ETag: etag,
		Checksums: opts.Checksums, Metadata: cloneMetadata(opts.Metadata), Headers: opts.Headers,
		LastModified: s.stamp(opts.ModTime), Blob: ref,
	}
	obj.Blob.ContentType = opts.Headers.ContentType
	obj.Blob.Filename = key

	s.mu.Lock()
	b, ok := s.buckets[bucket]
	if !ok {
		s.mu.Unlock()
		s.discard(ctx, ref)
		return Object{}, fmt.Errorf("%w: %s", ErrBucketNotFound, bucket)
	}
	prev, replacing := b.objects[key]
	if !replacing && s.objects >= s.limits.MaxObjects {
		s.mu.Unlock()
		s.discard(ctx, ref)
		return Object{}, ErrObjectLimit
	}
	b.objects[key] = obj
	if !replacing {
		s.objects++
	}
	s.mu.Unlock()
	if replacing && prev.Blob.ID != ref.ID {
		s.discard(ctx, prev.Blob)
	}
	return cloneObject(obj), nil
}

func (s *Store) CopyObject(ctx context.Context, source ObjectLocation, destination ObjectLocation, opts CopyOptions) (Object, error) {
	r, src, err := s.OpenObject(ctx, source.Bucket, source.Key)
	if err != nil {
		return Object{}, err
	}
	defer func() { _ = r.Close() }()
	put := opts.PutOptions
	if !strings.EqualFold(opts.MetadataDirective, "REPLACE") {
		put = PutOptions{Metadata: src.Metadata, Headers: src.Headers, Checksums: src.Checksums}
	}
	return s.PutObject(ctx, destination.Bucket, destination.Key, r, put)
}

func (s *Store) DeleteObject(ctx context.Context, bucket, key string) (Object, bool, error) {
	if err := s.validateKey(key); err != nil {
		return Object{}, false, err
	}
	s.mu.Lock()
	b, ok := s.buckets[bucket]
	if !ok {
		s.mu.Unlock()
		return Object{}, false, fmt.Errorf("%w: %s", ErrBucketNotFound, bucket)
	}
	obj, ok := b.objects[key]
	if !ok {
		s.mu.Unlock()
		return Object{}, false, nil
	}
	delete(b.objects, key)
	s.objects--
	s.mu.Unlock()
	s.discard(ctx, obj.Blob)
	return cloneObject(obj), true, nil
}

// DeleteObjects atomically removes every existing distinct key and returns only
// objects that were actually present, in the caller's first-seen key order.
func (s *Store) DeleteObjects(ctx context.Context, bucket string, keys []string) ([]Object, error) {
	for _, key := range keys {
		if err := s.validateKey(key); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	b, ok := s.buckets[bucket]
	if !ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrBucketNotFound, bucket)
	}
	seen := make(map[string]struct{}, len(keys))
	removed := make([]Object, 0, len(keys))
	for _, key := range keys {
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		if obj, exists := b.objects[key]; exists {
			removed = append(removed, cloneObject(obj))
			delete(b.objects, key)
			s.objects--
		}
	}
	s.mu.Unlock()
	refs := make([]blob.Ref, len(removed))
	for i := range removed {
		refs[i] = removed[i].Blob
	}
	s.discardAll(ctx, refs)
	return removed, nil
}

type listEntry struct {
	name   string
	object *Object
}

func (s *Store) ListObjects(bucket string, opts ListOptions) (ListResult, error) {
	s.mu.RLock()
	b, ok := s.buckets[bucket]
	if !ok {
		s.mu.RUnlock()
		return ListResult{}, fmt.Errorf("%w: %s", ErrBucketNotFound, bucket)
	}
	objects := make([]Object, 0, len(b.objects))
	for _, obj := range b.objects {
		objects = append(objects, cloneObject(obj))
	}
	s.mu.RUnlock()

	entries := make([]listEntry, 0, len(objects))
	prefixes := map[string]struct{}{}
	for i := range objects {
		obj := &objects[i]
		if !strings.HasPrefix(obj.Key, opts.Prefix) {
			continue
		}
		if opts.Delimiter != "" {
			rest := strings.TrimPrefix(obj.Key, opts.Prefix)
			if idx := strings.Index(rest, opts.Delimiter); idx >= 0 {
				prefix := opts.Prefix + rest[:idx+len(opts.Delimiter)]
				prefixes[prefix] = struct{}{}
				continue
			}
		}
		entries = append(entries, listEntry{name: obj.Key, object: obj})
	}
	for prefix := range prefixes {
		entries = append(entries, listEntry{name: prefix})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	marker := opts.ContinuationToken
	if marker == "" {
		marker = opts.StartAfter
	}
	if marker != "" {
		start := sort.Search(len(entries), func(i int) bool { return entries[i].name > marker })
		entries = entries[start:]
	}
	max := opts.MaxKeys
	if max <= 0 || max > 1000 {
		max = 1000
	}
	result := ListResult{Objects: []Object{}, CommonPrefixes: []string{}}
	page := entries
	if len(page) > max {
		page = page[:max]
		result.IsTruncated = true
	}
	for _, entry := range page {
		if entry.object == nil {
			result.CommonPrefixes = append(result.CommonPrefixes, entry.name)
		} else {
			result.Objects = append(result.Objects, cloneObject(*entry.object))
		}
	}
	if result.IsTruncated && len(page) > 0 {
		result.NextContinuationToken = page[len(page)-1].name
	}
	return result, nil
}

func (s *Store) CreateMultipart(bucket, key string, opts PutOptions) (MultipartUpload, error) {
	if err := s.validateKey(key); err != nil {
		return MultipartUpload{}, err
	}
	if err := s.validateMetadata(opts.Metadata); err != nil {
		return MultipartUpload{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buckets[bucket]; !ok {
		return MultipartUpload{}, fmt.Errorf("%w: %s", ErrBucketNotFound, bucket)
	}
	if len(s.uploads) >= s.limits.MaxActiveUploads {
		return MultipartUpload{}, ErrUploadLimit
	}
	id := s.newID()
	for s.uploads[id] != nil {
		id = s.newID()
	}
	up := MultipartUpload{ID: id, Bucket: bucket, Key: key, Initiated: s.now()}
	s.uploads[id] = &uploadState{MultipartUpload: up, opts: clonePutOptions(opts), parts: map[int]Part{}}
	return up, nil
}

func (s *Store) ListMultipartUploads(bucket string) ([]MultipartUpload, error) {
	s.mu.RLock()
	if _, ok := s.buckets[bucket]; !ok {
		s.mu.RUnlock()
		return nil, fmt.Errorf("%w: %s", ErrBucketNotFound, bucket)
	}
	out := make([]MultipartUpload, 0)
	for _, up := range s.uploads {
		if up.Bucket == bucket {
			out = append(out, up.MultipartUpload)
		}
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *Store) PutPart(ctx context.Context, uploadID string, number int, r io.Reader) (Part, error) {
	if number < 1 || number > 10000 {
		return Part{}, fmt.Errorf("%w: part number %d", ErrInvalidPart, number)
	}
	s.mu.RLock()
	up := s.uploads[uploadID]
	if up == nil {
		s.mu.RUnlock()
		return Part{}, fmt.Errorf("%w: %s", ErrUploadNotFound, uploadID)
	}
	if up.completing {
		s.mu.RUnlock()
		return Part{}, fmt.Errorf("%w: %s", ErrUploadBusy, uploadID)
	}
	key := up.Key
	s.mu.RUnlock()

	ref, etag, err := s.storeBlob(ctx, key, r, "application/octet-stream")
	if err != nil {
		return Part{}, err
	}
	part := Part{Number: number, Size: ref.Size, ETag: etag, LastModified: s.now(), Blob: ref}

	s.mu.Lock()
	up = s.uploads[uploadID]
	if up == nil {
		s.mu.Unlock()
		s.discard(ctx, ref)
		return Part{}, fmt.Errorf("%w: %s", ErrUploadNotFound, uploadID)
	}
	if up.completing {
		s.mu.Unlock()
		s.discard(ctx, ref)
		return Part{}, fmt.Errorf("%w: %s", ErrUploadBusy, uploadID)
	}
	prev, replacing := up.parts[number]
	if !replacing && len(up.parts) >= s.limits.MaxParts {
		s.mu.Unlock()
		s.discard(ctx, ref)
		return Part{}, ErrPartLimit
	}
	var total int64
	for n, p := range up.parts {
		if n != number {
			total += p.Size
		}
	}
	if total+part.Size > s.limits.MaxObjectBytes {
		s.mu.Unlock()
		s.discard(ctx, ref)
		return Part{}, ErrObjectTooLarge
	}
	up.parts[number] = part
	s.mu.Unlock()
	if replacing && prev.Blob.ID != ref.ID {
		s.discard(ctx, prev.Blob)
	}
	return part, nil
}

func (s *Store) ListParts(uploadID string) ([]Part, error) {
	s.mu.RLock()
	up := s.uploads[uploadID]
	if up == nil {
		s.mu.RUnlock()
		return nil, fmt.Errorf("%w: %s", ErrUploadNotFound, uploadID)
	}
	out := make([]Part, 0, len(up.parts))
	for _, part := range up.parts {
		out = append(out, part)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

func (s *Store) CompleteMultipart(ctx context.Context, uploadID string, completed []CompletedPart) (Object, error) {
	s.mu.Lock()
	up := s.uploads[uploadID]
	if up == nil {
		s.mu.Unlock()
		return Object{}, fmt.Errorf("%w: %s", ErrUploadNotFound, uploadID)
	}
	if up.completing {
		s.mu.Unlock()
		return Object{}, fmt.Errorf("%w: %s", ErrUploadBusy, uploadID)
	}
	parts, err := selectParts(up, completed)
	if err != nil {
		s.mu.Unlock()
		return Object{}, err
	}
	up.completing = true
	bucket, key, opts := up.Bucket, up.Key, clonePutOptions(up.opts)
	s.mu.Unlock()

	readers := make([]io.Reader, 0, len(parts))
	closers := make([]io.Closer, 0, len(parts))
	for _, part := range parts {
		r, _, openErr := s.blobStore().Open(ctx, part.Blob.ID)
		if openErr != nil {
			for _, closer := range closers {
				_ = closer.Close()
			}
			s.finishCompletionFailure(uploadID)
			return Object{}, openErr
		}
		readers = append(readers, r)
		closers = append(closers, r)
	}
	ref, _, storeErr := s.storeBlob(ctx, key, io.MultiReader(readers...), opts.Headers.ContentType)
	for _, closer := range closers {
		_ = closer.Close()
	}
	if storeErr != nil {
		s.finishCompletionFailure(uploadID)
		return Object{}, storeErr
	}
	etag, err := multipartETag(parts)
	if err != nil {
		s.discard(ctx, ref)
		s.finishCompletionFailure(uploadID)
		return Object{}, err
	}
	obj := Object{
		Bucket: bucket, Key: key, Size: ref.Size, ETag: etag,
		Checksums: opts.Checksums, Metadata: cloneMetadata(opts.Metadata), Headers: opts.Headers,
		LastModified: s.stamp(opts.ModTime), Blob: ref,
	}
	obj.Blob.ContentType = opts.Headers.ContentType
	obj.Blob.Filename = key

	s.mu.Lock()
	up = s.uploads[uploadID]
	b := s.buckets[bucket]
	if up == nil || !up.completing || b == nil {
		s.mu.Unlock()
		s.discard(ctx, ref)
		return Object{}, fmt.Errorf("%w: %s", ErrUploadNotFound, uploadID)
	}
	prev, replacing := b.objects[key]
	if !replacing && s.objects >= s.limits.MaxObjects {
		up.completing = false
		s.mu.Unlock()
		s.discard(ctx, ref)
		return Object{}, ErrObjectLimit
	}
	b.objects[key] = obj
	if !replacing {
		s.objects++
	}
	delete(s.uploads, uploadID)
	s.mu.Unlock()

	refs := make([]blob.Ref, 0, len(parts)+1)
	for _, part := range up.parts {
		refs = append(refs, part.Blob)
	}
	if replacing {
		refs = append(refs, prev.Blob)
	}
	s.discardAll(ctx, refs)
	return cloneObject(obj), nil
}

func selectParts(up *uploadState, completed []CompletedPart) ([]Part, error) {
	if len(completed) == 0 {
		return nil, fmt.Errorf("%w: no parts", ErrInvalidPart)
	}
	parts := make([]Part, 0, len(completed))
	last := 0
	for _, want := range completed {
		if want.Number <= last {
			return nil, fmt.Errorf("%w: parts must be in ascending order", ErrInvalidPart)
		}
		part, ok := up.parts[want.Number]
		if !ok {
			return nil, fmt.Errorf("%w: part %d is missing", ErrInvalidPart, want.Number)
		}
		if want.ETag != "" && trimETag(want.ETag) != trimETag(part.ETag) {
			return nil, fmt.Errorf("%w: part %d etag mismatch", ErrInvalidPart, want.Number)
		}
		parts = append(parts, part)
		last = want.Number
	}
	return parts, nil
}

func multipartETag(parts []Part) (string, error) {
	h := md5.New()
	for _, part := range parts {
		digest, err := hex.DecodeString(trimETag(part.ETag))
		if err != nil || len(digest) != md5.Size {
			return "", fmt.Errorf("%w: invalid etag for part %d", ErrInvalidPart, part.Number)
		}
		_, _ = h.Write(digest)
	}
	return hex.EncodeToString(h.Sum(nil)) + fmt.Sprintf("-%d", len(parts)), nil
}

func trimETag(etag string) string { return strings.Trim(etag, `"`) }

func (s *Store) finishCompletionFailure(uploadID string) {
	s.mu.Lock()
	if up := s.uploads[uploadID]; up != nil {
		up.completing = false
	}
	s.mu.Unlock()
}

func (s *Store) AbortMultipart(ctx context.Context, uploadID string) error {
	s.mu.Lock()
	up := s.uploads[uploadID]
	if up == nil {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrUploadNotFound, uploadID)
	}
	if up.completing {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrUploadBusy, uploadID)
	}
	delete(s.uploads, uploadID)
	refs := make([]blob.Ref, 0, len(up.parts))
	for _, part := range up.parts {
		refs = append(refs, part.Blob)
	}
	s.mu.Unlock()
	s.discardAll(ctx, refs)
	return nil
}

func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stats := Stats{Buckets: len(s.buckets), Objects: s.objects, ActiveUploads: len(s.uploads)}
	for _, b := range s.buckets {
		for _, obj := range b.objects {
			stats.Bytes += obj.Size
		}
	}
	for _, up := range s.uploads {
		for _, part := range up.parts {
			stats.Parts++
			stats.PartBytes += part.Size
		}
	}
	return stats
}

func (s *Store) validateBucket(name string) error {
	if name == "" || len(name) > s.limits.MaxBucketBytes || !utf8.ValidString(name) {
		return fmt.Errorf("%w: %q", ErrInvalidBucket, name)
	}
	return nil
}

func (s *Store) validateKey(key string) error {
	if key == "" || len(key) > s.limits.MaxKeyBytes || !utf8.ValidString(key) {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	return nil
}

func (s *Store) validateMetadata(metadata map[string]string) error {
	var size int
	for key, value := range metadata {
		size += len(key) + len(value)
		if size > s.limits.MaxMetadataBytes {
			return ErrMetadataTooLarge
		}
	}
	return nil
}

func (s *Store) stamp(t time.Time) time.Time {
	if !t.IsZero() {
		return t
	}
	return s.now()
}

func (s *Store) storeBlob(ctx context.Context, key string, r io.Reader, contentType string) (blob.Ref, string, error) {
	h := md5.New()
	limited := &objectLimitReader{r: io.TeeReader(r, h), left: s.limits.MaxObjectBytes + 1}
	ref, err := s.blobStore().Put(ctx, limited, blob.Ref{Filename: key, ContentType: contentType})
	if err != nil {
		return blob.Ref{}, "", err
	}
	if ref.Size > s.limits.MaxObjectBytes {
		s.discard(ctx, ref)
		return blob.Ref{}, "", ErrObjectTooLarge
	}
	return ref, hex.EncodeToString(h.Sum(nil)), nil
}

type objectLimitReader struct {
	r    io.Reader
	left int64
}

func (r *objectLimitReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, ErrObjectTooLarge
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.r.Read(p)
	r.left -= int64(n)
	return n, err
}

func (s *Store) discard(ctx context.Context, ref blob.Ref) {
	if ref.ID != "" {
		// State has already stopped referring to this blob. Finish cleanup even
		// when the request was canceled between blob creation and installation.
		_ = s.blobStore().Delete(context.WithoutCancel(ctx), ref.ID)
	}
}

func (s *Store) discardAll(ctx context.Context, refs []blob.Ref) {
	for _, ref := range refs {
		s.discard(ctx, ref)
	}
}

func cloneMetadata(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func clonePutOptions(in PutOptions) PutOptions {
	in.Metadata = cloneMetadata(in.Metadata)
	return in
}

func cloneObject(in Object) Object {
	in.Metadata = cloneMetadata(in.Metadata)
	return in
}
