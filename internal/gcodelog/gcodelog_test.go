package gcodelog

import (
	"testing"
)

func TestAppendSplitsAndTrimsLines(t *testing.T) {
	l := New(10)
	l.Append(DirRecv, SourceController, "line one\r\nline two\r\n\r\n")
	got := l.Recent()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (blank line dropped): %+v", len(got), got)
	}
	if got[0].Text != "line one" || got[1].Text != "line two" {
		t.Errorf("texts = %q, %q", got[0].Text, got[1].Text)
	}
	if got[0].Seq >= got[1].Seq {
		t.Errorf("seqs not increasing: %d, %d", got[0].Seq, got[1].Seq)
	}
}

func TestRingDropsOldest(t *testing.T) {
	l := New(3)
	l.Append(DirSend, SourceAPI, "a")
	l.Append(DirSend, SourceAPI, "b")
	l.Append(DirSend, SourceAPI, "c")
	l.Append(DirSend, SourceAPI, "d")
	got := l.Recent()
	if len(got) != 3 || got[0].Text != "b" || got[2].Text != "d" {
		t.Fatalf("ring = %+v", got)
	}
}

func TestSubscribeReceivesNewLines(t *testing.T) {
	l := New(10)
	ch, unsub := l.Subscribe()
	defer unsub()
	l.Append(DirSend, SourceAPI, "G0 X0")
	ln := <-ch
	if ln.Text != "G0 X0" || ln.Dir != DirSend || ln.Source != SourceAPI {
		t.Errorf("line = %+v", ln)
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	l := New(10)
	ch, unsub := l.Subscribe()
	unsub()
	if _, ok := <-ch; ok {
		t.Error("channel not closed after unsubscribe")
	}
	// A second unsubscribe must be a no-op, not a double-close panic.
	unsub()
	l.Append(DirSend, SourceAPI, "after") // must not block or panic
}

func TestRecentNeverNil(t *testing.T) {
	l := New(5)
	if got := l.Recent(); got == nil {
		t.Error("Recent() = nil, want empty slice")
	}
}
