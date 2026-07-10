package carveratest

import (
	"sort"
	"strconv"
	"time"

	"github.com/uwin/cnc-proxy/internal/machine"
	"github.com/uwin/cnc-proxy/internal/protocol"
)

// Snapshot is a read-only, JSON-friendly view of the fake machine's current
// observable state. It is intended for test tooling and the standalone
// fakemachine sidecar; it is not part of the production proxy's machine model.
type Snapshot struct {
	ObservedAt         time.Time               `json:"observed_at"`
	Addr               string                  `json:"addr"`
	Status             machine.Status          `json:"status"`
	Modal              SnapshotModal           `json:"modal"`
	Motion             []SnapshotMotionSegment `json:"motion"`
	Program            *SnapshotProgram        `json:"program,omitempty"`
	TransferActive     bool                    `json:"transfer_active"`
	HoldActive         bool                    `json:"hold_active"`
	Files              []SnapshotFile          `json:"files"`
	Dirs               []string                `json:"dirs"`
	Gcodes             []string                `json:"gcodes"`
	Controls           []SnapshotControl       `json:"controls"`
	UploadPacketSizes  []int                   `json:"upload_packet_sizes"`
	Ftype              string                  `json:"ftype"`
	CompressDownloads  bool                    `json:"compress_downloads"`
	DownloadPacketSize int                     `json:"download_packet_size"`
	ProbeLaserActive   bool                    `json:"probe_laser_active"`
	ProbeModel         *SnapshotProbeModel     `json:"probe_model,omitempty"`
	LastProbe          *SnapshotProbeResult    `json:"last_probe,omitempty"`
	InsertedTool       *SnapshotInsertedTool   `json:"inserted_tool,omitempty"`
	Simulation         SnapshotSimulation      `json:"simulation"`
	Config             map[string]string       `json:"config,omitempty"`
	MachineProfile     SnapshotMachineProfile  `json:"machine_profile"`
}

type SnapshotModal struct {
	DistanceMode string  `json:"distance_mode"`
	Units        string  `json:"units"`
	Motion       string  `json:"motion"`
	FeedMMMin    float64 `json:"feed_mm_min"`
}

type SnapshotMotionSegment struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	FromM []float64 `json:"from_m"`
	ToM   []float64 `json:"to_m"`
	FromW []float64 `json:"from_w,omitempty"`
	ToW   []float64 `json:"to_w,omitempty"`
}

type SnapshotProgram struct {
	Path       string        `json:"path"`
	Start      time.Time     `json:"start"`
	End        time.Time     `json:"end"`
	Lines      int           `json:"lines"`
	Percent    int           `json:"percent"`
	Elapsed    time.Duration `json:"elapsed"`
	ElapsedSec int64         `json:"elapsed_sec"`
}

type SnapshotFile struct {
	Path string `json:"path"`
	Size int    `json:"size"`
	MD5  string `json:"md5"`
}

type SnapshotControl struct {
	Byte  byte   `json:"byte"`
	Label string `json:"label"`
}

type SnapshotProbeModel struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Format       string                 `json:"format"`
	Triangles    int                    `json:"triangles"`
	Bounds       fakeBounds             `json:"bounds"`
	SourceBounds fakeBounds             `json:"source_bounds"`
	Placement    SnapshotModelPlacement `json:"placement"`
	LoadedAt     time.Time              `json:"loaded_at"`
}

type SnapshotModelPlacement struct {
	OffsetXMM   float64 `json:"offset_x_mm"`
	OffsetYMM   float64 `json:"offset_y_mm"`
	OffsetZMM   float64 `json:"offset_z_mm"`
	RotationDeg float64 `json:"rotation_deg"`
	XMinMM      float64 `json:"x_min_mm"`
	YMinMM      float64 `json:"y_min_mm"`
	TopZMM      float64 `json:"top_z_mm"`
}

type SnapshotProbeResult struct {
	Command string    `json:"command"`
	Hit     bool      `json:"hit"`
	Machine fakeVec3  `json:"machine"`
	Source  string    `json:"source,omitempty"`
	At      time.Time `json:"at"`
}

type SnapshotInsertedTool struct {
	Kind                 string     `json:"kind"`
	Label                string     `json:"label"`
	ToolID               int        `json:"tool_id"`
	FirmwareToolID       int        `json:"firmware_tool_id"`
	FirmwareTargetToolID *int       `json:"firmware_target_tool_id,omitempty"`
	MatchesFirmwareTool  bool       `json:"matches_firmware_tool"`
	DiameterMM           float64    `json:"diameter_mm"`
	StickoutMM           float64    `json:"stickout_mm"`
	MinStickoutMM        float64    `json:"min_stickout_mm"`
	MaxStickoutMM        float64    `json:"max_stickout_mm"`
	CalibrationMZ        float64    `json:"calibration_mz"`
	Probe                bool       `json:"probe"`
	SpindleLocked        bool       `json:"spindle_locked"`
	Calibrated           bool       `json:"calibrated"`
	CalibratedAt         *time.Time `json:"calibrated_at,omitempty"`
	CalibratedMZ         float64    `json:"calibrated_mz"`
	CalibratedOffsetMM   float64    `json:"calibrated_offset_mm"`
}

type SnapshotMachineProfile struct {
	Model        string  `json:"model"`
	MachineModel int     `json:"machine_model"`
	FuncSetting  int     `json:"func_setting"`
	ProbeAddr    int     `json:"probe_addr"`
	WorkSizeXMM  float64 `json:"worksize_x_mm"`
	WorkSizeYMM  float64 `json:"worksize_y_mm"`
	XMinMM       float64 `json:"x_min_mm"`
	XMaxMM       float64 `json:"x_max_mm"`
	YMinMM       float64 `json:"y_min_mm"`
	YMaxMM       float64 `json:"y_max_mm"`
	ZMinMM       float64 `json:"z_min_mm"`
	ZMaxMM       float64 `json:"z_max_mm"`
	ClearanceX   float64 `json:"clearance_x_mm"`
	ClearanceY   float64 `json:"clearance_y_mm"`
	ClearanceZ   float64 `json:"clearance_z_mm"`
	Anchor1X     float64 `json:"anchor1_x_mm"`
	Anchor1Y     float64 `json:"anchor1_y_mm"`
	Source       string  `json:"source"`
}

// Snapshot returns the fake machine's current state after advancing simulated
// motion/program clocks to now. Slices and maps in the returned value are owned
// by the caller.
func (m *FakeMachine) Snapshot() Snapshot {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	raw := m.statusAtLocked(now)
	st, ok := machine.ParseStatusPayload(raw)
	if !ok {
		st = machine.Status{State: machine.Unknown, Raw: raw}
	}
	st.ObservedAt = now

	files := make([]SnapshotFile, 0, len(m.files))
	for path, content := range m.files {
		files = append(files, SnapshotFile{
			Path: path,
			Size: len(content),
			MD5:  md5hex(content),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	dirs := make([]string, 0, len(m.dirs))
	for dir := range m.dirs {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	controls := make([]SnapshotControl, 0, len(m.controls))
	for _, c := range m.controls {
		label, ok := protocol.ControlLabel(c)
		if !ok {
			label = "0x" + hexByte(c)
		}
		controls = append(controls, SnapshotControl{Byte: c, Label: label})
	}

	ftype := m.ftype
	if ftype == "" {
		ftype = "nc"
	}
	config := cloneStringMap(m.config)

	snap := Snapshot{
		ObservedAt:         now,
		Addr:               m.Addr(),
		Status:             st,
		Modal:              m.snapshotModalLocked(),
		Motion:             m.snapshotMotionLocked(),
		TransferActive:     m.transferActive,
		HoldActive:         m.holdActive,
		Files:              files,
		Dirs:               dirs,
		Gcodes:             append([]string{}, m.gcodes...),
		Controls:           controls,
		UploadPacketSizes:  append([]int{}, m.uploadPacketSizes...),
		Ftype:              ftype,
		CompressDownloads:  m.compressDownloads,
		DownloadPacketSize: m.downloadPacketSize,
		ProbeLaserActive:   m.probeLaserActive,
		ProbeModel:         snapshotProbeModelLocked(m.probeModel),
		LastProbe:          snapshotProbeResultLocked(m.lastProbe),
		InsertedTool:       m.snapshotInsertedToolLocked(),
		Simulation:         m.snapshotSimulationLocked(),
		Config:             config,
		MachineProfile:     m.snapshotMachineProfileLocked(st, config),
	}
	if m.program != nil {
		percent, elapsed := m.programProgressLocked(now)
		snap.Program = &SnapshotProgram{
			Path:       m.program.path,
			Start:      m.program.start,
			End:        m.program.end,
			Lines:      m.program.lines,
			Percent:    percent,
			Elapsed:    elapsed,
			ElapsedSec: int64(elapsed.Seconds()),
		}
	}
	return snap
}

func snapshotProbeModelLocked(model *fakeProbeModel) *SnapshotProbeModel {
	if model == nil {
		return nil
	}
	return &SnapshotProbeModel{
		ID:           model.ID,
		Name:         model.Name,
		Format:       model.Format,
		Triangles:    len(model.Triangles),
		Bounds:       model.Bounds,
		SourceBounds: model.SourceBounds,
		Placement:    snapshotModelPlacement(model),
		LoadedAt:     model.LoadedAt,
	}
}

func snapshotModelPlacement(model *fakeProbeModel) SnapshotModelPlacement {
	if model == nil {
		return SnapshotModelPlacement{}
	}
	return SnapshotModelPlacement{
		OffsetXMM:   model.Placement.Offset.X,
		OffsetYMM:   model.Placement.Offset.Y,
		OffsetZMM:   model.Placement.Offset.Z,
		RotationDeg: model.Placement.RotationDeg,
		XMinMM:      model.Bounds.Min.X,
		YMinMM:      model.Bounds.Min.Y,
		TopZMM:      model.Bounds.Max.Z,
	}
}

func snapshotProbeResultLocked(result *fakeProbeResult) *SnapshotProbeResult {
	if result == nil {
		return nil
	}
	return &SnapshotProbeResult{
		Command: result.Command,
		Hit:     result.Hit,
		Machine: result.Machine,
		Source:  result.Source,
		At:      result.At,
	}
}

func (m *FakeMachine) snapshotModalLocked() SnapshotModal {
	distance := "G91"
	if m.absolute {
		distance = "G90"
	}
	units := "G21"
	if fakeNear(m.unit, 25.4) {
		units = "G20"
	}
	motion := "G0"
	switch m.motionCode {
	case 2:
		motion = "G2"
	case 3:
		motion = "G3"
	case 81:
		motion = "G81"
	case 82:
		motion = "G82"
	case 83:
		motion = "G83"
	case 1:
		motion = "G1"
	}
	return SnapshotModal{
		DistanceMode: distance,
		Units:        units,
		Motion:       motion,
		FeedMMMin:    m.feedMMMin,
	}
}

func (m *FakeMachine) snapshotMotionLocked() []SnapshotMotionSegment {
	out := make([]SnapshotMotionSegment, len(m.motion))
	for i, seg := range m.motion {
		out[i] = SnapshotMotionSegment{
			Start: seg.start,
			End:   seg.end,
			FromM: append([]float64(nil), seg.fromM...),
			ToM:   append([]float64(nil), seg.toM...),
			FromW: append([]float64(nil), seg.fromW...),
			ToW:   append([]float64(nil), seg.toW...),
		}
	}
	return out
}

func (m *FakeMachine) snapshotMachineProfileLocked(st machine.Status, cfg map[string]string) SnapshotMachineProfile {
	machineModel := m.machineModel
	funcSetting := m.funcSetting
	if len(st.Machine) >= 1 {
		machineModel = int(st.Machine[0])
	}
	if len(st.Machine) >= 2 {
		funcSetting = int(st.Machine[1])
	}
	model := m.modelName
	if model == "" || machineModel != m.machineModel {
		model = fakeMachineModelName(machineModel)
	}
	return SnapshotMachineProfile{
		Model:        model,
		MachineModel: machineModel,
		FuncSetting:  funcSetting,
		ProbeAddr:    m.probeAddr,
		WorkSizeXMM:  configFloat(cfg, "coordinate.worksize_x", 300),
		WorkSizeYMM:  configFloat(cfg, "coordinate.worksize_y", 200),
		XMinMM:       configFloat(cfg, "soft_endstop.x_min", -302),
		XMaxMM:       configFloat(cfg, "soft_endstop.x_max", 0),
		YMinMM:       configFloat(cfg, "soft_endstop.y_min", -212),
		YMaxMM:       configFloat(cfg, "soft_endstop.y_max", 0),
		ZMinMM:       configFloat(cfg, "soft_endstop.z_min", -121),
		ZMaxMM:       configFloat(cfg, "soft_endstop.z_max", 0),
		ClearanceX:   configFloat(cfg, "coordinate.clearance_x", -5),
		ClearanceY:   configFloat(cfg, "coordinate.clearance_y", -21),
		ClearanceZ:   configFloat(cfg, "coordinate.clearance_z", -3),
		Anchor1X:     configFloat(cfg, "coordinate.anchor1_x", -287.51),
		Anchor1Y:     configFloat(cfg, "coordinate.anchor1_y", -202.11),
		Source:       "model/status/config-get-all",
	}
}

func configFloat(cfg map[string]string, key string, fallback float64) float64 {
	if cfg == nil {
		return fallback
	}
	f, err := strconv.ParseFloat(cfg[key], 64)
	if err != nil {
		return fallback
	}
	return f
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func hexByte(b byte) string {
	const digits = "0123456789abcdef"
	return string([]byte{digits[b>>4], digits[b&0x0f]})
}
