package machinetransport

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type fakeSerialPort struct {
	mu sync.Mutex

	readData []byte
	timeout  time.Duration

	writes []byte

	dtrCalls []bool
	resets   int
	closed   bool
}

func (p *fakeSerialPort) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.readData) == 0 {
		return 0, nil
	}
	n := copy(b, p.readData)
	p.readData = p.readData[n:]
	return n, nil
}

func (p *fakeSerialPort) Write(b []byte) (int, error) {
	for i := range b {
		p.mu.Lock()
		p.writes = append(p.writes, b[i])
		p.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	return len(b), nil
}

func (p *fakeSerialPort) SetReadTimeout(t time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.timeout = t
	return nil
}

func (p *fakeSerialPort) ResetInputBuffer() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resets++
	return nil
}

func (p *fakeSerialPort) SetDTR(dtr bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dtrCalls = append(p.dtrCalls, dtr)
	return nil
}

func (p *fakeSerialPort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

func TestSerialConnDoesNotResetDTRByDefault(t *testing.T) {
	fp := &fakeSerialPort{}
	if _, err := newSerialConn(fp, false); err != nil {
		t.Fatalf("newSerialConn: %v", err)
	}
	if len(fp.dtrCalls) != 0 || fp.resets != 0 {
		t.Fatalf("default open touched DTR/reset: dtr=%v resets=%d", fp.dtrCalls, fp.resets)
	}
}

func TestSerialConnResetOnOpenIsOptIn(t *testing.T) {
	oldSleep := serialResetSleep
	serialResetSleep = func(time.Duration) {}
	defer func() { serialResetSleep = oldSleep }()

	fp := &fakeSerialPort{}
	if _, err := newSerialConn(fp, true); err != nil {
		t.Fatalf("newSerialConn: %v", err)
	}
	if fp.resets != 1 {
		t.Fatalf("resets = %d, want 1", fp.resets)
	}
	if len(fp.dtrCalls) != 2 || fp.dtrCalls[0] || !fp.dtrCalls[1] {
		t.Fatalf("DTR calls = %v, want [false true]", fp.dtrCalls)
	}
}

func TestSerialConnReadDeadlineReturnsTimeout(t *testing.T) {
	fp := &fakeSerialPort{}
	conn, err := newSerialConn(fp, false)
	if err != nil {
		t.Fatalf("newSerialConn: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	_, err = conn.Read(make([]byte, 8))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("Read err = %v, want timeout net.Error", err)
	}
}

func TestSerialConnSerializesWrites(t *testing.T) {
	fp := &fakeSerialPort{}
	conn, err := newSerialConn(fp, false)
	if err != nil {
		t.Fatalf("newSerialConn: %v", err)
	}

	start := make(chan struct{})
	done := make(chan error, 2)
	for _, data := range [][]byte{[]byte("aaa"), []byte("bbb")} {
		data := data
		go func() {
			<-start
			_, err := conn.Write(data)
			done <- err
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	got := string(fp.writes)
	if got != "aaabbb" && got != "bbbaaa" {
		t.Fatalf("writes interleaved: %q", got)
	}
}
