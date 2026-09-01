package files

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/event"
	"github.com/can3p/tommy/core/plugin"
	"github.com/can3p/tommy/core/server/ui/components"
)

// Event types this plugin emits. The VFS is the state and the event log is the
// history, with independent lifetimes: a file stays listed and downloadable
// long after the event that announced it has been evicted from the ring
// buffer, which is exactly why the bytes live in the blob store.
//
// Type is free-form by contract, so a provider is free to add its own on top
// of these - a login, a failed transfer - as long as it keeps the "files."
// prefix, which is what the tab's live refresh subscribes to.
const (
	EventUpload = "files.upload"
	EventMkdir  = "files.mkdir"
	EventDelete = "files.delete"
	EventRename = "files.rename"
)

// EventTypes is every type Session emits, in the order the UI lists them.
var EventTypes = []string{EventUpload, EventMkdir, EventDelete, EventRename}

// Op is the payload of a files event: what happened, to what, by whom.
type Op struct {
	// Op is "upload", "mkdir", "delete" or "rename" - EventUpload and friends
	// without the plugin prefix.
	Op string `json:"op"`
	// Path is the entry the operation landed on, cleaned and rooted.
	Path string `json:"path"`
	// From is a rename's source path, empty otherwise.
	From string `json:"from,omitempty"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size,omitempty"`
	// Entries is how many entries a recursive delete took with it.
	Entries int       `json:"entries,omitempty"`
	ModTime time.Time `json:"mod_time,omitzero"`
	// Blob is where the bytes are. It survives the event: even once this event
	// is evicted the file is still downloadable through the VFS.
	Blob *blob.Ref `json:"blob,omitempty"`
	// User is whatever credential the provider accepted, recorded rather than
	// checked - provider rule 1.
	User string `json:"user,omitempty"`
	// Peer is the client address.
	Peer string `json:"peer,omitempty"`
}

// Title is the one-line description the event list shows.
func (o *Op) Title() string {
	switch o.Op {
	case "rename":
		return o.From + " → " + o.Path
	default:
		return o.Path
	}
}

// Snippet says what happened in words, for the generic event list and search.
func (o *Op) Snippet() string {
	switch o.Op {
	case "upload":
		return fmt.Sprintf("uploaded %s (%s)", o.Path, components.BytesHuman(o.Size))
	case "mkdir":
		return "created directory " + o.Path
	case "delete":
		if o.Dir {
			return fmt.Sprintf("deleted directory %s (%d entries)", o.Path, o.Entries)
		}
		return "deleted " + o.Path
	case "rename":
		return "renamed " + o.From + " to " + o.Path
	default:
		return o.Op + " " + o.Path
	}
}

// OpOf extracts the operation from an event. It accepts the in-process payload
// a provider appended (*Op), a value copy, and a payload that has been through
// JSON, so a store that round-trips events later does not break every read
// surface.
func OpOf(e *event.Event) (*Op, bool) {
	if e == nil || e.Payload == nil {
		return nil, false
	}
	switch p := e.Payload.(type) {
	case *Op:
		if p == nil {
			return nil, false
		}
		clone := *p
		return &clone, true
	case Op:
		clone := p
		return &clone, true
	default:
		encoded, err := json.Marshal(e.Payload)
		if err != nil {
			return nil, false
		}
		var decoded Op
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return nil, false
		}
		if decoded.Op == "" || decoded.Path == "" {
			return nil, false
		}
		return &decoded, true
	}
}

// IsFilesEvent reports whether an event is one of this plugin's.
func IsFilesEvent(e *event.Event) bool {
	if e == nil {
		return false
	}
	for _, t := range EventTypes {
		if e.Type == t {
			return true
		}
	}
	return false
}

// EventOption adjusts the event a Session is about to append: the real
// protocol command in Raw.Body, extra Meta, a different peer. A provider uses
// it to keep the raw view honest.
type EventOption func(*event.Event)

// WithCommand records the protocol command that caused the operation - "STOR
// /upload/local.txt", "SSH_FXP_MKDIR /a" - as the event's raw body.
func WithCommand(cmd string) EventOption {
	return func(e *event.Event) {
		e.Raw.Body = []byte(cmd)
		e.Raw.Text = true
	}
}

// WithEventMeta adds one provider-specific key to the event's metadata.
func WithEventMeta(key string, value any) EventOption {
	return func(e *event.Event) {
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta[key] = value
	}
}

// WithEventRaw replaces the whole raw record, for a provider that can show the
// bytes its client actually sent.
func WithEventRaw(raw event.Raw) EventOption {
	return func(e *event.Event) { e.Raw = raw }
}

// Session is the surface a provider works through. It is a VFS bound to one
// provider's identity and to the store, so every mutation both changes the
// tree and appends the matching event without the provider having to remember
// to do the second half.
//
// Reads go straight through and record nothing: the event log is a record of
// change, and a directory listing is not one.
type Session struct {
	vfs       *VFS
	deps      plugin.Deps
	provider  string
	transport string
	peer      string
	user      string
	meta      map[string]any
}

// SessionOption configures a Session.
type SessionOption func(*Session)

// WithProvider names the provider doing the writing. It is stamped on both the
// event and the VFS node, which is what lets the tab show the protocol that
// wrote each file.
func WithProvider(name string) SessionOption {
	return func(s *Session) { s.provider = name }
}

// WithTransport sets event.Raw.Transport: "ftp" for FTP, "ssh" for SFTP.
func WithTransport(t string) SessionOption {
	return func(s *Session) { s.transport = t }
}

// WithPeer records the client address on every event.
func WithPeer(addr string) SessionOption {
	return func(s *Session) { s.peer = addr }
}

// WithUser records the credential the provider accepted. Auth is recorded,
// never rejected, unless the config pins one - provider rule 1.
func WithUser(user string) SessionOption {
	return func(s *Session) { s.user = user }
}

// WithSessionMeta seeds the metadata every event from this session carries.
func WithSessionMeta(meta map[string]any) SessionOption {
	return func(s *Session) { s.meta = meta }
}

// NewSession binds a VFS to one provider's identity. A listener provider
// typically makes one per connection, so the peer address and the accepted
// user land on every event that connection produces.
func NewSession(v *VFS, d plugin.Deps, opts ...SessionOption) *Session {
	s := &Session{vfs: v, deps: d.Normalize(), transport: "vfs"}
	for _, o := range opts {
		o(s)
	}
	if v != nil {
		// Whichever surface runs first wins; they are all handed the same
		// store, so this is idempotent in a real tommy.
		v.Attach(d.Blobs)
	}
	return s
}

// VFS returns the tree behind the session, for the reads that record nothing.
func (s *Session) VFS() *VFS { return s.vfs }

// Provider is the provider name the session writes under.
func (s *Session) Provider() string { return s.provider }

// Resolve, Stat, List, Exists and IsDir are the read surface, forwarded
// unchanged: they resolve paths through the same single gate every write does.
func (s *Session) Resolve(name string) (string, error) { return s.vfs.Resolve(name) }

// Stat returns one entry.
func (s *Session) Stat(name string) (Node, error) { return s.vfs.Stat(name) }

// List returns a directory's children.
func (s *Session) List(name string) ([]Node, error) { return s.vfs.List(name) }

// Exists reports whether anything is at name.
func (s *Session) Exists(name string) bool { return s.vfs.Exists(name) }

// IsDir reports whether name is an existing directory.
func (s *Session) IsDir(name string) bool { return s.vfs.IsDir(name) }

// Chtimes sets an entry's modification time - FTP's MFMT, SFTP's setstat.
// Nothing is recorded: a timestamp fixup is metadata, not a change to what the
// filesystem holds, and an event per setstat would drown the log.
func (s *Session) Chtimes(name string, mtime time.Time) error {
	return s.vfs.Chtimes(name, mtime)
}

// Truncate resizes a file - SFTP's setstat with a size. Like Chtimes it records
// nothing; a provider that wants it in the log appends its own event.
func (s *Session) Truncate(ctx context.Context, name string, size int64) error {
	return s.vfs.Truncate(ctx, name, size)
}

// Open opens a file for reading. Downloads are not mutations, so nothing is
// recorded; a provider that wants a download in the log appends its own event.
func (s *Session) Open(ctx context.Context, name string) (*File, error) {
	return s.vfs.Open(ctx, name)
}

func (s *Session) writeOptions() WriteOptions {
	return WriteOptions{Provider: s.provider}
}

// Put stores a file and records files.upload. The parent directory must exist,
// which is what a real FTP server answers to STOR into nowhere; call MkdirAll
// first for --ftp-create-dirs behavior.
func (s *Session) Put(ctx context.Context, name string, r io.Reader, opts ...EventOption) (Node, error) {
	n, err := s.vfs.Put(ctx, name, r, s.writeOptions())
	if err != nil {
		return Node{}, err
	}
	return n, s.recordNode(ctx, "upload", EventUpload, n, "", 0, opts)
}

// PutBytes is Put for content already in memory.
func (s *Session) PutBytes(ctx context.Context, name string, data []byte, opts ...EventOption) (Node, error) {
	n, err := s.vfs.PutBytes(ctx, name, data, s.writeOptions())
	if err != nil {
		return Node{}, err
	}
	return n, s.recordNode(ctx, "upload", EventUpload, n, "", 0, opts)
}

// Create opens a file for writing. The files.upload event is appended when the
// handle is closed and the content is visible in the tree, so an abandoned
// transfer leaves neither a file nor an event behind.
func (s *Session) Create(ctx context.Context, name string, opts ...EventOption) (*File, error) {
	return s.OpenFile(ctx, name, OpenWrite|OpenCreate|OpenTruncate, opts...)
}

// OpenFile is the general open, recording files.upload on commit for a handle
// opened for writing.
func (s *Session) OpenFile(ctx context.Context, name string, flag int, opts ...EventOption) (*File, error) {
	f, err := s.vfs.OpenFile(ctx, name, flag, s.writeOptions())
	if err != nil {
		return nil, err
	}
	if flag&OpenWrite != 0 || flag&OpenReadWrite != 0 {
		// The event is appended with the context the handle is closed with,
		// not the one it was opened with, so a provider whose per-request
		// context dies at end-of-transfer can still commit through
		// File.CloseContext and have the event land.
		f.OnCommit(func(closeCtx context.Context, n Node) error {
			return s.recordNode(closeCtx, "upload", EventUpload, n, "", 0, opts)
		})
	}
	return f, nil
}

// Mkdir creates one directory and records files.mkdir.
func (s *Session) Mkdir(ctx context.Context, name string, opts ...EventOption) (Node, error) {
	n, err := s.vfs.Mkdir(name, s.writeOptions())
	if err != nil {
		return Node{}, err
	}
	return n, s.recordNode(ctx, "mkdir", EventMkdir, n, "", 0, opts)
}

// MkdirAll creates a directory and every missing parent. One event is recorded,
// naming the directory that was asked for.
func (s *Session) MkdirAll(ctx context.Context, name string, opts ...EventOption) (Node, error) {
	existed := s.vfs.IsDir(name)
	n, err := s.vfs.MkdirAll(name, s.writeOptions())
	if err != nil {
		return Node{}, err
	}
	if existed {
		// Nothing changed, so there is no history to record.
		return n, nil
	}
	return n, s.recordNode(ctx, "mkdir", EventMkdir, n, "", 0, opts)
}

// Remove deletes a file or an empty directory and records files.delete.
func (s *Session) Remove(ctx context.Context, name string, opts ...EventOption) (Node, error) {
	n, count, err := s.vfs.remove(ctx, name, false)
	if err != nil {
		return Node{}, err
	}
	return n, s.recordNode(ctx, "delete", EventDelete, n, "", count, opts)
}

// RemoveAll deletes a file, or a directory and everything below it, and
// records one files.delete carrying the number of entries that went with it.
func (s *Session) RemoveAll(ctx context.Context, name string, opts ...EventOption) (Node, int, error) {
	n, count, err := s.vfs.RemoveAll(ctx, name)
	if err != nil {
		return Node{}, 0, err
	}
	return n, count, s.recordNode(ctx, "delete", EventDelete, n, "", count, opts)
}

// Rename moves an entry and records files.rename.
func (s *Session) Rename(ctx context.Context, oldName, newName string, opts ...EventOption) (Node, error) {
	from, err := s.vfs.Resolve(oldName)
	if err != nil {
		return Node{}, err
	}
	n, err := s.vfs.Rename(ctx, oldName, newName)
	if err != nil {
		return Node{}, err
	}
	return n, s.recordNode(ctx, "rename", EventRename, n, from, 0, opts)
}

// Clear empties the tree and records one files.delete for the root.
func (s *Session) Clear(ctx context.Context, opts ...EventOption) (int, error) {
	count, err := s.vfs.Clear(ctx)
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}
	root := s.vfs.Root()
	return count, s.recordNode(ctx, "delete", EventDelete, root, "", count, opts)
}

// recordNode builds and appends the event for one completed mutation.
func (s *Session) recordNode(ctx context.Context, op, typ string, n Node, from string, entries int, opts []EventOption) error {
	payload := &Op{
		Op:      op,
		Path:    n.Path,
		From:    from,
		Dir:     n.Dir,
		Size:    n.Size,
		Entries: entries,
		ModTime: n.ModTime,
		User:    s.user,
		Peer:    s.peer,
	}
	if !n.Dir && n.Blob.ID != "" && op != "delete" {
		ref := n.Blob
		payload.Blob = &ref
	}

	e := &event.Event{
		Plugin:   PluginName,
		Provider: s.provider,
		Type:     typ,
		Summary: event.Summary{
			From:    s.actor(),
			To:      []string{n.Path},
			Title:   payload.Title(),
			Snippet: payload.Snippet(),
		},
		Payload: payload,
		Raw: event.Raw{
			Transport: s.transport,
			PeerAddr:  s.peer,
			Body:      []byte(payload.Snippet()),
			Text:      true,
		},
	}
	if len(s.meta) > 0 {
		e.Meta = make(map[string]any, len(s.meta))
		for k, v := range s.meta {
			e.Meta[k] = v
		}
	}
	if s.user != "" {
		WithEventMeta("user", s.user)(e)
	}
	for _, o := range opts {
		o(e)
	}
	return s.deps.Append(ctx, e)
}

// actor is who the listing shows as the sender: the accepted user when there
// was one, otherwise the protocol.
func (s *Session) actor() string {
	if s.user != "" {
		return s.user
	}
	return s.provider
}
