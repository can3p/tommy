package http

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	stdhttp "net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	blobmem "github.com/can3p/tommy/core/blob/memory"
	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	corestore "github.com/can3p/tommy/core/store"
	storemem "github.com/can3p/tommy/core/store/memory"
	"github.com/can3p/tommy/plugins/s3"
)

var testNow = time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

type testServer struct {
	provider *Provider
	baseURL  string
	client   *stdhttp.Client
	store    *s3.Store
	events   *storemem.Store
	cancel   context.CancelFunc
	done     chan error
}

func startTestServer(t *testing.T, values map[string]any) *testServer {
	return startTestServerWithStore(t, values, nil)
}

func startTestServerWithStore(t *testing.T, values map[string]any, catalog *s3.Store) *testServer {
	t.Helper()
	if values == nil {
		values = map[string]any{}
	}
	values["bind"] = "127.0.0.1"
	values["port"] = 0
	values["shutdown_timeout"] = 2
	var ids atomic.Int64
	if catalog == nil {
		catalog = s3.NewStore(s3.WithClock(func() time.Time { return testNow }), s3.WithIDFunc(func() string { return fmt.Sprintf("upload-%d", ids.Add(1)) }))
	}
	p := New()
	p.BindStore(catalog)
	events := storemem.New(100)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	deps := plugin.Deps{
		Store: events, Blobs: blobmem.New(256 << 20), Config: config.NewProviderConfig(values),
		Now: func() time.Time { return testNow }, NewID: func() string { return fmt.Sprintf("event-%d", ids.Add(1)) },
	}
	go func() { done <- p.Listen(ctx, deps) }()
	addr, err := p.Addr(3 * time.Second)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	ts := &testServer{provider: p, baseURL: "http://" + addr, client: &stdhttp.Client{Timeout: 5 * time.Second}, store: catalog, events: events, cancel: cancel, done: done}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("listener stopped with error: %v", err)
			}
		case <-time.After(4 * time.Second):
			t.Error("listener did not stop gracefully")
		}
	})
	return ts
}

func (s *testServer) request(t *testing.T, method, path string, body []byte, headers map[string]string) *stdhttp.Response {
	t.Helper()
	req, err := stdhttp.NewRequest(method, s.baseURL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func responseBody(t *testing.T, response *stdhttp.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func requireStatus(t *testing.T, response *stdhttp.Response, want int) string {
	t.Helper()
	body := responseBody(t, response)
	if response.StatusCode != want {
		t.Fatalf("status = %d, want %d; body = %s", response.StatusCode, want, body)
	}
	if response.Header.Get("Server") != "tommy-s3" || response.Header.Get("x-amz-request-id") == "" || response.Header.Get("x-amz-id-2") == "" {
		t.Fatalf("missing S3 response headers: %#v", response.Header)
	}
	return body
}

func TestConformance(t *testing.T) {
	plugintest.ConformanceProvider(t, New())
}

func TestConfigDefaultsOverridesAndEphemeralAddress(t *testing.T) {
	defaults, err := LoadConfig(plugin.ProviderConfig{})
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if defaults.Bind != DefaultBind || defaults.Port != DefaultPort || defaults.MaxObjectBytes != s3.DefaultLimits.MaxObjectBytes || defaults.MaxObjectBytes != DefaultMaxObjectBytes || defaults.MaxXMLBytes != DefaultMaxXMLBytes {
		t.Fatalf("defaults = %#v", defaults)
	}
	configured, err := LoadConfig(config.NewProviderConfig(map[string]any{
		"bind": "0.0.0.0", "port": 0, "buckets": []string{" first ", "second"}, "max_object_bytes": 1234, "max_xml_bytes": 456,
		"read_header_timeout": 6, "read_timeout": 7, "write_timeout": 8, "idle_timeout": 9, "shutdown_timeout": 10,
	}))
	if err != nil {
		t.Fatalf("configured: %v", err)
	}
	if configured.ListenAddr() != "0.0.0.0:0" || configured.MaxObjectBytes != 1234 || configured.Limits.MaxObjectBytes != 1234 || configured.MaxXMLBytes != 456 || configured.ReadHeaderTimeout != 6*time.Second || configured.ReadTimeout != 7*time.Second || configured.WriteTimeout != 8*time.Second {
		t.Fatalf("configured = %#v", configured)
	}
	if fmt.Sprint(configured.Buckets) != "[first second]" {
		t.Fatalf("buckets = %q", configured.Buckets)
	}
	if got := New().ListenPort(config.NewProviderConfig(map[string]any{"port": 0})); got.Port != 0 || got.Network != "tcp" {
		t.Fatalf("ephemeral ListenPort = %#v", got)
	}

	server := startTestServer(t, nil)
	if strings.HasSuffix(server.baseURL, ":9000") || strings.HasSuffix(server.baseURL, ":0") {
		t.Fatalf("test listener did not use a resolved ephemeral address: %s", server.baseURL)
	}
}

func TestConfigRejectsInvalidBucketLists(t *testing.T) {
	for name, buckets := range map[string][]string{
		"empty":     {"valid", " "},
		"duplicate": {"same", " same "},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(config.NewProviderConfig(map[string]any{"buckets": buckets})); err == nil {
				t.Fatal("expected invalid buckets to fail")
			}
		})
	}
}

func TestConfiguredBucketsAreValidatedBeforeCatalogMutation(t *testing.T) {
	catalog := s3.NewStore()
	provider := New()
	provider.BindStore(catalog)
	err := provider.Listen(context.Background(), plugin.Deps{Config: config.NewProviderConfig(map[string]any{
		"buckets": []string{"one", "two"}, "max_buckets": 1,
	})})
	if !errors.Is(err, s3.ErrBucketLimit) {
		t.Fatalf("err = %v, want bucket limit", err)
	}
	if got := catalog.Stats().Buckets; got != 0 {
		t.Fatalf("catalog has %d buckets after validation failure", got)
	}
}

func TestConfiguredBucketsExistBeforeListenerReadinessWithoutEvents(t *testing.T) {
	catalog := s3.NewStore()
	if _, err := catalog.CreateBucket("existing"); err != nil {
		t.Fatal(err)
	}
	server := startTestServerWithStore(t, map[string]any{"buckets": []string{"existing", "media", "exports"}}, catalog)
	for _, name := range []string{"existing", "media", "exports"} {
		if _, err := server.store.HeadBucket(name); err != nil {
			t.Errorf("configured bucket %q is unavailable after listener readiness: %v", name, err)
		}
	}
	events, err := server.events.List(context.Background(), corestore.Query{Plugin: s3.PluginName})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("configured buckets emitted %d events", len(events))
	}
	body := requireStatus(t, server.request(t, stdhttp.MethodGet, "/", nil, nil), stdhttp.StatusOK)
	for _, name := range []string{"existing", "media", "exports"} {
		if !strings.Contains(body, "<Name>"+name+"</Name>") {
			t.Errorf("list buckets response does not include %q: %s", name, body)
		}
	}
}

func TestBucketAndObjectHTTPFixtures(t *testing.T) {
	server := startTestServer(t, nil)
	if body := requireStatus(t, server.request(t, stdhttp.MethodPut, "/fixture", nil, nil), stdhttp.StatusOK); body != "" {
		t.Fatalf("create body = %q", body)
	}
	payload := []byte("hello, s3")
	checksum := sha256.Sum256(payload)
	put := server.request(t, stdhttp.MethodPut, "/fixture/a%2Fb", payload, map[string]string{
		"Content-Type": "text/plain", "x-amz-meta-owner": "test", "x-amz-checksum-sha256": base64.StdEncoding.EncodeToString(checksum[:]),
	})
	requireStatus(t, put, stdhttp.StatusOK)
	if put.Header.Get("ETag") != `"78e54b877296d63a3ff6f8dd3da312ae"` {
		t.Fatalf("etag = %q", put.Header.Get("ETag"))
	}

	list := requireStatus(t, server.request(t, stdhttp.MethodGet, "/fixture?list-type=2", nil, nil), stdhttp.StatusOK)
	want := xml.Header + `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>fixture</Name><Prefix></Prefix><KeyCount>1</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated><Contents><Key>a/b</Key><LastModified>2024-01-02T03:04:05.000Z</LastModified><ETag>&#34;78e54b877296d63a3ff6f8dd3da312ae&#34;</ETag><Size>9</Size><StorageClass>STANDARD</StorageClass></Contents></ListBucketResult>`
	if list != want {
		t.Fatalf("list fixture mismatch\n got: %s\nwant: %s", list, want)
	}

	get := server.request(t, stdhttp.MethodGet, "/fixture/a%2Fb", nil, map[string]string{"x-amz-checksum-mode": "ENABLED"})
	if got := requireStatus(t, get, stdhttp.StatusOK); got != string(payload) {
		t.Fatalf("get body = %q", got)
	}
	if get.Header.Get("x-amz-meta-owner") != "test" || get.Header.Get("x-amz-checksum-sha256") == "" || get.Header.Get("Last-Modified") != "Tue, 02 Jan 2024 03:04:05 GMT" {
		t.Fatalf("object headers = %#v", get.Header)
	}

	location := requireStatus(t, server.request(t, stdhttp.MethodGet, "/fixture?location", nil, nil), stdhttp.StatusOK)
	if location != xml.Header+`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>` {
		t.Fatalf("location fixture = %q", location)
	}
}

func TestListV2PagingDelimiterAndURLEncoding(t *testing.T) {
	server := startTestServer(t, nil)
	requireStatus(t, server.request(t, stdhttp.MethodPut, "/bucket", nil, nil), stdhttp.StatusOK)
	for _, key := range []string{"a/one", "a/two", "b space"} {
		requireStatus(t, server.request(t, stdhttp.MethodPut, "/bucket/"+strings.ReplaceAll(key, " ", "%20"), []byte(key), nil), stdhttp.StatusOK)
	}
	first := requireStatus(t, server.request(t, stdhttp.MethodGet, "/bucket?list-type=2&delimiter=%2F&max-keys=1&encoding-type=url", nil, nil), stdhttp.StatusOK)
	var page struct {
		IsTruncated bool   `xml:"IsTruncated"`
		NextToken   string `xml:"NextContinuationToken"`
		Prefixes    []struct {
			Prefix string `xml:"Prefix"`
		} `xml:"CommonPrefixes"`
	}
	if err := xml.Unmarshal([]byte(first), &page); err != nil || !page.IsTruncated || page.NextToken == "" || len(page.Prefixes) != 1 || page.Prefixes[0].Prefix != "a%2F" {
		t.Fatalf("first list page = %#v, body=%s, err=%v", page, first, err)
	}
	second := requireStatus(t, server.request(t, stdhttp.MethodGet, "/bucket?list-type=2&delimiter=%2F&max-keys=1&encoding-type=url&continuation-token="+page.NextToken, nil, nil), stdhttp.StatusOK)
	if !strings.Contains(second, "<Key>b%20space</Key>") || strings.Contains(second, "<Key>a%2F") {
		t.Fatalf("second list page = %s", second)
	}
	startAfter := requireStatus(t, server.request(t, stdhttp.MethodGet, "/bucket?list-type=2&start-after=a%2Ftwo", nil, nil), stdhttp.StatusOK)
	if !strings.Contains(startAfter, "<Key>b space</Key>") || strings.Contains(startAfter, "<Key>a/") {
		t.Fatalf("start-after result = %s", startAfter)
	}
}

func TestRangesConditionsChecksumsAndErrors(t *testing.T) {
	server := startTestServer(t, map[string]any{"max_object_bytes": 8})
	requireStatus(t, server.request(t, stdhttp.MethodPut, "/bucket", nil, nil), stdhttp.StatusOK)
	put := server.request(t, stdhttp.MethodPut, "/bucket/key", []byte("abcdef"), map[string]string{"Content-MD5": "AAAAAAAAAAAAAAAAAAAAAA=="})
	if body := requireStatus(t, put, stdhttp.StatusBadRequest); !strings.Contains(body, "<Code>BadDigest</Code>") {
		t.Fatalf("bad digest response = %s", body)
	}
	crc := crc32.ChecksumIEEE([]byte("abcdef"))
	crcValue := base64.StdEncoding.EncodeToString([]byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)})
	requireStatus(t, server.request(t, stdhttp.MethodPut, "/bucket/key", []byte("abcdef"), map[string]string{"x-amz-checksum-crc32": crcValue}), stdhttp.StatusOK)

	ranged := server.request(t, stdhttp.MethodGet, "/bucket/key", nil, map[string]string{"Range": "bytes=1-3"})
	if body := requireStatus(t, ranged, stdhttp.StatusPartialContent); body != "bcd" || ranged.Header.Get("Content-Range") != "bytes 1-3/6" {
		t.Fatalf("range = %q, %q", body, ranged.Header.Get("Content-Range"))
	}
	etag := ranged.Header.Get("ETag")
	if response := server.request(t, stdhttp.MethodGet, "/bucket/key", nil, map[string]string{"If-None-Match": etag}); requireStatus(t, response, stdhttp.StatusNotModified) != "" {
		t.Fatal("304 returned a body")
	}
	if response := server.request(t, stdhttp.MethodHead, "/bucket/key", nil, map[string]string{"If-Match": `"wrong"`}); requireStatus(t, response, stdhttp.StatusPreconditionFailed) != "" {
		t.Fatal("HEAD error returned a body")
	}
	if response := server.request(t, stdhttp.MethodGet, "/bucket/key", nil, map[string]string{"If-Modified-Since": testNow.Add(time.Hour).Format(stdhttp.TimeFormat)}); requireStatus(t, response, stdhttp.StatusNotModified) != "" {
		t.Fatal("date condition returned a body")
	}
	if body := requireStatus(t, server.request(t, stdhttp.MethodPut, "/bucket/large", []byte("123456789"), nil), stdhttp.StatusBadRequest); !strings.Contains(body, "EntityTooLarge") {
		t.Fatalf("oversize response = %s", body)
	}
	if body := requireStatus(t, server.request(t, stdhttp.MethodPut, "/bucket/chunked", []byte("framing"), map[string]string{"Content-Encoding": "aws-chunked"}), stdhttp.StatusNotImplemented); !strings.Contains(body, "aws-chunked") {
		t.Fatalf("aws-chunked response = %s", body)
	}
	if body := requireStatus(t, server.request(t, stdhttp.MethodGet, "/bucket/missing", nil, nil), stdhttp.StatusNotFound); !strings.Contains(body, "<Code>NoSuchKey</Code>") {
		t.Fatalf("missing response = %s", body)
	}
}

func TestS3ErrorResponseTableOverSocket(t *testing.T) {
	server := startTestServer(t, nil)
	requireStatus(t, server.request(t, stdhttp.MethodPut, "/full", nil, nil), stdhttp.StatusOK)
	requireStatus(t, server.request(t, stdhttp.MethodPut, "/full/key", []byte("x"), nil), stdhttp.StatusOK)
	tests := []struct {
		name, method, path, code string
		status                   int
	}{
		{"missing bucket head", stdhttp.MethodHead, "/missing", "", stdhttp.StatusNotFound},
		{"missing object", stdhttp.MethodGet, "/full/missing", "NoSuchKey", stdhttp.StatusNotFound},
		{"non-empty bucket", stdhttp.MethodDelete, "/full", "BucketNotEmpty", stdhttp.StatusConflict},
		{"unsupported root method", stdhttp.MethodPost, "/", "MethodNotAllowed", stdhttp.StatusMethodNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := requireStatus(t, server.request(t, tc.method, tc.path, nil, nil), tc.status)
			if tc.code != "" && !strings.Contains(body, "<Code>"+tc.code+"</Code>") {
				t.Fatalf("error body = %s", body)
			}
		})
	}
}

func TestExactKeysCopyDeleteObjectsAndEvents(t *testing.T) {
	server := startTestServer(t, nil)
	requireStatus(t, server.request(t, stdhttp.MethodPut, "/bucket", nil, nil), stdhttp.StatusOK)
	keys := []string{"a//b", "dots/../stay", "encoded/slash"}
	for i, path := range []string{"a//b", "dots/../stay", "encoded%2Fslash"} {
		requireStatus(t, server.request(t, stdhttp.MethodPut, "/bucket/"+path, []byte(strconv.Itoa(i)), nil), stdhttp.StatusOK)
	}
	for i, key := range keys {
		data, _, err := server.store.GetObject(context.Background(), "bucket", key)
		if err != nil || string(data) != strconv.Itoa(i) {
			t.Fatalf("stored exact key %q = %q, %v", key, data, err)
		}
	}

	copyResponse := server.request(t, stdhttp.MethodPut, "/bucket/copied", nil, map[string]string{
		"x-amz-copy-source": "/bucket/encoded%2Fslash", "x-amz-metadata-directive": "REPLACE", "x-amz-meta-copy": "yes",
	})
	requireStatus(t, copyResponse, stdhttp.StatusOK)
	copied, err := server.store.HeadObject("bucket", "copied")
	if err != nil || copied.Metadata["copy"] != "yes" {
		t.Fatalf("copied object = %#v, %v", copied, err)
	}

	deleteXML := []byte(`<Delete><Object><Key>a//b</Key></Object><Object><Key>missing</Key></Object><Object><Key>copied</Key></Object></Delete>`)
	deleteResponse := requireStatus(t, server.request(t, stdhttp.MethodPost, "/bucket?delete", deleteXML, nil), stdhttp.StatusOK)
	if strings.Count(deleteResponse, "<Deleted>") != 3 {
		t.Fatalf("delete result = %s", deleteResponse)
	}
	quiet := requireStatus(t, server.request(t, stdhttp.MethodPost, "/bucket?delete", []byte(`<Delete><Object><Key>missing-again</Key></Object><Quiet>true</Quiet></Delete>`), nil), stdhttp.StatusOK)
	if strings.Contains(quiet, "<Deleted>") {
		t.Fatalf("quiet delete result = %s", quiet)
	}
	events, err := server.events.List(context.Background(), corestore.Query{Plugin: s3.PluginName})
	if err != nil {
		t.Fatal(err)
	}
	deleteEvents := 0
	for _, captured := range events {
		if captured.Type == s3.EventObjectDelete {
			deleteEvents++
			if string(captured.Raw.Body) != string(deleteXML) {
				t.Fatalf("bulk delete Raw.Body = %q", captured.Raw.Body)
			}
		}
	}
	if deleteEvents != 2 {
		t.Fatalf("delete events = %d, want only the two removed objects", deleteEvents)
	}
}

func TestMultipartCompletionAbortAndCleanup(t *testing.T) {
	server := startTestServer(t, nil)
	requireStatus(t, server.request(t, stdhttp.MethodPut, "/bucket", nil, nil), stdhttp.StatusOK)
	initBody := requireStatus(t, server.request(t, stdhttp.MethodPost, "/bucket/large?uploads", nil, map[string]string{"x-amz-meta-kind": "multipart"}), stdhttp.StatusOK)
	var initiated struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal([]byte(initBody), &initiated); err != nil || initiated.UploadID == "" {
		t.Fatalf("initiate = %q, %v", initBody, err)
	}
	if uploads := requireStatus(t, server.request(t, stdhttp.MethodGet, "/bucket?uploads", nil, nil), stdhttp.StatusOK); !strings.Contains(uploads, "<UploadId>"+initiated.UploadID+"</UploadId>") {
		t.Fatalf("list uploads = %s", uploads)
	}
	part1 := server.request(t, stdhttp.MethodPut, "/bucket/large?partNumber=1&uploadId="+initiated.UploadID, []byte("abc"), nil)
	requireStatus(t, part1, stdhttp.StatusOK)
	part2 := server.request(t, stdhttp.MethodPut, "/bucket/large?partNumber=2&uploadId="+initiated.UploadID, []byte("def"), nil)
	requireStatus(t, part2, stdhttp.StatusOK)
	if parts := requireStatus(t, server.request(t, stdhttp.MethodGet, "/bucket/large?uploadId="+initiated.UploadID, nil, nil), stdhttp.StatusOK); strings.Count(parts, "<Part>") != 2 {
		t.Fatalf("list parts = %s", parts)
	}
	completeXML := []byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>` + part1.Header.Get("ETag") + `</ETag></Part><Part><PartNumber>2</PartNumber><ETag>` + part2.Header.Get("ETag") + `</ETag></Part></CompleteMultipartUpload>`)
	requireStatus(t, server.request(t, stdhttp.MethodPost, "/bucket/large?uploadId="+initiated.UploadID, completeXML, nil), stdhttp.StatusOK)
	data, obj, err := server.store.GetObject(context.Background(), "bucket", "large")
	if err != nil || string(data) != "abcdef" || obj.Metadata["kind"] != "multipart" || server.store.Stats().ActiveUploads != 0 || server.store.Stats().Parts != 0 {
		t.Fatalf("completed object = %q %#v stats=%#v err=%v", data, obj, server.store.Stats(), err)
	}

	events, _ := server.events.List(context.Background(), corestore.Query{Type: s3.EventObjectPut})
	var completionEvents int
	for _, captured := range events {
		payload, _ := s3.PayloadOf(captured)
		if payload != nil && payload.Key == "large" {
			completionEvents++
			if string(captured.Raw.Body) != string(completeXML) {
				t.Fatalf("completion raw = %q", captured.Raw.Body)
			}
		}
	}
	if completionEvents != 1 {
		t.Fatalf("completion events = %d", completionEvents)
	}

	abortBody := requireStatus(t, server.request(t, stdhttp.MethodPost, "/bucket/abort?uploads", nil, nil), stdhttp.StatusOK)
	initiated.UploadID = ""
	_ = xml.Unmarshal([]byte(abortBody), &initiated)
	requireStatus(t, server.request(t, stdhttp.MethodPut, "/bucket/abort?partNumber=1&uploadId="+initiated.UploadID, []byte("discard"), nil), stdhttp.StatusOK)
	requireStatus(t, server.request(t, stdhttp.MethodDelete, "/bucket/abort?uploadId="+initiated.UploadID, nil, nil), stdhttp.StatusNoContent)
	if stats := server.store.Stats(); stats.ActiveUploads != 0 || stats.Parts != 0 {
		t.Fatalf("abort did not clean parts: %#v", stats)
	}
}

func TestSigV4PresignedCaptureAndFailedOperationsHaveNoEvents(t *testing.T) {
	server := startTestServer(t, nil)
	auth := "AWS4-HMAC-SHA256 Credential=AKID/20240102/eu-west-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=arbitrary"
	requireStatus(t, server.request(t, stdhttp.MethodPut, "/signed", nil, map[string]string{"Authorization": auth, "x-amz-security-token": "token"}), stdhttp.StatusOK)
	presigned := "/signed/key?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=QUERY%2F20240102%2Fap-south-1%2Fs3%2Faws4_request&X-Amz-SignedHeaders=host&X-Amz-Signature=anything&X-Amz-Security-Token=query-token"
	requireStatus(t, server.request(t, stdhttp.MethodPut, presigned, []byte("body"), nil), stdhttp.StatusOK)
	before := server.events.Len()
	requireStatus(t, server.request(t, stdhttp.MethodPut, "/missing/key", []byte("not stored"), nil), stdhttp.StatusNotFound)
	if server.events.Len() != before {
		t.Fatalf("failed operation emitted an event: before=%d after=%d", before, server.events.Len())
	}

	events, err := server.events.List(context.Background(), corestore.Query{Type: s3.EventObjectPut})
	if err != nil || len(events) != 1 {
		t.Fatalf("put events = %#v, %v", events, err)
	}
	captured := events[0]
	if captured.Meta["access_key"] != "QUERY" || captured.Meta["region"] != "ap-south-1" || captured.Meta["session_token"] != "query-token" || captured.Meta["presigned"] != true {
		t.Fatalf("presigned meta = %#v", captured.Meta)
	}
	if captured.Raw.Path != presigned || string(captured.Raw.Body) != "body" {
		t.Fatalf("raw = %#v", captured.Raw)
	}

	bucketEvents, _ := server.events.List(context.Background(), corestore.Query{Type: s3.EventBucketCreate})
	if len(bucketEvents) != 1 || bucketEvents[0].Meta["access_key"] != "AKID" || bucketEvents[0].Meta["region"] != "eu-west-1" || bucketEvents[0].Meta["session_token"] != "token" {
		t.Fatalf("authorization meta = %#v", bucketEvents)
	}
}

func TestProviderInterfaceAndPrivateFallback(t *testing.T) {
	var _ plugin.Provider = New()
	var _ plugin.ListenerProvider = New()
	var _ plugin.AddressableProvider = New()
	var _ plugin.PortProvider = New()
	var _ s3.StoreBinder = New()

	// No BindStore call: direct listener tests still get a private catalog.
	p := New()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- p.Listen(ctx, plugin.Deps{Store: storemem.New(10), Blobs: blobmem.New(1 << 20), Config: config.NewProviderConfig(map[string]any{"bind": "127.0.0.1", "port": 0})})
	}()
	addr, err := p.Addr(3 * time.Second)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	response, err := stdhttp.DefaultClient.Do(mustRequest(t, stdhttp.MethodPut, "http://"+addr+"/fallback", nil))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	requireStatus(t, response, stdhttp.StatusOK)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("private fallback listener did not stop")
	}
}

func mustRequest(t *testing.T, method, target string, body []byte) *stdhttp.Request {
	t.Helper()
	req, err := stdhttp.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}
