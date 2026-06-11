package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.PutEntry(Entry{Path: "/sd/gcodes/a.nc", Size: 10, Sync: PendingUpload})
	j, _ := s.Enqueue(Job{Kind: JobUpload, Path: "/sd/gcodes/a.nc", Size: 10})
	if j.ID != 1 {
		t.Errorf("first job id = %d, want 1", j.ID)
	}

	// Reopen and verify state survived.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := s2.GetEntry("/sd/gcodes/a.nc")
	if !ok || e.Size != 10 || e.Sync != PendingUpload {
		t.Errorf("reloaded entry = %+v ok=%v", e, ok)
	}
	jobs := s2.ListJobs()
	if len(jobs) != 1 || jobs[0].Kind != JobUpload {
		t.Errorf("reloaded jobs = %+v", jobs)
	}
	// Next enqueued ID continues from persisted counter.
	j2, _ := s2.Enqueue(Job{Kind: JobDelete, Path: "/sd/gcodes/b.nc"})
	if j2.ID != 2 {
		t.Errorf("second job id = %d, want 2", j2.ID)
	}
}

func TestNextQueuedOrder(t *testing.T) {
	s, _ := Open("")
	s.Enqueue(Job{Kind: JobMkdir, Path: "/sd/gcodes/d"})
	s.Enqueue(Job{Kind: JobUpload, Path: "/sd/gcodes/d/a.nc"})

	j, ok := s.NextQueued()
	if !ok || j.Kind != JobMkdir {
		t.Fatalf("first queued = %+v ok=%v, want mkdir", j, ok)
	}
	// Mark it done; next should be the upload.
	s.UpdateJob(j.ID, func(j *Job) { j.State = Done })
	j2, _ := s.NextQueued()
	if j2.Kind != JobUpload {
		t.Errorf("second queued = %+v, want upload", j2)
	}
}

func TestSupersedeQueuedUploads(t *testing.T) {
	s, _ := Open("")
	// Two queued uploads of the same path, plus a delete of another path.
	s.Enqueue(Job{Kind: JobUpload, Path: "/sd/gcodes/a.nc"})
	s.Enqueue(Job{Kind: JobUpload, Path: "/sd/gcodes/a.nc"})
	s.Enqueue(Job{Kind: JobDelete, Path: "/sd/gcodes/b.nc"})

	n, err := s.SupersedeQueuedUploads("/sd/gcodes/a.nc")
	if err != nil || n != 2 {
		t.Fatalf("superseded = %d err=%v, want 2", n, err)
	}
	// Both a.nc uploads are now Done; the b.nc delete is untouched.
	for _, j := range s.ListJobs() {
		if j.Path == "/sd/gcodes/a.nc" && j.State != Done {
			t.Errorf("a.nc upload state = %q, want done", j.State)
		}
		if j.Kind == JobDelete && j.State != Queued {
			t.Errorf("delete state = %q, want queued (untouched)", j.State)
		}
	}
	// A running upload must NOT be superseded.
	j, _ := s.Enqueue(Job{Kind: JobUpload, Path: "/sd/gcodes/c.nc"})
	s.UpdateJob(j.ID, func(j *Job) { j.State = Running })
	n, _ = s.SupersedeQueuedUploads("/sd/gcodes/c.nc")
	if n != 0 {
		t.Errorf("superseded a running upload (%d), should not", n)
	}
}

func TestSubscribeReceivesEvents(t *testing.T) {
	s, _ := Open("")
	ch, unsub := s.Subscribe()
	defer unsub()

	s.PutEntry(Entry{Path: "/sd/gcodes/x.nc", Sync: LocalOnly})
	select {
	case ev := <-ch:
		if ev.Kind != "entry" || ev.Entry == nil || ev.Entry.Path != "/sd/gcodes/x.nc" {
			t.Errorf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event received")
	}
}

func TestPruneDoneJobs(t *testing.T) {
	now := time.Unix(10000, 0)
	s, _ := Open("")
	s.now = func() time.Time { return now }

	j, _ := s.Enqueue(Job{Kind: JobUpload, Path: "/a"})
	s.UpdateJob(j.ID, func(j *Job) { j.State = Done })
	f, _ := s.Enqueue(Job{Kind: JobUpload, Path: "/b"})
	s.UpdateJob(f.ID, func(j *Job) { j.State = Failed })

	now = now.Add(time.Hour)
	s.PruneDoneJobs(time.Minute)

	jobs := s.ListJobs()
	if len(jobs) != 1 || jobs[0].State != Failed {
		t.Errorf("after prune = %+v, want only the failed job", jobs)
	}
}
