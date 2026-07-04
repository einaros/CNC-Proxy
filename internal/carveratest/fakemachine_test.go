package carveratest

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/machine"
	"github.com/uwin/cnc-proxy/internal/protocol"
)

func TestFakeMachineDiagnoseUsesDiagRes(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	c, err := net.Dial("tcp", m.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Write(protocol.Encode(protocol.CmdCtrlMulti, []byte("diagnose\n"))); err != nil {
		t.Fatal(err)
	}

	c.SetReadDeadline(time.Now().Add(time.Second))
	var scan protocol.Scanner
	buf := make([]byte, 1024)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	frames := scan.Push(buf[:n])
	if len(frames) == 0 {
		t.Fatal("no response frame")
	}
	if frames[0].Cmd != protocol.CmdDiagRes {
		t.Fatalf("diagnose response cmd = %s, want DIAG_RES", protocol.CmdName(frames[0].Cmd))
	}
}

func TestFakeMachineInstantJogInterpolatesStatusPosition(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "$J X30 F0.1\n")

	st := queryFakeStatus(t, c)
	if st.MPos["x"] >= 30 {
		t.Fatalf("instant jog status jumped to target immediately: %+v", st.MPos)
	}
}

func TestFakeMachineInstantJogFinishesAtTarget(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "$J X1.25 Y-2 Z0.5 F1\n")
	time.Sleep(100 * time.Millisecond)

	st := queryFakeStatus(t, c)
	assertAxis(t, st.MPos, "x", 1.25)
	assertAxis(t, st.MPos, "y", -2)
	assertAxis(t, st.MPos, "z", 0.5)
	assertAxis(t, st.WPos, "x", 1.25)
	assertAxis(t, st.WPos, "y", -2)
	assertAxis(t, st.WPos, "z", 0.5)
}

func TestFakeMachineG53MoveUpdatesMachineAndWorkPosition(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetStatus("<Idle|MPos:1,2,3|WPos:0,1,2|F:0,0,100>")
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "G53 G0 X5 Z4\n")
	time.Sleep(180 * time.Millisecond)

	st := queryFakeStatus(t, c)
	assertAxis(t, st.MPos, "x", 5)
	assertAxis(t, st.MPos, "y", 2)
	assertAxis(t, st.MPos, "z", 4)
	assertAxis(t, st.WPos, "x", 4)
	assertAxis(t, st.WPos, "y", 1)
	assertAxis(t, st.WPos, "z", 3)
	if st.Feed == nil {
		t.Fatal("status mutation dropped F field")
	}
}

func TestFakeMachineG0IgnoresFeedRate(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "G0 X1 F1\n")
	time.Sleep(100 * time.Millisecond)

	st := queryFakeStatus(t, c)
	assertAxis(t, st.MPos, "x", 1)
	assertAxis(t, st.WPos, "x", 1)
}

func TestFakeMachineG1UsesModalFeedRate(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "G1 F60\n")
	writeCtrlMulti(t, c, "X10\n")
	time.Sleep(120 * time.Millisecond)

	st := queryFakeStatus(t, c)
	got := st.MPos["x"]
	if got <= 0 || got >= 5 {
		t.Fatalf("modal feed move X = %v, want slow in-progress move below 5mm (all axes %+v)", got, st.MPos)
	}
}

func TestFakeMachineOriginCommandUpdatesWorkPosition(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetStatus("<Idle|MPos:5,2,-1|WPos:4,1,-2>")
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "G10L20P0Z0\n")

	st := queryFakeStatus(t, c)
	assertAxis(t, st.MPos, "x", 5)
	assertAxis(t, st.MPos, "y", 2)
	assertAxis(t, st.MPos, "z", -1)
	assertAxis(t, st.WPos, "x", 4)
	assertAxis(t, st.WPos, "y", 1)
	assertAxis(t, st.WPos, "z", 0)
}

func TestFakeMachineRenameMovesUploadedFile(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	conn := dialFakeClient(t, m)

	content := []byte("renamed gcode")
	uploadFakeContent(t, conn, "/sd/gcodes/original.nc", content)
	if err := conn.Rename("/sd/gcodes/original.nc", "/sd/gcodes/renamed.nc", time.Second); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	if _, ok := m.File("/sd/gcodes/original.nc"); ok {
		t.Fatal("old path still exists after rename")
	}
	got, ok := m.File("/sd/gcodes/renamed.nc")
	if !ok || !bytes.Equal(got, content) {
		t.Fatalf("renamed file = %q ok=%v, want %q", got, ok, content)
	}
}

func TestFakeMachineRejectsFileOpsWhileRunning(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "G1 F60\n")
	writeCtrlMulti(t, c, "X10\n")
	st := queryFakeStatus(t, c)
	if st.State != machine.Run {
		t.Fatalf("state after slow move = %s, want Run", st.State)
	}

	if _, err := c.Write(protocol.LsCommand("/sd/gcodes")); err != nil {
		t.Fatal(err)
	}
	f := readFakeFrame(t, c)
	if f.Cmd != protocol.CmdLoadError {
		t.Fatalf("ls while running cmd = %s, want LOAD_ERROR", protocol.CmdName(f.Cmd))
	}

	conn := dialFakeClient(t, m)
	err = conn.Upload("/sd/gcodes/busy.nc", bytes.NewReader([]byte("x")), 1, md5String([]byte("x")), time.Second, nil)
	if err == nil {
		t.Fatal("upload while running succeeded, want cancellation")
	}
}

func TestFakeMachineRealtimeControlTransitionsStatus(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "G1 F60\n")
	writeCtrlMulti(t, c, "X10\n")
	if st := queryFakeStatus(t, c); st.State != machine.Run {
		t.Fatalf("initial state = %s, want Run", st.State)
	}

	if _, err := c.Write(protocol.FeedHold()); err != nil {
		t.Fatal(err)
	}
	held := queryFakeStatus(t, c)
	if held.State != machine.Hold {
		t.Fatalf("held state = %s, want Hold", held.State)
	}
	time.Sleep(30 * time.Millisecond)
	stillHeld := queryFakeStatus(t, c)
	assertAxis(t, stillHeld.MPos, "x", held.MPos["x"])

	if _, err := c.Write(protocol.Resume()); err != nil {
		t.Fatal(err)
	}
	if resumed := queryFakeStatus(t, c); resumed.State != machine.Run {
		t.Fatalf("resumed state = %s, want Run", resumed.State)
	}

	if _, err := c.Write(protocol.Halt()); err != nil {
		t.Fatal(err)
	}
	alarm := queryFakeStatus(t, c)
	if alarm.State != machine.Alarm || alarm.HaltReason == nil {
		t.Fatalf("halt status = %+v, want Alarm with H field", alarm)
	}
	writeCtrlMulti(t, c, "$X\n")
	cleared := queryFakeStatus(t, c)
	if cleared.State != machine.Idle || cleared.HaltReason != nil {
		t.Fatalf("unlock status = %+v, want Idle without H field", cleared)
	}
}

func TestFakeMachineDefaultQueryRepliesReflectState(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetStatus("<Idle|MPos:5,6,7|WPos:1,2,3>")
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "M114\n")
	f := readFakeFrame(t, c)
	if f.Cmd != protocol.CmdNormalInfo {
		t.Fatalf("M114 cmd = %s, want NORMAL_INFO", protocol.CmdName(f.Cmd))
	}
	if got := string(f.Data); !strings.Contains(got, "ok C: X:1.0000 Y:2.0000 Z:3.0000") {
		t.Fatalf("M114 reply = %q", got)
	}

	writeCtrlMulti(t, c, "$G\n")
	f = readFakeFrame(t, c)
	if got := string(f.Data); !strings.Contains(got, "[GC:G0 G21 G90]") {
		t.Fatalf("$G reply = %q", got)
	}

	writeCtrlMulti(t, c, "version\n")
	f = readFakeFrame(t, c)
	if got := string(f.Data); !strings.Contains(got, "version = 1.0.5") {
		t.Fatalf("version reply = %q", got)
	}

	writeCtrlMulti(t, c, "model\n")
	f = readFakeFrame(t, c)
	if got := string(f.Data); !strings.Contains(got, "model = CA1, 2, 4, 0") {
		t.Fatalf("model reply = %q", got)
	}
}

func TestFakeMachineConfigGetAllAndSetMirrorFirmwareSurface(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "config-get-all -e\n")
	f := readFakeFrame(t, c)
	got := string(f.Data)
	if !strings.Contains(got, "coordinate.worksize_x=300.0\n") || !strings.Contains(got, "soft_endstop.x_min=-302.0\n") || !strings.Contains(got, "\x04") {
		t.Fatalf("config-get-all reply = %q", got)
	}

	writeCtrlMulti(t, c, "config-set sd coordinate.worksize_x 250.5\n")
	f = readFakeFrame(t, c)
	if got := string(f.Data); !strings.Contains(got, "sd: coordinate.worksize_x has been set to 250.5") {
		t.Fatalf("config-set reply = %q", got)
	}
	writeCtrlMulti(t, c, "config-set sd coordinate.clearance_x -7.5\n")
	f = readFakeFrame(t, c)
	if got := string(f.Data); !strings.Contains(got, "sd: coordinate.clearance_x has been set to -7.5") {
		t.Fatalf("negative config-set reply = %q", got)
	}

	snap := m.Snapshot()
	if snap.Config["coordinate.worksize_x"] != "250.5" || snap.MachineProfile.WorkSizeXMM != 250.5 {
		t.Fatalf("snapshot config/profile = %q %+v", snap.Config["coordinate.worksize_x"], snap.MachineProfile)
	}
	if snap.MachineProfile.ClearanceX != -7.5 {
		t.Fatalf("snapshot clearance = %+v", snap.MachineProfile)
	}
	if snap.MachineProfile.Model != "CA1" || snap.MachineProfile.MachineModel != 2 || snap.MachineProfile.FuncSetting != 4 {
		t.Fatalf("machine profile identity = %+v", snap.MachineProfile)
	}
}

func TestFakeMachineToolCommandsUpdateStatusTLO(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	st := queryFakeStatus(t, c)
	if st.Tool == nil || st.Tool.Active != 0 || st.Tool.Offset != 0 {
		t.Fatalf("default tool status = %+v", st.Tool)
	}

	writeCtrlMulti(t, c, "M493.2T3\n")
	st = queryFakeStatus(t, c)
	if st.Tool == nil || st.Tool.Active != 3 || st.Tool.Offset != 0 {
		t.Fatalf("set current tool status = %+v", st.Tool)
	}

	m.SetStatus("<Idle|MPos:0,0,-12.25|WPos:0,0,-12.25|C:2,4,0,1|T:3,0.000>")
	writeCtrlMulti(t, c, "M491\n")
	st = queryFakeStatus(t, c)
	if st.Tool == nil || st.Tool.Active != 3 || math.Abs(st.Tool.Offset-(-12.25)) > 0.001 {
		t.Fatalf("calibrated tool status = %+v", st.Tool)
	}

	writeCtrlMulti(t, c, "M6T-1\n")
	st = queryFakeStatus(t, c)
	if st.Tool == nil || st.Tool.Active != -1 || st.Tool.Offset != 0 {
		t.Fatalf("drop tool status = %+v", st.Tool)
	}
}

func TestFakeMachineInsertedToolCalibrationUsesReference(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	probe, err := m.InsertTool("probe")
	if err != nil {
		t.Fatal(err)
	}
	if probe.Calibrated || !probe.SpindleLocked {
		t.Fatalf("inserted probe state = %+v, want locked and uncalibrated", probe)
	}
	toolrackZ := configFloat(m.config, "coordinate.toolrack_z", -108)
	wantProbeContact := toolrackZ - 40
	if math.Abs(probe.CalibrationMZ-wantProbeContact) > 0.001 {
		t.Fatalf("probe calibration contact = %.3f, want %.3f", probe.CalibrationMZ, wantProbeContact)
	}
	writeCtrlMulti(t, c, "M491\n")
	st := queryFakeStatus(t, c)
	if st.Tool == nil || st.Tool.Active != 0 || math.Abs(st.Tool.Offset) > 0.001 {
		t.Fatalf("probe calibration status = %+v", st.Tool)
	}
	if snap := m.Snapshot(); snap.InsertedTool == nil || snap.InsertedTool.Kind != "probe" || !snap.InsertedTool.Calibrated || snap.LastProbe == nil || snap.LastProbe.Source != fakeCalibrationSwitchSource {
		t.Fatalf("probe calibration snapshot = %+v last=%+v", snap.InsertedTool, snap.LastProbe)
	}

	tool, err := m.InsertTool("tool_3_175")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(tool.CalibrationMZ-toolrackZ) > 0.001 {
		t.Fatalf("3.175mm calibration contact = %.3f, want %.3f", tool.CalibrationMZ, toolrackZ)
	}
	writeCtrlMulti(t, c, "M491\n")
	st = queryFakeStatus(t, c)
	wantOffset := tool.CalibrationMZ - probe.CalibrationMZ
	if math.Abs(wantOffset-40) > 0.001 {
		t.Fatalf("3.175mm expected offset = %.3f, want 40.000", wantOffset)
	}
	if st.Tool == nil || st.Tool.Active != 1 || math.Abs(st.Tool.Offset-wantOffset) > 0.001 {
		t.Fatalf("3.175mm calibration status = %+v, want offset %.3f", st.Tool, wantOffset)
	}
	assertAxis(t, st.WPos, "z", -wantOffset)
	if snap := m.Snapshot(); snap.InsertedTool == nil || !snap.InsertedTool.Calibrated || math.Abs(snap.InsertedTool.CalibratedOffsetMM-wantOffset) > 0.001 {
		t.Fatalf("calibrated 3.175mm snapshot = %+v, want offset %.3f", snap.InsertedTool, wantOffset)
	}

	writeCtrlMulti(t, c, "G10L20P0Z0\n")
	st = queryFakeStatus(t, c)
	if st.Tool == nil || st.Tool.Active != 1 || math.Abs(st.Tool.Offset) > 0.001 {
		t.Fatalf("G10 reference reset status = %+v", st.Tool)
	}
	tool6, err := m.InsertTool("6")
	if err != nil {
		t.Fatal(err)
	}
	writeCtrlMulti(t, c, "M491\n")
	st = queryFakeStatus(t, c)
	wantOffset = tool6.CalibrationMZ - tool.CalibrationMZ
	if st.Tool == nil || st.Tool.Active != 2 || math.Abs(st.Tool.Offset-wantOffset) > 0.001 {
		t.Fatalf("6mm calibration after reference reset = %+v, want offset %.3f", st.Tool, wantOffset)
	}
	assertAxis(t, st.WPos, "z", -wantOffset)
}

func TestFakeMachineToolChangeWaitsForContinueAndUpdatesTLO(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "M6T2\n")
	st := queryFakeStatus(t, c)
	if st.State != machine.Tool {
		t.Fatalf("state after M6T2 = %s, want Tool", st.State)
	}
	if st.Tool == nil || st.Tool.Active != 0 || st.Tool.Target == nil || *st.Tool.Target != 2 || st.Tool.Offset != 0 {
		t.Fatalf("tool target after M6T2 = %+v, want active 0 target 2", st.Tool)
	}

	writeCtrlMulti(t, c, "M490.2\n")
	st = queryFakeStatus(t, c)
	wantOffset := (configFloat(m.config, "coordinate.toolrack_z", -108)) - (configFloat(m.config, "coordinate.toolrack_z", -108) - 40)
	if st.State != machine.Idle {
		t.Fatalf("state after continue = %s, want Idle", st.State)
	}
	if st.Tool == nil || st.Tool.Active != 2 || st.Tool.Target != nil || math.Abs(st.Tool.Offset-wantOffset) > 0.001 {
		t.Fatalf("tool after continue = %+v, want active 2 offset %.3f", st.Tool, wantOffset)
	}
	assertAxis(t, st.WPos, "z", -wantOffset)
	if snap := m.Snapshot(); snap.InsertedTool == nil || snap.InsertedTool.ToolID != 2 || !snap.InsertedTool.Calibrated || math.Abs(snap.InsertedTool.CalibratedOffsetMM-wantOffset) > 0.001 {
		t.Fatalf("snapshot inserted tool = %+v, want calibrated tool 2 offset %.3f", snap.InsertedTool, wantOffset)
	}
}

func TestFakeMachineProbeToolChangeWaitsWhenNoToolIsInserted(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	if snap := m.Snapshot(); snap.InsertedTool != nil {
		t.Fatalf("initial inserted tool = %+v, want none", snap.InsertedTool)
	}
	st := queryFakeStatus(t, c)
	if st.Tool == nil || st.Tool.Active != 0 || st.Tool.Offset != 0 || st.Tool.Target != nil {
		t.Fatalf("initial tool status = %+v, want active probe with no target", st.Tool)
	}

	writeCtrlMulti(t, c, "M6T0\n")
	st = queryFakeStatus(t, c)
	if st.State != machine.Tool {
		t.Fatalf("state after M6T0 with no inserted probe = %s, want Tool", st.State)
	}
	if st.Tool == nil || st.Tool.Active != 0 || st.Tool.Target == nil || *st.Tool.Target != 0 || st.Tool.Offset != 0 {
		t.Fatalf("tool target after M6T0 = %+v, want active 0 target 0", st.Tool)
	}

	probe, err := m.InsertTool("probe")
	if err != nil {
		t.Fatal(err)
	}
	if probe.Kind != "probe" || probe.Calibrated {
		t.Fatalf("inserted probe = %+v, want uncalibrated probe", probe)
	}
	st = queryFakeStatus(t, c)
	if st.State != machine.Tool || st.Tool == nil || st.Tool.Target == nil || *st.Tool.Target != 0 {
		t.Fatalf("status after inserting probe during wait = state %s tool %+v, want Tool target 0", st.State, st.Tool)
	}

	writeCtrlMulti(t, c, "M490.2\n")
	st = queryFakeStatus(t, c)
	if st.State != machine.Idle {
		t.Fatalf("state after continue = %s, want Idle", st.State)
	}
	if st.Tool == nil || st.Tool.Active != 0 || st.Tool.Target != nil || math.Abs(st.Tool.Offset) > 0.001 {
		t.Fatalf("tool after probe continue = %+v, want active 0 offset 0", st.Tool)
	}
	assertAxis(t, st.WPos, "z", 0)
	if snap := m.Snapshot(); snap.InsertedTool == nil || snap.InsertedTool.Kind != "probe" || !snap.InsertedTool.Calibrated || math.Abs(snap.InsertedTool.CalibratedOffsetMM) > 0.001 {
		t.Fatalf("snapshot after probe continue = %+v, want calibrated probe offset 0", snap.InsertedTool)
	}
}

func TestFakeMachineToolStickoutAdjustmentRequiresUnlockedSpindle(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if _, err := m.InsertTool("tool_6"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetInsertedToolStickout(42); err == nil {
		t.Fatal("stickout changed while spindle locked")
	}
	unlocked, err := m.SetInsertedToolSpindleLocked(false)
	if err != nil {
		t.Fatal(err)
	}
	if unlocked.SpindleLocked {
		t.Fatalf("unlocked tool = %+v", unlocked)
	}
	adjusted, err := m.SetInsertedToolStickout(42)
	if err != nil {
		t.Fatal(err)
	}
	if adjusted.StickoutMM != 42 || adjusted.Calibrated {
		t.Fatalf("adjusted tool = %+v, want 42mm uncalibrated", adjusted)
	}

	c := dialFakeMachine(t, m)
	defer c.Close()
	writeCtrlMulti(t, c, "M491\n")
	st := queryFakeStatus(t, c)
	toolrackZ := configFloat(m.config, "coordinate.toolrack_z", -108)
	nominalStickout := fakeToolPresets[2].StickoutMM
	probeContact := toolrackZ - 40
	wantContact := toolrackZ + (adjusted.StickoutMM - nominalStickout)
	if math.Abs(adjusted.CalibrationMZ-wantContact) > 0.001 {
		t.Fatalf("adjusted calibration contact = %.3f, want %.3f", adjusted.CalibrationMZ, wantContact)
	}
	wantOffset := adjusted.CalibrationMZ - probeContact
	if st.Tool == nil || st.Tool.Active != 2 || math.Abs(st.Tool.Offset-wantOffset) > 0.001 {
		t.Fatalf("calibrated adjusted tool status = %+v, want offset %.3f", st.Tool, wantOffset)
	}
	if snap := m.Snapshot(); snap.InsertedTool == nil || !snap.InsertedTool.Calibrated || math.Abs(snap.InsertedTool.CalibratedMZ-wantContact) > 0.001 {
		t.Fatalf("calibrated adjusted snapshot = %+v, want MZ %.3f", snap.InsertedTool, wantContact)
	}

	if _, err := m.SetInsertedToolStickout(43); err != nil {
		t.Fatal(err)
	}
	if snap := m.Snapshot(); snap.InsertedTool == nil || snap.InsertedTool.Calibrated {
		t.Fatalf("adjustment did not invalidate calibration: %+v", snap.InsertedTool)
	}
}

func TestFakeMachineG386CalibrationProbeSavesToolOffset(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	c := dialFakeMachine(t, m)
	defer c.Close()

	tool, err := m.InsertTool("6.35")
	if err != nil {
		t.Fatal(err)
	}
	m.SetStatus("<Idle|MPos:0,0,-3|WPos:0,0,-3|C:2,4,0,1|T:3,0.000>")

	writeCtrlMulti(t, c, "G38.6 Z-120 F500\n")
	f := readFakeFrame(t, c)
	wantPRB := "[PRB:0.0000,0.0000," + strconv.FormatFloat(tool.CalibrationMZ, 'f', 4, 64) + ":1]"
	if got := string(f.Data); f.Cmd != protocol.CmdNormalInfo || !strings.Contains(got, wantPRB) {
		t.Fatalf("calibration probe reply cmd=%s data=%q want %q", protocol.CmdName(f.Cmd), got, wantPRB)
	}
	snap := m.Snapshot()
	if snap.LastProbe == nil || !snap.LastProbe.Hit || snap.LastProbe.Source != fakeCalibrationSwitchSource {
		t.Fatalf("last calibration probe = %+v", snap.LastProbe)
	}

	writeCtrlMulti(t, c, "M493.1\n")
	st := queryFakeStatus(t, c)
	probeContact := configFloat(m.config, "coordinate.toolrack_z", -108) - 40
	wantOffset := tool.CalibrationMZ - probeContact
	if math.Abs(wantOffset-40) > 0.001 {
		t.Fatalf("M493.1 expected offset = %.3f, want 40.000", wantOffset)
	}
	if st.Tool == nil || st.Tool.Active != 3 || math.Abs(st.Tool.Offset-wantOffset) > 0.001 {
		t.Fatalf("M493.1 tool status = %+v, want offset %.3f", st.Tool, wantOffset)
	}
}

func TestFakeMachineProbeHitsLoadedSTLAndTracksLaser(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.LoadProbeModel("plate.stl", []byte(testPlaneSTL(-5))); err != nil {
		t.Fatal(err)
	}
	m.SetStatus("<Idle|MPos:5,5,2|WPos:0,0,7|C:2,4,0,1|T:0,0.000>")
	c := dialFakeMachine(t, m)
	defer c.Close()

	writeCtrlMulti(t, c, "M841\n")
	waitForProbeLaser(t, m, true)

	writeCtrlMulti(t, c, "G38.2 Z-20 F100\n")
	f := readFakeFrame(t, c)
	if got := string(f.Data); f.Cmd != protocol.CmdNormalInfo || !strings.Contains(got, "[PRB:5.0000,5.0000,-5.0000:1]") {
		t.Fatalf("probe reply cmd=%s data=%q", protocol.CmdName(f.Cmd), got)
	}
	st := queryFakeStatus(t, c)
	assertAxis(t, st.MPos, "z", -5)
	assertAxis(t, st.WPos, "z", 0)
	snap := m.Snapshot()
	if snap.LastProbe == nil || !snap.LastProbe.Hit || snap.LastProbe.Source != "plate.stl" {
		t.Fatalf("last probe = %+v", snap.LastProbe)
	}

	writeCtrlMulti(t, c, "M842\n")
	waitForProbeLaser(t, m, false)
}

func TestProbeModelParsesGLTFAndGLB(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "tri.gltf", data: testTriangleGLTF(t)},
		{name: "tri.glb", data: testTriangleGLB(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := New()
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			if err := m.LoadProbeModel(tc.name, tc.data); err != nil {
				t.Fatal(err)
			}
			mesh, ok := m.ProbeModelMesh()
			if !ok {
				t.Fatal("missing probe mesh")
			}
			if mesh.Triangles != 1 || len(mesh.Positions) != 9 {
				t.Fatalf("mesh = %+v", mesh)
			}
			if mesh.Bounds.Min.Z != -4 || mesh.Bounds.Max.X != 1 || mesh.Bounds.Max.Y != 1 {
				t.Fatalf("bounds = %+v", mesh.Bounds)
			}
		})
	}
}

func TestFakeMachinePlayRunsUploadedProgramAndProgress(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	conn := dialFakeClient(t, m)
	uploadFakeContent(t, conn, "/sd/gcodes/run.nc", []byte("G4 P0.15\nG0 X1\n"))

	c := dialFakeMachine(t, m)
	defer c.Close()
	writeCtrlMulti(t, c, protocol.PlayLine("/sd/gcodes/run.nc"))
	running := queryFakeStatus(t, c)
	if running.State != machine.Run || running.Fields["P"] == "" {
		t.Fatalf("play status = %+v, want Run with progress field", running)
	}

	time.Sleep(250 * time.Millisecond)
	done := queryFakeStatus(t, c)
	if done.State != machine.Idle {
		t.Fatalf("final state = %s, want Idle", done.State)
	}
	if _, ok := done.Fields["P"]; ok {
		t.Fatalf("final status kept progress field: %+v", done.Fields)
	}
	assertAxis(t, done.MPos, "x", 1)
}

func TestFakeMachineSnapshotReportsVisualizedState(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.SetFtype("lz")
	m.SetCompressDownloads(true)
	m.PutFile("/sd/gcodes/visual.nc", []byte("G1 X2\n"))

	c := dialFakeMachine(t, m)
	defer c.Close()
	writeCtrlMulti(t, c, "G91\n")
	writeCtrlMulti(t, c, "G1 F60\n")
	writeCtrlMulti(t, c, "X5\n")
	if _, err := c.Write(protocol.FeedHold()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for len(m.Controls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	snap := m.Snapshot()
	if snap.Status.State != machine.Hold {
		t.Fatalf("snapshot state = %s, want Hold", snap.Status.State)
	}
	if !snap.HoldActive {
		t.Fatal("snapshot did not report hold_active")
	}
	if snap.Modal.DistanceMode != "G91" || snap.Modal.Motion != "G1" || snap.Modal.FeedMMMin != 60 {
		t.Fatalf("snapshot modal = %+v", snap.Modal)
	}
	if len(snap.Motion) == 0 {
		t.Fatal("snapshot missing queued motion segment")
	}
	if len(snap.Files) != 1 || snap.Files[0].Path != "/sd/gcodes/visual.nc" || snap.Files[0].Size != len("G1 X2\n") {
		t.Fatalf("snapshot files = %+v", snap.Files)
	}
	if snap.Ftype != "lz" || !snap.CompressDownloads {
		t.Fatalf("snapshot transfer features = ftype %q compress %v", snap.Ftype, snap.CompressDownloads)
	}
	if len(snap.Controls) != 1 || snap.Controls[0].Byte != protocol.CtrlFeedHold {
		t.Fatalf("snapshot controls = %+v", snap.Controls)
	}
}

func dialFakeMachine(t *testing.T, m *FakeMachine) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", m.Addr())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func dialFakeClient(t *testing.T, m *FakeMachine) *client.Conn {
	t.Helper()
	conn, err := client.Dial(m.Addr(), time.Second, client.WithUploadStartDelay(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func uploadFakeContent(t *testing.T, conn *client.Conn, path string, content []byte) {
	t.Helper()
	if err := conn.Upload(path, bytes.NewReader(content), int64(len(content)), md5String(content), time.Second, nil); err != nil {
		t.Fatalf("upload %s: %v", path, err)
	}
}

func md5String(content []byte) string {
	sum := md5.Sum(content)
	return hex.EncodeToString(sum[:])
}

func testPlaneSTL(z float64) string {
	return strings.Join([]string{
		"solid plate",
		"facet normal 0 0 1",
		"outer loop",
		"vertex 0 0 " + strconvFloat(z),
		"vertex 10 0 " + strconvFloat(z),
		"vertex 10 10 " + strconvFloat(z),
		"endloop",
		"endfacet",
		"facet normal 0 0 1",
		"outer loop",
		"vertex 0 0 " + strconvFloat(z),
		"vertex 10 10 " + strconvFloat(z),
		"vertex 0 10 " + strconvFloat(z),
		"endloop",
		"endfacet",
		"endsolid plate",
	}, "\n")
}

func testTriangleGLTF(t *testing.T) []byte {
	t.Helper()
	bin := testTriangleGLTFBIN()
	uri := "data:application/octet-stream;base64," + base64.StdEncoding.EncodeToString(bin)
	return testTriangleGLTFJSON(strconvQuote(uri))
}

func testTriangleGLTFJSON(bufferURI string) []byte {
	json := `{
		"asset":{"version":"2.0"},
		"buffers":[{` + bufferURIField(bufferURI) + `"byteLength":42}],
		"bufferViews":[{"buffer":0,"byteOffset":0,"byteLength":36},{"buffer":0,"byteOffset":36,"byteLength":6}],
		"accessors":[
			{"bufferView":0,"componentType":5126,"count":3,"type":"VEC3"},
			{"bufferView":1,"componentType":5123,"count":3,"type":"SCALAR"}
		],
		"meshes":[{"primitives":[{"attributes":{"POSITION":0},"indices":1}]}],
		"nodes":[{"mesh":0}],
		"scenes":[{"nodes":[0]}],
		"scene":0
	}`
	return []byte(json)
}

func testTriangleGLB(t *testing.T) []byte {
	t.Helper()
	jsonChunk := pad4(testTriangleGLTFJSON(""))
	binChunk := pad4(testTriangleGLTFBIN())
	total := 12 + 8 + len(jsonChunk) + 8 + len(binChunk)
	var out bytes.Buffer
	out.Write([]byte{'g', 'l', 'T', 'F'})
	binary.Write(&out, binary.LittleEndian, uint32(2))
	binary.Write(&out, binary.LittleEndian, uint32(total))
	binary.Write(&out, binary.LittleEndian, uint32(len(jsonChunk)))
	binary.Write(&out, binary.LittleEndian, uint32(0x4e4f534a))
	out.Write(jsonChunk)
	binary.Write(&out, binary.LittleEndian, uint32(len(binChunk)))
	binary.Write(&out, binary.LittleEndian, uint32(0x004e4942))
	out.Write(binChunk)
	return out.Bytes()
}

func bufferURIField(uri string) string {
	if uri == "" {
		return ""
	}
	return `"uri":` + uri + `,`
}

func testTriangleGLTFBIN() []byte {
	var b bytes.Buffer
	for _, f := range []float32{0, 0, -4, 1, 0, -4, 0, 1, -4} {
		binary.Write(&b, binary.LittleEndian, f)
	}
	for _, idx := range []uint16{0, 1, 2} {
		binary.Write(&b, binary.LittleEndian, idx)
	}
	return b.Bytes()
}

func pad4(in []byte) []byte {
	out := append([]byte(nil), in...)
	for len(out)%4 != 0 {
		out = append(out, ' ')
	}
	return out
}

func strconvFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func writeCtrlMulti(t *testing.T, c net.Conn, line string) {
	t.Helper()
	if _, err := c.Write(protocol.Encode(protocol.CmdCtrlMulti, []byte(line))); err != nil {
		t.Fatal(err)
	}
}

func queryFakeStatus(t *testing.T, c net.Conn) machine.Status {
	t.Helper()
	if _, err := c.Write(protocol.QueryStatus()); err != nil {
		t.Fatal(err)
	}
	var scan protocol.Scanner
	buf := make([]byte, 1024)
	deadline := time.Now().Add(time.Second)
	for {
		if err := c.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		n, err := c.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range scan.Push(buf[:n]) {
			if f.Cmd != protocol.CmdStatusRes {
				continue
			}
			st, ok := machine.ParseStatusPayload(string(f.Data))
			if !ok {
				t.Fatalf("malformed status payload %q", string(f.Data))
			}
			return st
		}
	}
}

func waitForProbeLaser(t *testing.T, m *FakeMachine, want bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := m.Snapshot().ProbeLaserActive; got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("probe laser active = %v, want %v", m.Snapshot().ProbeLaserActive, want)
}

func readFakeFrame(t *testing.T, c net.Conn) protocol.Frame {
	t.Helper()
	var scan protocol.Scanner
	buf := make([]byte, 1024)
	deadline := time.Now().Add(time.Second)
	for {
		if err := c.SetReadDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		n, err := c.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			continue
		}
		frames := scan.Push(buf[:n])
		if len(frames) > 0 {
			return frames[0]
		}
	}
}

func assertAxis(t *testing.T, vals machine.AxisValues, axis string, want float64) {
	t.Helper()
	got, ok := vals[axis]
	if !ok {
		t.Fatalf("axis %s missing from %+v", axis, vals)
	}
	if math.Abs(got-want) > 0.0001 {
		t.Fatalf("axis %s = %v, want %v (all axes %+v)", axis, got, want, vals)
	}
}
