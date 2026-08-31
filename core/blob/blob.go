// Package blob stores the bytes that must never live inside an Event.
//
// Mail attachments and uploaded files hold a Ref, never a []byte. That caps
// memory in one place, keeps event JSON small, and - crucially - gives blobs a
// lifetime independent of the event ring buffer, so evicting old events never
// deletes a file the user is about to download.
package blob

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned by Open, Stat and Delete for an unknown id.
var ErrNotFound = errors.New("blob: not found")

// ErrCapacityExceeded is returned by Put when storing the blob would push the
// store past its configured limit. Blobs are never evicted to make room: the
// event log is history, the blob store is state.
var ErrCapacityExceeded = errors.New("blob: capacity exceeded")

// Ref describes a stored blob. It is what plugins embed in their models.
type Ref struct {
	ID          string `json:"id"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type,omitempty"`
	Filename    string `json:"filename,omitempty"`
}

// BlobStore holds blob bytes.
type BlobStore interface {
	// Put reads r to completion and stores it. meta supplies ContentType and
	// Filename; meta.ID is used when set, otherwise one is generated. The
	// returned Ref carries the final id and the real size.
	Put(ctx context.Context, r io.Reader, meta Ref) (Ref, error)
	// Open returns a reader positioned at the start of the blob. The caller
	// closes it. ReadSeekCloser so the API can serve range requests.
	Open(ctx context.Context, id string) (io.ReadSeekCloser, Ref, error)
	Delete(ctx context.Context, id string) error
	Stat(ctx context.Context, id string) (Ref, error)
}
