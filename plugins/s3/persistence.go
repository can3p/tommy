package s3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/state"
)

// catalogStateKey is the one snapshot the catalog is saved under. The whole
// catalog is rewritten on every change: it is bounded by Limits, and a single
// file that is replaced atomically cannot be left half-updated.
const catalogStateKey = "catalog.json"

// catalogVersion is bumped on any incompatible change to the snapshot. An
// unknown version stops startup rather than discarding someone's data.
const catalogVersion = 1

type catalogSnapshot struct {
	Version int           `json:"version"`
	Buckets []savedBucket `json:"buckets"`
	Uploads []savedUpload `json:"uploads,omitempty"`
}

type savedBucket struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Objects []Object  `json:"objects"`
}

type savedUpload struct {
	Upload  MultipartUpload `json:"upload"`
	Options PutOptions      `json:"options"`
	Parts   []Part          `json:"parts"`
}

// BindState restores the catalog from storage and saves every later change
// there. A nil storage is a memory scope: the catalog stays as it is and
// nothing is saved. It must be called before the store is used.
//
// Once restored, blobs in the store that the catalog does not reference are
// deleted: bytes written by a process that died before saving the snapshot
// naming them, or kept by one that died before deleting what it replaced.
func (s *Store) BindState(ctx context.Context, storage state.Store) error {
	if storage == nil {
		return nil
	}
	data, err := storage.Load(ctx, catalogStateKey)
	switch {
	case errors.Is(err, state.ErrNotFound):
	case err != nil:
		return err
	default:
		var snapshot catalogSnapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return fmt.Errorf("decode catalog snapshot: %w", err)
		}
		if err := s.restore(ctx, snapshot); err != nil {
			return fmt.Errorf("restore catalog snapshot: %w", err)
		}
	}
	if _, err := blob.Sweep(ctx, s.blobStore(), s.liveBlobs()); err != nil {
		return err
	}
	s.state = storage
	return nil
}

// liveBlobs is every blob the catalog references.
func (s *Store) liveBlobs() map[string]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	live := map[string]struct{}{}
	for _, b := range s.buckets {
		for _, o := range b.objects {
			live[o.Blob.ID] = struct{}{}
		}
	}
	for _, up := range s.uploads {
		for _, part := range up.parts {
			live[part.Blob.ID] = struct{}{}
		}
	}
	return live
}

func (s *Store) restore(ctx context.Context, snapshot catalogSnapshot) error {
	if snapshot.Version != catalogVersion {
		return fmt.Errorf("unsupported catalog snapshot version %d", snapshot.Version)
	}
	buckets := make(map[string]*bucketState, len(snapshot.Buckets))
	objects := 0
	for _, saved := range snapshot.Buckets {
		if err := s.validateBucket(saved.Name); err != nil {
			return err
		}
		if _, exists := buckets[saved.Name]; exists {
			return fmt.Errorf("duplicate bucket %q", saved.Name)
		}
		if saved.Created.IsZero() {
			return fmt.Errorf("bucket %q has no creation time", saved.Name)
		}
		bucket := &bucketState{created: saved.Created, objects: map[string]Object{}}
		for _, object := range saved.Objects {
			if object.Bucket != saved.Name {
				return fmt.Errorf("object %q belongs to bucket %q, not %q", object.Key, object.Bucket, saved.Name)
			}
			if err := s.validateKey(object.Key); err != nil {
				return err
			}
			if err := s.validateMetadata(object.Metadata); err != nil {
				return err
			}
			if _, exists := bucket.objects[object.Key]; exists {
				return fmt.Errorf("duplicate object %q in bucket %q", object.Key, saved.Name)
			}
			if err := s.validateBlob(ctx, object.Blob, object.Size); err != nil {
				return fmt.Errorf("object %s/%s: %w", saved.Name, object.Key, err)
			}
			bucket.objects[object.Key] = cloneObject(object)
			objects++
		}
		buckets[saved.Name] = bucket
	}
	if len(buckets) > s.limits.MaxBuckets {
		return ErrBucketLimit
	}
	if objects > s.limits.MaxObjects {
		return ErrObjectLimit
	}
	uploads := make(map[string]*uploadState, len(snapshot.Uploads))
	for _, saved := range snapshot.Uploads {
		up := saved.Upload
		if up.ID == "" {
			return fmt.Errorf("multipart upload has no id")
		}
		if _, exists := uploads[up.ID]; exists {
			return fmt.Errorf("duplicate multipart upload %q", up.ID)
		}
		if buckets[up.Bucket] == nil {
			return fmt.Errorf("multipart upload %q names missing bucket %q", up.ID, up.Bucket)
		}
		if err := s.validateKey(up.Key); err != nil {
			return err
		}
		parts := make(map[int]Part, len(saved.Parts))
		for _, part := range saved.Parts {
			if part.Number < 1 || part.Number > 10000 {
				return ErrInvalidPart
			}
			if _, exists := parts[part.Number]; exists {
				return fmt.Errorf("multipart upload %q has duplicate part %d", up.ID, part.Number)
			}
			if err := s.validateBlob(ctx, part.Blob, part.Size); err != nil {
				return fmt.Errorf("multipart upload %q part %d: %w", up.ID, part.Number, err)
			}
			parts[part.Number] = part
		}
		if len(parts) > s.limits.MaxParts {
			return ErrPartLimit
		}
		uploads[up.ID] = &uploadState{MultipartUpload: up, opts: clonePutOptions(saved.Options), parts: parts}
	}
	if len(uploads) > s.limits.MaxActiveUploads {
		return ErrUploadLimit
	}
	s.mu.Lock()
	s.buckets = buckets
	s.uploads = uploads
	s.objects = objects
	s.mu.Unlock()
	return nil
}

func (s *Store) validateBlob(ctx context.Context, ref blob.Ref, size int64) error {
	if ref.ID == "" || ref.Size != size {
		return errors.New("invalid blob reference")
	}
	stored, err := s.blobStore().Stat(ctx, ref.ID)
	if err != nil {
		return err
	}
	if stored.Size != size {
		return fmt.Errorf("blob size %d does not match object size %d", stored.Size, size)
	}
	return nil
}

// changed records a mutation. Callers hold s.mu for writing.
func (s *Store) changed() { s.revision++ }

// persist saves the catalog if it has changed since the last save. Callers
// have released s.mu, so no disk I/O happens under the catalog lock.
//
// Saves are serialized, and each writes the newest revision rather than the
// caller's own, so a slow save can never overwrite a newer snapshot, and
// callers that queue behind one save find their change already written.
//
// A failed save is returned to the caller: the change is visible in this
// process but not durable. The next successful save includes it.
func (s *Store) persist(ctx context.Context) error {
	if s.state == nil {
		return nil
	}
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.RLock()
	if s.revision <= s.persisted {
		s.mu.RUnlock()
		return nil
	}
	revision := s.revision
	snapshot := s.snapshotLocked()
	s.mu.RUnlock()
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if err := s.state.Save(context.WithoutCancel(ctx), catalogStateKey, data); err != nil {
		return fmt.Errorf("s3: save catalog: %w", err)
	}
	s.persisted = revision
	return nil
}

func (s *Store) snapshotLocked() catalogSnapshot {
	snapshot := catalogSnapshot{Version: catalogVersion}
	for name, bucket := range s.buckets {
		saved := savedBucket{Name: name, Created: bucket.created}
		for _, object := range bucket.objects {
			saved.Objects = append(saved.Objects, cloneObject(object))
		}
		sort.Slice(saved.Objects, func(i, j int) bool { return saved.Objects[i].Key < saved.Objects[j].Key })
		snapshot.Buckets = append(snapshot.Buckets, saved)
	}
	sort.Slice(snapshot.Buckets, func(i, j int) bool { return snapshot.Buckets[i].Name < snapshot.Buckets[j].Name })
	for _, upload := range s.uploads {
		saved := savedUpload{Upload: upload.MultipartUpload, Options: clonePutOptions(upload.opts)}
		for _, part := range upload.parts {
			saved.Parts = append(saved.Parts, part)
		}
		sort.Slice(saved.Parts, func(i, j int) bool { return saved.Parts[i].Number < saved.Parts[j].Number })
		snapshot.Uploads = append(snapshot.Uploads, saved)
	}
	sort.Slice(snapshot.Uploads, func(i, j int) bool { return snapshot.Uploads[i].Upload.ID < snapshot.Uploads[j].Upload.ID })
	return snapshot
}
