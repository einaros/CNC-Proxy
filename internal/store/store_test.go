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

func TestUISettingsPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ui, err := s.SetUISettings(UISettings{
		Macros: []Macro{{
			ID:    "probe",
			Name:  "Probe",
			Lines: []string{"", "G38.2 Z-5 F50", "G10 L20 P1 Z0"},
		}},
		MacroButtons: []MacroSlot{{ID: "slot1", MacroID: "probe", Region: "toolbar", Order: 4}},
		Log:          LogSettings{Filter: "jog", Autoscroll: false},
		Gamepad: Gamepad{
			Axes: GamepadAxes{
				X: GamepadAxis{Axis: 2, Scale: 0.5},
				Y: GamepadAxis{Axis: 1, Scale: 0.75},
				Z: GamepadAxis{Axis: 3, Invert: true, Scale: 0.25},
			},
			DeadmanButton: 7,
			SlowButtons:   []int{6},
			MacroButtons:  []GamepadMacroButton{{ID: "gp1", Button: 1, MacroID: "probe"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ui.Macros) != 1 || len(ui.Macros[0].Lines) != 2 {
		t.Fatalf("normalized ui = %+v", ui)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.UISettings()
	if len(got.Macros) != 1 || got.Macros[0].ID != "probe" || got.Macros[0].Lines[0] != "G38.2 Z-5 F50" {
		t.Fatalf("reopened macros = %+v", got.Macros)
	}
	if len(got.MacroButtons) != 1 || got.MacroButtons[0].Region != "toolbar" {
		t.Fatalf("reopened buttons = %+v", got.MacroButtons)
	}
	if got.Log.Filter != "jog" || got.Log.Autoscroll {
		t.Fatalf("reopened log settings = %+v", got.Log)
	}
	if got.Gamepad.Axes.X.Axis != 2 || got.Gamepad.Axes.X.Scale != 0.5 || got.Gamepad.DeadmanButton != 7 {
		t.Fatalf("reopened gamepad = %+v", got.Gamepad)
	}
	if len(got.Gamepad.SlowButtons) != 1 || got.Gamepad.SlowButtons[0] != 6 {
		t.Fatalf("reopened gamepad slow buttons = %+v", got.Gamepad.SlowButtons)
	}
	if len(got.Gamepad.MacroButtons) != 1 || got.Gamepad.MacroButtons[0].MacroID != "probe" {
		t.Fatalf("reopened gamepad macro buttons = %+v", got.Gamepad.MacroButtons)
	}
}

func TestUISettingsCopiesAndNormalizesSlots(t *testing.T) {
	s, _ := Open("")
	ui, err := s.SetUISettings(UISettings{
		Macros: []Macro{{ID: "probe", Name: "Probe", Lines: []string{"M114"}}},
		MacroButtons: []MacroSlot{
			{ID: "a", MacroID: "probe", Region: "toolbar", Order: 0},
			{ID: "b", MacroID: "probe", Region: "panel", Order: 1},
		},
		Log: LogSettings{Filter: "all", Autoscroll: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ui.MacroButtons) != 1 {
		t.Fatalf("macro buttons = %+v, want one placement per macro", ui.MacroButtons)
	}

	ui.Macros[0].Lines[0] = "M115"
	got := s.UISettings()
	if got.Macros[0].Lines[0] != "M114" {
		t.Fatalf("SetUISettings returned shared macro lines: %+v", got.Macros)
	}
	got.Macros[0].Lines[0] = "version"
	got.Gamepad.SlowButtons = append(got.Gamepad.SlowButtons, 9)
	got.Gamepad.MacroButtons = append(got.Gamepad.MacroButtons, GamepadMacroButton{ID: "x", Button: 2, MacroID: "probe"})
	gotAgain := s.UISettings()
	if gotAgain.Macros[0].Lines[0] != "M114" {
		t.Fatalf("UISettings returned shared macro lines: %+v", gotAgain.Macros)
	}
	if len(gotAgain.Gamepad.SlowButtons) != 2 || len(gotAgain.Gamepad.MacroButtons) != 0 {
		t.Fatalf("UISettings returned shared gamepad slices: %+v", gotAgain.Gamepad)
	}
}

func TestUISettingsGamepadDefaults(t *testing.T) {
	s, _ := Open("")
	got := s.UISettings()
	if got.Gamepad.Axes.X.Axis != 0 || got.Gamepad.Axes.X.Scale != 1 {
		t.Fatalf("default X axis = %+v", got.Gamepad.Axes.X)
	}
	if got.Gamepad.Axes.Y.Axis != 1 || !got.Gamepad.Axes.Y.Invert || got.Gamepad.Axes.Y.Scale != 1 {
		t.Fatalf("default Y axis = %+v", got.Gamepad.Axes.Y)
	}
	if got.Gamepad.Axes.Z.Axis != 3 || !got.Gamepad.Axes.Z.Invert || got.Gamepad.Axes.Z.Scale != 1 {
		t.Fatalf("default Z axis = %+v", got.Gamepad.Axes.Z)
	}
	if got.Gamepad.DeadmanButton != 0 || len(got.Gamepad.SlowButtons) != 2 {
		t.Fatalf("default gamepad buttons = %+v", got.Gamepad)
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
	removed, err := s.PruneDoneJobs(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}

	jobs := s.ListJobs()
	if len(jobs) != 1 || jobs[0].State != Failed {
		t.Errorf("after prune = %+v, want only the failed job", jobs)
	}
}
