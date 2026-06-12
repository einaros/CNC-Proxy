package protocol

import "testing"

func TestParseLsLine(t *testing.T) {
	e, ok := ParseLsLine("part.nc 12345 20260101120000\r\n")
	if !ok {
		t.Fatal("expected valid entry")
	}
	if e.Name != "part.nc" || e.IsDir || e.Size != 12345 {
		t.Errorf("entry = %+v", e)
	}
	if e.MTime.IsZero() {
		t.Error("expected parsed mtime")
	}

	d, ok := ParseLsLine("subdir/ 0 20260101120000")
	if !ok || !d.IsDir || d.Name != "subdir" {
		t.Errorf("dir entry = %+v ok=%v", d, ok)
	}

	// Space in name is encoded as 0x01 on the wire.
	s, ok := ParseLsLine("my\x01part.nc 10 20260101120000")
	if !ok || s.Name != "my part.nc" {
		t.Errorf("spaced name = %q ok=%v", s.Name, ok)
	}
}

func TestParseLsLineRejects(t *testing.T) {
	bad := []string{
		"",
		"<Idle|MPos:0,0,0>", // status report leaked into buffer
		".hidden 10 20260101120000",
		"only two fields",
		"name notanumber 20260101120000",
		"Load directory finished.\r\n",
	}
	for _, s := range bad {
		if _, ok := ParseLsLine(s); ok {
			t.Errorf("ParseLsLine(%q) unexpectedly ok", s)
		}
	}
}

func TestParseListingMultiline(t *testing.T) {
	payload := "a.nc 1 20260101120000\r\nsub/ 0 20260101120000\r\n.git 0 20260101120000\r\n"
	entries := ParseListing(payload)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (hidden skipped)", len(entries))
	}
}

func TestParseMd5Response(t *testing.T) {
	d, ok := ParseMd5Response("d41d8cd98f00b204e9800998ecf8427e /sd/gcodes/x.nc\r\n")
	if !ok || d != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Errorf("digest = %q ok=%v", d, ok)
	}
	if _, ok := ParseMd5Response("File not found: x.nc\r\n"); ok {
		t.Error("expected not-found message to be rejected")
	}
	if _, ok := ParseMd5Response("deadbeef x.nc"); ok {
		t.Error("expected short digest to be rejected")
	}
}
