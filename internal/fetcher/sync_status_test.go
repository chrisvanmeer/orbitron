package fetcher

import (
	"sync"
	"testing"
)

func TestSyncTrackerBeginEndProducesHistory(t *testing.T) {
	tr := NewSyncTracker()

	if snap := tr.snapshot(); snap.Current != nil {
		t.Fatal("expected no current job before begin")
	}

	tr.begin(SyncKindRoles, 3)
	snap := tr.snapshot()
	if snap.Current == nil {
		t.Fatal("expected a current job after begin")
	}
	if snap.Current.Kind != SyncKindRoles || snap.Current.Status != "running" {
		t.Errorf("unexpected current job: %+v", snap.Current)
	}
	if snap.Current.Total != 3 {
		t.Errorf("total = %d, want 3", snap.Current.Total)
	}

	tr.itemDone()
	tr.itemDone()
	tr.itemFailed("geerlingguy.nginx", errSentinel{})

	tr.end()

	snap = tr.snapshot()
	if snap.Current != nil {
		t.Error("expected no current job after end")
	}
	if len(snap.History) != 1 {
		t.Fatalf("history length = %d, want 1", len(snap.History))
	}
	job := snap.History[0]
	if job.Status != "completed" {
		t.Errorf("status = %q, want completed", job.Status)
	}
	if job.Done != 2 || job.Failed != 1 {
		t.Errorf("done=%d failed=%d, want done=2 failed=1", job.Done, job.Failed)
	}
	if job.FinishedAt.Before(job.StartedAt) {
		t.Error("finished_at should be after started_at")
	}
	if len(job.Failures) != 1 || job.Failures[0].Name != "geerlingguy.nginx" {
		t.Errorf("unexpected failures: %+v", job.Failures)
	}
}

func TestSyncTrackerBoundedHistory(t *testing.T) {
	tr := NewSyncTracker()
	for i := 0; i < 12; i++ {
		tr.begin(SyncKindFull, 0)
		tr.end()
	}
	if got := len(tr.snapshot().History); got > syncHistoryLimit {
		t.Errorf("history length %d exceeds limit %d", got, syncHistoryLimit)
	}
}

func TestSyncTrackerConcurrentAccess(t *testing.T) {
	tr := NewSyncTracker()
	tr.begin(SyncKindFull, 100)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 10; n++ {
				tr.itemDone()
				_ = tr.snapshot()
			}
		}()
	}
	wg.Wait()

	snap := tr.snapshot()
	if snap.Current == nil {
		t.Fatal("expected running job")
	}
	if snap.Current.Done != 100 {
		t.Errorf("done = %d, want 100 (no lost updates)", snap.Current.Done)
	}
	tr.end()
}

func TestProcessRolesReportsToTracker(t *testing.T) {
	f := NewFetcher(t.TempDir(), 4, ProxyConfig{})

	// Two galaxy-style roles. Both will fail to reach galaxy.ansible.com in
	// tests, which is exactly what we want: the tracker must count done items
	// and record the failures without eating the panic.
	items := []RoleItem{
		{Name: "acme.neverexists-a", Version: "1.0.0"},
		{Name: "acme.neverexists-b", Version: "1.0.0"},
	}
	f.tracker.begin(SyncKindRoles, len(items))
	f.runRoleJobs(items)
	f.tracker.end()

	snap := f.StatusSnapshot()
	if len(snap.History) != 1 {
		t.Fatalf("history length = %d, want 1", len(snap.History))
	}
	job := snap.History[0]
	if job.Done != 2 {
		t.Errorf("done = %d, want 2", job.Done)
	}
	if len(job.Failures) != 2 {
		t.Errorf("failures = %d, want 2 (network fetches should fail)", len(job.Failures))
	}
}

// errSentinel is a tiny sentinel error used to exercise tracker failure paths.
type errSentinel struct{}

func (errSentinel) Error() string { return "sentinel" }
