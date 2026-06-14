package machine

import (
	"testing"
	"time"
)

func TestParseStatus(t *testing.T) {
	cases := []struct {
		in    string
		want  State
		valid bool
	}{
		{"<Idle|MPos:68.99,-49.92,40.0,12.3|WPos:1,2,3|F:1,2|S:1,2>", Idle, true},
		{"Idle|MPos:0,0,0", Idle, true},
		{"<Run|P:10,50,3>", Run, true},
		{"<Hold|MPos:0,0,0>", Hold, true},
		{"<Alarm|H:3>", Alarm, true},
		{"<Pause|MPos:0,0,0>", Pause, true},
		{"<Wait|MPos:0,0,0>", Wait, true},
		{"<Tool|MPos:0,0,0>", Tool, true},
		{"  <Idle>  ", Idle, true},
		{"<Bogus|x:1>", Unknown, true},
		{"Bogus|x:1", Unknown, true},
		{"", Unknown, false},
		{"ok\r\n", Unknown, false},
	}
	for _, c := range cases {
		got, ok := ParseStatus(c.in)
		if got != c.want || ok != c.valid {
			t.Errorf("ParseStatus(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.valid)
		}
	}
}

func TestParseStatusPayloadRichFields(t *testing.T) {
	st, ok := ParseStatusPayload("<Run|MPos:1.25,-2.5,3.75,4,5|WPos:6,7,8|F:10,20,150|S:1000,12000,80,1,31.5,42.0,0,0,1|T:2,12.345,3|P:100,45,12|C:1,7,0,1>")
	if !ok {
		t.Fatal("status should parse")
	}
	if st.State != Run {
		t.Fatalf("state = %q", st.State)
	}
	if st.Raw == "" || st.Fields["MPos"] == "" || st.Fields["S"] == "" {
		t.Fatalf("raw fields not preserved: %+v", st)
	}
	if st.MPos["x"] != 1.25 || st.MPos["y"] != -2.5 || st.MPos["z"] != 3.75 || st.MPos["a"] != 4 || st.MPos["b"] != 5 {
		t.Fatalf("mpos = %+v", st.MPos)
	}
	if st.WPos["x"] != 6 || st.WPos["y"] != 7 || st.WPos["z"] != 8 {
		t.Fatalf("wpos = %+v", st.WPos)
	}
	if st.Feed == nil || st.Feed.Current != 10 || st.Feed.Target != 20 || st.Feed.Override != 150 {
		t.Fatalf("feed = %+v", st.Feed)
	}
	if st.Spindle == nil || st.Spindle.CurrentRPM != 1000 || st.Spindle.TargetRPM != 12000 || st.Spindle.Override != 80 {
		t.Fatalf("spindle = %+v", st.Spindle)
	}
	if st.Tool == nil || st.Tool.Active != 2 || st.Tool.Offset != 12.345 || st.Tool.Target == nil || *st.Tool.Target != 3 {
		t.Fatalf("tool = %+v", st.Tool)
	}
	if len(st.Progress) != 3 || st.Progress[1] != 45 {
		t.Fatalf("progress = %+v", st.Progress)
	}
	if len(st.Machine) != 4 || st.Machine[3] != 1 {
		t.Fatalf("machine = %+v", st.Machine)
	}
}

func TestParseStatusPayloadDuplicateFields(t *testing.T) {
	st, ok := ParseStatusPayload("<Idle|T:1,0|T:2,0>")
	if !ok {
		t.Fatal("status should parse")
	}
	if st.Fields["T"] != "1,0" || st.Fields["T#2"] != "2,0" {
		t.Fatalf("duplicate fields = %+v", st.Fields)
	}
}

func TestCanRunFileOps(t *testing.T) {
	if !Idle.CanRunFileOps() {
		t.Error("Idle should allow file ops")
	}
	for _, s := range []State{Run, Hold, Alarm, Unknown, Sleep, Home, Pause, Wait, Tool} {
		if s.CanRunFileOps() {
			t.Errorf("%q should not allow file ops", s)
		}
	}
}

func TestTrackerFreshness(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := &Tracker{now: func() time.Time { return now }}

	if st, _ := tr.Snapshot(); st != Unknown {
		t.Errorf("unobserved snapshot state = %q, want Unknown", st)
	}
	if tr.Fresh(time.Second) {
		t.Error("unobserved tracker should not be fresh")
	}

	tr.Observe(Idle)
	if !tr.Fresh(time.Second) {
		t.Error("just-observed state should be fresh")
	}
	now = now.Add(5 * time.Second)
	if tr.Fresh(time.Second) {
		t.Error("5s-old state should be stale at 1s maxAge")
	}
	st, age := tr.Snapshot()
	if st != Idle || age != 5*time.Second {
		t.Errorf("snapshot = (%q,%v)", st, age)
	}
}

func TestTrackerUnknownStatusFailsClosed(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := &Tracker{now: func() time.Time { return now }}
	tr.Observe(Idle)

	now = now.Add(time.Second)
	if !tr.ObserveStatusPayload("<FutureState|MPos:0,0,0>") {
		t.Fatal("well-formed unknown status should be observed")
	}
	st, age := tr.Snapshot()
	if st != Unknown || age != 0 {
		t.Fatalf("snapshot after unknown status = (%q,%v), want fresh Unknown", st, age)
	}
	if st.CanRunFileOps() {
		t.Fatal("fresh unknown state must not allow file ops")
	}
}

func TestTrackerCurrentAndSubscribe(t *testing.T) {
	now := time.Unix(1000, 0)
	tr := &Tracker{now: func() time.Time { return now }}
	ch, unsub := tr.Subscribe()
	defer unsub()

	if !tr.ObserveStatusPayload("<Idle|MPos:1,2,3|WPos:4,5,6>") {
		t.Fatal("status should parse")
	}
	select {
	case st := <-ch:
		if st.State != Idle || st.MPos["x"] != 1 {
			t.Fatalf("subscription status = %+v", st)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for status subscription")
	}

	now = now.Add(250 * time.Millisecond)
	st, age := tr.Current()
	if st.State != Idle || st.MPos["z"] != 3 || age != 250*time.Millisecond {
		t.Fatalf("current = %+v age=%v", st, age)
	}

	// Mutating the returned snapshot must not mutate the tracker.
	st.MPos["x"] = 99
	st2, _ := tr.Current()
	if st2.MPos["x"] != 1 {
		t.Fatalf("tracker leaked mutable status map: %+v", st2.MPos)
	}
}
