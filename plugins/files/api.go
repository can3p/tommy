package files

import (
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/can3p/tommy/core/plugin"
)

// EntryLinks are the absolute paths for the parts of an entry that are served
// as bytes or as another listing.
type EntryLinks struct {
	// Self is this entry's metadata.
	Self string `json:"self"`
	// Content streams a file's bytes. Empty for a directory.
	Content string `json:"content,omitempty"`
	// Tree lists a directory's children. Empty for a file.
	Tree string `json:"tree,omitempty"`
}

// EntryView is one node of the tree as the API reports it: the node itself,
// inlined, plus the links a client needs to go further.
type EntryView struct {
	Node
	Links EntryLinks `json:"links"`
}

// NewEntryView builds the API resource for one entry.
func NewEntryView(n Node) EntryView {
	v := EntryView{Node: n, Links: EntryLinks{Self: statURL(n.Path)}}
	if n.Dir {
		v.Links.Tree = treeURL(n.Path)
	} else {
		v.Links.Content = contentURL(n.Path)
	}
	return v
}

// Crumb is one step of the breadcrumb from the root down to the listed path.
type Crumb struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// URL lists this step.
	URL string `json:"url"`
}

// TreeView is what GET /tree returns: one directory listing, plus enough
// context to navigate without a second request.
type TreeView struct {
	Path string `json:"path"`
	// Parent is the directory above, empty at the root.
	Parent     string      `json:"parent,omitempty"`
	Breadcrumb []Crumb     `json:"breadcrumb"`
	Entries    []EntryView `json:"entries"`
	// Recursive says whether Entries covers the whole subtree.
	Recursive bool `json:"recursive,omitempty"`
	// Stats counts the whole filesystem, not just this directory.
	Stats Stats `json:"stats"`
}

// APIEndpoints documents what RegisterAPI mounts, and is what the OpenAPI
// description is generated from.
//
// These routes describe the filesystem rather than the event log: what is in
// the tree right now is what a test wants to assert on, and the operations
// that produced it are events on the generic surface.
func (p *Plugin) APIEndpoints() []plugin.Endpoint {
	return []plugin.Endpoint{
		{Method: "GET", Path: "/tree", Description: "One directory of the shared virtual filesystem, or the whole subtree.",
			Query: []plugin.Param{
				{Name: "path", Description: "The directory to list; the root when omitted."},
				{Name: "recursive", Description: "List the whole subtree rather than one level.", Type: "boolean"},
			},
			Response: TreeView{}},
		{Method: "DELETE", Path: "/tree", Description: "Empty the virtual filesystem.",
			Status: http.StatusNoContent},
		{Method: "GET", Path: "/stat/{path...}", Description: "One entry's metadata: size, mode, times, and which provider wrote it.",
			Response: EntryView{}},
		{Method: "GET", Path: "/content/{path...}", Description: "A file's bytes, streamed from the blob store with range support.",
			Produces: "application/octet-stream"},
		{Method: "DELETE", Path: "/content/{path...}", Description: "Delete one file, or a directory with ?recursive=1.",
			Query:  []plugin.Param{{Name: "recursive", Description: "Required to delete a directory, so a mistyped path cannot take a subtree with it.", Type: "boolean"}},
			Status: http.StatusNoContent},
	}
}

// RegisterAPI mounts the files read-back API. The core strips /api/v1/files,
// so the patterns here are relative to it.
func (p *Plugin) RegisterAPI(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()
	p.vfs.Attach(d.Blobs)
	h := &apiHandler{p: p, d: d}
	mux.HandleFunc("GET /tree", h.tree)
	mux.HandleFunc("DELETE /tree", h.clear)
	mux.HandleFunc("GET /stat/{path...}", h.stat)
	mux.HandleFunc("GET /content/{path...}", h.content)
	mux.HandleFunc("DELETE /content/{path...}", h.delete)
}

type apiHandler struct {
	p *Plugin
	d plugin.Deps
}

// tree serves GET /tree?path=&recursive=. The listing comes from the VFS, not
// from the event log: the tree is state, and it outlives the events that built
// it.
func (h *apiHandler) tree(w http.ResponseWriter, r *http.Request) {
	v := h.p.vfs
	name := r.URL.Query().Get("path")
	if name == "" {
		name = "/"
	}
	clean, err := v.Resolve(name)
	if err != nil {
		writeVFSError(w, err)
		return
	}
	if !v.IsDir(clean) {
		if n, statErr := v.Stat(clean); statErr == nil && !n.Dir {
			writeError(w, http.StatusBadRequest, "not a directory: "+clean)
			return
		}
		writeError(w, http.StatusNotFound, "no such directory: "+clean)
		return
	}

	view := TreeView{
		Path:       clean,
		Breadcrumb: breadcrumb(clean),
		Entries:    []EntryView{},
		Stats:      v.Stats(),
	}
	if clean != "/" {
		parent, _ := split(clean)
		view.Parent = parent
	}

	if boolParam(r, "recursive") {
		view.Recursive = true
		prefix := clean
		if prefix != "/" {
			prefix += "/"
		}
		_ = v.Walk(func(n Node) error {
			if clean == "/" || strings.HasPrefix(n.Path, prefix) {
				view.Entries = append(view.Entries, NewEntryView(n))
			}
			return nil
		})
		writeJSON(w, http.StatusOK, view)
		return
	}

	entries, err := v.List(clean)
	if err != nil {
		writeVFSError(w, err)
		return
	}
	for _, n := range entries {
		view.Entries = append(view.Entries, NewEntryView(n))
	}
	writeJSON(w, http.StatusOK, view)
}

// stat serves GET /stat/{path...}: one entry, file or directory.
func (h *apiHandler) stat(w http.ResponseWriter, r *http.Request) {
	n, err := h.p.vfs.Stat("/" + r.PathValue("path"))
	if err != nil {
		writeVFSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, NewEntryView(n))
}

// content serves GET /content/{path...}: the file's bytes, streamed out of the
// blob store with the recorded content type, a correct Content-Length and
// range support - all of which come free from ServeContent because the blob
// store hands back a ReadSeeker.
func (h *apiHandler) content(w http.ResponseWriter, r *http.Request) {
	name := "/" + r.PathValue("path")
	n, err := h.p.vfs.Stat(name)
	if err != nil {
		writeVFSError(w, err)
		return
	}
	if n.Dir {
		writeError(w, http.StatusBadRequest, "is a directory: "+n.Path)
		return
	}
	f, err := h.p.vfs.Open(r.Context(), n.Path)
	if err != nil {
		writeVFSError(w, err)
		return
	}
	defer func() { _ = f.Close() }()

	ct := n.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	head := w.Header()
	head.Set("Content-Type", ct)
	// The bytes are whatever a client uploaded, so the browser must not be
	// allowed to decide they are something more interesting than they are.
	head.Set("X-Content-Type-Options", "nosniff")
	kind := "attachment"
	if boolParam(r, "inline") {
		kind = "inline"
	}
	head.Set("Content-Disposition", disposition(kind, n.Name))
	http.ServeContent(w, r, n.Name, n.ModTime, f)
}

// delete serves DELETE /content/{path...}. A non-empty directory needs
// ?recursive=1, so a mis-typed path cannot take a subtree with it.
func (h *apiHandler) delete(w http.ResponseWriter, r *http.Request) {
	name := "/" + r.PathValue("path")
	s := h.p.session(h.d)
	var err error
	if boolParam(r, "recursive") {
		_, _, err = s.RemoveAll(r.Context(), name, WithCommand("DELETE "+name))
	} else {
		_, err = s.Remove(r.Context(), name, WithCommand("DELETE "+name))
	}
	if err != nil {
		writeVFSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clear serves DELETE /tree: empty the whole filesystem.
//
// The captured events deliberately survive, the same way clearing mail leaves
// its attachments alone: the tree is state and the log is history, and wiping
// the state is itself something the history should show.
func (h *apiHandler) clear(w http.ResponseWriter, r *http.Request) {
	if _, err := h.p.session(h.d).Clear(r.Context(), WithCommand("DELETE /tree")); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// URL building
// ---------------------------------------------------------------------------

// escapePath percent-encodes each segment of an already-resolved path. A file
// name is untrusted input, so it is never pasted into a URL raw.
func escapePath(clean string) string {
	if clean == "/" {
		return ""
	}
	segs := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "/" + strings.Join(segs, "/")
}

func contentURL(clean string) string { return APIPrefix + "/content" + escapePath(clean) }
func statURL(clean string) string    { return APIPrefix + "/stat" + escapePath(clean) }

func treeURL(clean string) string {
	return APIPrefix + "/tree?path=" + url.QueryEscape(clean)
}

// breadcrumb walks from the root down to clean.
func breadcrumb(clean string) []Crumb {
	crumbs := []Crumb{{Name: "/", Path: "/", URL: treeURL("/")}}
	if clean == "/" {
		return crumbs
	}
	cur := ""
	for _, seg := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		cur += "/" + seg
		crumbs = append(crumbs, Crumb{Name: seg, Path: cur, URL: treeURL(cur)})
	}
	return crumbs
}

// ---------------------------------------------------------------------------
// Responses
// ---------------------------------------------------------------------------

// statusFor maps a VFS error onto the HTTP status that says the same thing.
func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrNotExist):
		return http.StatusNotFound
	case errors.Is(err, ErrNotEmpty), errors.Is(err, ErrExist):
		return http.StatusConflict
	case errors.Is(err, ErrIsDir), errors.Is(err, ErrNotDir):
		return http.StatusBadRequest
	case errors.Is(err, fs.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, ErrDirFull), errors.Is(err, ErrTreeFull), errors.Is(err, ErrFileTooLarge):
		return http.StatusInsufficientStorage
	default:
		return http.StatusInternalServerError
	}
}

func writeVFSError(w http.ResponseWriter, err error) {
	writeError(w, statusFor(err), err.Error())
}

// disposition builds a Content-Disposition value, RFC 2231-encoding a filename
// that is not plain ASCII rather than mangling it.
func disposition(kind, filename string) string {
	if filename == "" {
		return kind
	}
	return mime.FormatMediaType(kind, map[string]string{"filename": filename})
}

func boolParam(r *http.Request, name string) bool {
	v := strings.ToLower(r.URL.Query().Get(name))
	return v == "1" || v == "true" || v == "yes"
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
