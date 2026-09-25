package http

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	stdhttp "net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/plugins/s3"
)

const xmlNamespace = "http://s3.amazonaws.com/doc/2006-03-01/"

type handler struct {
	store   *s3.Store
	session *s3.Session
	cfg     Config
	ids     atomic.Uint64
}

func newHandler(store *s3.Store, d plugin.Deps, cfg Config) *handler {
	return &handler{
		store:   store,
		session: s3.NewSession(store, d, s3.WithProvider(ProviderName), s3.WithTransport("http")),
		cfg:     cfg,
	}
}

func (h *handler) ServeHTTP(w stdhttp.ResponseWriter, r *stdhttp.Request) {
	requestID := fmt.Sprintf("%016x", h.ids.Add(1))
	w.Header().Set("Server", "tommy-s3")
	w.Header().Set("x-amz-request-id", requestID)
	w.Header().Set("x-amz-id-2", requestID+requestID)

	bucket, key, err := requestTarget(r)
	if err != nil {
		h.writeError(w, r, requestID, s3Error{"InvalidURI", "Could not parse the specified URI.", stdhttp.StatusBadRequest, err, ""})
		return
	}

	q := r.URL.Query()
	if bucket == "" {
		if r.Method == stdhttp.MethodGet && key == "" {
			h.listBuckets(w, r, requestID)
			return
		}
		h.writeError(w, r, requestID, s3Error{"MethodNotAllowed", "The specified method is not allowed against this resource.", stdhttp.StatusMethodNotAllowed, nil, ""})
		return
	}

	if key == "" {
		switch {
		case r.Method == stdhttp.MethodPut:
			h.createBucket(w, r, requestID, bucket)
		case r.Method == stdhttp.MethodHead:
			h.headBucket(w, r, requestID, bucket)
		case r.Method == stdhttp.MethodDelete:
			h.deleteBucket(w, r, requestID, bucket)
		case r.Method == stdhttp.MethodGet && q.Has("location"):
			h.getBucketLocation(w, r, requestID, bucket)
		case r.Method == stdhttp.MethodGet && q.Has("uploads"):
			h.listMultipartUploads(w, r, requestID, bucket)
		case r.Method == stdhttp.MethodGet:
			h.listObjectsV2(w, r, requestID, bucket)
		case r.Method == stdhttp.MethodPost && q.Has("delete"):
			h.deleteObjects(w, r, requestID, bucket)
		default:
			h.writeError(w, r, requestID, s3Error{"MethodNotAllowed", "The specified method is not allowed against this resource.", stdhttp.StatusMethodNotAllowed, nil, ""})
		}
		return
	}

	switch {
	case r.Method == stdhttp.MethodPut && q.Get("uploadId") != "" && q.Get("partNumber") != "":
		h.putPart(w, r, requestID, bucket, key)
	case r.Method == stdhttp.MethodPut && r.Header.Get("x-amz-copy-source") != "":
		h.copyObject(w, r, requestID, bucket, key)
	case r.Method == stdhttp.MethodPut:
		h.putObject(w, r, requestID, bucket, key)
	case r.Method == stdhttp.MethodGet && q.Get("uploadId") != "":
		h.listParts(w, r, requestID, bucket, key)
	case r.Method == stdhttp.MethodGet:
		h.getObject(w, r, requestID, bucket, key, false)
	case r.Method == stdhttp.MethodHead:
		h.getObject(w, r, requestID, bucket, key, true)
	case r.Method == stdhttp.MethodDelete && q.Get("uploadId") != "":
		h.abortMultipart(w, r, requestID, bucket, key)
	case r.Method == stdhttp.MethodDelete:
		h.deleteObject(w, r, requestID, bucket, key)
	case r.Method == stdhttp.MethodPost && q.Has("uploads"):
		h.createMultipart(w, r, requestID, bucket, key)
	case r.Method == stdhttp.MethodPost && q.Get("uploadId") != "":
		h.completeMultipart(w, r, requestID, bucket, key)
	default:
		h.writeError(w, r, requestID, s3Error{"MethodNotAllowed", "The specified method is not allowed against this resource.", stdhttp.StatusMethodNotAllowed, nil, ""})
	}
}

func requestTarget(r *stdhttp.Request) (string, string, error) {
	escaped := r.URL.EscapedPath()
	if escaped == "" || escaped == "/" {
		return "", "", nil
	}
	if !strings.HasPrefix(escaped, "/") {
		return "", "", errors.New("path is not absolute")
	}
	relative := strings.TrimPrefix(escaped, "/")
	bucketPart, keyPart, hasKey := strings.Cut(relative, "/")
	bucket, err := url.PathUnescape(bucketPart)
	if err != nil {
		return "", "", err
	}
	if !hasKey {
		return bucket, "", nil
	}
	key, err := url.PathUnescape(keyPart)
	if err != nil {
		return "", "", err
	}
	return bucket, key, nil
}

func (h *handler) listBuckets(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID string) {
	buckets := h.store.ListBuckets()
	out := listAllMyBucketsResult{XMLNS: xmlNamespace, Owner: owner{ID: "tommy", DisplayName: "tommy"}}
	out.Buckets.Bucket = make([]bucketXML, 0, len(buckets))
	for _, bucket := range buckets {
		out.Buckets.Bucket = append(out.Buckets.Bucket, bucketXML{Name: bucket.Name, CreationDate: isoTime(bucket.CreationTime)})
	}
	h.writeXML(w, stdhttp.StatusOK, out)
}

func (h *handler) createBucket(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket string) {
	body, serr := h.readBody(r, h.cfg.MaxXMLBytes)
	if serr != nil {
		h.writeError(w, r, requestID, serr.withResource(bucket))
		return
	}
	if serr = validateChecksums(r.Header, body); serr != nil {
		h.writeError(w, r, requestID, serr.withResource(bucket))
		return
	}
	_, err := h.session.CreateBucket(r.Context(), bucket, eventOptions(r, body, true)...)
	if err != nil {
		h.writeStoreError(w, r, requestID, bucket, err)
		return
	}
	w.Header().Set("Location", "/"+url.PathEscape(bucket))
	w.WriteHeader(stdhttp.StatusOK)
}

func (h *handler) headBucket(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket string) {
	if _, err := h.store.HeadBucket(bucket); err != nil {
		h.writeStoreError(w, r, requestID, bucket, err)
		return
	}
	w.WriteHeader(stdhttp.StatusOK)
}

func (h *handler) getBucketLocation(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket string) {
	if _, err := h.store.HeadBucket(bucket); err != nil {
		h.writeStoreError(w, r, requestID, bucket, err)
		return
	}
	h.writeXML(w, stdhttp.StatusOK, locationConstraint{XMLNS: xmlNamespace})
}

func (h *handler) deleteBucket(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket string) {
	body, serr := h.readBody(r, h.cfg.MaxXMLBytes)
	if serr != nil {
		h.writeError(w, r, requestID, serr.withResource(bucket))
		return
	}
	_, err := h.session.DeleteBucket(r.Context(), bucket, false, eventOptions(r, body, true)...)
	if err != nil {
		h.writeStoreError(w, r, requestID, bucket, err)
		return
	}
	w.WriteHeader(stdhttp.StatusNoContent)
}

func (h *handler) putObject(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket, key string) {
	body, serr := h.readBody(r, h.maxObjectBytes())
	if serr != nil {
		h.writeError(w, r, requestID, serr.withResource(bucket+"/"+key))
		return
	}
	if serr = validateChecksums(r.Header, body); serr != nil {
		h.writeError(w, r, requestID, serr.withResource(bucket+"/"+key))
		return
	}
	put := putOptions(r.Header)
	obj, err := h.session.PutObject(r.Context(), bucket, key, bytes.NewReader(body), put, eventOptions(r, body, bodyIsText(r.Header, body))...)
	if err != nil {
		h.writeStoreError(w, r, requestID, bucket+"/"+key, err)
		return
	}
	writeObjectHeaders(w.Header(), obj, true)
	w.WriteHeader(stdhttp.StatusOK)
}

func (h *handler) getObject(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket, key string, head bool) {
	data, obj, err := h.store.GetObject(r.Context(), bucket, key)
	if err != nil {
		h.writeStoreError(w, r, requestID, bucket+"/"+key, err)
		return
	}
	writeObjectHeaders(w.Header(), obj, checksumRequested(r))
	w.Header().Set("Accept-Ranges", "bytes")
	if status := evaluateConditions(r.Header, obj); status != 0 {
		w.WriteHeader(status)
		return
	}

	start, end, partial, rangeErr := parseRange(r.Header.Get("Range"), int64(len(data)))
	if rangeErr != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
		h.writeError(w, r, requestID, s3Error{"InvalidRange", "The requested range is not satisfiable.", stdhttp.StatusRequestedRangeNotSatisfiable, rangeErr, bucket + "/" + key})
		return
	}
	status := stdhttp.StatusOK
	payload := data
	if partial {
		status = stdhttp.StatusPartialContent
		payload = data[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(status)
	if !head {
		_, _ = w.Write(payload)
	}
}

func (h *handler) deleteObject(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket, key string) {
	body, serr := h.readBody(r, h.cfg.MaxXMLBytes)
	if serr != nil {
		h.writeError(w, r, requestID, serr.withResource(bucket+"/"+key))
		return
	}
	_, _, err := h.session.DeleteObject(r.Context(), bucket, key, eventOptions(r, body, true)...)
	if err != nil {
		h.writeStoreError(w, r, requestID, bucket+"/"+key, err)
		return
	}
	w.WriteHeader(stdhttp.StatusNoContent)
}

func (h *handler) listObjectsV2(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket string) {
	q := r.URL.Query()
	maxKeys := 1000
	if value := q.Get("max-keys"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 || parsed > 1000 {
			h.writeError(w, r, requestID, s3Error{"InvalidArgument", "Argument max-keys must be an integer between 0 and 1000.", stdhttp.StatusBadRequest, err, bucket})
			return
		}
		maxKeys = parsed
	}
	token := q.Get("continuation-token")
	if token != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil {
			h.writeError(w, r, requestID, s3Error{"InvalidArgument", "The continuation token provided is incorrect.", stdhttp.StatusBadRequest, err, bucket})
			return
		}
		token = string(decoded)
	}
	storeMax := maxKeys
	if storeMax == 0 {
		storeMax = 1000
	}
	result, err := h.store.ListObjects(bucket, s3.ListOptions{
		Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"), StartAfter: q.Get("start-after"),
		ContinuationToken: token, MaxKeys: storeMax,
	})
	if err != nil {
		h.writeStoreError(w, r, requestID, bucket, err)
		return
	}
	if maxKeys == 0 {
		result.IsTruncated = len(result.Objects)+len(result.CommonPrefixes) > 0
		result.Objects = nil
		result.CommonPrefixes = nil
		result.NextContinuationToken = ""
	}
	encodeURL := q.Get("encoding-type") == "url"
	encode := func(value string) string {
		if encodeURL {
			return url.PathEscape(value)
		}
		return value
	}
	out := listBucketResult{
		XMLNS: xmlNamespace, Name: bucket, Prefix: encode(q.Get("prefix")), Delimiter: encode(q.Get("delimiter")),
		MaxKeys: maxKeys, KeyCount: len(result.Objects) + len(result.CommonPrefixes), IsTruncated: result.IsTruncated,
		StartAfter: encode(q.Get("start-after")), ContinuationToken: q.Get("continuation-token"),
	}
	if encodeURL {
		out.EncodingType = "url"
	}
	for _, obj := range result.Objects {
		out.Contents = append(out.Contents, objectXML{Key: encode(obj.Key), LastModified: isoTime(obj.LastModified), ETag: quoteETag(obj.ETag), Size: obj.Size, StorageClass: "STANDARD"})
	}
	for _, prefix := range result.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{Prefix: encode(prefix)})
	}
	if result.NextContinuationToken != "" {
		out.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(result.NextContinuationToken))
	}
	h.writeXML(w, stdhttp.StatusOK, out)
}

func (h *handler) copyObject(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket, key string) {
	body, serr := h.readBody(r, h.cfg.MaxXMLBytes)
	if serr != nil {
		h.writeError(w, r, requestID, serr.withResource(bucket+"/"+key))
		return
	}
	source, err := parseCopySource(r.Header.Get("x-amz-copy-source"))
	if err != nil {
		h.writeError(w, r, requestID, s3Error{"InvalidArgument", "The x-amz-copy-source header is invalid.", stdhttp.StatusBadRequest, err, bucket + "/" + key})
		return
	}
	directive := r.Header.Get("x-amz-metadata-directive")
	if directive != "" && !strings.EqualFold(directive, "COPY") && !strings.EqualFold(directive, "REPLACE") {
		h.writeError(w, r, requestID, s3Error{"InvalidArgument", "Unknown metadata directive.", stdhttp.StatusBadRequest, nil, bucket + "/" + key})
		return
	}
	opts := s3.CopyOptions{MetadataDirective: directive}
	if strings.EqualFold(directive, "REPLACE") {
		opts.PutOptions = putOptions(r.Header)
	}
	obj, err := h.session.CopyObject(r.Context(), source, s3.ObjectLocation{Bucket: bucket, Key: key}, opts, eventOptions(r, body, true)...)
	if err != nil {
		h.writeStoreError(w, r, requestID, bucket+"/"+key, err)
		return
	}
	writeChecksumHeaders(w.Header(), obj.Checksums)
	h.writeXML(w, stdhttp.StatusOK, copyObjectResult{LastModified: isoTime(obj.LastModified), ETag: quoteETag(obj.ETag)})
}

func parseCopySource(value string) (s3.ObjectLocation, error) {
	value = strings.TrimSpace(value)
	if before, _, ok := strings.Cut(value, "?"); ok {
		value = before
	}
	decoded, err := url.PathUnescape(strings.TrimPrefix(value, "/"))
	if err != nil {
		return s3.ObjectLocation{}, err
	}
	bucket, key, ok := strings.Cut(decoded, "/")
	if !ok || bucket == "" || key == "" {
		return s3.ObjectLocation{}, errors.New("copy source must contain bucket and key")
	}
	return s3.ObjectLocation{Bucket: bucket, Key: key}, nil
}

func (h *handler) deleteObjects(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket string) {
	body, serr := h.readBody(r, h.cfg.MaxXMLBytes)
	if serr != nil {
		h.writeError(w, r, requestID, serr.withResource(bucket))
		return
	}
	if serr = validateChecksums(r.Header, body); serr != nil {
		h.writeError(w, r, requestID, serr.withResource(bucket))
		return
	}
	var input deleteRequest
	if err := xml.Unmarshal(body, &input); err != nil {
		h.writeError(w, r, requestID, s3Error{"MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.", stdhttp.StatusBadRequest, err, bucket})
		return
	}
	if len(input.Objects) > 1000 {
		h.writeError(w, r, requestID, s3Error{"MalformedXML", "DeleteObjects accepts at most 1000 keys.", stdhttp.StatusBadRequest, nil, bucket})
		return
	}
	keys := make([]string, len(input.Objects))
	for i, object := range input.Objects {
		keys[i] = object.Key
	}
	removed, err := h.session.DeleteObjects(r.Context(), bucket, keys, eventOptions(r, body, true)...)
	if err != nil {
		h.writeStoreError(w, r, requestID, bucket, err)
		return
	}
	_ = removed
	out := deleteResult{XMLNS: xmlNamespace}
	if !input.Quiet {
		for _, object := range input.Objects {
			out.Deleted = append(out.Deleted, deletedObject(object))
		}
	}
	h.writeXML(w, stdhttp.StatusOK, out)
}

func (h *handler) createMultipart(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket, key string) {
	if _, err := h.store.HeadBucket(bucket); err != nil {
		h.writeStoreError(w, r, requestID, bucket, err)
		return
	}
	upload, err := h.session.CreateMultipart(bucket, key, putOptions(r.Header))
	if err != nil {
		h.writeStoreError(w, r, requestID, bucket+"/"+key, err)
		return
	}
	h.writeXML(w, stdhttp.StatusOK, initiateMultipartUploadResult{XMLNS: xmlNamespace, Bucket: bucket, Key: key, UploadID: upload.ID})
}

func (h *handler) listMultipartUploads(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket string) {
	uploads, err := h.store.ListMultipartUploads(bucket)
	if err != nil {
		h.writeStoreError(w, r, requestID, bucket, err)
		return
	}
	out := listMultipartUploadsResult{XMLNS: xmlNamespace, Bucket: bucket, MaxUploads: 1000, IsTruncated: false}
	for _, upload := range uploads {
		out.Uploads = append(out.Uploads, uploadXML{Key: upload.Key, UploadID: upload.ID, Initiated: isoTime(upload.Initiated), StorageClass: "STANDARD", Initiator: owner{ID: "tommy", DisplayName: "tommy"}, Owner: owner{ID: "tommy", DisplayName: "tommy"}})
	}
	h.writeXML(w, stdhttp.StatusOK, out)
}

func (h *handler) putPart(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket, key string) {
	partNumber, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || partNumber < 1 || partNumber > 10000 {
		h.writeError(w, r, requestID, s3Error{"InvalidArgument", "Part number must be an integer between 1 and 10000.", stdhttp.StatusBadRequest, err, bucket + "/" + key})
		return
	}
	body, serr := h.readBody(r, h.maxObjectBytes())
	if serr != nil {
		h.writeError(w, r, requestID, serr.withResource(bucket+"/"+key))
		return
	}
	if serr = validateChecksums(r.Header, body); serr != nil {
		h.writeError(w, r, requestID, serr.withResource(bucket+"/"+key))
		return
	}
	part, err := h.session.PutPart(r.Context(), r.URL.Query().Get("uploadId"), partNumber, bytes.NewReader(body))
	if err != nil {
		h.writeStoreError(w, r, requestID, bucket+"/"+key, err)
		return
	}
	w.Header().Set("ETag", quoteETag(part.ETag))
	copyChecksumHeaders(w.Header(), r.Header)
	w.WriteHeader(stdhttp.StatusOK)
}

func (h *handler) listParts(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket, key string) {
	parts, err := h.session.ListParts(r.URL.Query().Get("uploadId"))
	if err != nil {
		h.writeStoreError(w, r, requestID, bucket+"/"+key, err)
		return
	}
	out := listPartsResult{XMLNS: xmlNamespace, Bucket: bucket, Key: key, UploadID: r.URL.Query().Get("uploadId"), StorageClass: "STANDARD", IsTruncated: false, MaxParts: 1000, Owner: owner{ID: "tommy", DisplayName: "tommy"}, Initiator: owner{ID: "tommy", DisplayName: "tommy"}}
	for _, part := range parts {
		out.Parts = append(out.Parts, partXML{PartNumber: part.Number, LastModified: isoTime(part.LastModified), ETag: quoteETag(part.ETag), Size: part.Size})
	}
	h.writeXML(w, stdhttp.StatusOK, out)
}

func (h *handler) completeMultipart(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket, key string) {
	body, serr := h.readBody(r, h.cfg.MaxXMLBytes)
	if serr != nil {
		h.writeError(w, r, requestID, serr.withResource(bucket+"/"+key))
		return
	}
	if serr = validateChecksums(r.Header, body); serr != nil {
		h.writeError(w, r, requestID, serr.withResource(bucket+"/"+key))
		return
	}
	var input completeMultipartUpload
	if err := xml.Unmarshal(body, &input); err != nil {
		h.writeError(w, r, requestID, s3Error{"MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.", stdhttp.StatusBadRequest, err, bucket + "/" + key})
		return
	}
	parts := make([]s3.CompletedPart, len(input.Parts))
	for i, part := range input.Parts {
		parts[i] = s3.CompletedPart{Number: part.PartNumber, ETag: part.ETag}
	}
	obj, err := h.session.CompleteMultipart(r.Context(), r.URL.Query().Get("uploadId"), parts)
	if err != nil {
		h.writeStoreError(w, r, requestID, bucket+"/"+key, err)
		return
	}
	if err := h.session.RecordObjectPut(r.Context(), obj, eventOptions(r, body, true)...); err != nil {
		h.writeStoreError(w, r, requestID, bucket+"/"+key, err)
		return
	}
	location := "http://" + r.Host + "/" + url.PathEscape(bucket) + "/" + escapeKey(key)
	h.writeXML(w, stdhttp.StatusOK, completeMultipartUploadResult{XMLNS: xmlNamespace, Location: location, Bucket: bucket, Key: key, ETag: quoteETag(obj.ETag)})
}

func (h *handler) abortMultipart(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, bucket, key string) {
	if err := h.session.AbortMultipart(r.Context(), r.URL.Query().Get("uploadId")); err != nil {
		h.writeStoreError(w, r, requestID, bucket+"/"+key, err)
		return
	}
	w.WriteHeader(stdhttp.StatusNoContent)
}

func (h *handler) readBody(r *stdhttp.Request, limit int64) ([]byte, *s3Error) {
	if strings.Contains(strings.ToLower(r.Header.Get("Content-Encoding")), "aws-chunked") || strings.HasPrefix(strings.ToUpper(r.Header.Get("x-amz-content-sha256")), "STREAMING-") {
		return nil, &s3Error{"NotImplemented", "aws-chunked streaming payloads are not supported; send a normal HTTP request body.", stdhttp.StatusNotImplemented, nil, ""}
	}
	if r.ContentLength > limit {
		return nil, &s3Error{"EntityTooLarge", "Your proposed upload exceeds the maximum allowed object size.", stdhttp.StatusBadRequest, nil, ""}
	}
	if r.Body == nil {
		return []byte{}, nil
	}
	reader := io.LimitReader(r.Body, limit+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, &s3Error{"InvalidRequest", "Could not read the request body.", stdhttp.StatusBadRequest, err, ""}
	}
	if int64(len(body)) > limit {
		return nil, &s3Error{"EntityTooLarge", "Your proposed upload exceeds the maximum allowed object size.", stdhttp.StatusBadRequest, nil, ""}
	}
	return body, nil
}

func (h *handler) maxObjectBytes() int64 {
	max := h.cfg.MaxObjectBytes
	if storeMax := h.store.Limits().MaxObjectBytes; storeMax > 0 && storeMax < max {
		max = storeMax
	}
	return max
}

func eventOptions(r *stdhttp.Request, body []byte, text bool) []s3.EventOption {
	raw := event.Raw{
		Transport: "http", PeerAddr: r.RemoteAddr, Method: r.Method, Path: r.URL.RequestURI(),
		Headers: r.Header.Clone(), Body: bytes.Clone(body), Text: text,
	}
	opts := []s3.EventOption{s3.WithEventRaw(raw)}
	credentials := parseCredentials(r)
	for key, value := range credentials {
		opts = append(opts, s3.WithEventMeta(key, value))
	}
	return opts
}

func parseCredentials(r *stdhttp.Request) map[string]any {
	meta := map[string]any{}
	credential := ""
	authorization := r.Header.Get("Authorization")
	if strings.HasPrefix(authorization, "AWS4-HMAC-SHA256") {
		meta["signature_version"] = "SigV4"
		for _, field := range strings.Split(strings.TrimSpace(strings.TrimPrefix(authorization, "AWS4-HMAC-SHA256")), ",") {
			key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
			if !ok {
				continue
			}
			switch strings.ToLower(key) {
			case "credential":
				credential = value
			case "signedheaders":
				meta["signed_headers"] = strings.Split(value, ";")
			}
		}
	}
	q := r.URL.Query()
	if queryCredential := q.Get("X-Amz-Credential"); queryCredential != "" {
		meta["signature_version"] = "SigV4"
		meta["presigned"] = true
		credential = queryCredential
		if signed := q.Get("X-Amz-SignedHeaders"); signed != "" {
			meta["signed_headers"] = strings.Split(signed, ";")
		}
	}
	if credential != "" {
		parts := strings.Split(credential, "/")
		meta["access_key"] = parts[0]
		if len(parts) >= 3 {
			meta["region"] = parts[2]
		}
	}
	if token := r.Header.Get("x-amz-security-token"); token != "" {
		meta["session_token"] = token
	} else if token := q.Get("X-Amz-Security-Token"); token != "" {
		meta["session_token"] = token
	}
	return meta
}

func putOptions(headers stdhttp.Header) s3.PutOptions {
	metadata := map[string]string{}
	for key, values := range headers {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "x-amz-meta-") {
			metadata[strings.TrimPrefix(lower, "x-amz-meta-")] = strings.Join(values, ",")
		}
	}
	if len(metadata) == 0 {
		metadata = nil
	}
	var expires time.Time
	if value := headers.Get("Expires"); value != "" {
		expires, _ = stdhttp.ParseTime(value)
	}
	return s3.PutOptions{
		Metadata: metadata,
		Headers: s3.ContentHeaders{
			ContentType: headers.Get("Content-Type"), ContentEncoding: headers.Get("Content-Encoding"),
			ContentLanguage: headers.Get("Content-Language"), ContentDisposition: headers.Get("Content-Disposition"),
			CacheControl: headers.Get("Cache-Control"), Expires: expires,
		},
		Checksums: checksumsFromHeaders(headers),
	}
}

func bodyIsText(headers stdhttp.Header, body []byte) bool {
	contentType := strings.ToLower(headers.Get("Content-Type"))
	return strings.HasPrefix(contentType, "text/") || strings.Contains(contentType, "json") || strings.Contains(contentType, "xml") || (contentType == "" && utf8.Valid(body))
}

func writeObjectHeaders(headers stdhttp.Header, obj s3.Object, includeChecksums bool) {
	headers.Set("ETag", quoteETag(obj.ETag))
	headers.Set("Last-Modified", obj.LastModified.UTC().Format(stdhttp.TimeFormat))
	if obj.Headers.ContentType != "" {
		headers.Set("Content-Type", obj.Headers.ContentType)
	} else {
		headers.Set("Content-Type", "application/octet-stream")
	}
	if obj.Headers.ContentEncoding != "" {
		headers.Set("Content-Encoding", obj.Headers.ContentEncoding)
	}
	if obj.Headers.ContentLanguage != "" {
		headers.Set("Content-Language", obj.Headers.ContentLanguage)
	}
	if obj.Headers.ContentDisposition != "" {
		headers.Set("Content-Disposition", obj.Headers.ContentDisposition)
	}
	if obj.Headers.CacheControl != "" {
		headers.Set("Cache-Control", obj.Headers.CacheControl)
	}
	if !obj.Headers.Expires.IsZero() {
		headers.Set("Expires", obj.Headers.Expires.UTC().Format(stdhttp.TimeFormat))
	}
	for key, value := range obj.Metadata {
		headers.Set("x-amz-meta-"+key, value)
	}
	if includeChecksums {
		writeChecksumHeaders(headers, obj.Checksums)
	}
}

func checksumRequested(r *stdhttp.Request) bool {
	return strings.EqualFold(r.Header.Get("x-amz-checksum-mode"), "ENABLED")
}

func quoteETag(etag string) string { return `"` + strings.Trim(etag, `"`) + `"` }
func isoTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func evaluateConditions(headers stdhttp.Header, obj s3.Object) int {
	etag := quoteETag(obj.ETag)
	if value := headers.Get("If-Match"); value != "" && !etagMatches(value, etag) {
		return stdhttp.StatusPreconditionFailed
	}
	if value := headers.Get("If-Unmodified-Since"); value != "" {
		if date, err := stdhttp.ParseTime(value); err == nil && obj.LastModified.After(date.Add(time.Second)) {
			return stdhttp.StatusPreconditionFailed
		}
	}
	if value := headers.Get("If-None-Match"); value != "" && etagMatches(value, etag) {
		return stdhttp.StatusNotModified
	}
	if headers.Get("If-None-Match") == "" {
		if value := headers.Get("If-Modified-Since"); value != "" {
			if date, err := stdhttp.ParseTime(value); err == nil && !obj.LastModified.After(date.Add(time.Second)) {
				return stdhttp.StatusNotModified
			}
		}
	}
	return 0
}

func etagMatches(list, etag string) bool {
	for _, candidate := range strings.Split(list, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

func parseRange(value string, size int64) (start, end int64, partial bool, err error) {
	if value == "" {
		if size == 0 {
			return 0, -1, false, nil
		}
		return 0, size - 1, false, nil
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") || size == 0 {
		return 0, 0, false, errors.New("invalid byte range")
	}
	first, last, ok := strings.Cut(strings.TrimPrefix(value, "bytes="), "-")
	if !ok {
		return 0, 0, false, errors.New("invalid byte range")
	}
	if first == "" {
		suffix, parseErr := strconv.ParseInt(last, 10, 64)
		if parseErr != nil || suffix <= 0 {
			return 0, 0, false, errors.New("invalid suffix range")
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, size - 1, true, nil
	}
	start, err = strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, errors.New("invalid range start")
	}
	end = size - 1
	if last != "" {
		end, err = strconv.ParseInt(last, 10, 64)
		if err != nil || end < start {
			return 0, 0, false, errors.New("invalid range end")
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true, nil
}

func (h *handler) writeXML(w stdhttp.ResponseWriter, status int, value any) {
	encoded, err := xml.Marshal(value)
	if err != nil {
		stdhttp.Error(w, "could not encode S3 response", stdhttp.StatusInternalServerError)
		return
	}
	body := append([]byte(xml.Header), encoded...)
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

type s3Error struct {
	Code     string
	Message  string
	Status   int
	Cause    error
	Resource string
}

func (e s3Error) withResource(resource string) s3Error { e.Resource = resource; return e }

func (h *handler) writeStoreError(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID, resource string, err error) {
	h.writeError(w, r, requestID, mapStoreError(err).withResource(resource))
}

func mapStoreError(err error) s3Error {
	switch {
	case errors.Is(err, s3.ErrBucketNotFound):
		return s3Error{"NoSuchBucket", "The specified bucket does not exist.", stdhttp.StatusNotFound, err, ""}
	case errors.Is(err, s3.ErrBucketExists):
		return s3Error{"BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.", stdhttp.StatusConflict, err, ""}
	case errors.Is(err, s3.ErrBucketNotEmpty):
		return s3Error{"BucketNotEmpty", "The bucket you tried to delete is not empty.", stdhttp.StatusConflict, err, ""}
	case errors.Is(err, s3.ErrObjectNotFound):
		return s3Error{"NoSuchKey", "The specified key does not exist.", stdhttp.StatusNotFound, err, ""}
	case errors.Is(err, s3.ErrUploadNotFound):
		return s3Error{"NoSuchUpload", "The specified multipart upload does not exist.", stdhttp.StatusNotFound, err, ""}
	case errors.Is(err, s3.ErrInvalidBucket):
		return s3Error{"InvalidBucketName", "The specified bucket is not valid.", stdhttp.StatusBadRequest, err, ""}
	case errors.Is(err, s3.ErrInvalidKey), errors.Is(err, s3.ErrInvalidPart):
		return s3Error{"InvalidArgument", "One or more arguments were invalid.", stdhttp.StatusBadRequest, err, ""}
	case errors.Is(err, s3.ErrMetadataTooLarge):
		return s3Error{"MetadataTooLarge", "Your metadata headers exceed the maximum allowed metadata size.", stdhttp.StatusBadRequest, err, ""}
	case errors.Is(err, s3.ErrObjectTooLarge):
		return s3Error{"EntityTooLarge", "Your proposed upload exceeds the maximum allowed object size.", stdhttp.StatusBadRequest, err, ""}
	case errors.Is(err, s3.ErrBucketLimit), errors.Is(err, s3.ErrObjectLimit), errors.Is(err, s3.ErrUploadLimit), errors.Is(err, s3.ErrPartLimit), errors.Is(err, s3.ErrUploadBusy):
		return s3Error{"ServiceUnavailable", "The configured S3 limit has been reached.", stdhttp.StatusServiceUnavailable, err, ""}
	default:
		return s3Error{"InternalError", "We encountered an internal error. Please try again.", stdhttp.StatusInternalServerError, err, ""}
	}
}

func (h *handler) writeError(w stdhttp.ResponseWriter, r *stdhttp.Request, requestID string, value s3Error) {
	if value.Status == 0 {
		value.Status = stdhttp.StatusInternalServerError
	}
	if r.Method == stdhttp.MethodHead {
		w.WriteHeader(value.Status)
		return
	}
	h.writeXML(w, value.Status, errorResponse{Code: value.Code, Message: value.Message, Resource: value.Resource, RequestID: requestID, HostID: requestID + requestID})
}

// XML wire shapes.
type owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}
type bucketXML struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}
type listAllMyBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	XMLNS   string   `xml:"xmlns,attr"`
	Owner   owner    `xml:"Owner"`
	Buckets struct {
		Bucket []bucketXML `xml:"Bucket"`
	} `xml:"Buckets"`
}
type locationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	XMLNS   string   `xml:"xmlns,attr"`
}
type objectXML struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}
type commonPrefix struct {
	Prefix string `xml:"Prefix"`
}
type listBucketResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	XMLNS                 string         `xml:"xmlns,attr"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	KeyCount              int            `xml:"KeyCount"`
	MaxKeys               int            `xml:"MaxKeys"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	EncodingType          string         `xml:"EncodingType,omitempty"`
	IsTruncated           bool           `xml:"IsTruncated"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
	Contents              []objectXML    `xml:"Contents"`
	CommonPrefixes        []commonPrefix `xml:"CommonPrefixes"`
}
type copyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	LastModified string   `xml:"LastModified"`
	ETag         string   `xml:"ETag"`
}
type deleteObjectXML struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
}
type deleteRequest struct {
	XMLName xml.Name          `xml:"Delete"`
	Objects []deleteObjectXML `xml:"Object"`
	Quiet   bool              `xml:"Quiet"`
}
type deletedObject struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
}
type deleteResult struct {
	XMLName xml.Name        `xml:"DeleteResult"`
	XMLNS   string          `xml:"xmlns,attr"`
	Deleted []deletedObject `xml:"Deleted"`
}
type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}
type uploadXML struct {
	Key          string `xml:"Key"`
	UploadID     string `xml:"UploadId"`
	Initiator    owner  `xml:"Initiator"`
	Owner        owner  `xml:"Owner"`
	StorageClass string `xml:"StorageClass"`
	Initiated    string `xml:"Initiated"`
}
type listMultipartUploadsResult struct {
	XMLName            xml.Name    `xml:"ListMultipartUploadsResult"`
	XMLNS              string      `xml:"xmlns,attr"`
	Bucket             string      `xml:"Bucket"`
	KeyMarker          string      `xml:"KeyMarker"`
	UploadIDMarker     string      `xml:"UploadIdMarker"`
	NextKeyMarker      string      `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker string      `xml:"NextUploadIdMarker,omitempty"`
	MaxUploads         int         `xml:"MaxUploads"`
	IsTruncated        bool        `xml:"IsTruncated"`
	Uploads            []uploadXML `xml:"Upload"`
}
type partXML struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}
type listPartsResult struct {
	XMLName              xml.Name  `xml:"ListPartsResult"`
	XMLNS                string    `xml:"xmlns,attr"`
	Bucket               string    `xml:"Bucket"`
	Key                  string    `xml:"Key"`
	UploadID             string    `xml:"UploadId"`
	Initiator            owner     `xml:"Initiator"`
	Owner                owner     `xml:"Owner"`
	StorageClass         string    `xml:"StorageClass"`
	PartNumberMarker     int       `xml:"PartNumberMarker"`
	NextPartNumberMarker int       `xml:"NextPartNumberMarker"`
	MaxParts             int       `xml:"MaxParts"`
	IsTruncated          bool      `xml:"IsTruncated"`
	Parts                []partXML `xml:"Part"`
}
type completedPartXML struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}
type completeMultipartUpload struct {
	XMLName xml.Name           `xml:"CompleteMultipartUpload"`
	Parts   []completedPartXML `xml:"Part"`
}
type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}
type errorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId"`
	HostID    string   `xml:"HostId"`
}
