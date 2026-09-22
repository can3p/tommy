package s3_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	blobmem "github.com/can3p/tommy/core/blob/memory"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/store"
	storemem "github.com/can3p/tommy/core/store/memory"
	"github.com/can3p/tommy/plugins/s3"
)

func newSession(t *testing.T, eventCapacity int) (*s3.Session, *s3.Store, *storemem.Store, *blobmem.Store) {
	t.Helper()
	events := storemem.New(eventCapacity)
	blobs := blobmem.New(16 << 20)
	catalog := s3.NewStore()
	session := s3.NewSession(catalog, plugin.Deps{Store: events, Blobs: blobs},
		s3.WithProvider("fake"), s3.WithSessionMeta(map[string]any{"account": "test"}))
	return session, catalog, events, blobs
}

func TestSessionRecordsLogicalEventsAndPreservesOptions(t *testing.T) {
	session, _, events, _ := newSession(t, 20)
	collector := plugin.NewEventCollector()
	ctx := plugin.WithEventCollector(context.Background(), collector)
	raw := event.Raw{
		Transport: "http", Method: http.MethodPut, Path: "/bucket/a//b",
		Headers: http.Header{"X-Test": {"untouched"}}, Body: []byte("raw-body"), Text: true,
	}
	if _, err := session.CreateBucket(ctx, "bucket", s3.WithEventMeta("request_id", "one")); err != nil {
		t.Fatal(err)
	}
	put, err := session.PutObject(ctx, "bucket", "a//b", bytes.NewBufferString("body"), s3.PutOptions{Headers: s3.ContentHeaders{ContentType: "text/plain"}},
		s3.WithEventRaw(raw), s3.WithEventMeta("request_id", "two"))
	if err != nil {
		t.Fatal(err)
	}
	copy, err := session.CopyObject(ctx, s3.ObjectLocation{Bucket: "bucket", Key: "a//b"}, s3.ObjectLocation{Bucket: "bucket", Key: "copy"}, s3.CopyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, removed, err := session.DeleteObject(ctx, "bucket", "missing"); err != nil || removed {
		t.Fatalf("missing delete = %v, %v", removed, err)
	}
	removed, err := session.DeleteObjects(ctx, "bucket", []string{"copy", "missing", "a//b"})
	if err != nil || len(removed) != 2 {
		t.Fatalf("DeleteObjects = %#v, %v", removed, err)
	}
	if _, err := session.DeleteBucket(ctx, "bucket", false); err != nil {
		t.Fatal(err)
	}

	got, err := events.List(context.Background(), store.Query{Plugin: s3.PluginName})
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{
		s3.EventBucketDelete,
		s3.EventObjectDelete,
		s3.EventObjectDelete,
		s3.EventObjectCopy,
		s3.EventObjectPut,
		s3.EventBucketCreate,
	}
	if len(got) != len(wantTypes) {
		t.Fatalf("recorded %d events: %#v", len(got), got)
	}
	for i, typ := range wantTypes {
		if got[i].Type != typ || !s3.IsS3Event(got[i]) {
			t.Errorf("event %d type = %q", i, got[i].Type)
		}
		if _, ok := s3.PayloadOf(got[i]); !ok {
			t.Errorf("event %d payload does not decode", i)
		}
	}
	putEvent := got[4]
	payload, _ := s3.PayloadOf(putEvent)
	if payload.Bucket != "bucket" || payload.Key != "a//b" || payload.Size != put.Size || payload.ETag != put.ETag || payload.Blob == nil || payload.ContentType != "text/plain" {
		t.Fatalf("put payload = %#v", payload)
	}
	if !reflect.DeepEqual(putEvent.Raw, raw) {
		t.Fatalf("raw changed: %#v", putEvent.Raw)
	}
	if putEvent.Meta["account"] != "test" || putEvent.Meta["request_id"] != "two" {
		t.Fatalf("meta = %#v", putEvent.Meta)
	}
	copyEvent := got[3]
	copyPayload, _ := s3.PayloadOf(copyEvent)
	if copyPayload.Source == nil || copyPayload.Source.Key != "a//b" || copyPayload.Key != copy.Key {
		t.Fatalf("copy payload = %#v", copyPayload)
	}
	found, _ := events.List(context.Background(), store.Query{Plugin: s3.PluginName, Search: "a//b"})
	if len(found) == 0 {
		t.Fatal("events are not searchable by exact key")
	}
	if len(collector.IDs()) != len(wantTypes) {
		t.Fatalf("caller context collected %d ids, want %d", len(collector.IDs()), len(wantTypes))
	}
}

func TestPayloadJSONRoundTrip(t *testing.T) {
	original := &event.Event{Payload: &s3.Payload{
		Operation: "object.copy", Bucket: "destination", Key: "dots/../and//slashes", Size: 12, ETag: "etag",
		Source: &s3.ObjectLocation{Bucket: "source", Key: "source/key"},
	}}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded event.Event
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	payload, ok := s3.PayloadOf(&decoded)
	if !ok || payload.Key != "dots/../and//slashes" || payload.Source == nil || payload.Source.Bucket != "source" {
		t.Fatalf("decoded payload = %#v, %v", payload, ok)
	}
}

func TestObjectsOutliveEventEviction(t *testing.T) {
	session, catalog, events, blobs := newSession(t, 1)
	ctx := context.Background()
	if _, err := session.CreateBucket(ctx, "bucket"); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"one", "two", "three"} {
		if _, err := session.PutObject(ctx, "bucket", key, bytes.NewBufferString(key), s3.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if events.Len() != 1 {
		t.Fatalf("event ring retained %d events", events.Len())
	}
	for _, key := range []string{"one", "two", "three"} {
		data, _, err := catalog.GetObject(ctx, "bucket", key)
		if err != nil || string(data) != key {
			t.Fatalf("object %q after eviction = %q, %v", key, data, err)
		}
	}
	if blobs.Len() != 3 {
		t.Fatalf("event eviction changed blob state: %d blobs", blobs.Len())
	}
}

func TestMultipartPlumbingEmitsOnlyExplicitCompletionEvent(t *testing.T) {
	session, _, events, _ := newSession(t, 10)
	ctx := context.Background()
	if _, err := session.CreateBucket(ctx, "bucket"); err != nil {
		t.Fatal(err)
	}
	if err := events.Clear(ctx, s3.PluginName); err != nil {
		t.Fatal(err)
	}
	upload, err := session.CreateMultipart("bucket", "key", s3.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	part, err := session.PutPart(ctx, upload.ID, 1, bytes.NewBufferString("whole"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.ListParts(upload.ID); err != nil {
		t.Fatal(err)
	}
	obj, err := session.CompleteMultipart(ctx, upload.ID, []s3.CompletedPart{{Number: 1, ETag: part.ETag}})
	if err != nil {
		t.Fatal(err)
	}
	if events.Len() != 0 {
		t.Fatalf("multipart plumbing emitted %d events", events.Len())
	}
	if err := session.RecordObjectPut(ctx, obj); err != nil {
		t.Fatal(err)
	}
	got, err := events.List(ctx, store.Query{Plugin: s3.PluginName})
	if err != nil || len(got) != 1 || got[0].Type != s3.EventObjectPut {
		t.Fatalf("completion events = %#v, %v", got, err)
	}
}
