package files

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

// ActivityLimit caps how many recent events the tab's activity list shows.
const ActivityLimit = 50

// RegisterUI mounts the file-browser tab. The core strips /ui/files, so the
// patterns here are relative to it.
//
// GET /events/{id} is deliberately left unclaimed, so the core's generic view
// still opens any files event in a raw inspector - which is where you go when
// you want to see exactly what the protocol did.
func (p *Plugin) RegisterUI(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()
	p.vfs.Attach(d.Blobs)
	h := &uiHandler{p: p, d: d}
	h.tpl, h.tplErr = coreui.PluginTemplates(p.Templates())

	mux.HandleFunc("GET /{$}", h.page)
	mux.HandleFunc("GET /list", h.list)
	mux.HandleFunc("DELETE /entry", h.deleteEntry)
	mux.HandleFunc("DELETE /tree", h.clear)
}

type uiHandler struct {
	p      *Plugin
	d      plugin.Deps
	tpl    *template.Template
	tplErr error
}

// crumbView is one step of the breadcrumb.
//
// URL is the page a browser navigates to and what htmx pushes into the
// address bar; FetchURL is the fragment htmx actually swaps in. Every
// navigable thing in this tab carries both, so the tab works with or without
// JavaScript and a directory URL can still be pasted into a bug report.
type crumbView struct {
	// Name is a path segment, so it is untrusted input: the template
	// interpolates it as a plain string and never as template.HTML.
	Name     string
	URL      string
	FetchURL string
	Last     bool
}

// entryRow is one line of the directory table.
type entryRow struct {
	// Name and Path are untrusted: a client picks them.
	Name string
	Path string
	Dir  bool
	Size int64
	// SizeText is Size rendered for humans; blank for a directory.
	SizeText    string
	ModTime     time.Time
	Provider    string
	ContentType string
	// URL browses a directory or downloads a file; FetchURL is the listing
	// fragment htmx swaps in for a directory, empty for a file.
	URL      string
	FetchURL string
	// DeleteURL removes this entry and returns the refreshed listing.
	DeleteURL string
}

// activityRow is one line of the "recent activity" list: the history half of
// the tab, which is what makes an overwritten or deleted file still visible.
type activityRow struct {
	ID       string
	Type     string
	Op       string
	Provider string
	Text     string
	At       time.Time
	EventURL string
}

// tabView is everything the tab renders.
type tabView struct {
	Base    string
	APIBase string

	Path           string
	AtRoot         bool
	ParentURL      string
	ParentFetchURL string
	Crumbs         []crumbView
	Entries        []entryRow
	Activity       []activityRow
	Stats          Stats

	// Missing is set when the path in the URL is not there any more, which
	// happens routinely: something deletes a directory while you are in it.
	Missing bool

	// Info describes this plugin's providers, scoped to the files tab, so the
	// how-to-test panel and the empty state offer a command that actually puts
	// a file in, rendered against the ports this instance bound.
	Info []plugin.PluginInfo
}

// Empty reports whether the whole filesystem is empty, not just this directory.
func (v tabView) Empty() bool { return v.Stats.Dirs == 0 && v.Stats.Files == 0 }

// HowToTest carries the panel that says how to get files in here, open when
// nothing has been uploaded yet - which is exactly when someone needs it.
func (v tabView) HowToTest() components.HowToTest {
	return components.HowToTest{Info: v.Info, Open: v.Empty()}
}

// EmptyState is shown instead of a listing when nothing has been uploaded.
func (v tabView) EmptyState() components.EmptyState {
	return components.EmptyState{
		Title:     "No files yet",
		Message:   "Point an FTP or SFTP client at tommy and whatever it uploads appears here, live - browsable, downloadable, and with the protocol that wrote each file.",
		Providers: flattenProviders(v.Info),
	}
}

// ListURL is where the listing fragment is fetched from, carrying the
// directory currently open so a live refresh does not jump back to the root.
func (v tabView) ListURL() string {
	return v.Base + "/list?path=" + url.QueryEscape(v.Path)
}

// PageURL is the same directory as an address a browser can navigate to.
func (v tabView) PageURL() string {
	if v.AtRoot {
		return v.Base + "/"
	}
	return v.Base + "/?path=" + url.QueryEscape(v.Path)
}

// ClearURL empties the whole filesystem.
func (v tabView) ClearURL() string { return v.Base + "/tree" }

// RefreshTrigger is the htmx trigger that keeps the tab live. It is built from
// EventTypes rather than written out, so a provider adding a new files.* type
// only has to add it to that list.
func (v tabView) RefreshTrigger() string {
	parts := make([]string, 0, len(EventTypes))
	for _, t := range EventTypes {
		parts = append(parts, "sse:"+t+" throttle:400ms")
	}
	return strings.Join(parts, ", ")
}

// flattenProviders pulls the providers out of a plugin info list.
func flattenProviders(info []plugin.PluginInfo) []plugin.ProviderInfo {
	var out []plugin.ProviderInfo
	for _, p := range info {
		out = append(out, p.Providers...)
	}
	return out
}

// view builds the whole tab: the tree from the VFS, the activity from the
// store. Two sources on purpose - state and history are not the same thing.
func (h *uiHandler) view(r *http.Request) (tabView, error) {
	shell := coreui.ShellFrom(r)
	v := tabView{
		Base:    UIPrefix,
		APIBase: strings.TrimSuffix(shell.APIBase, "/"),
		Info:    providerInfo(shell),
		Stats:   h.p.vfs.Stats(),
	}

	want := r.URL.Query().Get("path")
	if want == "" {
		want = "/"
	}
	clean, err := h.p.vfs.Resolve(want)
	if err != nil {
		clean = "/"
	}
	if !h.p.vfs.IsDir(clean) && clean != "/" {
		v.Missing = true
		clean = "/"
	}
	v.Path = clean
	v.AtRoot = clean == "/"
	if !v.AtRoot {
		parent, _ := split(clean)
		v.ParentURL = h.browseURL(parent)
		v.ParentFetchURL = h.fetchURL(parent)
	}
	for _, c := range breadcrumb(clean) {
		v.Crumbs = append(v.Crumbs, crumbView{
			Name:     c.Name,
			URL:      h.browseURL(c.Path),
			FetchURL: h.fetchURL(c.Path),
		})
	}
	if n := len(v.Crumbs); n > 0 {
		v.Crumbs[n-1].Last = true
	}

	entries, err := h.p.vfs.List(clean)
	if err != nil {
		return v, err
	}
	for _, n := range entries {
		row := entryRow{
			Name:        n.Name,
			Path:        n.Path,
			Dir:         n.Dir,
			Size:        n.Size,
			ModTime:     n.ModTime,
			Provider:    n.Provider,
			ContentType: n.ContentType,
			DeleteURL:   v.Base + "/entry?path=" + url.QueryEscape(n.Path),
		}
		if n.Dir {
			row.URL = h.browseURL(n.Path)
			row.FetchURL = h.fetchURL(n.Path)
		} else {
			row.URL = v.APIBase + "/" + PluginName + "/content" + escapePath(n.Path)
			row.SizeText = components.BytesHuman(n.Size)
		}
		v.Entries = append(v.Entries, row)
	}

	events, err := h.d.Store.List(r.Context(), store.Query{Plugin: PluginName, Limit: ActivityLimit})
	if err != nil {
		return v, err
	}
	for _, e := range events {
		op, ok := OpOf(e)
		if !ok {
			continue
		}
		v.Activity = append(v.Activity, activityRow{
			ID:       string(e.ID),
			Type:     e.Type,
			Op:       op.Op,
			Provider: e.Provider,
			Text:     op.Snippet(),
			At:       e.ReceivedAt,
			EventURL: coreui.EventURL("", e.ID),
		})
	}
	return v, nil
}

// browseURL is the address bar URL for a directory: a real page, so a deep
// link into a subdirectory can be pasted anywhere.
func (h *uiHandler) browseURL(clean string) string {
	if clean == "/" {
		return UIPrefix + "/"
	}
	return UIPrefix + "/?path=" + url.QueryEscape(clean)
}

// fetchURL is the fragment htmx swaps in for the same directory.
func (h *uiHandler) fetchURL(clean string) string {
	return UIPrefix + "/list?path=" + url.QueryEscape(clean)
}

func (h *uiHandler) page(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, err := h.render("files-tab", v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := coreui.Render(w, r, "Files", body); err != nil {
		h.d.Logger.Warn("render files tab", "err", err)
	}
}

func (h *uiHandler) list(w http.ResponseWriter, r *http.Request) {
	v, err := h.view(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.fragment(w, "files-body", v)
}

// deleteEntry removes one entry and returns the refreshed tab body, so the
// listing, the counters and the activity list all update in one swap.
func (h *uiHandler) deleteEntry(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("path")
	if name == "" {
		http.Error(w, "no path given", http.StatusBadRequest)
		return
	}
	s := h.p.session(h.d)
	if _, _, err := s.RemoveAll(r.Context(), name, WithCommand("DELETE "+name)); err != nil {
		http.Error(w, err.Error(), statusFor(err))
		return
	}
	h.list(w, r)
}

// clear empties the filesystem. The events survive, so the activity list still
// shows what was there - including the clear itself.
func (h *uiHandler) clear(w http.ResponseWriter, r *http.Request) {
	if _, err := h.p.session(h.d).Clear(r.Context(), WithCommand("DELETE /tree")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Whatever directory was open is gone, so the refreshed body starts again
	// at the root.
	r.URL.RawQuery = ""
	h.list(w, r)
}

func (h *uiHandler) render(name string, data any) (template.HTML, error) {
	if h.tplErr != nil {
		return "", h.tplErr
	}
	var buf bytes.Buffer
	if err := h.tpl.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
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

// providerInfo pulls this plugin's own entry out of the shell, so the
// how-to-test panel and the empty state show snippets rendered against the
// ports actually in use.
func providerInfo(shell *coreui.Shell) []plugin.PluginInfo {
	for _, p := range shell.Info() {
		if p.Name == PluginName {
			return []plugin.PluginInfo{p}
		}
	}
	return nil
}
