package service

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/carveratest"
	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/machine"
	"github.com/uwin/cnc-proxy/internal/protocol"
	"github.com/uwin/cnc-proxy/internal/relay"
	"github.com/uwin/cnc-proxy/internal/runhistory"
	"github.com/uwin/cnc-proxy/internal/session"
	"github.com/uwin/cnc-proxy/internal/store"
)

// seedOnMachine uploads content directly to the fake machine via a client.
func seedOnMachine(t *testing.T, addr, remote string, content []byte) {
	t.Helper()
	conn, err := client.Dial(addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sum := md5.Sum(content)
	if err := conn.Upload(remote, bytes.NewReader(content), int64(len(content)), hex.EncodeToString(sum[:]), 2*time.Second, nil); err != nil {
		t.Fatalf("seed %s: %v", remote, err)
	}
}

func newService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	arb := session.New(session.Config{
		Dial: func() (*client.Conn, error) { return nil, io.EOF }, // never dialed in these tests
	})
	svc, err := New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	return svc, st
}

func TestUploadCreatesEntryAndJob(t *testing.T) {
	svc, st := newService(t)
	content := []byte("G0 X0 Y0\nG1 X10\n")
	entry, err := svc.Upload("part.nc", bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if entry.Path != "/sd/gcodes/part.nc" {
		t.Errorf("path = %q", entry.Path)
	}
	if entry.Sync != store.PendingUpload {
		t.Errorf("sync = %q, want pending_upload", entry.Sync)
	}
	sum := md5.Sum(content)
	if entry.MD5 != hex.EncodeToString(sum[:]) {
		t.Errorf("md5 = %q", entry.MD5)
	}
	jobs := st.ListJobs()
	if len(jobs) != 1 || jobs[0].Kind != store.JobUpload || jobs[0].Path != entry.Path {
		t.Errorf("jobs = %+v", jobs)
	}

	// Content is readable from cache immediately (Drive behavior).
	rc, _, err := svc.ReadCache("part.nc")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, content) {
		t.Errorf("cached content mismatch")
	}
}

func TestPathTraversalRejected(t *testing.T) {
	svc, _ := newService(t)
	if _, err := svc.Upload("../../etc/passwd", bytes.NewReader([]byte("x"))); err == nil {
		t.Error("expected traversal to be rejected")
	}
	if _, err := svc.Upload("/etc/passwd", bytes.NewReader([]byte("x"))); err == nil {
		t.Error("expected absolute path outside root to be rejected")
	}
}

func TestDeleteAndRenameRequireExisting(t *testing.T) {
	svc, _ := newService(t)
	if err := svc.Delete("nope.nc"); err != ErrNotFound {
		t.Errorf("delete missing = %v, want ErrNotFound", err)
	}
	if err := svc.Rename("nope.nc", "x.nc"); err != ErrNotFound {
		t.Errorf("rename missing = %v, want ErrNotFound", err)
	}

	svc.Upload("a.nc", bytes.NewReader([]byte("x")))
	if err := svc.Delete("a.nc"); err != nil {
		t.Errorf("delete existing: %v", err)
	}
	e, _ := svc.store.GetEntry("/sd/gcodes/a.nc")
	if e.Sync != store.PendingDelete {
		t.Errorf("sync after delete = %q, want pending_delete", e.Sync)
	}
}

func TestDownloadOnDemand(t *testing.T) {
	// A service wired to a real arbiter + fake machine, so Open() can fetch.
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	tr := machine.NewTracker()
	tr.Observe(machine.Idle)
	arb := session.New(session.Config{
		Tracker: tr,
		Dial:    func() (*client.Conn, error) { return client.Dial(m.Addr(), 2*time.Second) },
	})
	svc, err := New(st, arb)
	if err != nil {
		t.Fatal(err)
	}

	// Seed a file on the machine and record it as remote_only (as a reconcile
	// sweep would).
	content := []byte("G0 X1 Y1 ; on the machine only\n")
	seedOnMachine(t, m.Addr(), "/sd/gcodes/remote.nc", content)
	svc.PutRemoteOnly("remote.nc", int64(len(content)), time.Unix(0, 0), "")

	// Opening it should fetch from the machine into the cache, then serve it.
	rc, entry, err := svc.Open("remote.nc")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != string(content) {
		t.Errorf("downloaded content = %q, want %q", got, content)
	}
	if entry.Sync != store.Synced && entry.Sync != store.RemoteOnly {
		t.Errorf("entry sync = %q", entry.Sync)
	}
	// After the fetch the catalog entry should be cached + synced.
	if e, _ := svc.Lookup("remote.nc"); e.Sync != store.Synced || e.CachePath == "" {
		t.Errorf("after fetch entry = %+v, want synced+cached", e)
	}
}

func TestDownloadCompressedSidecar(t *testing.T) {
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetCompressDownloads(true) // machine sends .lz, reports uncompressed MD5

	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	tr := machine.NewTracker()
	tr.Observe(machine.Idle)
	arb := session.New(session.Config{
		Tracker: tr,
		Dial:    func() (*client.Conn, error) { return client.Dial(m.Addr(), 2*time.Second) },
	})
	svc, err := New(st, arb)
	if err != nil {
		t.Fatal(err)
	}

	content := bytes.Repeat([]byte("compress me please\n"), 1000)
	seedOnMachine(t, m.Addr(), "/sd/gcodes/z.nc", content)
	svc.PutRemoteOnly("z.nc", int64(len(content)), time.Unix(0, 0), "")

	rc, _, err := svc.Open("z.nc")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, content) {
		t.Errorf("decompressed download mismatch: got %d bytes, want %d", len(got), len(content))
	}
}

// TestConcurrentUploadsSamePath ensures simultaneous uploads of the same path
// don't corrupt each other's cache file (they used to share one ".tmp"). Each
// upload must end with a coherent cache file matching its own content's MD5.
func TestConcurrentUploadsSamePath(t *testing.T) {
	svc, st := newService(t)

	const n = 8
	done := make(chan []byte, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			// Distinct content per goroutine, all to the SAME remote path.
			content := bytes.Repeat([]byte{byte('A' + i)}, 4096)
			if _, err := svc.Upload("race.nc", bytes.NewReader(content)); err != nil {
				t.Errorf("upload %d: %v", i, err)
				done <- nil
				return
			}
			done <- content
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}

	// The winning entry's cache file must exactly match its recorded MD5 — i.e.
	// no interleaved/corrupted write survived.
	entry, ok := st.GetEntry("/sd/gcodes/race.nc")
	if !ok {
		t.Fatal("no entry after concurrent uploads")
	}
	rc, _, err := svc.ReadCache("race.nc")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	sum := md5.Sum(got)
	if hex.EncodeToString(sum[:]) != entry.MD5 {
		t.Errorf("cache file MD5 %x does not match entry MD5 %s — corrupted write",
			sum, entry.MD5)
	}
	// And no leftover temp files in the cache dir.
	leftovers, _ := filepath.Glob(filepath.Join(st.CacheDir(), "*.tmp"))
	if len(leftovers) != 0 {
		t.Errorf("leftover temp files: %v", leftovers)
	}
}

func TestStatusReflectsArbiter(t *testing.T) {
	svc, _ := newService(t)
	st := svc.Status()
	if st.Mode != "owner" {
		t.Errorf("mode = %q, want owner", st.Mode)
	}
	if !st.Reconnecting {
		t.Error("owner mode with no fresh status should report reconnecting")
	}
	if !svc.arb.Tracker().ObserveStatusPayload("<Idle|MPos:0,0,0|WPos:0,0,0>") {
		t.Fatal("status should parse")
	}
	if st := svc.Status(); st.Reconnecting || st.Stale {
		t.Errorf("fresh owner status should not reconnect/stale: %+v", st)
	}
	svc.arb.EnterRelay()
	if st := svc.Status(); st.Mode != "relay" || st.Reconnecting {
		t.Error("expected relay mode after EnterRelay")
	}
	_ = time.Now
}

// serviceWithMachine wires a service to a real arbiter + fake machine with an
// explicit tracker so tests can drive machine state for idle gating.
func serviceWithMachine(t *testing.T) (*Service, *carveratest.FakeMachine, *machine.Tracker) {
	t.Helper()
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	tr := machine.NewTracker()
	arb := session.New(session.Config{
		Tracker: tr,
		Dial:    func() (*client.Conn, error) { return client.Dial(m.Addr(), 2*time.Second) },
	})
	svc, err := New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	return svc, m, tr
}

// TestSendGcodeQueryRunsRegardlessOfState confirms a read-only query (M114)
// runs even while the machine is in Run state (e.g. a controller program), and
// returns the machine's payload.
func TestSendGcodeQueryRunsRegardlessOfState(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	m.SetGcodeReply("M114", "ok C: X:1.000 Y:2.000 Z:3.000")
	tr.Observe(machine.Run) // a program is running

	out, err := svc.SendGcode("M114")
	if err != nil {
		t.Fatalf("M114 during Run should be allowed: %v", err)
	}
	if out != "C: X:1.000 Y:2.000 Z:3.000" {
		t.Errorf("M114 out = %q", out)
	}
	if g := m.Gcodes(); len(g) != 1 || g[0] != "M114" {
		t.Errorf("machine gcodes = %v", g)
	}
}

// TestSendGcodeMotionRequiresIdle confirms a motion command is rejected (and
// never reaches the machine) while a program runs, but succeeds once Idle.
func TestSendGcodeMotionRequiresIdle(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)

	m.SetStatus("<Run|MPos:0,0,0|WPos:0,0,0>")
	tr.Observe(machine.Run)
	_, err := svc.SendGcode("G91 G0 X-10")
	if !session.Retryable(err) {
		t.Fatalf("motion during Run = %v, want retryable ErrNotIdle", err)
	}
	if g := m.Gcodes(); len(g) != 0 {
		t.Fatalf("motion leaked to machine during Run: %v", g)
	}

	// Now Idle: the move is accepted and reaches the machine.
	m.SetStatus("<Idle|MPos:0,0,0|WPos:0,0,0>")
	tr.Observe(machine.Idle)
	if _, err := svc.SendGcode("G91 G0 X-10"); err != nil {
		t.Fatalf("motion during Idle: %v", err)
	}
	if g := m.Gcodes(); len(g) != 1 || g[0] != "G91 G0 X-10" {
		t.Errorf("machine gcodes = %v, want the move", g)
	}
}

// TestSendGcodeMotionDoesNotWaitForOk is the regression for the "second move
// spins forever" bug. The firmware sends NO terminating "ok" for motion gcode
// over WiFi (verified on hardware), so SendGcode must NOT block waiting for one
// — if it did, the first move would hold opMu until the command timeout and
// every later command would queue behind it. This sends several motion commands
// back-to-back; each must return promptly and all must reach the machine in
// order. A regression (waiting for ok) makes this hang well past the deadline.
func TestSendGcodeMotionDoesNotWaitForOk(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	tr.Observe(machine.Idle)

	moves := []string{"G91 G0 X-10", "G91 G0 X10", "G91 G0 X-10", "G91 G0 X10"}
	done := make(chan error, 1)
	go func() {
		for _, mv := range moves {
			if _, err := svc.SendGcode(mv); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("sequential motion: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("sequential motion commands hung — SendGcode is waiting for an ok the firmware never sends")
	}

	if g := m.Gcodes(); len(g) != len(moves) {
		t.Fatalf("machine received %d gcodes, want %d: %v", len(g), len(moves), g)
	}
}

// TestSendControlNotIdleGated confirms feed-hold/resume/halt reach the machine
// even while it is running — that is the whole point of realtime control.
func TestSendControlNotIdleGated(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	tr.Observe(machine.Run)

	if err := svc.SendControl(ControlFeedHold); err != nil {
		t.Fatalf("feed-hold during Run: %v", err)
	}
	if err := svc.SendControl(ControlHalt); err != nil {
		t.Fatalf("halt during Run: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(m.Controls()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := m.Controls(); len(got) != 2 || got[0] != '!' || got[1] != 0x18 {
		t.Errorf("controls = %v, want [! 0x18]", got)
	}
}

// TestSendControlRejectsUnknown guards the action-mapping.
func TestSendControlRejectsUnknown(t *testing.T) {
	svc, _, _ := serviceWithMachine(t)
	if err := svc.SendControl('Q'); err == nil {
		t.Error("expected error for unsupported control char")
	}
}

func TestRecoverAlarmSoftLimitUnlocksAndVerifies(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	if !tr.ObserveStatusPayload("<Alarm|MPos:0,0,0|WPos:0,0,0|H:10>") {
		t.Fatal("alarm status should parse")
	}

	start := time.Now()
	res, err := svc.RecoverAlarm("recover")
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("recovery waited too long: %s", elapsed)
	}
	if !res.Recovered || res.State != machine.Idle || !res.NeedsHome {
		t.Fatalf("recovery result = %+v, want recovered Idle with needs_home", res)
	}
	if g := m.Gcodes(); len(g) != 1 || g[0] != "$X" {
		t.Fatalf("recovery gcodes = %v, want [$X]", g)
	}
}

func TestRecoverAlarmViaRelayInjectionVerifiesStatus(t *testing.T) {
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	status := "<Alarm|MPos:0,0,0|WPos:0,0,0|H:10>"
	m.SetStatus(status)

	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	tr := machine.NewTracker()
	arb := session.New(session.Config{
		Tracker: tr,
		Dial:    func() (*client.Conn, error) { return client.Dial(m.Addr(), 2*time.Second) },
	})
	relaySrv := &relay.Server{
		Dial:     func() (string, error) { return m.Addr(), nil },
		Observer: arb,
	}
	arb.SetInjector(relayAdapter{relaySrv})
	arb.SetControlWriter(relaySrv)
	svc, err := New(st, arb)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go relaySrv.Serve(ln)

	controller, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { controller.Close() })
	if _, err := controller.Write(protocol.QueryStatus()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		st, _ := tr.Current()
		if arb.Mode() == session.ModeRelay && st.State == machine.Alarm {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("relay did not observe initial alarm status; mode=%s status=%+v", arb.Mode(), st)
		}
		time.Sleep(10 * time.Millisecond)
	}

	res, err := svc.RecoverAlarm("recover")
	if err != nil {
		t.Fatalf("recover via relay: %v", err)
	}
	if !res.Recovered || res.State != machine.Idle || !res.NeedsHome {
		t.Fatalf("recovery result = %+v, want recovered Idle with needs_home", res)
	}
	if g := m.Gcodes(); len(g) != 1 || g[0] != "$X" {
		t.Fatalf("recovery gcodes = %v, want [$X]", g)
	}
}

type relayAdapter struct{ srv *relay.Server }

func (a relayAdapter) AcquireMachine() (session.InjectTransport, func(), error) {
	it, release, err := a.srv.AcquireMachine()
	if err != nil {
		return nil, nil, err
	}
	return it, release, nil
}

func TestRecoverAlarmSoftLimitFallsBackToM999(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	m.SetUnlockDoesNotClear(true)
	if !tr.ObserveStatusPayload("<Alarm|MPos:0,0,0|WPos:0,0,0|H:10>") {
		t.Fatal("alarm status should parse")
	}
	m.SetStatus("<Alarm|MPos:0,0,0|WPos:0,0,0|H:10>")

	res, err := svc.RecoverAlarm("recover")
	if err != nil {
		t.Fatalf("recover with M999 fallback: %v", err)
	}
	if !res.Recovered || res.State != machine.Idle {
		t.Fatalf("recovery result = %+v, want recovered Idle", res)
	}
	if g := m.Gcodes(); len(g) != 2 || g[0] != "$X" || g[1] != "M999" {
		t.Fatalf("recovery gcodes = %v, want [$X M999]", g)
	}
}

func TestRecoverAlarmStillAlarmReturnsUnavailable(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	m.SetUnlockDoesNotClear(true)
	m.SetM999DoesNotClear(true)
	status := "<Alarm|MPos:0,0,0|WPos:0,0,0|H:10>"
	m.SetStatus(status)
	if !tr.ObserveStatusPayload(status) {
		t.Fatal("alarm status should parse")
	}

	res, err := svc.RecoverAlarm("recover")
	if err == nil {
		t.Fatal("expected recovery failure")
	}
	if !errors.Is(err, ErrRecoveryUnavailable) {
		t.Fatalf("recover err = %v, want ErrRecoveryUnavailable", err)
	}
	if res.Recovered || res.State != machine.Alarm {
		t.Fatalf("recovery result = %+v, want unrecovered Alarm", res)
	}
	if g := m.Gcodes(); len(g) != 2 || g[0] != "$X" || g[1] != "M999" {
		t.Fatalf("recovery gcodes = %v, want [$X M999]", g)
	}
}

func TestRecoverAlarmHomeBypassesAlarmIdleGate(t *testing.T) {
	svc, m, tr := serviceWithMachine(t)
	if !tr.ObserveStatusPayload("<Alarm|MPos:0,0,0|WPos:0,0,0|H:10>") {
		t.Fatal("alarm status should parse")
	}

	res, err := svc.RecoverAlarm("home")
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	if !res.Recovered || res.State != machine.Idle {
		t.Fatalf("home result = %+v, want recovered Idle", res)
	}
	if g := m.Gcodes(); len(g) != 1 || g[0] != "$H" {
		t.Fatalf("recovery gcodes = %v, want [$H]", g)
	}
}

func TestRecoverAlarmRefreshesStaleStatus(t *testing.T) {
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	status := "<Alarm|MPos:0,0,0|WPos:0,0,0|H:10>"
	m.SetStatus(status)
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	tr := machine.NewTracker()
	if !tr.ObserveStatusPayload(status) {
		t.Fatal("alarm status should parse")
	}
	arb := session.New(session.Config{
		Tracker:     tr,
		StateMaxAge: 50 * time.Millisecond,
		Dial:        func() (*client.Conn, error) { return client.Dial(m.Addr(), 2*time.Second) },
	})
	svc, err := New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(75 * time.Millisecond)

	res, err := svc.RecoverAlarm("unlock")
	if err != nil {
		t.Fatalf("unlock with stale cached status should refresh first: %v", err)
	}
	if !res.Recovered || res.State != machine.Idle {
		t.Fatalf("recovery result = %+v, want recovered Idle", res)
	}
	if g := m.Gcodes(); len(g) != 1 || g[0] != "$X" {
		t.Fatalf("recovery gcodes = %v, want [$X]", g)
	}
}

func TestBackupExportImportRoundTrip(t *testing.T) {
	svc, _ := newService(t)
	if _, err := svc.Upload("part.nc", strings.NewReader("G0 X0\n")); err != nil {
		t.Fatal(err)
	}
	ui, err := svc.SetUISettings(store.UISettings{
		Macros:       []store.Macro{{ID: "m1", Name: "Position", Lines: []string{"M114"}}},
		MacroButtons: []store.MacroSlot{{ID: "s1", MacroID: "m1", Region: "toolbar"}},
		Log:          store.LogSettings{Filter: "all", Autoscroll: true},
		Gamepad:      store.Gamepad{DeadmanButton: 2},
	})
	if err != nil || len(ui.Macros) != 1 {
		t.Fatalf("settings = %+v err=%v", ui, err)
	}
	svc.gcodeLog.Append("send", "api", "M114")
	svc.runHistory.Replace([]runhistory.Run{{ID: 1, File: "part.nc", StartedAt: time.Unix(1000, 0)}})
	backup := svc.ExportBackup()

	restored, _ := newService(t)
	if err := restored.ImportBackup(backup); err != nil {
		t.Fatal(err)
	}
	if _, ok := restored.Lookup("part.nc"); !ok {
		t.Fatal("restored backup missing catalog entry")
	}
	if got := restored.UISettings(); len(got.Macros) != 1 || got.Macros[0].Name != "Position" || got.Gamepad.DeadmanButton != 2 {
		t.Fatalf("restored UI = %+v", got)
	}
	if lines := restored.GcodeLog().Recent(); len(lines) != 1 || lines[0].Text != "M114" {
		t.Fatalf("restored log = %+v", lines)
	}
	if runs := restored.RunHistory(); len(runs) != 1 || runs[0].File != "part.nc" {
		t.Fatalf("restored runs = %+v", runs)
	}
}

func TestJobDiagnostics(t *testing.T) {
	svc, st := newService(t)
	st.Enqueue(store.Job{Kind: store.JobUpload, Path: "/sd/gcodes/a.nc"})
	if got := svc.Jobs()[0]; got.BlockedReason != "stale_status" {
		t.Fatalf("stale diagnostic = %+v", got)
	}
	svc.arb.Tracker().Observe(machine.Run)
	if got := svc.Jobs()[0]; got.BlockedReason != "not_idle" {
		t.Fatalf("run diagnostic = %+v", got)
	}
	svc.arb.Tracker().Observe(machine.Idle)
	if got := svc.Jobs()[0]; got.BlockedReason != "ready" {
		t.Fatalf("ready diagnostic = %+v", got)
	}
	svc.arb.EnterRelay()
	if got := svc.Jobs()[0]; got.BlockedReason != "relay_active" {
		t.Fatalf("relay diagnostic = %+v", got)
	}
	svc.arb.ExitRelay()
	if err := st.UpdateJob(1, func(j *store.Job) {
		j.Attempts = 1
		j.LastError = "temporary"
	}); err != nil {
		t.Fatal(err)
	}
	if got := svc.Jobs()[0]; got.BlockedReason != "backoff" || got.BlockedUntil == nil {
		t.Fatalf("backoff diagnostic = %+v", got)
	}
}

func TestPruneCacheRemovesOnlyUnreferencedOldFiles(t *testing.T) {
	svc, st := newService(t)
	cacheDir := st.CacheDir()
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ref := filepath.Join(cacheDir, "referenced")
	orphan := filepath.Join(cacheDir, "orphan")
	temp := filepath.Join(cacheDir, "upload-old.tmp")
	for _, p := range []string{ref, orphan, temp} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutEntry(store.Entry{Path: "/sd/gcodes/a.nc", CachePath: ref, Sync: store.Synced}); err != nil {
		t.Fatal(err)
	}
	report, err := svc.PruneCache(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if report.FilesRemoved != 2 {
		t.Fatalf("report = %+v, want two files removed", report)
	}
	if _, err := os.Stat(ref); err != nil {
		t.Fatalf("referenced cache should remain: %v", err)
	}
	for _, p := range []string{orphan, temp} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s stat = %v, want removed", p, err)
		}
	}
}
