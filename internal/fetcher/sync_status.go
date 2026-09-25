package fetcher

import (
	"strconv"
	"sync"
	"time"
)

// SyncKind identifies the scope of a background sync operation.
type SyncKind string

const (
	SyncKindRoles       SyncKind = "roles"
	SyncKindCollections SyncKind = "collections"
	SyncKindFull        SyncKind = "full"
)

const syncHistoryLimit = 5

// SyncItemResult records a single failed item within a sync job.
type SyncItemResult struct {
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}

// SyncJob is a point-in-time snapshot of one background sync operation.
type SyncJob struct {
	ID         string           `json:"id"`
	Kind       SyncKind         `json:"kind"`
	Status     string           `json:"status"`
	StartedAt  time.Time        `json:"started_at,omitempty"`
	FinishedAt time.Time        `json:"finished_at,omitempty"`
	Total      int              `json:"total"`
	Done       int              `json:"done"`
	Failed     int              `json:"failed"`
	Failures   []SyncItemResult `json:"failures,omitempty"`
}

// SyncSnapshot is a serializable view handed to API consumers.
type SyncSnapshot struct {
	Current *SyncJob  `json:"current"`
	History []SyncJob `json:"history"`
}

// SyncTracker is a concurrency-safe registry of running and recently finished
// sync jobs. It is owned by the Fetcher and reported on by the sync functions.
type SyncTracker struct {
	mu      sync.Mutex
	current *SyncJob
	history []SyncJob
	seq     int
}

func NewSyncTracker() *SyncTracker {
	return &SyncTracker{history: []SyncJob{}}
}

// begin starts tracking a new sync job, promoting the (already finished)
// previous current job into the bounded history buffer.
func (t *SyncTracker) begin(kind SyncKind, total int) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.current != nil && t.current.Status == "running" {
		t.current.FinishedAt = time.Now()
		t.current.Status = t.finalStatus()
		t.pushHistory(*t.current)
	}

	t.seq++
	t.current = &SyncJob{
		ID:        strconv.Itoa(t.seq),
		Kind:      kind,
		Status:    "running",
		StartedAt: time.Now(),
		Total:     total,
	}
}

// finalStatus derives the terminal state of the current job from its failure
// count so a partially or fully failed sync is reported as "failed" instead of
// masquerading as a clean "completed" run.
func (t *SyncTracker) finalStatus() string {
	if t.current != nil && t.current.Failed > 0 {
		return "failed"
	}
	return "completed"
}

// itemDone marks one item of the current job as processed.
func (t *SyncTracker) itemDone() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current != nil {
		t.current.Done++
	}
}

// itemFailed records a failed item in the current job.
func (t *SyncTracker) itemFailed(name string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current == nil {
		return
	}
	t.current.Failed++
	if err != nil {
		t.current.Failures = append(t.current.Failures, SyncItemResult{Name: name, Error: err.Error()})
	}
}

// end finalizes the current running job.
func (t *SyncTracker) end() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current == nil {
		return
	}
	t.current.FinishedAt = time.Now()
	t.current.Status = t.finalStatus()
	t.pushHistory(*t.current)
	t.current = nil
}

// snapshot returns a deep copy safe for JSON serialization.
func (t *SyncTracker) snapshot() SyncSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	snap := SyncSnapshot{
		History: make([]SyncJob, len(t.history)),
	}
	copy(snap.History, t.history)

	if t.current != nil {
		cur := *t.current
		cur.Failures = append([]SyncItemResult(nil), t.current.Failures...)
		snap.Current = &cur
	}
	return snap
}

// pushHistory inserts a finished job at the head of the bounded history buffer.
func (t *SyncTracker) pushHistory(job SyncJob) {
	t.history = append([]SyncJob{job}, t.history...)
	if len(t.history) > syncHistoryLimit {
		t.history = t.history[:syncHistoryLimit]
	}
}
