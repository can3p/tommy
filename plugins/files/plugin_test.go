package files_test

import (
	"context"
	"errors"
	"testing"

	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/plugin/plugintest"
	coreui "github.com/can3p/tommy/core/server/ui"
	"github.com/can3p/tommy/core/store"
	"github.com/can3p/tommy/core/testutil"
	"github.com/can3p/tommy/plugins/files"
)

func TestConformance(t *testing.T) {
	plugintest.Conformance(t, files.New(newFake()))
}

func TestPluginIdentity(t *testing.T) {
	t.Parallel()
	p := files.New()
	if p.Name() != "files" || p.Title() != "Files" {
		t.Errorf("name/title = %q/%q", p.Name(), p.Title())
	}
	if len(p.Description()) < plugintest.MinDescription {
		t.Errorf("description is too short: %q", p.Description())
	}
	if got := p.Providers(); got == nil || len(got) != 0 {
		t.Errorf("Providers() = %v, want an empty non-nil slice", got)
	}
	if p.Templates() == nil {
		t.Error("Templates() must expose the tab's templates")
	}
	if p.VFS() == nil {
		t.Error("New must create the shared filesystem")
	}
}

// New hands every provider the same VFS, which is the whole reason a file
// uploaded over SFTP is visible over FTP and in the tab.
func TestProvidersShareOneVFS(t *testing.T) {
	t.Parallel()
	a, b := newFake(), newFake()
	p := files.New(a, b)
	if a.vfs == nil || b.vfs == nil {
		t.Fatal("BindVFS was not called")
	}
	if a.vfs != p.VFS() || b.vfs != p.VFS() {
		t.Fatal("providers were given different filesystems")
	}
}

// The tab is composed from the shared component library, so its templates only
// parse together with the components.
func TestTemplatesParseWithTheComponentLibrary(t *testing.T) {
	t.Parallel()
	tpl, err := coreui.PluginTemplates(files.New().Templates())
	if err != nil {
		t.Fatalf("PluginTemplates: %v", err)
	}
	for _, name := range []string{"files-tab", "files-body", "files-table", "files-activity", "files-style"} {
		if tpl.Lookup(name) == nil {
			t.Errorf("template %q is missing", name)
		}
	}
}

// A plugin with no providers must still boot, serve its API and render its
// tab: that is what this task ships before the FTP and SFTP providers land.
func TestPluginWithoutProvidersStillServes(t *testing.T) {
	p := files.New()
	in := testutil.Start(t, nil, p)

	if _, err := p.VFS().PutBytes(context.Background(), "/direct.txt", []byte("x"), files.WriteOptions{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	var tree files.TreeView
	if status := in.GetJSON(in.API("/files/tree"), &tree); status != 200 || len(tree.Entries) != 1 {
		t.Fatalf("tree = %d %v", status, tree.Entries)
	}
	if status, _ := in.GetBody(in.UI("/files/")); status != 200 {
		t.Fatalf("tab status = %d", status)
	}
}

// Every mutation lands in the tree and in the log at once, tagged with the
// provider that did it. That pairing is the plugin's central claim.
func TestSessionRecordsEveryMutation(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	s := h.session("ftp")

	if _, err := s.Mkdir(ctx, "/inbox"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := s.PutBytes(ctx, "/inbox/a.txt", []byte("hello")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := s.Rename(ctx, "/inbox/a.txt", "/inbox/b.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := s.Remove(ctx, "/inbox/b.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}

	events := h.Events(store.Query{Plugin: files.PluginName})
	if len(events) != 4 {
		t.Fatalf("recorded %d events, want 4", len(events))
	}
	// Newest first.
	wantTypes := []string{files.EventDelete, files.EventRename, files.EventUpload, files.EventMkdir}
	for i, e := range events {
		if e.Type != wantTypes[i] {
			t.Errorf("event %d type = %q, want %q", i, e.Type, wantTypes[i])
		}
		if e.Provider != "ftp" {
			t.Errorf("event %d provider = %q, want ftp", i, e.Provider)
		}
		if e.Raw.Transport != "ftp" || len(e.Raw.Body) == 0 {
			t.Errorf("event %d raw = %+v; Raw must always be populated", i, e.Raw)
		}
		if !files.IsFilesEvent(e) {
			t.Errorf("event %d is not recognized as a files event", i)
		}
		if _, ok := files.OpOf(e); !ok {
			t.Errorf("event %d payload does not decode", i)
		}
	}

	rename, _ := files.OpOf(events[1])
	if rename.From != "/inbox/a.txt" || rename.Path != "/inbox/b.txt" {
		t.Errorf("rename payload = %+v", rename)
	}
	if events[1].Summary.Title != "/inbox/a.txt → /inbox/b.txt" {
		t.Errorf("rename summary = %q", events[1].Summary.Title)
	}
	upload, _ := files.OpOf(events[2])
	if upload.Size != 5 || upload.Blob == nil {
		t.Errorf("upload payload = %+v", upload)
	}
	if events[2].Summary.From != "anonymous" {
		t.Errorf("summary from = %q, want the recorded user", events[2].Summary.From)
	}

	// A search over the log finds the path, because the summary carries it.
	found := h.Events(store.Query{Plugin: files.PluginName, Search: "inbox/b.txt"})
	if len(found) == 0 {
		t.Error("searching the log by path found nothing")
	}
}

// A write that fails records nothing: the log must not claim a file arrived
// when it did not.
func TestFailedMutationsRecordNothing(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	s := h.session("ftp")

	if _, err := s.PutBytes(ctx, "/missing-dir/a.txt", []byte("x")); !errors.Is(err, files.ErrNotExist) {
		t.Fatalf("put into a missing directory = %v", err)
	}
	if _, err := s.Remove(ctx, "/nope"); !errors.Is(err, files.ErrNotExist) {
		t.Fatalf("remove a missing file = %v", err)
	}
	if _, err := s.Mkdir(ctx, "/a\x00b"); !errors.Is(err, files.ErrInvalidPath) {
		t.Fatalf("mkdir with a hostile path = %v", err)
	}
	if events := h.Events(store.Query{Plugin: files.PluginName}); len(events) != 0 {
		t.Fatalf("failed operations recorded %d events", len(events))
	}
}

// A handle abandoned mid-transfer leaves neither a file nor an event.
func TestAbortedTransferRecordsNothing(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	s := h.session("sftp")

	f, err := s.Create(ctx, "/half.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	if err := f.Abort(); err != nil {
		t.Fatal(err)
	}
	if h.VFS.Exists("/half.bin") {
		t.Error("the aborted file is in the tree")
	}
	if events := h.Events(store.Query{Plugin: files.PluginName}); len(events) != 0 {
		t.Fatalf("an aborted transfer recorded %d events", len(events))
	}

	// The same handle closed instead of aborted records exactly one.
	f2, err := s.Create(ctx, "/whole.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.Write([]byte("whole")); err != nil {
		t.Fatal(err)
	}
	if err := f2.Close(); err != nil {
		t.Fatal(err)
	}
	events := h.Events(store.Query{Plugin: files.PluginName, Type: files.EventUpload})
	if len(events) != 1 {
		t.Fatalf("recorded %d upload events, want 1", len(events))
	}
	op, _ := files.OpOf(events[0])
	if op.Path != "/whole.bin" || op.Size != 5 {
		t.Errorf("payload = %+v", op)
	}
}

// A Session made with a plugin.Deps carrying the core blob store attaches it,
// so a provider never has to think about where the bytes go.
func TestSessionAttachesTheBlobStore(t *testing.T) {
	in := testutil.Start(t, nil, files.New())
	v := files.NewVFS()
	s := files.NewSession(v, plugin.Deps{Store: in.Store, Blobs: in.Blobs}, files.WithProvider("ftp"))
	if v.Blobs() != in.Blobs {
		t.Fatal("NewSession did not attach the core blob store")
	}
	if _, err := s.PutBytes(context.Background(), "/a.txt", []byte("bytes")); err != nil {
		t.Fatal(err)
	}
	n, err := v.Stat("/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := in.Blobs.Open(context.Background(), n.Blob.ID); err != nil {
		t.Errorf("the bytes are not in the core blob store: %v", err)
	}
}
