package s3

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
)

// BucketLinks are the read-back resources related to a bucket.
type BucketLinks struct {
	Self    string `json:"self"`
	Objects string `json:"objects"`
}

// BucketView is a current bucket snapshot with navigable links.
type BucketView struct {
	Bucket
	Links BucketLinks `json:"links"`
}

// BucketsView is the current S3 catalog and its totals.
type BucketsView struct {
	Buckets []BucketView `json:"buckets"`
	Stats   Stats        `json:"stats"`
	Self    string       `json:"self"`
}

// ObjectLinks are the metadata, content, and containing-list resources for an object.
type ObjectLinks struct {
	Self    string `json:"self"`
	Content string `json:"content"`
	List    string `json:"list"`
}

// ObjectView is one exact S3 key and its current metadata.
type ObjectView struct {
	Object
	Links ObjectLinks `json:"links"`
}

// CommonPrefixView is a delimiter-produced prefix with a link that browses it.
type CommonPrefixView struct {
	Prefix string `json:"prefix"`
	List   string `json:"list"`
}

// ObjectsView is one stable, lexicographic page of a bucket listing.
type ObjectsView struct {
	Bucket                BucketView         `json:"bucket"`
	Prefix                string             `json:"prefix,omitempty"`
	Delimiter             string             `json:"delimiter,omitempty"`
	StartAfter            string             `json:"start_after,omitempty"`
	ContinuationToken     string             `json:"continuation_token,omitempty"`
	MaxKeys               int                `json:"max_keys"`
	Objects               []ObjectView       `json:"objects"`
	CommonPrefixes        []CommonPrefixView `json:"common_prefixes"`
	IsTruncated           bool               `json:"is_truncated"`
	NextContinuationToken string             `json:"next_continuation_token,omitempty"`
	Self                  string             `json:"self"`
	Next                  string             `json:"next,omitempty"`
}

// APIEndpoints documents every route mounted by RegisterAPI. The core strips
// /api/v1/s3 before dispatching these patterns.
func (p *Plugin) APIEndpoints() []plugin.APIEndpoint {
	listQuery := []plugin.APIParam{
		{Name: "prefix", Description: "Only keys beginning with this exact prefix."},
		{Name: "delimiter", Description: "Group keys after the prefix into common prefixes."},
		{Name: "start_after", Description: "Start strictly after this key or common prefix."},
		{Name: "continuation", Description: "Continue strictly after the token returned by the previous page."},
		{Name: "max_keys", Description: "Maximum objects and common prefixes in this page; capped at 1000.", Type: "integer"},
	}
	return []plugin.APIEndpoint{
		{Method: http.MethodGet, Path: "/buckets", Description: "List current buckets and aggregate S3 catalog statistics.", Response: BucketsView{}},
		{Method: http.MethodDelete, Path: "/buckets", Description: "Delete every bucket, object, and inactive multipart upload while retaining the event history.", Status: http.StatusNoContent},
		{Method: http.MethodGet, Path: "/buckets/{bucket}", Description: "Get one current bucket snapshot and its object-list link.", Response: BucketView{}},
		{Method: http.MethodDelete, Path: "/buckets/{bucket}", Description: "Delete an empty bucket, or all of its state when recursive is true.", Query: []plugin.APIParam{{Name: "recursive", Description: "Delete objects and inactive multipart uploads before deleting the bucket.", Type: "boolean"}}, Status: http.StatusNoContent},
		{Method: http.MethodGet, Path: "/buckets/{bucket}/objects", Description: "List one bucket with exact S3 prefix, delimiter, continuation, and maximum-key semantics.", Query: listQuery, Response: ObjectsView{}},
		{Method: http.MethodGet, Path: "/buckets/{bucket}/objects/{key...}", Description: "Get metadata for one exact object key, without treating the key as a filesystem path.", Response: ObjectView{}},
		{Method: http.MethodDelete, Path: "/buckets/{bucket}/objects/{key...}", Description: "Delete one exact object and append an s3.object.delete event.", Status: http.StatusNoContent},
		{Method: http.MethodGet, Path: "/buckets/{bucket}/content/{key...}", Description: "Download one object's bytes and recorded representation metadata, with HTTP range support.", Produces: "application/octet-stream"},
	}
}

// RegisterAPI mounts the current-state S3 read-back API.
func (p *Plugin) RegisterAPI(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()
	p.store.Attach(d.Blobs)
	h := &apiHandler{p: p, d: d}
	mux.HandleFunc("GET /buckets", h.buckets)
	mux.HandleFunc("DELETE /buckets", h.clear)
	mux.HandleFunc("GET /buckets/{bucket}", h.bucket)
	mux.HandleFunc("DELETE /buckets/{bucket}", h.deleteBucket)
	mux.HandleFunc("GET /buckets/{bucket}/objects", h.objects)
	mux.HandleFunc("GET /buckets/{bucket}/objects/{key...}", h.object)
	mux.HandleFunc("DELETE /buckets/{bucket}/objects/{key...}", h.deleteObject)
	mux.HandleFunc("GET /buckets/{bucket}/content/{key...}", h.content)
}

type apiHandler struct {
	p *Plugin
	d plugin.Deps
}

func (h *apiHandler) buckets(w http.ResponseWriter, _ *http.Request) {
	buckets := h.p.store.ListBuckets()
	view := BucketsView{Buckets: make([]BucketView, 0, len(buckets)), Stats: h.p.store.Stats(), Self: APIPrefix + "/buckets"}
	for _, bucket := range buckets {
		view.Buckets = append(view.Buckets, newBucketView(bucket))
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *apiHandler) bucket(w http.ResponseWriter, r *http.Request) {
	bucket, err := h.p.store.HeadBucket(r.PathValue("bucket"))
	if err != nil {
		writeS3Error(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newBucketView(bucket))
}

func (h *apiHandler) objects(w http.ResponseWriter, r *http.Request) {
	maxKeys := 0
	if raw := r.URL.Query().Get("max_keys"); raw != "" {
		var err error
		maxKeys, err = strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "max_keys must be an integer")
			return
		}
	}
	bucketName := r.PathValue("bucket")
	bucket, err := h.p.store.HeadBucket(bucketName)
	if err != nil {
		writeS3Error(w, err)
		return
	}
	opts := ListOptions{
		Prefix:            r.URL.Query().Get("prefix"),
		Delimiter:         r.URL.Query().Get("delimiter"),
		StartAfter:        r.URL.Query().Get("start_after"),
		ContinuationToken: r.URL.Query().Get("continuation"),
		MaxKeys:           maxKeys,
	}
	result, err := h.p.store.ListObjects(bucketName, opts)
	if err != nil {
		writeS3Error(w, err)
		return
	}
	effectiveMax := maxKeys
	if effectiveMax <= 0 || effectiveMax > 1000 {
		effectiveMax = 1000
	}
	view := ObjectsView{
		Bucket:                newBucketView(bucket),
		Prefix:                opts.Prefix,
		Delimiter:             opts.Delimiter,
		StartAfter:            opts.StartAfter,
		ContinuationToken:     opts.ContinuationToken,
		MaxKeys:               effectiveMax,
		Objects:               make([]ObjectView, 0, len(result.Objects)),
		CommonPrefixes:        make([]CommonPrefixView, 0, len(result.CommonPrefixes)),
		IsTruncated:           result.IsTruncated,
		NextContinuationToken: result.NextContinuationToken,
	}
	view.Self = objectsURL(bucketName, opts)
	for _, object := range result.Objects {
		view.Objects = append(view.Objects, newObjectView(object, opts.Prefix, opts.Delimiter))
	}
	for _, prefix := range result.CommonPrefixes {
		nextOpts := opts
		nextOpts.Prefix = prefix
		nextOpts.StartAfter = ""
		nextOpts.ContinuationToken = ""
		view.CommonPrefixes = append(view.CommonPrefixes, CommonPrefixView{Prefix: prefix, List: objectsURL(bucketName, nextOpts)})
	}
	if result.NextContinuationToken != "" {
		nextOpts := opts
		nextOpts.StartAfter = ""
		nextOpts.ContinuationToken = result.NextContinuationToken
		view.Next = objectsURL(bucketName, nextOpts)
	}
	writeJSON(w, http.StatusOK, view)
}

func (h *apiHandler) object(w http.ResponseWriter, r *http.Request) {
	object, err := h.p.store.HeadObject(r.PathValue("bucket"), r.PathValue("key"))
	if err != nil {
		writeS3Error(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newObjectView(object, "", ""))
}

func (h *apiHandler) content(w http.ResponseWriter, r *http.Request) {
	reader, object, err := h.p.store.OpenObject(r.Context(), r.PathValue("bucket"), r.PathValue("key"))
	if err != nil {
		writeS3Error(w, err)
		return
	}
	defer func() { _ = reader.Close() }()

	head := w.Header()
	contentType := object.Headers.ContentType
	if contentType == "" || !validHeaderValue(contentType) {
		contentType = "application/octet-stream"
	}
	name := object.Key
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	head.Set("Content-Type", contentType)
	// The bytes and their headers are whatever a client uploaded, and this
	// route shares the UI's origin. The stored Content-Disposition is not
	// echoed - an "inline" text/html object would otherwise run as tommy - so
	// the browser always downloads, and a sandbox CSP covers anything that
	// still manages to render.
	head.Set("X-Content-Type-Options", "nosniff")
	head.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	head.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	setHeader(head, "Content-Encoding", object.Headers.ContentEncoding)
	setHeader(head, "Content-Language", object.Headers.ContentLanguage)
	setHeader(head, "Cache-Control", object.Headers.CacheControl)
	if !object.Headers.Expires.IsZero() {
		head.Set("Expires", object.Headers.Expires.UTC().Format(http.TimeFormat))
	}
	if object.ETag != "" {
		head.Set("ETag", quoteETag(object.ETag))
	}
	setHeader(head, "X-Amz-Checksum-Crc32", object.Checksums.CRC32)
	setHeader(head, "X-Amz-Checksum-Crc32c", object.Checksums.CRC32C)
	setHeader(head, "X-Amz-Checksum-Sha1", object.Checksums.SHA1)
	setHeader(head, "X-Amz-Checksum-Sha256", object.Checksums.SHA256)
	for key, value := range object.Metadata {
		name := "X-Amz-Meta-" + key
		if validHeaderName(name) {
			setHeader(head, name, value)
		}
	}
	http.ServeContent(w, r, name, object.LastModified, reader)
}

func (h *apiHandler) deleteObject(w http.ResponseWriter, r *http.Request) {
	_, removed, err := h.p.session(h.d).DeleteObject(r.Context(), r.PathValue("bucket"), r.PathValue("key"), WithEventRaw(rawHTTPRequest(r)))
	if err != nil {
		writeS3Error(w, err)
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "s3: object not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *apiHandler) deleteBucket(w http.ResponseWriter, r *http.Request) {
	_, err := h.p.session(h.d).DeleteBucket(r.Context(), r.PathValue("bucket"), boolParam(r, "recursive"), WithEventRaw(rawHTTPRequest(r)))
	if err != nil {
		writeS3Error(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *apiHandler) clear(w http.ResponseWriter, r *http.Request) {
	session := h.p.session(h.d)
	raw := rawHTTPRequest(r)
	for _, bucket := range h.p.store.ListBuckets() {
		if _, err := session.DeleteBucket(r.Context(), bucket.Name, true, WithEventRaw(raw)); err != nil {
			writeS3Error(w, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func newBucketView(bucket Bucket) BucketView {
	base := APIPrefix + "/buckets/" + escapePathValue(bucket.Name)
	return BucketView{Bucket: bucket, Links: BucketLinks{Self: base, Objects: base + "/objects"}}
}

func newObjectView(object Object, prefix, delimiter string) ObjectView {
	base := APIPrefix + "/buckets/" + escapePathValue(object.Bucket)
	return ObjectView{Object: object, Links: ObjectLinks{
		Self:    base + "/objects/" + escapePathValue(object.Key),
		Content: base + "/content/" + escapePathValue(object.Key),
		List:    objectsURL(object.Bucket, ListOptions{Prefix: prefix, Delimiter: delimiter}),
	}}
}

func objectsURL(bucket string, opts ListOptions) string {
	base := APIPrefix + "/buckets/" + escapePathValue(bucket) + "/objects"
	q := url.Values{}
	if opts.Prefix != "" {
		q.Set("prefix", opts.Prefix)
	}
	if opts.Delimiter != "" {
		q.Set("delimiter", opts.Delimiter)
	}
	if opts.StartAfter != "" {
		q.Set("start_after", opts.StartAfter)
	}
	if opts.ContinuationToken != "" {
		q.Set("continuation", opts.ContinuationToken)
	}
	if opts.MaxKeys != 0 {
		q.Set("max_keys", strconv.Itoa(opts.MaxKeys))
	}
	if encoded := q.Encode(); encoded != "" {
		return base + "?" + encoded
	}
	return base
}

// escapePathValue encodes an entire bucket or key as one URL path value. In
// particular, slashes, repeated slashes, and dot segments remain data rather
// than becoming path syntax that net/http is allowed to clean.
func escapePathValue(value string) string {
	escaped := url.PathEscape(value)
	return strings.ReplaceAll(escaped, ".", "%2E")
}

func quoteETag(etag string) string {
	etag = strings.TrimSpace(etag)
	if len(etag) >= 2 && etag[0] == '"' && etag[len(etag)-1] == '"' {
		return etag
	}
	return `"` + strings.ReplaceAll(etag, `"`, "") + `"`
}

func setHeader(header http.Header, name, value string) {
	if value != "" && validHeaderValue(value) {
		header.Set(name, value)
	}
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	const separators = `()<>@,;:\"/[]?={} `
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c <= 0x20 || c >= 0x7f || strings.ContainsRune(separators, rune(c)) {
			return false
		}
	}
	return true
}

func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '\r' || c == '\n' || (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

func rawHTTPRequest(r *http.Request) event.Raw {
	return event.Raw{
		Transport: "http",
		PeerAddr:  r.RemoteAddr,
		Method:    r.Method,
		Path:      r.URL.RequestURI(),
		Headers:   r.Header.Clone(),
		Text:      true,
	}
}

func statusForS3(err error) int {
	switch {
	case errors.Is(err, ErrBucketNotFound), errors.Is(err, ErrObjectNotFound), errors.Is(err, ErrUploadNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrBucketExists), errors.Is(err, ErrBucketNotEmpty), errors.Is(err, ErrUploadBusy):
		return http.StatusConflict
	case errors.Is(err, ErrInvalidBucket), errors.Is(err, ErrInvalidKey), errors.Is(err, ErrInvalidPart), errors.Is(err, ErrMetadataTooLarge):
		return http.StatusBadRequest
	case errors.Is(err, ErrBucketLimit), errors.Is(err, ErrObjectLimit), errors.Is(err, ErrUploadLimit), errors.Is(err, ErrPartLimit), errors.Is(err, ErrObjectTooLarge):
		return http.StatusInsufficientStorage
	default:
		return http.StatusInternalServerError
	}
}

func writeS3Error(w http.ResponseWriter, err error) {
	writeError(w, statusForS3(err), err.Error())
}

func boolParam(r *http.Request, name string) bool {
	switch strings.ToLower(r.URL.Query().Get(name)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
