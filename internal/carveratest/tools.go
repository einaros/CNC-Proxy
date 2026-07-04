package carveratest

import (
	"fmt"
	"math"
	"strings"
	"time"
)

const fakeCalibrationSwitchSource = "calibration switch"

type fakeInsertedTool struct {
	Kind               string
	Label              string
	ToolID             int
	DiameterMM         float64
	StickoutMM         float64
	MinStickoutMM      float64
	MaxStickoutMM      float64
	Probe              bool
	SpindleLocked      bool
	Calibrated         bool
	CalibratedAt       time.Time
	CalibratedMZ       float64
	CalibratedOffsetMM float64
}

var fakeToolPresets = []fakeInsertedTool{
	{Kind: "probe", Label: "Probe", ToolID: 0, DiameterMM: 2.0, StickoutMM: 48.0, MinStickoutMM: 20, MaxStickoutMM: 75, Probe: true},
	{Kind: "tool_3_175", Label: "3.175 mm tool", ToolID: 1, DiameterMM: 3.175, StickoutMM: 38.0, MinStickoutMM: 10, MaxStickoutMM: 70},
	{Kind: "tool_6", Label: "6 mm tool", ToolID: 2, DiameterMM: 6.0, StickoutMM: 34.0, MinStickoutMM: 10, MaxStickoutMM: 70},
	{Kind: "tool_6_35", Label: "6.35 mm tool", ToolID: 3, DiameterMM: 6.35, StickoutMM: 36.0, MinStickoutMM: 10, MaxStickoutMM: 70},
}

// InsertTool simulates the operator physically inserting a tool in the fake
// spindle. Outside the firmware's Tool wait state this also updates the visible
// T status for direct sidecar setup. During a pending tool change it must not
// overwrite the firmware target: the controller already requested a tool number
// and M490.2 is what confirms that physical insertion.
func (m *FakeMachine) InsertTool(kind string) (SnapshotInsertedTool, error) {
	tool, ok := fakeToolPresetByKind(kind)
	if !ok {
		return SnapshotInsertedTool{}, fmt.Errorf("unknown tool kind %q", kind)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	cp := tool
	cp.SpindleLocked = true
	cp.CalibratedMZ = math.NaN()
	m.insertedTool = &cp
	if m.toolChangeWaitingLocked() {
		if _, _, _, ok := m.currentToolTargetStatusLocked(); !ok {
			active, offset, ok := m.currentToolStatusLocked()
			if !ok {
				active = 0
				offset = 0
			}
			m.setToolTargetStatusLocked(active, offset, cp.ToolID)
		}
	} else {
		m.setToolStatusLocked(cp.ToolID, 0)
	}
	if snap := m.snapshotInsertedToolLocked(); snap != nil {
		return *snap, nil
	}
	return SnapshotInsertedTool{}, fmt.Errorf("tool insertion failed")
}

// SetInsertedToolSpindleLocked locks or unlocks the fake spindle. Tool stickout
// can only be adjusted while it is unlocked.
func (m *FakeMachine) SetInsertedToolSpindleLocked(locked bool) (SnapshotInsertedTool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.insertedTool == nil {
		return SnapshotInsertedTool{}, fmt.Errorf("no tool inserted")
	}
	m.insertedTool.SpindleLocked = locked
	if snap := m.snapshotInsertedToolLocked(); snap != nil {
		return *snap, nil
	}
	return SnapshotInsertedTool{}, fmt.Errorf("no tool inserted")
}

// SetInsertedToolStickout changes how far the physical fake tool protrudes from
// the spindle. This is a sidecar-only physical action, so it invalidates the
// firmware's existing calibration until the controller calibrates again.
func (m *FakeMachine) SetInsertedToolStickout(stickoutMM float64) (SnapshotInsertedTool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.insertedTool == nil {
		return SnapshotInsertedTool{}, fmt.Errorf("no tool inserted")
	}
	if m.insertedTool.SpindleLocked {
		return SnapshotInsertedTool{}, fmt.Errorf("spindle is locked")
	}
	if !fakeFinite(stickoutMM) {
		return SnapshotInsertedTool{}, fmt.Errorf("invalid stickout")
	}
	if stickoutMM < m.insertedTool.MinStickoutMM || stickoutMM > m.insertedTool.MaxStickoutMM {
		return SnapshotInsertedTool{}, fmt.Errorf("stickout %.3fmm outside %.3f-%.3fmm", stickoutMM, m.insertedTool.MinStickoutMM, m.insertedTool.MaxStickoutMM)
	}
	m.insertedTool.StickoutMM = stickoutMM
	m.invalidateInsertedToolCalibrationLocked()
	if snap := m.snapshotInsertedToolLocked(); snap != nil {
		return *snap, nil
	}
	return SnapshotInsertedTool{}, fmt.Errorf("no tool inserted")
}

func fakeToolPresetByKind(kind string) (fakeInsertedTool, bool) {
	key := strings.ToLower(strings.TrimSpace(kind))
	key = strings.NewReplacer("-", "_", ".", "_").Replace(key)
	key = strings.TrimPrefix(key, "tool_")
	aliases := map[string]string{
		"probe": "probe",
		"3_175": "tool_3_175",
		"3175":  "tool_3_175",
		"1_8":   "tool_3_175",
		"6":     "tool_6",
		"6_0":   "tool_6",
		"6_35":  "tool_6_35",
		"635":   "tool_6_35",
		"1_4":   "tool_6_35",
	}
	if canonical, ok := aliases[key]; ok {
		key = canonical
	} else if key != "probe" {
		key = "tool_" + key
	}
	for _, tool := range fakeToolPresets {
		if tool.Kind == key {
			return tool, true
		}
	}
	return fakeInsertedTool{}, false
}

func fakeToolPresetByID(toolID int) (fakeInsertedTool, bool) {
	for _, tool := range fakeToolPresets {
		if tool.ToolID == toolID {
			return tool, true
		}
	}
	return fakeInsertedTool{}, false
}

func fakeToolForID(toolID int) (fakeInsertedTool, bool) {
	if tool, ok := fakeToolPresetByID(toolID); ok {
		return tool, true
	}
	if toolID >= 1 && toolID <= 999 {
		return fakeInsertedTool{
			Kind:          "tool_" + itoa(toolID),
			Label:         "Tool " + itoa(toolID),
			ToolID:        toolID,
			DiameterMM:    3.175,
			StickoutMM:    38,
			MinStickoutMM: 10,
			MaxStickoutMM: 70,
		}, true
	}
	return fakeInsertedTool{}, false
}

func (m *FakeMachine) setInsertedToolForIDLocked(toolID int) {
	if tool, ok := fakeToolForID(toolID); ok {
		cp := tool
		cp.SpindleLocked = true
		cp.CalibratedMZ = math.NaN()
		m.insertedTool = &cp
		return
	}
	if toolID < 0 || toolID == 8888 || toolID == 9999 {
		m.insertedTool = nil
	}
}

func (m *FakeMachine) setActiveToolPreserveOffsetLocked(toolID int) {
	_, offset, ok := m.currentToolStatusLocked()
	if !ok || toolID <= 0 || toolID == 8888 || toolID == 9999 {
		offset = 0
	}
	m.setToolStatusLocked(toolID, offset)
}

func (m *FakeMachine) beginToolChangeLocked(toolID int) {
	if toolID < 0 {
		m.insertedTool = nil
		m.setToolStatusLocked(toolID, 0)
		m.setStatusStateLocked("Idle")
		return
	}
	active, offset, ok := m.currentToolStatusLocked()
	if !ok {
		active = 0
		offset = 0
	}
	if active == toolID && m.toolChangeTargetSatisfiedLocked(toolID, offset) {
		return
	}
	m.setToolTargetStatusLocked(active, offset, toolID)
	m.setStatusStateLocked("Tool")
}

func (m *FakeMachine) toolChangeTargetSatisfiedLocked(toolID int, offset float64) bool {
	if toolID == 8888 || toolID == 9999 {
		return m.insertedTool == nil
	}
	if m.insertedTool == nil || !m.insertedTool.Calibrated {
		return false
	}
	if math.Abs(m.insertedTool.CalibratedOffsetMM-offset) > 0.001 {
		return false
	}
	if toolID == 0 {
		return m.insertedTool.Probe && m.insertedTool.ToolID == 0
	}
	return m.insertedTool.ToolID == toolID
}

func (m *FakeMachine) enterToolChangeWaitLocked(toolID int, hasTool bool) {
	active, offset, target, ok := m.currentToolTargetStatusLocked()
	if !ok {
		active, offset, ok = m.currentToolStatusLocked()
		if !ok {
			active = 0
			offset = 0
		}
		target = active
	}
	if hasTool {
		target = toolID
	}
	m.setToolTargetStatusLocked(active, offset, target)
	m.setStatusStateLocked("Tool")
}

func (m *FakeMachine) continueToolChangeLocked() {
	_, _, target, ok := m.currentToolTargetStatusLocked()
	if !ok {
		m.setStatusStateLocked("Idle")
		return
	}
	switch {
	case target < 0:
		m.insertedTool = nil
		m.setToolStatusLocked(target, 0)
	case target == 8888 || target == 9999:
		m.insertedTool = nil
		m.probeLaserActive = target == 9999
		m.setToolStatusLocked(target, 0)
	default:
		if m.insertedTool == nil {
			m.setInsertedToolForIDLocked(target)
		}
		m.setToolStatusLocked(target, 0)
		m.calibrateActiveToolLocked("M6")
	}
	m.setStatusStateLocked("Idle")
}

func (m *FakeMachine) toolChangeWaitingLocked() bool {
	_, state, _, ok := parseFakeStatus(m.status)
	return ok && strings.EqualFold(state, "Tool")
}

func (m *FakeMachine) calibrateActiveToolLocked(command string) {
	active, _, ok := m.currentToolStatusLocked()
	if !ok {
		active = 0
	}
	if m.insertedTool == nil {
		contact := m.currentMPosZLocked()
		m.curToolMZ = contact
		m.setToolStatusLocked(active, contact)
		return
	}

	contact := m.insertedToolCalibrationMZLocked(*m.insertedTool)
	m.curToolMZ = contact
	offset := m.toolOffsetForContactLocked(active, contact, m.insertedTool)
	m.setToolStatusLocked(active, offset)
	m.markInsertedToolCalibratedLocked(contact, offset)
	point := m.currentMPosPointLocked()
	point.Z = contact
	m.lastProbe = &fakeProbeResult{
		Command: command,
		Hit:     true,
		Machine: point,
		Source:  fakeCalibrationSwitchSource,
		At:      time.Now(),
	}
}

func (m *FakeMachine) setToolOffsetFromProbeLocked() {
	active, _, ok := m.currentToolStatusLocked()
	if !ok {
		active = 0
	}
	if m.lastProbe != nil && m.lastProbe.Hit {
		contact := m.lastProbe.Machine.Z
		m.curToolMZ = contact
		offset := m.toolOffsetForContactLocked(active, contact, m.insertedTool)
		m.setToolStatusLocked(active, offset)
		m.markInsertedToolCalibratedLocked(contact, offset)
		return
	}
	if m.insertedTool != nil {
		m.calibrateActiveToolLocked("M493.1")
	}
}

func (m *FakeMachine) toolOffsetForContactLocked(active int, contact float64, tool *fakeInsertedTool) float64 {
	if !fakeFinite(contact) {
		return 0
	}
	if math.IsNaN(m.refToolMZ) {
		switch {
		case active == 0:
			m.refToolMZ = contact
		case tool != nil && tool.Probe:
			m.refToolMZ = contact
		default:
			m.refToolMZ = m.insertedToolCalibrationMZLocked(fakeToolPresets[0])
		}
	}
	if m.refToolMZ < 0 {
		return contact - m.refToolMZ
	}
	return 0
}

func (m *FakeMachine) setReferenceToolMZLocked() {
	switch {
	case fakeFinite(m.curToolMZ):
		m.refToolMZ = m.curToolMZ
	case m.insertedTool != nil:
		m.curToolMZ = m.insertedToolCalibrationMZLocked(*m.insertedTool)
		m.refToolMZ = m.curToolMZ
	default:
		m.curToolMZ = m.currentMPosZLocked()
		m.refToolMZ = m.curToolMZ
	}
	active, _, ok := m.currentToolStatusLocked()
	if !ok {
		active = 0
	}
	m.setToolStatusLocked(active, 0)
	if m.insertedTool != nil && m.insertedTool.Calibrated {
		m.insertedTool.CalibratedOffsetMM = 0
		m.insertedTool.CalibratedMZ = m.refToolMZ
		m.insertedTool.CalibratedAt = time.Now()
	}
}

func (m *FakeMachine) insertedToolCalibrationMZLocked(tool fakeInsertedTool) float64 {
	nominal := tool.StickoutMM
	if preset, ok := fakeToolPresetByID(tool.ToolID); ok && preset.Kind == tool.Kind {
		nominal = preset.StickoutMM
	} else if preset, ok := fakeToolPresetByKind(tool.Kind); ok {
		nominal = preset.StickoutMM
	}
	return m.nominalToolCalibrationMZLocked(tool) + (tool.StickoutMM - nominal)
}

func (m *FakeMachine) nominalToolCalibrationMZLocked(tool fakeInsertedTool) float64 {
	toolrackZ := configFloat(m.config, "coordinate.toolrack_z", -108)
	if m.funcSetting&(1<<3) != 0 {
		if tool.Probe {
			return toolrackZ - 44.5
		}
		return toolrackZ - 4.5
	}
	if tool.Probe {
		return toolrackZ - 40
	}
	return toolrackZ
}

func (m *FakeMachine) snapshotInsertedToolLocked() *SnapshotInsertedTool {
	if m.insertedTool == nil {
		return nil
	}
	tool := *m.insertedTool
	calibratedMZ := tool.CalibratedMZ
	if !fakeFinite(calibratedMZ) {
		calibratedMZ = 0
	}
	return &SnapshotInsertedTool{
		Kind:               tool.Kind,
		Label:              tool.Label,
		ToolID:             tool.ToolID,
		DiameterMM:         tool.DiameterMM,
		StickoutMM:         tool.StickoutMM,
		MinStickoutMM:      tool.MinStickoutMM,
		MaxStickoutMM:      tool.MaxStickoutMM,
		CalibrationMZ:      m.insertedToolCalibrationMZLocked(tool),
		Probe:              tool.Probe,
		SpindleLocked:      tool.SpindleLocked,
		Calibrated:         tool.Calibrated,
		CalibratedAt:       snapshotTimePtr(tool.CalibratedAt),
		CalibratedMZ:       calibratedMZ,
		CalibratedOffsetMM: tool.CalibratedOffsetMM,
	}
}

func (m *FakeMachine) markInsertedToolCalibratedLocked(contact, offset float64) {
	if m.insertedTool == nil {
		return
	}
	m.insertedTool.Calibrated = true
	m.insertedTool.CalibratedAt = time.Now()
	m.insertedTool.CalibratedMZ = contact
	m.insertedTool.CalibratedOffsetMM = offset
}

func (m *FakeMachine) invalidateInsertedToolCalibrationLocked() {
	if m.insertedTool == nil {
		return
	}
	m.insertedTool.Calibrated = false
	m.insertedTool.CalibratedAt = time.Time{}
	m.insertedTool.CalibratedMZ = math.NaN()
	m.insertedTool.CalibratedOffsetMM = 0
	if m.lastProbe != nil && m.lastProbe.Source == fakeCalibrationSwitchSource {
		m.lastProbe = nil
	}
}

func snapshotTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	cp := t
	return &cp
}

func (m *FakeMachine) currentMPosPointLocked() fakeVec3 {
	_, _, fields, ok := parseFakeStatus(m.status)
	if !ok {
		return fakeVec3{}
	}
	if mi := findFakeStatusField(fields, "MPos"); mi >= 0 {
		if vals, ok := parseFakeAxisList(fields[mi].value); ok {
			vals = ensureFakeAxisLen(vals, 3)
			return fakeVec3{X: vals[0], Y: vals[1], Z: vals[2]}
		}
	}
	return fakeVec3{}
}
