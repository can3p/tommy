package files_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/can3p/tommy/core/config"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/files"
)

// fakeProvider is a test-only files provider. It exists because the real FTP
// and SFTP providers are the next task and the plugin still has to be proven
// end to end: it writes into the shared VFS exactly the way they will, through
// a Session, so every mutation lands in the tree and in the event log at once.
//
// It is also the worked example of the provider contract: it receives the VFS
// through BindVFS, records rather than rejects credentials, and mounts an
// ingress route that a snippet can actually exercise from a cold start.
type fakeProvider struct {
	vfs *files.VFS
}

func newFake() *fakeProvider { return &fakeProvider{} }

// BindVFS receives the tree the plugin created. This is how ftp and sftp will
// be handed the same filesystem.
func (p *fakeProvider) BindVFS(v *files.VFS) { p.vfs = v }

func (p *fakeProvider) Name() string   { return "fake" }
func (p *fakeProvider) Plugin() string { return files.PluginName }

func (p *fakeProvider) Description() string {
	return "A test-only file drop that accepts an upload over plain HTTP, so the files plugin can be exercised end to end before the FTP and SFTP providers exist."
}

func (p *fakeProvider) Endpoints() []plugin.Endpoint {
	return []plugin.Endpoint{
		{
			Method:      "PUT",
			Path:        "/fake-files/{path...}",
			Description: "Store the request body at that path in the shared filesystem, creating parent directories.",
		},
		{
			Method:      "DELETE",
			Path:        "/fake-files/{path...}",
			Description: "Delete a file or a whole directory from the shared filesystem.",
		},
	}
}

func (p *fakeProvider) Snippets() []plugin.Snippet {
	return []plugin.Snippet{{
		Title: "Upload a file",
		Lang:  "bash",
		Code:  `echo 'it works' | curl -s -T - {{.IngressURL}}/fake-files/upload/hello.txt`,
	}}
}

func (p *fakeProvider) RegisterIngress(mux plugin.Mux, d plugin.Deps) {
	d = d.Normalize()
	mux.HandleFunc("PUT /fake-files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		name := "/" + r.PathValue("path")
		s := p.session(d, r)
		dir, _ := splitDir(name)
		if _, err := s.MkdirAll(r.Context(), dir, files.WithCommand("MKD "+dir)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		n, err := s.Put(r.Context(), name, io.LimitReader(r.Body, 1<<20), files.WithCommand("STOR "+name))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, n.Path+"\n")
	})
	mux.HandleFunc("DELETE /fake-files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		name := "/" + r.PathValue("path")
		s := p.session(d, r)
		if _, _, err := s.RemoveAll(r.Context(), name, files.WithCommand("DELE "+name)); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// session binds the VFS to this provider for one request. Credentials are
// recorded, never checked - provider rule 1.
func (p *fakeProvider) session(d plugin.Deps, r *http.Request) *files.Session {
	user, _, _ := r.BasicAuth()
	return files.NewSession(p.vfs, d,
		files.WithProvider("fake"),
		files.WithTransport("http"),
		files.WithPeer(r.RemoteAddr),
		files.WithUser(user),
	)
}

// splitDir is the parent directory of a path, or "/".
func splitDir(name string) (string, string) {
	i := strings.LastIndexByte(name, '/')
	if i <= 0 {
		return "/", strings.TrimPrefix(name, "/")
	}
	return name[:i], name[i+1:]
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// harness is a whole tommy with the files plugin and the fake provider, plus
// the handles a test needs to write into the tree directly.
type harness struct {
	*testutil.Instance
	Plugin *files.Plugin
	VFS    *files.VFS
}

func start(t *testing.T) *harness {
	t.Helper()
	return startWith(t, nil)
}

// startWith boots with a config of the test's choosing - a tiny event capacity,
// for one.
func startWith(t *testing.T, cfg *config.Config) *harness {
	t.Helper()
	p := files.New(newFake())
	in := testutil.Start(t, cfg, p)
	return &harness{Instance: in, Plugin: p, VFS: p.VFS()}
}

// session writes into the tree as if a provider had done it.
func (h *harness) session(provider string) *files.Session {
	return files.NewSession(h.VFS, plugin.Deps{Store: h.Store, Blobs: h.Blobs},
		files.WithProvider(provider),
		files.WithTransport("ftp"),
		files.WithUser("anonymous"),
	)
}

// upload puts a file into the tree the way a provider does, events and all.
func (h *harness) upload(t *testing.T, provider, path, body string) files.Node {
	t.Helper()
	s := h.session(provider)
	dir, _ := splitDir(path)
	if _, err := s.MkdirAll(context.Background(), dir); err != nil {
		t.Fatalf("mkdirall %s: %v", dir, err)
	}
	n, err := s.PutBytes(context.Background(), path, []byte(body))
	if err != nil {
		t.Fatalf("upload %s: %v", path, err)
	}
	return n
}
