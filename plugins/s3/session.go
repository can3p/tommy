package s3

import (
	"context"
	"io"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
)

// Session binds the shared catalog to one provider and the event store.
type Session struct {
	store     *Store
	deps      plugin.Deps
	provider  string
	transport string
	meta      map[string]any
}

// SessionOption configures a Session.
type SessionOption func(*Session)

func WithProvider(name string) SessionOption {
	return func(s *Session) { s.provider = name }
}

func WithTransport(transport string) SessionOption {
	return func(s *Session) { s.transport = transport }
}

// WithSessionMeta seeds provider-specific metadata on every recorded event.
func WithSessionMeta(meta map[string]any) SessionOption {
	return func(s *Session) { s.meta = meta }
}

// NewSession attaches the core blob store and returns a mutation-and-event layer.
func NewSession(store *Store, deps plugin.Deps, opts ...SessionOption) *Session {
	if store == nil {
		store = NewStore()
	}
	s := &Session{store: store, deps: deps.Normalize(), transport: "http"}
	for _, opt := range opts {
		opt(s)
	}
	store.Attach(deps.Blobs)
	return s
}

func (s *Session) Store() *Store { return s.store }

func (s *Session) CreateBucket(ctx context.Context, name string, opts ...EventOption) (Bucket, error) {
	bucket, err := s.store.CreateBucket(name)
	if err != nil {
		return Bucket{}, err
	}
	payload := &Payload{Operation: "bucket.create", Bucket: name}
	return bucket, s.record(ctx, EventBucketCreate, payload, opts)
}

func (s *Session) DeleteBucket(ctx context.Context, name string, force bool, opts ...EventOption) (Bucket, error) {
	bucket, err := s.store.DeleteBucket(ctx, name, force)
	if err != nil {
		return Bucket{}, err
	}
	payload := &Payload{Operation: "bucket.delete", Bucket: name}
	return bucket, s.record(ctx, EventBucketDelete, payload, opts)
}

func (s *Session) PutObject(ctx context.Context, bucket, key string, r io.Reader, put PutOptions, opts ...EventOption) (Object, error) {
	obj, err := s.store.PutObject(ctx, bucket, key, r, put)
	if err != nil {
		return Object{}, err
	}
	return obj, s.RecordObjectPut(ctx, obj, opts...)
}

// RecordObjectPut records a completed object as one object.put event. Multipart
// completion itself emits nothing, so a provider calls this after replying only
// when its complete request should be captured.
func (s *Session) RecordObjectPut(ctx context.Context, obj Object, opts ...EventOption) error {
	payload := objectPayload("object.put", obj)
	return s.record(ctx, EventObjectPut, payload, opts)
}

func (s *Session) CopyObject(ctx context.Context, source, destination ObjectLocation, copyOpts CopyOptions, opts ...EventOption) (Object, error) {
	obj, err := s.store.CopyObject(ctx, source, destination, copyOpts)
	if err != nil {
		return Object{}, err
	}
	payload := objectPayload("object.copy", obj)
	payload.Source = &ObjectLocation{Bucket: source.Bucket, Key: source.Key}
	return obj, s.record(ctx, EventObjectCopy, payload, opts)
}

func (s *Session) DeleteObject(ctx context.Context, bucket, key string, opts ...EventOption) (Object, bool, error) {
	obj, removed, err := s.store.DeleteObject(ctx, bucket, key)
	if err != nil || !removed {
		return obj, removed, err
	}
	payload := objectPayload("object.delete", obj)
	payload.Blob = nil
	return obj, true, s.record(ctx, EventObjectDelete, payload, opts)
}

// DeleteObjects records one event for each object that actually existed.
func (s *Session) DeleteObjects(ctx context.Context, bucket string, keys []string, opts ...EventOption) ([]Object, error) {
	removed, err := s.store.DeleteObjects(ctx, bucket, keys)
	if err != nil {
		return nil, err
	}
	for _, obj := range removed {
		payload := objectPayload("object.delete", obj)
		payload.Blob = nil
		if err := s.record(ctx, EventObjectDelete, payload, opts); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// Multipart methods deliberately do not append events.
func (s *Session) CreateMultipart(bucket, key string, opts PutOptions) (MultipartUpload, error) {
	return s.store.CreateMultipart(bucket, key, opts)
}

func (s *Session) PutPart(ctx context.Context, uploadID string, number int, r io.Reader) (Part, error) {
	return s.store.PutPart(ctx, uploadID, number, r)
}

func (s *Session) ListParts(uploadID string) ([]Part, error) {
	return s.store.ListParts(uploadID)
}

func (s *Session) CompleteMultipart(ctx context.Context, uploadID string, parts []CompletedPart) (Object, error) {
	return s.store.CompleteMultipart(ctx, uploadID, parts)
}

func (s *Session) AbortMultipart(ctx context.Context, uploadID string) error {
	return s.store.AbortMultipart(ctx, uploadID)
}

func objectPayload(operation string, obj Object) *Payload {
	payload := &Payload{
		Operation:   operation,
		Bucket:      obj.Bucket,
		Key:         obj.Key,
		Size:        obj.Size,
		ETag:        obj.ETag,
		ContentType: obj.Headers.ContentType,
	}
	if obj.Blob.ID != "" {
		ref := obj.Blob
		payload.Blob = &ref
	}
	return payload
}

func (s *Session) record(ctx context.Context, typ string, payload *Payload, opts []EventOption) error {
	targets := []string{payload.Bucket}
	if payload.Key != "" {
		targets = append(targets, payload.Key, payload.Bucket+"/"+payload.Key)
	}
	e := &event.Event{
		Plugin:   PluginName,
		Provider: s.provider,
		Type:     typ,
		Summary: event.Summary{
			From:    s.provider,
			To:      targets,
			Title:   payload.Title(),
			Snippet: payload.Snippet(),
		},
		Payload: payload,
		Raw: event.Raw{
			Transport: s.transport,
			Body:      []byte(payload.Snippet()),
			Text:      true,
		},
	}
	if len(s.meta) > 0 {
		e.Meta = make(map[string]any, len(s.meta))
		for key, value := range s.meta {
			e.Meta[key] = value
		}
	}
	for _, opt := range opts {
		opt(e)
	}
	// The access key is the closest thing S3 has to a sender, so the list
	// views show who wrote rather than which listener it arrived on.
	if key, ok := e.Meta["access_key"].(string); ok && key != "" {
		e.Summary.From = key
	}
	return s.deps.Append(ctx, e)
}
