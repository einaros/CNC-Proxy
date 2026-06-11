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
		{"  <Idle>  ", Idle, true},
		{"<Bogus|x:1>", Unknown, false},
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

func TestCanRunFileOps(t *testing.T) {
	if !Idle.CanRunFileOps() {
		t.Error("Idle should allow file ops")
	}
	for _, s := range []State{Run, Hold, Alarm, Unknown, Sleep} {
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
