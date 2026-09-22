package s3_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/s3"
)

type apiHarness struct {
	*testutil.Instance
	plugin *s3.Plugin
}

func startS3(t *testing.T) *apiHarness {
	t.Helper()
	p := s3.New(&fakeProvider{})
	return &apiHarness{Instance: testutil.Start(t, nil, p), plugin: p}
}

func (h *apiHarness) session(provider string) *s3.Session {
	return s3.NewSession(h.plugin.Store(), plugin.Deps{Store: h.Store, Blobs: h.Blobs}, s3.WithProvider(provider))
}

func (h *apiHarness) put(t *testing.T, bucket, key, body string, opts s3.PutOptions) s3.Object {
	t.Helper()
	session := h.session("fake")
	if _, err := h.plugin.Store().HeadBucket(bucket); err != nil {
		if _, err := session.CreateBucket(context.Background(), bucket); err != nil {
			t.Fatal(err)
		}
	}
	object, err := session.PutObject(context.Background(), bucket, key, strings.NewReader(body), opts)
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func TestS3APIEndpointDeclarationsAndJSONViews(t *testing.T) {
	h := startS3(t)
	h.put(t, "bucket", "a//b", "one", s3.PutOptions{Metadata: map[string]string{"origin": "test"}})
	h.put(t, "bucket", "a/child/two", "two", s3.PutOptions{})

	endpoints := h.plugin.APIEndpoints()
	if len(endpoints) != 8 {
		t.Fatalf("APIEndpoints() has %d routes, want 8", len(endpoints))
	}
	for _, endpoint := range endpoints {
		if endpoint.Path == "" || endpoint.Description == "" {
			t.Fatalf("incomplete endpoint: %+v", endpoint)
		}
	}

	var buckets s3.BucketsView
	if status := h.GetJSON(h.API("/s3/buckets"), &buckets); status != http.StatusOK {
		t.Fatalf("buckets status = %d", status)
	}
	if buckets.Stats.Buckets != 1 || buckets.Stats.Objects != 2 || len(buckets.Buckets) != 1 {
		t.Fatalf("buckets = %+v", buckets)
	}
	if buckets.Buckets[0].Links.Objects != "/api/v1/s3/buckets/bucket/objects" {
		t.Errorf("objects link = %q", buckets.Buckets[0].Links.Objects)
	}

	var objects s3.ObjectsView
	listing := h.API("/s3/buckets/bucket/objects?prefix=" + url.QueryEscape("a/") + "&delimiter=" + url.QueryEscape("/"))
	if status := h.GetJSON(listing, &objects); status != http.StatusOK {
		t.Fatalf("objects status = %d", status)
	}
	if len(objects.CommonPrefixes) != 2 || objects.CommonPrefixes[0].Prefix != "a//" || objects.CommonPrefixes[1].Prefix != "a/child/" {
		t.Fatalf("common prefixes = %+v", objects.CommonPrefixes)
	}

	keyURL := h.API("/s3/buckets/bucket/objects/" + encodedPathValue("a//b"))
	var object s3.ObjectView
	if status := h.GetJSON(keyURL, &object); status != http.StatusOK {
		t.Fatalf("object status = %d", status)
	}
	if object.Key != "a//b" || object.Metadata["origin"] != "test" || !strings.Contains(object.Links.Content, "a%2F%2Fb") {
		t.Fatalf("object = %+v", object)
	}
}

func TestS3APIByteAndRangeDownload(t *testing.T) {
	h := startS3(t)
	payload := make([]byte, 512)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	modified := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	session := h.session("fake")
	if _, err := session.CreateBucket(context.Background(), "bucket"); err != nil {
		t.Fatal(err)
	}
	object, err := session.PutObject(context.Background(), "bucket", "dots/../and//slashes", bytes.NewReader(payload), s3.PutOptions{
		Metadata:  map[string]string{"origin": "fixture"},
		Checksums: s3.Checksums{SHA256: "checksum"},
		Headers:   s3.ContentHeaders{ContentType: "application/x-test", CacheControl: "no-cache", ContentLanguage: "en"}, ModTime: modified,
	})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := h.API("/s3/buckets/bucket/content/" + encodedPathValue(object.Key))

	resp := h.Get(endpoint)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, payload) {
		t.Fatalf("download = %d, %d bytes", resp.StatusCode, len(body))
	}
	if resp.ContentLength != int64(len(payload)) || resp.Header.Get("ETag") != `"`+object.ETag+`"` || resp.Header.Get("Content-Type") != "application/x-test" {
		t.Errorf("headers = %#v", resp.Header)
	}
	if resp.Header.Get("Last-Modified") != modified.Format(http.TimeFormat) || resp.Header.Get("Cache-Control") != "no-cache" ||
		resp.Header.Get("X-Amz-Meta-Origin") != "fixture" || resp.Header.Get("X-Amz-Checksum-Sha256") != "checksum" {
		t.Errorf("metadata headers = %#v", resp.Header)
	}

	req, _ := http.NewRequest(http.MethodGet, endpoint, nil)
	req.Header.Set("Range", "bytes=100-199")
	ranged := h.Do(req)
	rangeBody, _ := io.ReadAll(ranged.Body)
	_ = ranged.Body.Close()
	if ranged.StatusCode != http.StatusPartialContent || !bytes.Equal(rangeBody, payload[100:200]) || ranged.Header.Get("Content-Range") != "bytes 100-199/512" {
		t.Fatalf("range = %d %#v %d bytes", ranged.StatusCode, ranged.Header, len(rangeBody))
	}
}

// TestS3APIContentNeverRendersUploadedHTML pins the download route to an
// attachment under a sandbox CSP: it shares the UI's origin, so an uploaded
// text/html object stored with "inline" must not be able to run as tommy.
func TestS3APIContentNeverRendersUploadedHTML(t *testing.T) {
	h := startS3(t)
	session := h.session("fake")
	if _, err := session.CreateBucket(context.Background(), "bucket"); err != nil {
		t.Fatal(err)
	}
	if _, err := session.PutObject(context.Background(), "bucket", "site/evil.html", strings.NewReader("<script>alert(1)</script>"), s3.PutOptions{
		Headers: s3.ContentHeaders{ContentType: "text/html", ContentDisposition: "inline"},
	}); err != nil {
		t.Fatal(err)
	}
	resp := h.Get(h.API("/s3/buckets/bucket/content/site/evil.html"))
	_ = resp.Body.Close()
	if got := resp.Header.Get("Content-Disposition"); got != `attachment; filename=evil.html` {
		t.Errorf("Content-Disposition = %q, want an attachment", got)
	}
	if got := resp.Header.Get("Content-Security-Policy"); !strings.Contains(got, "sandbox") {
		t.Errorf("Content-Security-Policy = %q, want a sandbox", got)
	}
}

func TestS3APIDeletesRecordRawEventsAndStateOutlivesHistory(t *testing.T) {
	h := startS3(t)
	h.put(t, "bucket", "keep//../object", "still here", s3.PutOptions{})
	h.put(t, "bucket", "delete/me", "gone", s3.PutOptions{})

	deleteURL := h.API("/s3/buckets/bucket/objects/" + encodedPathValue("delete/me"))
	req, _ := http.NewRequest(http.MethodDelete, deleteURL, nil)
	req.Header.Set("X-Test", "raw")
	resp := h.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	events := h.Events(store.Query{Plugin: s3.PluginName, Type: s3.EventObjectDelete})
	if len(events) != 1 || events[0].Raw.Method != http.MethodDelete || events[0].Raw.Headers.Get("X-Test") != "raw" {
		t.Fatalf("delete events = %+v", events)
	}

	if err := h.Store.Clear(context.Background(), s3.PluginName); err != nil {
		t.Fatal(err)
	}
	if got := h.Events(store.Query{Plugin: s3.PluginName}); len(got) != 0 {
		t.Fatalf("events survived explicit eviction: %d", len(got))
	}
	status, body := h.GetBody(h.API("/s3/buckets/bucket/content/" + encodedPathValue("keep//../object")))
	if status != http.StatusOK || body != "still here" {
		t.Fatalf("download after event eviction = %d %q", status, body)
	}

	bucketDelete, _ := http.NewRequest(http.MethodDelete, h.API("/s3/buckets/bucket?recursive=1"), nil)
	deleted := h.Do(bucketDelete)
	_ = deleted.Body.Close()
	if deleted.StatusCode != http.StatusNoContent || h.plugin.Store().Stats().Buckets != 0 {
		t.Fatalf("recursive bucket delete = %d, stats %+v", deleted.StatusCode, h.plugin.Store().Stats())
	}
}

func TestS3APIErrorShape(t *testing.T) {
	h := startS3(t)
	resp := h.Get(h.API("/s3/buckets/missing"))
	defer func() { _ = resp.Body.Close() }()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound || body["error"] == "" {
		t.Fatalf("error = %d %+v", resp.StatusCode, body)
	}
}

func encodedPathValue(value string) string {
	return strings.ReplaceAll(url.PathEscape(value), ".", "%2E")
}
