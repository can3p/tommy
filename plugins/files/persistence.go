package files

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/can3p/tommy/core/blob"
	"github.com/can3p/tommy/core/state"
)

// treeStateKey is the one snapshot the tree is saved under. The whole tree is
// rewritten on every change: it is bounded by Limits, and a single snapshot
// that is replaced atomically cannot be left half-updated.
const treeStateKey = "tree.json"

// treeVersion is bumped on any incompatible change to the snapshot. An unknown
// version stops startup rather than discarding someone's files.
const treeVersion = 1

type treeSnapshot struct {
	Version int          `json:"version"`
	Root    savedEntry   `json:"root"`
	Entries []savedEntry `json:"entries"`
}

// savedEntry is one directory or file. Entries are written parents before
// children, in the order Walk uses, so a restore can insert them in turn.
type savedEntry struct {
	Path        string    `json:"path"`
	Dir         bool      `json:"dir,omitempty"`
	ModTime     time.Time `json:"mod_time"`
	Provider    string    `json:"provider,omitempty"`
	ContentType string    `json:"content_type,omitempty"`
	Size        int64     `json:"size,omitempty"`
	Blob        blob.Ref  `json:"blob,omitzero"`
}

// BindState restores the tree from storage and saves every later change there.
// A nil storage is a memory scope: the tree stays as it is and nothing is
// saved. It must be called before the VFS is used, with the blob store the
// snapshot's bytes live in already attached.
//
// Once restored, blobs in the store that the tree does not reference are
// deleted: bytes written by a process that died before saving the snapshot
// naming them, or kept by one that died before deleting what it replaced.
func (v *VFS) BindState(ctx context.Context, storage state.Store) error {
	if storage == nil {
		return nil
	}
	data, err := storage.Load(ctx, treeStateKey)
	switch {
	case errors.Is(err, state.ErrNotFound):
	case err != nil:
		return fmt.Errorf("files: load tree snapshot: %w", err)
	default:
		var snapshot treeSnapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return fmt.Errorf("files: decode tree snapshot: %w", err)
		}
		if err := v.restore(ctx, snapshot); err != nil {
			return fmt.Errorf("files: restore tree snapshot: %w", err)
		}
	}
	if _, err := blob.Sweep(ctx, v.Blobs(), v.liveBlobs()); err != nil {
		return fmt.Errorf("files: %w", err)
	}
	v.state = storage
	return nil
}

// liveBlobs is every blob the tree references.
func (v *VFS) liveBlobs() map[string]struct{} {
	live := map[string]struct{}{}
	_ = v.Walk(func(n Node) error {
		if !n.Dir {
			live[n.Blob.ID] = struct{}{}
		}
		return nil
	})
	return live
}

// restore builds a whole new tree from the snapshot and swaps it in only once
// every entry has been checked, so a bad snapshot leaves the VFS untouched.
// Every path goes through Resolve, the one place paths are interpreted, and
// must already be in the form Resolve produces.
func (v *VFS) restore(ctx context.Context, snapshot treeSnapshot) error {
	if snapshot.Version != treeVersion {
		return fmt.Errorf("unsupported tree snapshot version %d", snapshot.Version)
	}
	if snapshot.Root.Path != "/" || !snapshot.Root.Dir {
		return errors.New("snapshot root is not the directory \"/\"")
	}
	root := &dirNode{
		modTime:  snapshot.Root.ModTime,
		provider: snapshot.Root.Provider,
		dirs:     map[string]*dirNode{},
		files:    map[string]*fileNode{},
	}
	dirs := map[string]*dirNode{"/": root}
	nodes := 0
	for _, e := range snapshot.Entries {
		clean, err := v.Resolve(e.Path)
		if err != nil {
			return err
		}
		if clean != e.Path || clean == "/" {
			return fmt.Errorf("entry path %q is not a canonical path", e.Path)
		}
		dirPath, base := split(clean)
		parent, ok := dirs[dirPath]
		if !ok {
			return fmt.Errorf("entry %q has no parent directory in the snapshot", clean)
		}
		if _, taken := parent.dirs[base]; taken {
			return fmt.Errorf("duplicate entry %q", clean)
		}
		if _, taken := parent.files[base]; taken {
			return fmt.Errorf("duplicate entry %q", clean)
		}
		if parent.len() >= v.limits.MaxDirEntries {
			return fmt.Errorf("%s: %w", dirPath, ErrDirFull)
		}
		if nodes >= v.limits.MaxNodes {
			return ErrTreeFull
		}
		if e.Dir {
			if e.Blob.ID != "" || e.Size != 0 {
				return fmt.Errorf("directory %q carries file content", clean)
			}
			d := &dirNode{
				name:     base,
				parent:   parent,
				modTime:  e.ModTime,
				provider: e.Provider,
				dirs:     map[string]*dirNode{},
				files:    map[string]*fileNode{},
			}
			parent.dirs[base] = d
			dirs[clean] = d
		} else {
			if err := v.validateBlob(ctx, e.Blob, e.Size); err != nil {
				return fmt.Errorf("file %q: %w", clean, err)
			}
			parent.files[base] = &fileNode{
				name:        base,
				modTime:     e.ModTime,
				provider:    e.Provider,
				contentType: e.ContentType,
				ref:         e.Blob,
			}
		}
		nodes++
	}

	v.mu.Lock()
	v.root = root
	v.nodes = nodes
	v.mu.Unlock()
	return nil
}

func (v *VFS) validateBlob(ctx context.Context, ref blob.Ref, size int64) error {
	if ref.ID == "" || ref.Size != size {
		return errors.New("invalid blob reference")
	}
	if size < 0 || size > v.limits.MaxFileSize {
		return ErrFileTooLarge
	}
	stored, err := v.Blobs().Stat(ctx, ref.ID)
	if err != nil {
		return fmt.Errorf("blob %s: %w", ref.ID, err)
	}
	if stored.Size != size {
		return fmt.Errorf("blob size %d does not match file size %d", stored.Size, size)
	}
	return nil
}

// changedLocked records a committed mutation. Callers hold v.mu for writing.
func (v *VFS) changedLocked() { v.revision++ }

// persist saves the tree if it has changed since the last save. Callers have
// released v.mu, so no disk I/O ever happens under the tree lock.
//
// Saves are serialized, and each writes the newest revision rather than the
// caller's own, so a slow save can never overwrite a newer snapshot, and
// callers that queue behind one save find their change already written. The
// save ignores the caller's cancellation: a client that disconnects mid-way
// must not leave the tree in memory and the tree on disk disagreeing.
//
// A failed save is returned to the caller: the change is visible in this
// process but not durable. The next successful save includes it.
func (v *VFS) persist(ctx context.Context) error {
	if v.state == nil {
		return nil
	}
	v.persistMu.Lock()
	defer v.persistMu.Unlock()
	v.mu.RLock()
	if v.revision <= v.persisted {
		v.mu.RUnlock()
		return nil
	}
	revision := v.revision
	snapshot := v.snapshotLocked()
	v.mu.RUnlock()
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("files: encode tree snapshot: %w", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := v.state.Save(context.WithoutCancel(ctx), treeStateKey, data); err != nil {
		return fmt.Errorf("files: save tree snapshot: %w", err)
	}
	v.persisted = revision
	return nil
}

// snapshotLocked captures the tree. Callers hold v.mu for reading. The walk
// order is sorted, so the same tree always produces the same bytes.
func (v *VFS) snapshotLocked() treeSnapshot {
	snapshot := treeSnapshot{
		Version: treeVersion,
		Root:    savedEntry{Path: "/", Dir: true, ModTime: v.root.modTime, Provider: v.root.provider},
		Entries: []savedEntry{},
	}
	_ = v.walk(v.root, func(n Node) error {
		snapshot.Entries = append(snapshot.Entries, savedEntry{
			Path:        n.Path,
			Dir:         n.Dir,
			ModTime:     n.ModTime,
			Provider:    n.Provider,
			ContentType: n.ContentType,
			Size:        n.Size,
			Blob:        n.Blob,
		})
		return nil
	})
	return snapshot
}
