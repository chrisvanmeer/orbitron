// Package access records the last time each cached role version and collection
// version was actually served to a client (a download). These access
// timestamps replace the old requirements-manifest bookkeeping: a mirror only
// stores what has not yet been pulled onto disk, so there is no longer any
// need to keep manifests around to decide what is "active".
//
// The recorder persists a small, atomic JSON index per storage path so that
// both the dashboard (LAST ACCESS column) and the pruner (--days selection)
// share the exact same source of truth without external state.
package access

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"orbitron/internal/logger"
)

// Key uniquely identifies a cached version. Roles use their directory name as
// the identity; collections use the full "<namespace>.<name>" name.
type Key string

func RoleKey(name, version string) Key {
	return Key("role:" + name + "@" + version)
}

func CollectionKey(name, version string) Key {
	return Key("collection:" + name + "@" + version)
}

// Entry is a single recorded access timestamp.
type Entry struct {
	Key        Key       `json:"key"`
	LastAccess time.Time `json:"last_access"`
}

// Recorder keeps an in-memory view of last-access timestamps and persists it
// atomically after every update. All methods are safe for concurrent use.
type Recorder struct {
	mu       sync.Mutex
	path     string
	accessed map[Key]time.Time
}

// New returns a Recorder that persists its state to <storage>/.access.json.
// Any existing index is loaded; a missing or corrupt file starts empty. The
// storage directory is created if it does not exist yet.
func New(storagePath string) *Recorder {
	r := &Recorder{
		path:     filepath.Join(storagePath, ".access.json"),
		accessed: make(map[Key]time.Time),
	}
	if err := r.load(); err != nil && !os.IsNotExist(err) {
		logger.Warn("Access recorder: failed to read %s: %v", r.path, err)
	}
	return r
}

func (r *Recorder) load() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	data, err := os.ReadFile(r.path)
	if err != nil {
		return err
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}
	now := time.Now()
	for _, e := range entries {
		if e.LastAccess.After(now) || e.LastAccess.IsZero() {
			continue
		}
		r.accessed[e.Key] = e.LastAccess
	}
	return nil
}

func (r *Recorder) saveLocked() {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		logger.Error("Access recorder: cannot create storage dir %s: %v", filepath.Dir(r.path), err)
		return
	}

	data, err := json.Marshal(r.snapshotLocked())
	if err != nil {
		logger.Error("Access recorder: marshal failed: %v", err)
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		logger.Error("Access recorder: cannot write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		logger.Error("Access recorder: cannot rename %s -> %s: %v", tmp, r.path, err)
		return
	}
}

func (r *Recorder) snapshotLocked() []Entry {
	keys := make([]Key, 0, len(r.accessed))
	for k := range r.accessed {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	entries := make([]Entry, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, Entry{Key: k, LastAccess: r.accessed[k]})
	}
	return entries
}

// Touch records that the given version was served now. It is called from the
// download handlers whenever a client actually pulls a role or collection
// version from the local cache.
func (r *Recorder) Touch(k Key) {
	r.mu.Lock()
	r.accessed[k] = time.Now()
	r.saveLocked()
	r.mu.Unlock()
}

// LastAccessed returns the recorded access time for a version, if any.
func (r *Recorder) LastAccessed(k Key) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.accessed[k]
	return t, ok
}

// ShouldKeep reports whether the version should be retained given an
// access-based retention policy: it is kept whenever it was accessed at least
// within the last maxAge, or never touched (brand-new artifacts that have not
// yet been requested are always kept to avoid immediately pruning fresh
// mirrors).
func (r *Recorder) ShouldKeep(k Key, maxAge time.Duration) bool {
	t, ok := r.LastAccessed(k)
	if !ok {
		return true
	}
	return time.Since(t) < maxAge
}

// Remove deletes a version from the index (used when the pruner deletes the
// on-disk copy, keeping the index free of stale rows).
func (r *Recorder) Remove(k Key) {
	r.mu.Lock()
	if _, ok := r.accessed[k]; ok {
		delete(r.accessed, k)
		r.saveLocked()
	}
	r.mu.Unlock()
}

// Snapshot returns a copy of the entire index for callers that need to scan
// it (e.g. the dashboard matrix).
func (r *Recorder) Snapshot() map[Key]time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLockedMap()
}

func (r *Recorder) snapshotLockedMap() map[Key]time.Time {
	out := make(map[Key]time.Time, len(r.accessed))
	for k, t := range r.accessed {
		out[k] = t
	}
	return out
}
