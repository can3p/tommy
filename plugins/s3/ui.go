package s3

import (
	"bytes"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/can3p/tommy/core/plugin"
	coreui "github.com/can3p/tommy/core/server/ui"
	"github.com/can3p/tommy/core/server/ui/components"
	"github.com/can3p/tommy/core/store"
)

// ActivityLimit caps the recent logical operations rendered by the S3 tab.
const ActivityLimit = 50

// RegisterUI mounts the S3 bucket and prefix browser. GET /events/{id} is left
// unclaimed so the core generic event inspector remains the detail view.
func (p *Plugin) RegisterUI(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()
	p.store.Attach(d.Blobs)
	h := &uiHandler{p: p, d: d}
	h.tpl, h.tplErr = coreui.PluginTemplates(p.Templates())
	mux.HandleFunc("GET /{$}", h.page)
	mux.HandleFunc("GET /list", h.list)
	mux.HandleFunc("DELETE /object", h.deleteObject)
	mux.HandleFunc("DELETE /bucket", h.deleteBucket)
	mux.HandleFunc("DELETE /buckets", h.clear)
}

type uiHandler struct {
	p      *Plugin
	d      plugin.Deps
	tpl    *template.Template
	tplErr error
}

type bucketRow struct {
	Name     string
	Objects  int
	Bytes    int64
	Selected bool
	URL      string
	FetchURL string
}

type prefixCrumb struct {
	Name     string
	Prefix   string
	URL      string
	FetchURL string
	Last     bool
}

type prefixRow struct {
	Prefix   string
	URL      string
	FetchURL string
}

type objectRow struct {
	Key         string
	Size        int64
	SizeText    string
	Modified    time.Time
	ETag        string
	ContentType string
	Metadata    map[string]string
	DownloadURL string
	DeleteURL   string
}

type s3ActivityRow struct {
	Type     string
	Text     string
	Provider string
	At       time.Time
	EventURL string
}

type s3TabView struct {
	Base    string
	APIBase string
	Info    []plugin.PluginInfo
	Stats   Stats

	Buckets       []bucketRow
	Selected      *Bucket
	Requested     string
	MissingBucket bool
	Prefix        string
	Delimiter     string
	Crumbs        []prefixCrumb
	Prefixes      []prefixRow
	Objects       []objectRow
	Activity      []s3ActivityRow
	Truncated     bool
	NextURL       string
	NextFetchURL  string
}

func (v s3TabView) Empty() bool { return v.Stats.Buckets == 0 }

func (v s3TabView) HowToTest() components.HowToTest {
	return components.HowToTest{Info: v.Info, Open: v.Empty()}
}

func (v s3TabView) EmptyState() components.EmptyState {
	return components.EmptyState{
		Title:     "No S3 buckets yet",
		Message:   "Point an S3 client at tommy. Buckets and objects will appear here live, with exact keys and downloadable bytes.",
		Providers: flattenS3Providers(v.Info),
	}
}

func (v s3TabView) ListURL() string {
	return s3UIURL(v.Base+"/list", selectedName(v.Selected), v.Prefix, v.Delimiter)
}

func (v s3TabView) PageURL() string {
	return s3UIURL(v.Base+"/", selectedName(v.Selected), v.Prefix, v.Delimiter)
}

func (v s3TabView) ClearURL() string { return v.Base + "/buckets" }

func (v s3TabView) DeleteBucketURL() string {
	if v.Selected == nil {
		return ""
	}
	q := url.Values{"bucket": {v.Selected.Name}, "recursive": {"1"}}
	return v.Base + "/bucket?" + q.Encode()
}

func (v s3TabView) RefreshTrigger() string {
	parts := make([]string, 0, len(EventTypes))
	for _, typ := range EventTypes {
		parts = append(parts, "sse:"+typ+" throttle:400ms")
	}
	return strings.Join(parts, ", ")
}

func selectedName(bucket *Bucket) string {
	if bucket == nil {
		return ""
	}
	return bucket.Name
}

func flattenS3Providers(info []plugin.PluginInfo) []plugin.ProviderInfo {
	var providers []plugin.ProviderInfo
	for _, item := range info {
		providers = append(providers, item.Providers...)
	}
	return providers
}

func (h *uiHandler) view(r *http.Request) (s3TabView, error) {
	shell := coreui.ShellFrom(r)
	view := s3TabView{
		Base:      UIPrefix,
		APIBase:   strings.TrimSuffix(shell.APIBase, "/"),
		Info:      s3ProviderInfo(shell),
		Stats:     h.p.store.Stats(),
		Requested: r.URL.Query().Get("bucket"),
		Prefix:    r.URL.Query().Get("prefix"),
		Delimiter: r.URL.Query().Get("delimiter"),
	}
	if view.Delimiter == "" {
		view.Delimiter = "/"
	}

	buckets := h.p.store.ListBuckets()
	selected := view.Requested
	if selected != "" {
		if _, err := h.p.store.HeadBucket(selected); err != nil {
			view.MissingBucket = true
			selected = ""
		}
	}
	if selected == "" && len(buckets) > 0 {
		selected = buckets[0].Name
	}
	for _, bucket := range buckets {
		row := bucketRow{Name: bucket.Name, Objects: bucket.Objects, Bytes: bucket.Bytes, Selected: bucket.Name == selected}
		row.URL = h.browseURL(bucket.Name, "", view.Delimiter)
		row.FetchURL = h.fetchURL(bucket.Name, "", view.Delimiter)
		view.Buckets = append(view.Buckets, row)
		if row.Selected {
			copy := bucket
			view.Selected = &copy
		}
	}

	if view.Selected != nil {
		result, err := h.p.store.ListObjects(view.Selected.Name, ListOptions{
			Prefix: view.Prefix, Delimiter: view.Delimiter, ContinuationToken: r.URL.Query().Get("continuation"),
		})
		if err != nil {
			return view, err
		}
		view.Crumbs = h.prefixCrumbs(view.Selected.Name, view.Prefix, view.Delimiter)
		for _, prefix := range result.CommonPrefixes {
			view.Prefixes = append(view.Prefixes, prefixRow{
				Prefix: prefix, URL: h.browseURL(view.Selected.Name, prefix, view.Delimiter), FetchURL: h.fetchURL(view.Selected.Name, prefix, view.Delimiter),
			})
		}
		for _, object := range result.Objects {
			base := view.APIBase + "/" + PluginName + "/buckets/" + escapePathValue(object.Bucket)
			deleteQuery := url.Values{
				"bucket": {object.Bucket}, "key": {object.Key}, "prefix": {view.Prefix}, "delimiter": {view.Delimiter},
			}
			view.Objects = append(view.Objects, objectRow{
				Key: object.Key, Size: object.Size, SizeText: components.BytesHuman(object.Size), Modified: object.LastModified,
				ETag: object.ETag, ContentType: object.Headers.ContentType, Metadata: object.Metadata,
				DownloadURL: base + "/content/" + escapePathValue(object.Key),
				DeleteURL:   view.Base + "/object?" + deleteQuery.Encode(),
			})
		}
		view.Truncated = result.IsTruncated
		if result.NextContinuationToken != "" {
			// UI pages at the store's normal maximum, so this is unusual but must
			// still be navigable without changing the selected prefix.
			view.NextURL = h.browseURL(view.Selected.Name, view.Prefix, view.Delimiter) + "&continuation=" + url.QueryEscape(result.NextContinuationToken)
			view.NextFetchURL = h.fetchURL(view.Selected.Name, view.Prefix, view.Delimiter) + "&continuation=" + url.QueryEscape(result.NextContinuationToken)
		}
	}

	events, err := h.d.Store.List(r.Context(), store.Query{Plugin: PluginName, Limit: ActivityLimit})
	if err != nil {
		return view, err
	}
	for _, item := range events {
		payload, ok := PayloadOf(item)
		if !ok {
			continue
		}
		view.Activity = append(view.Activity, s3ActivityRow{
			Type: item.Type, Text: payload.Snippet(), Provider: item.Provider, At: item.ReceivedAt, EventURL: coreui.EventURL("", item.ID),
		})
	}
	return view, nil
}

func (h *uiHandler) prefixCrumbs(bucket, prefix, delimiter string) []prefixCrumb {
	crumbs := []prefixCrumb{{Name: "/", Prefix: "", URL: h.browseURL(bucket, "", delimiter), FetchURL: h.fetchURL(bucket, "", delimiter)}}
	if prefix == "" {
		crumbs[0].Last = true
		return crumbs
	}
	start := 0
	for i := 0; i < len(prefix); i++ {
		if prefix[i] != '/' {
			continue
		}
		name := prefix[start:i]
		if name == "" {
			name = "(empty)"
		}
		exact := prefix[:i+1]
		crumbs = append(crumbs, prefixCrumb{Name: name, Prefix: exact, URL: h.browseURL(bucket, exact, delimiter), FetchURL: h.fetchURL(bucket, exact, delimiter)})
		start = i + 1
	}
	if start < len(prefix) {
		exact := prefix
		crumbs = append(crumbs, prefixCrumb{Name: prefix[start:], Prefix: exact, URL: h.browseURL(bucket, exact, delimiter), FetchURL: h.fetchURL(bucket, exact, delimiter)})
	}
	crumbs[len(crumbs)-1].Last = true
	return crumbs
}

func (h *uiHandler) browseURL(bucket, prefix, delimiter string) string {
	return s3UIURL(UIPrefix+"/", bucket, prefix, delimiter)
}

func (h *uiHandler) fetchURL(bucket, prefix, delimiter string) string {
	return s3UIURL(UIPrefix+"/list", bucket, prefix, delimiter)
}

func s3UIURL(base, bucket, prefix, delimiter string) string {
	q := url.Values{}
	if bucket != "" {
		q.Set("bucket", bucket)
	}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if delimiter != "" {
		q.Set("delimiter", delimiter)
	}
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}

func (h *uiHandler) page(w http.ResponseWriter, r *http.Request) {
	view, err := h.view(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, err := h.render("s3-tab", view)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := coreui.Render(w, r, "S3", body); err != nil {
		h.d.Logger.Warn("render s3 tab", "err", err)
	}
}

func (h *uiHandler) list(w http.ResponseWriter, r *http.Request) {
	view, err := h.view(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.fragment(w, "s3-body", view)
}

func (h *uiHandler) deleteObject(w http.ResponseWriter, r *http.Request) {
	bucket, key := r.URL.Query().Get("bucket"), r.URL.Query().Get("key")
	if bucket == "" || key == "" {
		http.Error(w, "bucket and key are required", http.StatusBadRequest)
		return
	}
	_, removed, err := h.p.session(h.d).DeleteObject(r.Context(), bucket, key, WithEventRaw(rawHTTPRequest(r)))
	if err != nil {
		http.Error(w, err.Error(), statusForS3(err))
		return
	}
	if !removed {
		http.Error(w, "s3: object not found", http.StatusNotFound)
		return
	}
	h.list(w, r)
}

func (h *uiHandler) deleteBucket(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		http.Error(w, "bucket is required", http.StatusBadRequest)
		return
	}
	_, err := h.p.session(h.d).DeleteBucket(r.Context(), bucket, boolParam(r, "recursive"), WithEventRaw(rawHTTPRequest(r)))
	if err != nil {
		http.Error(w, err.Error(), statusForS3(err))
		return
	}
	r.URL.RawQuery = ""
	h.list(w, r)
}

func (h *uiHandler) clear(w http.ResponseWriter, r *http.Request) {
	session := h.p.session(h.d)
	raw := rawHTTPRequest(r)
	for _, bucket := range h.p.store.ListBuckets() {
		if _, err := session.DeleteBucket(r.Context(), bucket.Name, true, WithEventRaw(raw)); err != nil {
			http.Error(w, err.Error(), statusForS3(err))
			return
		}
	}
	r.URL.RawQuery = ""
	h.list(w, r)
}

func (h *uiHandler) render(name string, data any) (template.HTML, error) {
	if h.tplErr != nil {
		return "", h.tplErr
	}
	var output bytes.Buffer
	if err := h.tpl.ExecuteTemplate(&output, name, data); err != nil {
		return "", err
	}
	return template.HTML(output.String()), nil
}

func (h *uiHandler) fragment(w http.ResponseWriter, name string, data any) {
	body, err := h.render(name, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

func s3ProviderInfo(shell *coreui.Shell) []plugin.PluginInfo {
	for _, item := range shell.Info() {
		if item.Name == PluginName {
			return []plugin.PluginInfo{item}
		}
	}
	return nil
}
