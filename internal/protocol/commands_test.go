package protocol

import "testing"

func TestIsStatusQuery(t *testing.T) {
	queries := []string{
		"M114", "m114", "M114.1", "  M115 ", "M119", "M105",
		"version", "VERSION", "ftype", "model", "diagnose",
		"$", "$$", "$G", "$g", "$#", "$I", "$N",
	}
	for _, q := range queries {
		if !IsStatusQuery(q) {
			t.Errorf("IsStatusQuery(%q) = false, want true", q)
		}
	}

	mutating := []string{
		"", "G0 X10", "G1 Y5", "G91 G0 X-10", "G28", "G28.1", "G92 X0",
		"M3 S1000", "M5", "$H", "$X", "T1", "M6 T2",
		"ls -e -s /sd/gcodes", "cat foo.nc -e", "md5sum foo.nc", "rm foo.nc -e",
		"mkdir /sd/gcodes/x -e", "buffer M6T1",
	}
	for _, m := range mutating {
		if IsStatusQuery(m) {
			t.Errorf("IsStatusQuery(%q) = true, want false (must be idle-gated)", m)
		}
	}
}

func TestClassifyGcode(t *testing.T) {
	cases := []struct {
		line        string
		wantResp    Response
		wantReqIdle bool
	}{
		// Pure queries: reply expected, not idle-gated.
		{"M114", ReplyExpected, false},
		{"m114.1", ReplyExpected, false},
		{"M115", ReplyExpected, false},
		{"M119", ReplyExpected, false},
		{"M105", ReplyExpected, false},
		{"version", ReplyExpected, false},
		{"$#", ReplyExpected, false},
		{"$G", ReplyExpected, false},
		{"N10 M114", ReplyExpected, false}, // leading line number stripped

		// Motion / modal / dwell / SD: silent, idle-gated.
		{"G91 G0 X-10", FireAndForget, true},
		{"G90", FireAndForget, true},
		{"M5", FireAndForget, true},
		{"M400", FireAndForget, true},
		{"G4 P100", FireAndForget, true},
		{"$H", FireAndForget, true},
		{"$X", FireAndForget, true},
		{"", FireAndForget, true},

		// Dual-nature: bare = report (reply, not gated); with arg = set (silent, gated).
		{"M220", ReplyExpected, false},
		{"M220 S150", FireAndForget, true},
		{"M211", ReplyExpected, false},
		{"M211 S1", FireAndForget, true},
		{"M204", ReplyExpected, false},
		{"M204 S150", FireAndForget, true},
	}
	for _, c := range cases {
		resp, reqIdle := ClassifyGcode(c.line)
		if resp != c.wantResp || reqIdle != c.wantReqIdle {
			t.Errorf("ClassifyGcode(%q) = (%v, idle=%v), want (%v, idle=%v)",
				c.line, resp, reqIdle, c.wantResp, c.wantReqIdle)
		}
	}
}

func TestUnescapeRoundTrip(t *testing.T) {
	for _, s := range []string{"a b c", "f?o*o!~", "/sd/gcodes/my part.nc", "plain"} {
		if got := Unescape(Escape(s)); got != s {
			t.Errorf("Unescape(Escape(%q)) = %q", s, got)
		}
	}
}

func TestControllerActionLines(t *testing.T) {
	if got := PlayLine("/sd/gcodes/my part.nc"); got != "play /sd/gcodes/my\x01part.nc\n" {
		t.Errorf("PlayLine escaped = %q", got)
	}
	if got := SetCurrentToolLine(7); got != "M493.2T7\n" {
		t.Errorf("SetCurrentToolLine = %q", got)
	}
	if got := ChangeToolLine(7); got != "M6T7\n" {
		t.Errorf("ChangeToolLine = %q", got)
	}
	if got := CalibrateCurrentToolLine(); got != "M491\n" {
		t.Errorf("CalibrateCurrentToolLine = %q", got)
	}
}
