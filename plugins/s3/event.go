package s3

import (
	"encoding/json"
	"fmt"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
)

// Logical state-change events emitted by Session.
const (
	EventBucketCreate = "s3.bucket.create"
	EventBucketDelete = "s3.bucket.delete"
	EventObjectPut    = "s3.object.put"
	EventObjectCopy   = "s3.object.copy"
	EventObjectDelete = "s3.object.delete"
)

// EventTypes is every logical event Session emits.
var EventTypes = []string{
	EventBucketCreate,
	EventBucketDelete,
	EventObjectPut,
	EventObjectCopy,
	EventObjectDelete,
}

// Payload is the canonical payload of an S3 state-change event.
type Payload struct {
	Operation   string          `json:"operation"`
	Bucket      string          `json:"bucket"`
	Key         string          `json:"key,omitempty"`
	Source      *ObjectLocation `json:"source,omitempty"`
	Size        int64           `json:"size,omitempty"`
	ETag        string          `json:"etag,omitempty"`
	ContentType string          `json:"content_type,omitempty"`
	Blob        *blob.Ref       `json:"blob,omitempty"`
}

func (p *Payload) Title() string {
	if p == nil {
		return ""
	}
	if p.Key == "" {
		return p.Bucket
	}
	return p.Bucket + "/" + p.Key
}

func (p *Payload) Snippet() string {
	if p == nil {
		return ""
	}
	target := p.Title()
	switch p.Operation {
	case "bucket.create":
		return "created bucket " + p.Bucket
	case "bucket.delete":
		return "deleted bucket " + p.Bucket
	case "object.put":
		return fmt.Sprintf("stored s3://%s (%d bytes)", target, p.Size)
	case "object.copy":
		if p.Source != nil {
			return fmt.Sprintf("copied s3://%s/%s to s3://%s (%d bytes)", p.Source.Bucket, p.Source.Key, target, p.Size)
		}
		return fmt.Sprintf("copied object to s3://%s (%d bytes)", target, p.Size)
	case "object.delete":
		return "deleted s3://" + target
	default:
		return p.Operation + " " + target
	}
}

// PayloadOf accepts the in-process pointer or value and JSON-round-tripped payloads.
func PayloadOf(e *event.Event) (*Payload, bool) {
	if e == nil || e.Payload == nil {
		return nil, false
	}
	var payload Payload
	switch value := e.Payload.(type) {
	case *Payload:
		if value == nil {
			return nil, false
		}
		payload = *value
		if value.Source != nil {
			source := *value.Source
			payload.Source = &source
		}
		if value.Blob != nil {
			ref := *value.Blob
			payload.Blob = &ref
		}
	case Payload:
		payload = value
	default:
		encoded, err := json.Marshal(value)
		if err != nil || json.Unmarshal(encoded, &payload) != nil {
			return nil, false
		}
	}
	if payload.Operation == "" || payload.Bucket == "" {
		return nil, false
	}
	return &payload, true
}

// IsS3Event reports whether e is one of the logical events emitted here.
func IsS3Event(e *event.Event) bool {
	if e == nil {
		return false
	}
	for _, typ := range EventTypes {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// EventOption adjusts an event immediately before it is appended.
type EventOption func(*event.Event)

// WithEventRaw preserves the provider's untouched request or protocol record.
func WithEventRaw(raw event.Raw) EventOption {
	return func(e *event.Event) { e.Raw = raw }
}

// WithEventMeta adds provider-specific metadata without changing the payload.
func WithEventMeta(key string, value any) EventOption {
	return func(e *event.Event) {
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta[key] = value
	}
}
