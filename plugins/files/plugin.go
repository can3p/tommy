// Package files is tommy's file-transfer content type: one virtual filesystem
// every provider shares, a read-back API, and a file-browser tab. The tree is
// held in memory and, when storage is configured as persistent, also saved as
// a snapshot so that it and its bytes survive a restart.
//
// It is the first plugin that is stateful. Mail and SMS are pure event
// streams - something arrives, you look at it - but people create directories
// over FTP, overwrite files, list the current tree, and download something
// they uploaded an hour ago. So this plugin keeps two things with deliberately
// independent lifetimes:
//
//   - the VFS is the state. It holds the tree as it is right now, and each
//     file's bytes live in the core blob store behind a blob.Ref;
//   - the event log is the history. Every mutation also appends an event
//     (files.upload, files.mkdir, files.delete, files.rename) tagged with the
//     provider that did it, so the SSE stream, /api/v1/events and the "what
//     just happened" view stay uniform across plugins.
//
// The consequence that matters: a file stays listed and downloadable long
// after the event announcing it has been evicted from the ring buffer - and,
// with persistent storage, after a restart that forgot every event.
//
// The plugin is named for what it holds rather than for one protocol. SFTP is
// an SSH subsystem, not FTP-with-TLS, and FTPS is a third thing again, so
// "ftp" would have been the wrong name for the tab the moment the second
// provider landed. Providers live in plugins/files/providers/... and never
// talk to each other; all they share is the VFS.
package files

import (
	"context"
	"embed"
	"io/fs"

	"github.com/can3p/tommy/core/plugin"
	coreapi "github.com/can3p/tommy/core/server/api"
	coreui "github.com/can3p/tommy/core/server/ui"
)

// PluginName is the URL segment the plugin is mounted under.
const PluginName = "files"

// APIPrefix is where the core mounts this plugin's API. Routes are registered
// with the prefix already stripped; it is spelled out here so the API and the
// tab can hand out links a browser can follow.
const APIPrefix = coreapi.Prefix + "/" + PluginName

// UIPrefix is where the core mounts this plugin's tab, likewise stripped from
// the patterns RegisterUI uses.
const UIPrefix = coreui.Prefix + "/" + PluginName

//go:embed ui/*.html
var uiFS embed.FS

// VFSBinder is implemented by a provider that needs the shared tree. New calls
// BindVFS on every provider that implements it, which is how ftp and sftp are
// handed the same filesystem without the plugin having to import either of
// them or the wiring having to construct the VFS itself.
type VFSBinder interface {
	BindVFS(v *VFS)
}

// Plugin is the files content type.
type Plugin struct {
	providers []plugin.Provider
	vfs       *VFS
}

// New returns the files plugin serving the given providers, all of them
// sharing one freshly created VFS. Passing none is valid and yields a plugin
// that still serves whatever is written into its VFS directly, which is how
// the tests drive it.
func New(providers ...plugin.Provider) *Plugin {
	return NewWithVFS(NewVFS(), providers...)
}

// NewWithVFS is New over a filesystem the caller built, for a test that wants
// tighter Limits or an injected clock.
func NewWithVFS(v *VFS, providers ...plugin.Provider) *Plugin {
	if v == nil {
		v = NewVFS()
	}
	p := &Plugin{providers: providers, vfs: v}
	for _, prov := range providers {
		if b, ok := prov.(VFSBinder); ok {
			b.BindVFS(v)
		}
	}
	return p
}

var _ plugin.StorageBinder = (*Plugin)(nil)

// BindStorage gives the shared tree its storage and restores it. It runs
// before any provider serves or any surface mounts, so the tree's bytes go to
// the plugin's own scope rather than to whichever deps first reach Attach.
func (p *Plugin) BindStorage(ctx context.Context, st plugin.Storage) error {
	if st.State == nil {
		// A memory scope: nothing to restore and nothing to save.
		p.vfs.Attach(st.Blobs)
		return nil
	}
	if err := p.vfs.bindBlobs(st.Blobs); err != nil {
		return err
	}
	return p.vfs.BindState(ctx, st.State)
}

// VFS returns the shared filesystem. Providers normally receive it through
// BindVFS instead; this is for the wiring and the tests.
func (p *Plugin) VFS() *VFS { return p.vfs }

// Name implements plugin.Plugin.
func (p *Plugin) Name() string { return PluginName }

// Title implements plugin.Plugin.
func (p *Plugin) Title() string { return "Files" }

// Description implements plugin.Plugin.
func (p *Plugin) Description() string {
	return "Accepts the files your application uploads over FTP, SFTP or TFTP instead of shipping them anywhere, and keeps them in a virtual filesystem you can browse, download from and assert against. " +
		"Every upload, mkdir, delete and rename is also recorded as an event, so the tree shows what is there now and the log shows how it got that way."
}

// Providers implements plugin.Plugin.
func (p *Plugin) Providers() []plugin.Provider {
	if p.providers == nil {
		return []plugin.Provider{}
	}
	return p.providers
}

// Templates returns the tab's templates, which compose the shared component
// library rather than re-implementing page chrome.
func (p *Plugin) Templates() fs.FS {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		// Impossible: the directory is embedded above.
		panic("files: embedded ui templates: " + err.Error())
	}
	return sub
}

// session returns a Session for the plugin's own surfaces, which act as the
// plugin rather than as any provider: a delete from the tab is recorded with
// no provider name, so nothing looks as though FTP did it.
func (p *Plugin) session(d plugin.Deps) *Session {
	return NewSession(p.vfs, d, WithTransport("http"))
}
