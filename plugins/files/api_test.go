package files_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/plugins/files"
)

func TestAPITree(t *testing.T) {
	h := start(t)
	h.upload(t, "ftp", "/upload/report.csv", "a,b\n1,2\n")
	h.upload(t, "sftp", "/upload/nested/deep.txt", "deep")
	h.upload(t, "ftp", "/top.txt", "top")

	t.Run("root", func(t *testing.T) {
		var tree files.TreeView
		if status := h.GetJSON(h.API("/files/tree"), &tree); status != 200 {
			t.Fatalf("status = %d", status)
		}
		if tree.Path != "/" || tree.Parent != "" {
			t.Errorf("path/parent = %q/%q", tree.Path, tree.Parent)
		}
		if len(tree.Breadcrumb) != 1 || tree.Breadcrumb[0].Path != "/" {
			t.Errorf("breadcrumb = %+v", tree.Breadcrumb)
		}
		names := entryNames(tree)
		if strings.Join(names, ",") != "upload,top.txt" {
			t.Errorf("entries = %v, want the directory first", names)
		}
		if tree.Stats.Files != 3 || tree.Stats.Dirs != 2 {
			t.Errorf("stats = %+v", tree.Stats)
		}
	})

	t.Run("subdirectory", func(t *testing.T) {
		var tree files.TreeView
		if status := h.GetJSON(h.API("/files/tree?path=/upload"), &tree); status != 200 {
			t.Fatalf("status = %d", status)
		}
		if tree.Parent != "/" {
			t.Errorf("parent = %q", tree.Parent)
		}
		if len(tree.Breadcrumb) != 2 {
			t.Errorf("breadcrumb = %+v", tree.Breadcrumb)
		}
		var csv *files.EntryView
		for i := range tree.Entries {
			if tree.Entries[i].Name == "report.csv" {
				csv = &tree.Entries[i]
			}
		}
		if csv == nil {
			t.Fatalf("report.csv is missing from %v", entryNames(tree))
		}
		if csv.Provider != "ftp" {
			t.Errorf("provider = %q, want ftp", csv.Provider)
		}
		if csv.Size != 8 || csv.Blob.ID == "" {
			t.Errorf("entry = %+v", csv)
		}
		if csv.Links.Content != "/api/v1/files/content/upload/report.csv" {
			t.Errorf("content link = %q", csv.Links.Content)
		}
		if csv.Links.Tree != "" {
			t.Error("a file must not carry a tree link")
		}
	})

	t.Run("recursive", func(t *testing.T) {
		var tree files.TreeView
		if status := h.GetJSON(h.API("/files/tree?path=/upload&recursive=1"), &tree); status != 200 {
			t.Fatalf("status = %d", status)
		}
		names := entryNames(tree)
		if len(names) != 3 {
			t.Fatalf("recursive listing = %v, want three entries", names)
		}
	})

	t.Run("missing", func(t *testing.T) {
		var body map[string]string
		if status := h.GetJSON(h.API("/files/tree?path=/nope"), &body); status != 404 {
			t.Errorf("status = %d, want 404", status)
		}
	})

	t.Run("a file is not a directory", func(t *testing.T) {
		var body map[string]string
		if status := h.GetJSON(h.API("/files/tree?path=/top.txt"), &body); status != 400 {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("traversal in the query", func(t *testing.T) {
		// It resolves inside the tree rather than escaping it, so this is a
		// listing of the root and never anything of the host's.
		var tree files.TreeView
		if status := h.GetJSON(h.API("/files/tree?path="+url.QueryEscape("/../../../..")), &tree); status != 200 {
			t.Fatalf("status = %d", status)
		}
		if tree.Path != "/" {
			t.Errorf("path = %q, want /", tree.Path)
		}
	})
}

func TestAPIStat(t *testing.T) {
	h := start(t)
	h.upload(t, "ftp", "/dir/file.txt", "hello")

	var entry files.EntryView
	if status := h.GetJSON(h.API("/files/stat/dir/file.txt"), &entry); status != 200 {
		t.Fatalf("status = %d", status)
	}
	if entry.Path != "/dir/file.txt" || entry.Size != 5 || entry.Dir {
		t.Errorf("entry = %+v", entry)
	}
	if entry.Links.Self != "/api/v1/files/stat/dir/file.txt" {
		t.Errorf("self link = %q", entry.Links.Self)
	}

	var dir files.EntryView
	if status := h.GetJSON(h.API("/files/stat/dir"), &dir); status != 200 {
		t.Fatalf("status = %d", status)
	}
	if !dir.Dir || dir.Links.Content != "" || dir.Links.Tree == "" {
		t.Errorf("directory entry = %+v", dir)
	}

	var body map[string]string
	if status := h.GetJSON(h.API("/files/stat/dir/nope"), &body); status != 404 {
		t.Errorf("missing file status = %d, want 404", status)
	}
}

func TestAPIContentDownload(t *testing.T) {
	h := start(t)
	// Binary content, so a byte-exact assertion means something.
	payload := make([]byte, 0, 512)
	for i := range 512 {
		payload = append(payload, byte(i%251))
	}
	s := h.session("ftp")
	if _, err := s.MkdirAll(context.Background(), "/bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutBytes(context.Background(), "/bin/blob.dat", payload); err != nil {
		t.Fatal(err)
	}
	h.upload(t, "sftp", "/bin/notes.txt", "plain text\n")

	t.Run("byte exact", func(t *testing.T) {
		resp := h.Get(h.API("/files/content/bin/blob.dat"))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(payload) {
			t.Fatalf("downloaded %d bytes, want the %d uploaded", len(got), len(payload))
		}
		if resp.ContentLength != int64(len(payload)) {
			t.Errorf("Content-Length = %d, want %d", resp.ContentLength, len(payload))
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
			t.Errorf("Content-Type = %q", ct)
		}
		if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename=blob.dat`) {
			t.Errorf("Content-Disposition = %q", cd)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Error("uploaded bytes must be served with nosniff")
		}
	})

	t.Run("recorded content type", func(t *testing.T) {
		resp := h.Get(h.API("/files/content/bin/notes.txt?inline=1"))
		defer func() { _ = resp.Body.Close() }()
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("Content-Type = %q, want text/plain", ct)
		}
		if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
			t.Errorf("Content-Disposition = %q, want inline", cd)
		}
	})

	t.Run("range request", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, h.API("/files/content/bin/blob.dat"), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Range", "bytes=100-199")
		resp := h.Do(req)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", resp.StatusCode)
		}
		got, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 100 || string(got) != string(payload[100:200]) {
			t.Errorf("range body = %d bytes, want payload[100:200]", len(got))
		}
		if cr := resp.Header.Get("Content-Range"); cr != "bytes 100-199/512" {
			t.Errorf("Content-Range = %q", cr)
		}
	})

	t.Run("a directory is not content", func(t *testing.T) {
		var body map[string]string
		if status := h.GetJSON(h.API("/files/content/bin"), &body); status != 400 {
			t.Errorf("status = %d, want 400", status)
		}
	})

	t.Run("missing", func(t *testing.T) {
		var body map[string]string
		if status := h.GetJSON(h.API("/files/content/bin/nope.txt"), &body); status != 404 {
			t.Errorf("status = %d, want 404", status)
		}
	})
}

// A filename that needs escaping survives the round trip through the URL the
// API hands out.
func TestAPIContentEscapesNames(t *testing.T) {
	h := start(t)
	const name = "a b&c#d?e.txt"
	h.upload(t, "ftp", "/"+name, "escaped")

	var tree files.TreeView
	if status := h.GetJSON(h.API("/files/tree"), &tree); status != 200 {
		t.Fatalf("status = %d", status)
	}
	if len(tree.Entries) != 1 {
		t.Fatalf("entries = %v", entryNames(tree))
	}
	link := tree.Entries[0].Links.Content
	if strings.Contains(link, " ") || strings.Contains(link, "#") || strings.Contains(link, "?") {
		t.Fatalf("content link is not escaped: %q", link)
	}
	resp := h.Get(strings.TrimSuffix(h.APIURL, "/api/v1") + link)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "escaped" {
		t.Errorf("GET %s = %d %q", link, resp.StatusCode, body)
	}
}

func TestAPIDelete(t *testing.T) {
	h := start(t)
	h.upload(t, "ftp", "/dir/keep.txt", "keep")
	h.upload(t, "ftp", "/dir/sub/gone.txt", "gone")
	h.upload(t, "ftp", "/solo.txt", "solo")

	t.Run("a file", func(t *testing.T) {
		resp := h.Do(mustRequest(t, http.MethodDelete, h.API("/files/content/solo.txt")))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", resp.StatusCode)
		}
		if h.VFS.Exists("/solo.txt") {
			t.Error("the file survived")
		}
		// The deletion is recorded, so the log still explains where it went.
		events := h.Events(store.Query{Plugin: files.PluginName, Type: files.EventDelete})
		if len(events) != 1 {
			t.Fatalf("recorded %d delete events, want 1", len(events))
		}
		op, ok := files.OpOf(events[0])
		if !ok || op.Path != "/solo.txt" || op.Op != "delete" {
			t.Errorf("payload = %+v (decoded: %v)", op, ok)
		}
	})

	t.Run("a non-empty directory needs recursive", func(t *testing.T) {
		resp := h.Do(mustRequest(t, http.MethodDelete, h.API("/files/content/dir")))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409", resp.StatusCode)
		}
		if !h.VFS.Exists("/dir/keep.txt") {
			t.Error("a refused delete must change nothing")
		}
	})

	t.Run("recursive", func(t *testing.T) {
		resp := h.Do(mustRequest(t, http.MethodDelete, h.API("/files/content/dir?recursive=1")))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", resp.StatusCode)
		}
		if h.VFS.Exists("/dir") {
			t.Error("the subtree survived")
		}
	})

	t.Run("missing", func(t *testing.T) {
		resp := h.Do(mustRequest(t, http.MethodDelete, h.API("/files/content/nope")))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != 404 {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}

func TestAPIClearTree(t *testing.T) {
	h := start(t)
	h.upload(t, "ftp", "/a/b.txt", "b")
	h.upload(t, "sftp", "/c.txt", "c")

	resp := h.Do(mustRequest(t, http.MethodDelete, h.API("/files/tree")))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if got := h.VFS.Stats(); got != (files.Stats{}) {
		t.Errorf("Stats() = %+v, want empty", got)
	}
	// The history survives the state being wiped - including the wipe.
	events := h.Events(store.Query{Plugin: files.PluginName})
	if len(events) == 0 {
		t.Fatal("clearing the tree must not clear the event log")
	}
	if _, ok := files.OpOf(events[0]); !ok {
		t.Error("the newest event is not a files operation")
	}
}

// The whole reason the VFS and the event log are separate stores: a file stays
// downloadable long after the event announcing it has fallen out of the ring
// buffer. If this ever fails, uploads start disappearing under load.
func TestFileOutlivesItsEvent(t *testing.T) {
	cfg := config.Ephemeral()
	cfg.Storage.Capacity = 3
	h := startWith(t, cfg)

	uploaded := h.upload(t, "ftp", "/keep/me.txt", "still here")

	uploadEvents := h.Events(store.Query{Plugin: files.PluginName, Type: files.EventUpload})
	if len(uploadEvents) != 1 {
		t.Fatalf("recorded %d upload events, want 1", len(uploadEvents))
	}
	uploadID := uploadEvents[0].ID

	// Push the upload event out of the ring buffer.
	s := h.session("sftp")
	for i := range 10 {
		if _, err := s.Mkdir(context.Background(), "/noise"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Store.Get(context.Background(), uploadID); err == nil {
		t.Fatal("the upload event should have been evicted; the test proves nothing")
	}
	if evicted := h.Events(store.Query{Plugin: files.PluginName, Type: files.EventUpload}); len(evicted) != 0 {
		t.Fatalf("the upload event is still listed: %v", evicted)
	}

	// The file is still in the tree...
	if got, err := h.VFS.Stat("/keep/me.txt"); err != nil || got.Blob.ID != uploaded.Blob.ID {
		t.Fatalf("Stat after eviction = %+v, %v", got, err)
	}
	// ...still listed by the API...
	var tree files.TreeView
	if status := h.GetJSON(h.API("/files/tree?path=/keep"), &tree); status != 200 || len(tree.Entries) != 1 {
		t.Fatalf("tree after eviction: status %d, %v", status, entryNames(tree))
	}
	// ...and still downloadable, byte for byte.
	status, body := h.GetBody(h.API("/files/content/keep/me.txt"))
	if status != 200 || body != "still here" {
		t.Fatalf("download after eviction = %d %q", status, body)
	}
}

// The fake provider's ingress route is a real upload path, so the plugin is
// proven end to end rather than only through direct VFS calls.
func TestUploadThroughTheIngress(t *testing.T) {
	h := start(t)

	req := mustRequest(t, http.MethodPut, h.Ingress("/fake-files/upload/hello.txt"))
	req.Body = io.NopCloser(strings.NewReader("it works\n"))
	req.SetBasicAuth("someone", "any-password")
	resp := h.Do(req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	status, body := h.GetBody(h.API("/files/content/upload/hello.txt"))
	if status != 200 || body != "it works\n" {
		t.Fatalf("download = %d %q", status, body)
	}

	events := h.WaitForEvents(1, store.Query{Plugin: files.PluginName, Type: files.EventUpload}, 2*time.Second)
	if len(events) != 1 {
		t.Fatalf("recorded %d upload events", len(events))
	}
	e := events[0]
	if e.Provider != "fake" {
		t.Errorf("provider = %q", e.Provider)
	}
	if e.Summary.Title != "/upload/hello.txt" {
		t.Errorf("summary title = %q", e.Summary.Title)
	}
	if len(e.Raw.Body) == 0 {
		t.Error("Raw must always be populated")
	}
	// Credentials are recorded, never checked.
	if e.Meta["user"] != "someone" {
		t.Errorf("meta = %v, want the presented user recorded", e.Meta)
	}
	op, ok := files.OpOf(e)
	if !ok || op.Blob == nil || op.Blob.ID == "" {
		t.Fatalf("payload = %+v (decoded: %v)", op, ok)
	}

	// The event survives a JSON round trip, which is what /api/v1/events does.
	var listed []event.Event
	if status := h.GetJSON(h.API("/events?plugin=files&type=files.upload"), &listed); status != 200 {
		t.Fatalf("events status = %d", status)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d events", len(listed))
	}
	decoded, ok := files.OpOf(&listed[0])
	if !ok || decoded.Path != "/upload/hello.txt" || decoded.Size != 9 {
		t.Errorf("decoded from JSON = %+v (ok: %v)", decoded, ok)
	}
}

func entryNames(tree files.TreeView) []string {
	out := make([]string, 0, len(tree.Entries))
	for _, e := range tree.Entries {
		out = append(out, e.Name)
	}
	return out
}

func mustRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
