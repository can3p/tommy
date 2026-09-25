//go:build integration

// S3 integration tests drive tommy's dedicated path-style S3 HTTP listener
// with the official AWS SDK for Go v2 - the client real applications run -
// covering the operations an application under test actually issues: bucket
// and object CRUD, conditional/range reads, prefix/delimiter listing, copy,
// bulk delete, a genuine multipart upload through the transfer manager, and
// presigned URLs driven with plain net/http. Getting a 200 back is necessary
// but not sufficient: the real proof of fidelity is that the SDK's own
// decoder is happy with the wire shape, so every assertion here goes through
// the generated SDK types rather than raw HTTP.
//
// The default-config test (TestS3SDKDefaultConfigFullLifecycle) deliberately
// does NOT set RequestChecksumCalculation: recent SDK versions default that
// to WhenSupported, which is exactly the configuration an application that
// never heard of tommy would run with. A separate test
// (TestS3SDKChecksumWhenRequiredWorkaroundLifecycle) additionally covers the
// WhenRequired workaround, in addition to, not instead of, the default one.
package integration

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	tommyconfig "github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	tommys3 "github.com/can3p/tommy/plugins/s3"
	s3http "github.com/can3p/tommy/plugins/s3/providers/http"
)

// s3AccessKey and s3SecretKey match the values docs/clients.md and the
// provider's own README/snippets use - tommy accepts any credentials by
// default (rule 1), so any value would work, but matching the documented
// ones is what a reader who followed the README would actually type.
const (
	s3AccessKey = "tommy"
	s3SecretKey = "tommy"
)

// waitForS3Addr polls until the s3 http listener has bound and published its
// address. Mirrors waitForSMTPAddr: listener providers bind asynchronously
// (core/server.Start starts every ListenerProvider concurrently and resolves
// addresses in its own goroutine), so it is not guaranteed to be ready the
// instant startTommy returns.
func waitForS3Addr(t *testing.T, inst *testutil.Instance, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if addr := inst.Server.SnippetCtx().Addr(tommys3.PluginName, s3http.ProviderName); addr != "" {
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatal("s3 http listener address never resolved")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// newS3Client builds an s3.Client pointed at tommy's path-style listener
// exactly as the provider's own snippet and README document: BaseEndpoint
// plus UsePathStyle, static credentials, region us-east-1. optFns lets a
// specific test layer on extra s3.Options, e.g. an explicit
// RequestChecksumCalculation.
func newS3Client(t *testing.T, inst *testutil.Instance, optFns ...func(*s3.Options)) *s3.Client {
	t.Helper()
	addr := waitForS3Addr(t, inst, 3*time.Second)
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(s3AccessKey, s3SecretKey, "")),
	)
	if err != nil {
		t.Fatalf("LoadDefaultConfig: %v", err)
	}
	base := []func(*s3.Options){
		func(o *s3.Options) {
			o.BaseEndpoint = aws.String("http://" + addr)
			o.UsePathStyle = true
		},
	}
	return s3.NewFromConfig(cfg, append(base, optFns...)...)
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

func TestS3SDKConfiguredBucketsExistWithoutEvents(t *testing.T) {
	cfg := tommyconfig.Ephemeral()
	cfg.SetProvider(tommys3.PluginName, s3http.ProviderName, tommyconfig.NewProviderConfig(map[string]any{
		"port": 0, "buckets": []string{"media", "exports"},
	}))
	inst := testutil.Start(t, cfg, tommys3.New(s3http.New()))
	client := newS3Client(t, inst)
	ctx := context.Background()
	for _, name := range []string{"media", "exports"} {
		if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)}); err != nil {
			t.Fatalf("HeadBucket(%q): %v", name, err)
		}
	}
	listed, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(listed.Buckets))
	for _, bucket := range listed.Buckets {
		got = append(got, aws.ToString(bucket.Name))
	}
	sort.Strings(got)
	if fmt.Sprint(got) != "[exports media]" {
		t.Fatalf("buckets = %v", got)
	}
	if events := inst.Events(store.Query{Plugin: tommys3.PluginName}); len(events) != 0 {
		t.Fatalf("configured buckets emitted %d events", len(events))
	}
}

// s3EventPayload waits for at least n events of typ and returns the payload
// of the most recently captured one that matches bucket/key, failing the
// test if none does.
func s3EventPayload(t *testing.T, inst *testutil.Instance, typ string, n int, bucket, key string) *tommys3.Payload {
	t.Helper()
	events := inst.WaitForEvents(n, store.Query{Plugin: tommys3.PluginName, Provider: s3http.ProviderName, Type: typ}, 3*time.Second)
	for _, e := range events {
		payload, ok := tommys3.PayloadOf(e)
		if !ok {
			continue
		}
		if payload.Bucket == bucket && payload.Key == key {
			return payload
		}
	}
	t.Fatalf("no %s event found for %s/%s among %d events", typ, bucket, key, len(events))
	return nil
}

// TestS3SDKDefaultConfigFullLifecycle drives every major S3 operation through
// the official SDK's default configuration - no RequestChecksumCalculation
// override - because that is what an application that has never heard of
// tommy actually runs. If this fails because tommy rejects the SDK's default
// aws-chunked/trailing-checksum framing, that is a real provider bug against
// the single most important client and must be fixed, not worked around.
func TestS3SDKDefaultConfigFullLifecycle(t *testing.T) {
	inst := startTommy(t)
	client := newS3Client(t, inst)
	ctx := context.Background()

	const bucket = "tommy-integration"

	// --- CreateBucket ---
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	createPayload := s3EventPayload(t, inst, tommys3.EventBucketCreate, 1, bucket, "")
	if createPayload.Operation != "bucket.create" {
		t.Errorf("bucket.create payload Operation = %q", createPayload.Operation)
	}

	// --- HeadBucket ---
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("HeadBucket: %v", err)
	}

	// --- ListBuckets ---
	listBuckets, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	var found bool
	for _, b := range listBuckets.Buckets {
		if b.Name != nil && *b.Name == bucket {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListBuckets did not include %q: %+v", bucket, listBuckets.Buckets)
	}

	// --- PutObject with metadata and content type ---
	const key = "hello.txt"
	body := []byte("captured by tommy\n")
	putOut, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("text/plain"),
		Metadata:    map[string]string{"owner": "integration-test"},
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	wantETag := `"` + md5Hex(body) + `"`
	if putOut.ETag == nil || *putOut.ETag != wantETag {
		t.Errorf("PutObject ETag = %v, want %s", putOut.ETag, wantETag)
	}

	putPayload := s3EventPayload(t, inst, tommys3.EventObjectPut, 1, bucket, key)
	if putPayload.Size != int64(len(body)) {
		t.Errorf("put event Size = %d, want %d", putPayload.Size, len(body))
	}
	if putPayload.ContentType != "text/plain" {
		t.Errorf("put event ContentType = %q, want text/plain", putPayload.ContentType)
	}
	if putPayload.Blob == nil {
		t.Fatal("put event Blob is nil")
	}
	blobContent, blobRef := readBlob(t, inst, putPayload.Blob.ID)
	if blobContent != string(body) {
		t.Errorf("blob content = %q, want %q", blobContent, string(body))
	}
	if blobRef.Size != int64(len(body)) {
		t.Errorf("blob Ref.Size = %d, want %d", blobRef.Size, len(body))
	}

	// --- HeadObject ---
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if head.ContentLength == nil || *head.ContentLength != int64(len(body)) {
		t.Errorf("HeadObject ContentLength = %v, want %d", head.ContentLength, len(body))
	}
	if head.ContentType == nil || *head.ContentType != "text/plain" {
		t.Errorf("HeadObject ContentType = %v, want text/plain", head.ContentType)
	}
	if head.Metadata["owner"] != "integration-test" {
		t.Errorf("HeadObject Metadata[owner] = %q, want integration-test", head.Metadata["owner"])
	}

	// --- GetObject (full) ---
	getOut, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	gotBody, err := io.ReadAll(getOut.Body)
	_ = getOut.Body.Close()
	if err != nil {
		t.Fatalf("read GetObject body: %v", err)
	}
	if string(gotBody) != string(body) {
		t.Errorf("GetObject body = %q, want %q", gotBody, body)
	}
	if getOut.Metadata["owner"] != "integration-test" {
		t.Errorf("GetObject Metadata[owner] = %q, want integration-test", getOut.Metadata["owner"])
	}

	// --- GetObject with Range ---
	rangeOut, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Range: aws.String("bytes=0-6"),
	})
	if err != nil {
		t.Fatalf("GetObject (range): %v", err)
	}
	rangeBody, err := io.ReadAll(rangeOut.Body)
	_ = rangeOut.Body.Close()
	if err != nil {
		t.Fatalf("read ranged GetObject body: %v", err)
	}
	if string(rangeBody) != string(body[:7]) {
		t.Errorf("ranged GetObject body = %q, want %q", rangeBody, body[:7])
	}
	if rangeOut.ContentLength == nil || *rangeOut.ContentLength != 7 {
		t.Errorf("ranged GetObject ContentLength = %v, want 7", rangeOut.ContentLength)
	}
	wantRange := fmt.Sprintf("bytes 0-6/%d", len(body))
	if rangeOut.ContentRange == nil || *rangeOut.ContentRange != wantRange {
		t.Errorf("ranged GetObject ContentRange = %v, want %s", rangeOut.ContentRange, wantRange)
	}

	// --- More objects, for ListObjectsV2 prefix/delimiter coverage ---
	extraKeys := []string{"logs/2024/jan.txt", "logs/2024/feb.txt", "logs/other/note.txt", "logs/root.txt"}
	for _, k := range extraKeys {
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(bucket), Key: aws.String(k), Body: bytes.NewReader([]byte(k)),
		}); err != nil {
			t.Fatalf("PutObject(%s): %v", k, err)
		}
	}

	list, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket), Prefix: aws.String("logs/"), Delimiter: aws.String("/"),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	var contentsKeys []string
	for _, obj := range list.Contents {
		contentsKeys = append(contentsKeys, aws.ToString(obj.Key))
	}
	var prefixes []string
	for _, p := range list.CommonPrefixes {
		prefixes = append(prefixes, aws.ToString(p.Prefix))
	}
	sort.Strings(contentsKeys)
	sort.Strings(prefixes)
	if len(contentsKeys) != 1 || contentsKeys[0] != "logs/root.txt" {
		t.Errorf("ListObjectsV2 Contents = %v, want [logs/root.txt]", contentsKeys)
	}
	if len(prefixes) != 2 || prefixes[0] != "logs/2024/" || prefixes[1] != "logs/other/" {
		t.Errorf("ListObjectsV2 CommonPrefixes = %v, want [logs/2024/ logs/other/]", prefixes)
	}

	// --- CopyObject ---
	const copyKey = "copies/hello-copy.txt"
	copyOut, err := client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(bucket),
		Key:        aws.String(copyKey),
		CopySource: aws.String(bucket + "/" + key),
	})
	if err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	if copyOut.CopyObjectResult == nil || copyOut.CopyObjectResult.ETag == nil || *copyOut.CopyObjectResult.ETag != wantETag {
		t.Errorf("CopyObject result ETag = %+v, want %s", copyOut.CopyObjectResult, wantETag)
	}
	copyPayload := s3EventPayload(t, inst, tommys3.EventObjectCopy, 1, bucket, copyKey)
	if copyPayload.Source == nil || copyPayload.Source.Bucket != bucket || copyPayload.Source.Key != key {
		t.Errorf("copy event Source = %+v, want %s/%s", copyPayload.Source, bucket, key)
	}

	copyGet, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(copyKey)})
	if err != nil {
		t.Fatalf("GetObject(copy): %v", err)
	}
	copyBody, err := io.ReadAll(copyGet.Body)
	_ = copyGet.Body.Close()
	if err != nil {
		t.Fatalf("read copy body: %v", err)
	}
	if string(copyBody) != string(body) {
		t.Errorf("copied object body = %q, want %q", copyBody, body)
	}

	// --- DeleteObject ---
	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(copyKey)}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	s3EventPayload(t, inst, tommys3.EventObjectDelete, 1, bucket, copyKey)
	if _, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(copyKey)}); err == nil {
		t.Error("GetObject after DeleteObject: want error, got nil")
	} else {
		var nsk *types.NoSuchKey
		if !errors.As(err, &nsk) {
			t.Errorf("GetObject after DeleteObject: err = %v, want *types.NoSuchKey", err)
		}
	}

	// --- DeleteObjects (bulk) ---
	remaining := append([]string{key}, extraKeys...)
	var toDelete []types.ObjectIdentifier
	for _, k := range remaining {
		toDelete = append(toDelete, types.ObjectIdentifier{Key: aws.String(k)})
	}
	bulk, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(bucket), Delete: &types.Delete{Objects: toDelete},
	})
	if err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	if len(bulk.Deleted) != len(remaining) {
		t.Errorf("DeleteObjects Deleted = %d entries, want %d", len(bulk.Deleted), len(remaining))
	}
	inst.WaitForEvents(1+len(remaining), store.Query{Plugin: tommys3.PluginName, Provider: s3http.ProviderName, Type: tommys3.EventObjectDelete}, 3*time.Second)

	afterBulk, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatalf("ListObjectsV2 (after bulk delete): %v", err)
	}
	if len(afterBulk.Contents) != 0 {
		t.Errorf("bucket not empty after DeleteObjects: %d objects remain", len(afterBulk.Contents))
	}

	// --- Presigned PUT and GET, driven with plain net/http ---
	presignClient := s3.NewPresignClient(client)
	const presignedKey = "presigned.txt"
	presignedBody := []byte("uploaded through a presigned URL\n")

	presignedPut, err := presignClient.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(presignedKey),
	}, s3.WithPresignExpires(5*time.Minute))
	if err != nil {
		t.Fatalf("PresignPutObject: %v", err)
	}
	putReq, err := http.NewRequest(presignedPut.Method, presignedPut.URL, bytes.NewReader(presignedBody))
	if err != nil {
		t.Fatalf("build presigned PUT request: %v", err)
	}
	for k, v := range presignedPut.SignedHeader {
		putReq.Header[k] = v
	}
	putResp, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatalf("presigned PUT: %v", err)
	}
	_ = putResp.Body.Close()
	if putResp.StatusCode != http.StatusOK {
		t.Fatalf("presigned PUT status = %d, want 200", putResp.StatusCode)
	}
	s3EventPayload(t, inst, tommys3.EventObjectPut, len(remaining)+1, bucket, presignedKey)

	presignedGet, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(presignedKey),
	}, s3.WithPresignExpires(5*time.Minute))
	if err != nil {
		t.Fatalf("PresignGetObject: %v", err)
	}
	getReq, err := http.NewRequest(presignedGet.Method, presignedGet.URL, nil)
	if err != nil {
		t.Fatalf("build presigned GET request: %v", err)
	}
	for k, v := range presignedGet.SignedHeader {
		getReq.Header[k] = v
	}
	getResp, err := http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatalf("presigned GET: %v", err)
	}
	defer func() { _ = getResp.Body.Close() }()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("presigned GET status = %d, want 200", getResp.StatusCode)
	}
	presignedGotBody, err := io.ReadAll(getResp.Body)
	if err != nil {
		t.Fatalf("read presigned GET body: %v", err)
	}
	if string(presignedGotBody) != string(presignedBody) {
		t.Errorf("presigned GET body = %q, want %q", presignedGotBody, presignedBody)
	}

	// --- DeleteBucket (bucket must be empty first) ---
	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(presignedKey)}); err != nil {
		t.Fatalf("DeleteObject(presigned key): %v", err)
	}
	if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	s3EventPayload(t, inst, tommys3.EventBucketDelete, 1, bucket, "")
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err == nil {
		t.Error("HeadBucket after DeleteBucket: want error, got nil")
	}
}

// TestS3SDKMultipartUploadDefaultConfig drives a real multipart upload
// through feature/s3/manager's Uploader - the official transfer manager,
// not a hand-rolled CreateMultipartUpload/UploadPart/CompleteMultipartUpload
// sequence - with a 12 MiB body and a 5 MiB part size, so it genuinely spans
// more than one part (5 + 5 + 2). It uses the SDK's default configuration,
// same as the main lifecycle test, for the same reason: this is what an
// application that has never heard of tommy actually runs.
func TestS3SDKMultipartUploadDefaultConfig(t *testing.T) {
	inst := startTommy(t)
	client := newS3Client(t, inst)
	ctx := context.Background()

	const bucket = "tommy-multipart"
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	const size = 12 << 20 // 12 MiB: 5 MiB + 5 MiB + 2 MiB parts.
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251) // deterministic, non-repeating-at-part-boundaries pattern
	}

	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = 5 << 20
	})
	const key = "big-object.bin"
	out, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(payload),
	})
	if err != nil {
		t.Fatalf("Uploader.Upload: %v", err)
	}
	if out.UploadID == "" {
		t.Error("UploadOutput.UploadID is empty, want a real multipart upload to have run")
	}
	if len(out.CompletedParts) != 3 {
		t.Errorf("UploadOutput.CompletedParts = %d parts, want 3", len(out.CompletedParts))
	}

	getOut, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	gotBody, err := io.ReadAll(getOut.Body)
	_ = getOut.Body.Close()
	if err != nil {
		t.Fatalf("read GetObject body: %v", err)
	}
	if !bytes.Equal(gotBody, payload) {
		t.Fatalf("multipart object round-trip mismatch: got %d bytes, want %d bytes", len(gotBody), len(payload))
	}

	putPayload := s3EventPayload(t, inst, tommys3.EventObjectPut, 1, bucket, key)
	if putPayload.Size != int64(size) {
		t.Errorf("put event Size = %d, want %d", putPayload.Size, size)
	}
	if putPayload.Blob == nil {
		t.Fatal("put event Blob is nil")
	}
	blobContent, blobRef := readBlob(t, inst, putPayload.Blob.ID)
	if blobRef.Size != int64(size) {
		t.Errorf("blob Ref.Size = %d, want %d", blobRef.Size, size)
	}
	if !bytes.Equal([]byte(blobContent), payload) {
		t.Error("blob content does not match the uploaded multipart bytes")
	}
}

// TestS3SDKChecksumWhenRequiredWorkaroundLifecycle covers the same basic
// put/get/delete path with RequestChecksumCalculationWhenRequired, the
// documented workaround for clients that would otherwise send a trailing
// checksum. This exists in addition to, not instead of, the default-config
// tests above, per the wave's instructions - it is not a substitute for
// proving the default configuration works.
func TestS3SDKChecksumWhenRequiredWorkaroundLifecycle(t *testing.T) {
	inst := startTommy(t)
	client := newS3Client(t, inst, func(o *s3.Options) {
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	ctx := context.Background()

	const bucket = "tommy-checksum-when-required"
	const key = "note.txt"
	body := []byte("checksum-when-required workaround path\n")

	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body), ContentType: aws.String("text/plain"),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	getOut, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	gotBody, err := io.ReadAll(getOut.Body)
	_ = getOut.Body.Close()
	if err != nil {
		t.Fatalf("read GetObject body: %v", err)
	}
	if string(gotBody) != string(body) {
		t.Errorf("GetObject body = %q, want %q", gotBody, body)
	}

	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	s3EventPayload(t, inst, tommys3.EventObjectDelete, 1, bucket, key)
}
