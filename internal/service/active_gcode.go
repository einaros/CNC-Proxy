package service

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/gcodelog"
	"github.com/uwin/cnc-proxy/internal/machine"
	"github.com/uwin/cnc-proxy/internal/protocol"
	"github.com/uwin/cnc-proxy/internal/store"
)

const (
	maxPreviewSegments = 50000
	previewArcSegment  = 0.5
	previewArcError    = 0.01

	activeGcodeProbeInterval = 5 * time.Second
)

type activeGcodeState struct {
	Path       string
	Preview    GcodePreview
	SelectedAt time.Time
}

// ActiveGcode is the currently selected file and its cached preview.
type ActiveGcode struct {
	Path      string        `json:"path,omitempty"`
	Entry     *store.Entry  `json:"entry,omitempty"`
	Preview   *GcodePreview `json:"preview,omitempty"`
	Runnable  bool          `json:"runnable"`
	Message   string        `json:"message,omitempty"`
	UpdatedAt time.Time     `json:"updated_at,omitempty"`
}

// GcodePreview is a bounded 3D/4-axis toolpath summary suitable for the web UI.
type GcodePreview struct {
	LineCount       int            `json:"line_count"`
	MoveCount       int            `json:"move_count"`
	PlottedSegments int            `json:"plotted_segments"`
	Truncated       bool           `json:"truncated"`
	TotalDistance   float64        `json:"total_distance"`
	Has4Axis        bool           `json:"has_4axis"`
	Bounds          *GcodeBounds   `json:"bounds,omitempty"`
	Tools           []int          `json:"tools,omitempty"`
	Segments        []GcodeSegment `json:"segments,omitempty"`
}

type GcodeBounds struct {
	Min  [3]float64 `json:"min"`
	Max  [3]float64 `json:"max"`
	MinA float64    `json:"min_a,omitempty"`
	MaxA float64    `json:"max_a,omitempty"`
}

type GcodeSegment struct {
	Kind          string     `json:"kind"`
	Line          int        `json:"line"`
	Tool          int        `json:"tool,omitempty"`
	From          [4]float64 `json:"from"`
	To            [4]float64 `json:"to"`
	DistanceStart float64    `json:"distance_start"`
	DistanceEnd   float64    `json:"distance_end"`
}

// MachineActionResult is returned by synchronous machine-action endpoints.
type MachineActionResult struct {
	Action   string `json:"action"`
	Path     string `json:"path,omitempty"`
	ToolID   int    `json:"tool_id,omitempty"`
	Command  string `json:"command,omitempty"`
	Output   string `json:"output,omitempty"`
	Message  string `json:"message"`
	Verified bool   `json:"verified"`
}

// ActiveGcode returns the current proxy-side file selection.
func (s *Service) ActiveGcode() ActiveGcode {
	s.activeMu.Lock()
	active := s.activeGcode
	s.activeMu.Unlock()
	storedPath := s.store.ActiveGcodePath()
	if storedPath == "" {
		if active.Path != "" {
			s.clearActiveGcode(active.Path)
		}
		return ActiveGcode{Message: "No active gcode selected."}
	}
	if active.Path == "" || active.Path != storedPath {
		return s.activeGcodeFromStoredPath(storedPath)
	}
	entry, ok := s.store.GetEntry(active.Path)
	if !ok {
		s.clearActiveGcode(active.Path)
		return ActiveGcode{Message: "No active gcode selected."}
	}
	return s.activeGcodeSnapshot(active, entry)
}

func (s *Service) activeGcodeFromStoredPath(remotePath string) ActiveGcode {
	entry, ok := s.store.GetEntry(remotePath)
	if !ok {
		s.clearActiveGcode(remotePath)
		return ActiveGcode{Message: "No active gcode selected."}
	}
	if !entry.IsDir {
		rc, cacheEntry, err := s.ReadCache(remotePath)
		if err == nil {
			defer rc.Close()
			preview, err := ParseGcodePreview(rc)
			if err == nil {
				active := activeGcodeState{Path: cacheEntry.Path, Preview: preview, SelectedAt: time.Now()}
				s.activeMu.Lock()
				if s.activeGcode.Path == "" || s.activeGcode.Path == active.Path {
					s.activeGcode = active
				}
				s.activeMu.Unlock()
				return s.activeGcodeSnapshot(active, cacheEntry)
			}
		}
	}
	entryCopy := entry
	runnable, message := runnableGcode(entry)
	return ActiveGcode{
		Path:      entry.Path,
		Entry:     &entryCopy,
		Runnable:  runnable,
		Message:   message,
		UpdatedAt: time.Time{},
	}
}

func (s *Service) clearActiveGcode(remotePath string) {
	s.activeMu.Lock()
	if s.activeGcode.Path == remotePath {
		s.activeGcode = activeGcodeState{}
	}
	s.activeMu.Unlock()
	if s.store.ActiveGcodePath() == remotePath {
		_ = s.store.SetActiveGcodePath("")
	}
}

// SelectActiveGcode selects a catalog file and parses a preview from its local
// cache, fetching remote-only files through the existing download-on-demand path.
func (s *Service) SelectActiveGcode(remotePath string) (ActiveGcode, error) {
	rc, entry, err := s.Open(remotePath)
	if err != nil {
		return ActiveGcode{}, err
	}
	defer rc.Close()
	if entry.IsDir {
		return ActiveGcode{}, fmt.Errorf("%w: active gcode must be a file", ErrInvalidArgument)
	}
	preview, err := ParseGcodePreview(rc)
	if err != nil {
		return ActiveGcode{}, err
	}
	active := activeGcodeState{Path: entry.Path, Preview: preview, SelectedAt: time.Now()}
	if err := s.store.SetActiveGcodePath(entry.Path); err != nil {
		return ActiveGcode{}, err
	}
	s.activeMu.Lock()
	s.activeGcode = active
	s.activeMu.Unlock()
	return s.activeGcodeSnapshot(active, entry), nil
}

func (s *Service) activeGcodeSnapshot(active activeGcodeState, entry store.Entry) ActiveGcode {
	entryCopy := entry
	previewCopy := copyPreview(active.Preview)
	runnable, message := runnableGcode(entry)
	return ActiveGcode{
		Path:      active.Path,
		Entry:     &entryCopy,
		Preview:   &previewCopy,
		Runnable:  runnable,
		Message:   message,
		UpdatedAt: active.SelectedAt,
	}
}

func (s *Service) maybeLoadActiveGcodeFromMachine(st machine.Status) {
	// Ignore synthetic state-only observations used by tests. Real machine status
	// comes through ParseStatusPayload and keeps Raw populated.
	if st.Raw == "" {
		return
	}
	if !stateMayReportActiveGcode(st.State) {
		s.activeProbeMu.Lock()
		s.activeProbeLoaded = false
		s.activeProbeLast = time.Time{}
		s.activeProbeMu.Unlock()
		return
	}
	if len(st.Progress) < 3 {
		return
	}

	now := time.Now()
	s.activeProbeMu.Lock()
	if s.activeProbeLoaded || s.activeProbeInFlight || (!s.activeProbeLast.IsZero() && now.Sub(s.activeProbeLast) < activeGcodeProbeInterval) {
		s.activeProbeMu.Unlock()
		return
	}
	s.activeProbeInFlight = true
	s.activeProbeLast = now
	s.activeProbeMu.Unlock()

	go s.loadActiveGcodeFromMachineProgress()
}

func stateMayReportActiveGcode(st machine.State) bool {
	switch st {
	case machine.Run, machine.Hold, machine.Pause, machine.Wait, machine.Tool:
		return true
	default:
		return false
	}
}

func (s *Service) loadActiveGcodeFromMachineProgress() {
	loaded := false
	defer func() {
		s.activeProbeMu.Lock()
		s.activeProbeInFlight = false
		if loaded {
			s.activeProbeLoaded = true
		}
		s.activeProbeMu.Unlock()
	}()

	remote, ok, err := s.queryMachineActiveGcode()
	if err != nil || !ok {
		return
	}
	if err := s.setMachineReportedActiveGcode(remote); err != nil {
		return
	}
	loaded = true
}

func (s *Service) queryMachineActiveGcode() (string, bool, error) {
	var out string
	err := s.arb.WithMachine(false, func(c *client.Conn) error {
		o, e := c.SendConsoleCommand("progress\n", client.GcodeOpts{
			ExpectReply: true,
			Cap:         gcodeReplyCap,
		})
		out = o
		return e
	})
	if err != nil {
		return "", false, err
	}
	remote, ok := parseMachineProgressGcodePath(out)
	return remote, ok, nil
}

func parseMachineProgressGcodePath(out string) (string, bool) {
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(protocol.Unescape(scanner.Text()))
		if len(line) < len("file: ") || !strings.EqualFold(line[:len("file: ")], "file: ") {
			continue
		}
		rest := strings.TrimSpace(line[len("file: "):])
		name, _, _ := strings.Cut(rest, ",")
		remote, err := normalizeRemote(strings.TrimSpace(name))
		if err != nil {
			return "", false
		}
		return remote, true
	}
	return "", false
}

func (s *Service) setMachineReportedActiveGcode(remotePath string) error {
	remote, err := normalizeRemote(remotePath)
	if err != nil {
		return err
	}
	return s.store.Batch(func(b *store.Batch) error {
		if _, ok := b.GetEntry(remote); !ok {
			b.PutEntry(store.Entry{
				Path:       remote,
				Sync:       store.RemoteOnly,
				CacheState: store.CacheNone,
			})
		}
		b.SetActiveGcodePath(remote)
		return nil
	})
}

func runnableGcode(entry store.Entry) (bool, string) {
	if entry.IsDir {
		return false, "Active gcode is a directory."
	}
	switch entry.Sync {
	case store.Synced, store.RemoteOnly:
		return true, ""
	case store.PendingUpload, store.Uploading:
		return false, "Waiting for upload sync before this file can run."
	case store.PendingDelete, store.Deleting:
		return false, "This file is queued for deletion."
	case store.Error:
		return false, "Resolve the file sync error before running."
	default:
		return false, "This file is not synced to the machine."
	}
}

func copyPreview(in GcodePreview) GcodePreview {
	out := in
	if in.Bounds != nil {
		b := *in.Bounds
		out.Bounds = &b
	}
	out.Tools = append([]int(nil), in.Tools...)
	out.Segments = append([]GcodeSegment(nil), in.Segments...)
	return out
}

// RunActiveGcode sends the same controller-compatible `play <path>` command
// that the official UI sends for its selected remote file.
func (s *Service) RunActiveGcode() (MachineActionResult, error) {
	path := s.store.ActiveGcodePath()
	if path == "" {
		s.activeMu.Lock()
		path = s.activeGcode.Path
		s.activeMu.Unlock()
	}
	if path == "" {
		return MachineActionResult{Action: "run_gcode", Message: ErrNoActiveGcode.Error()}, ErrNoActiveGcode
	}
	entry, ok := s.store.GetEntry(path)
	if !ok {
		return MachineActionResult{Action: "run_gcode", Path: path, Message: ErrNotFound.Error()}, ErrNotFound
	}
	if runnable, message := runnableGcode(entry); !runnable {
		return MachineActionResult{Action: "run_gcode", Path: path, Message: message}, ErrActiveGcodeUnavailable
	}
	display := "play " + path
	out, err := s.sendConsoleMachineAction(display, protocol.PlayLine(path))
	res := MachineActionResult{
		Action:  "run_gcode",
		Path:    path,
		Command: display,
		Output:  out,
		Message: "Run command sent for " + path + "; machine confirmation was not available.",
	}
	if err != nil {
		res.Message = err.Error()
		return res, err
	}
	return res, nil
}

func validCurrentToolID(toolID int) bool {
	return toolID == -1 || toolID == 0 || toolID == 8888 || (toolID >= 1 && toolID <= 999)
}

func validChangeToolID(toolID int) bool {
	return toolID == 0 || toolID == 8888 || (toolID >= 1 && toolID <= 999)
}

func toolDisplayName(toolID int) string {
	switch toolID {
	case -1:
		return "Empty"
	case 0:
		return "Probe"
	case 8888:
		return "Laser"
	default:
		return fmt.Sprintf("Tool %d", toolID)
	}
}

// SetCurrentToolID mirrors the controller's M493.2T<n> action for manually
// declaring which tool is currently installed.
func (s *Service) SetCurrentToolID(toolID int) (MachineActionResult, error) {
	if !validCurrentToolID(toolID) {
		err := fmt.Errorf("%w: tool_id must be Empty (-1), Probe (0), Laser (8888), or between 1 and 999", ErrInvalidArgument)
		return MachineActionResult{Action: "set_tool", ToolID: toolID, Message: err.Error()}, err
	}
	display := strings.TrimSpace(protocol.SetCurrentToolLine(toolID))
	out, err := s.sendToolMachineAction(display, protocol.SetCurrentToolLine(toolID))
	res := MachineActionResult{
		Action:  "set_tool",
		ToolID:  toolID,
		Command: display,
		Output:  out,
		Message: fmt.Sprintf("Set-tool command sent for %s; machine confirmation was not available.", toolDisplayName(toolID)),
	}
	if err != nil {
		res.Message = err.Error()
		return res, err
	}
	return res, nil
}

// ChangeTool mirrors the controller's M6T<n> action for changing to a selected
// tool with the firmware's normal tool-change flow.
func (s *Service) ChangeTool(toolID int) (MachineActionResult, error) {
	if !validChangeToolID(toolID) {
		err := fmt.Errorf("%w: tool_id must be Probe (0), Laser (8888), or between 1 and 999", ErrInvalidArgument)
		return MachineActionResult{Action: "change_tool", ToolID: toolID, Message: err.Error()}, err
	}
	display := strings.TrimSpace(protocol.ChangeToolLine(toolID))
	out, err := s.sendToolMachineAction(display, protocol.ChangeToolLine(toolID))
	res := MachineActionResult{
		Action:  "change_tool",
		ToolID:  toolID,
		Command: display,
		Output:  out,
		Message: fmt.Sprintf("Change-tool command sent for %s; machine confirmation was not available.", toolDisplayName(toolID)),
	}
	if err != nil {
		res.Message = err.Error()
		return res, err
	}
	return res, nil
}

// ContinueToolChange mirrors the controller's M490.2 action, which clears the
// firmware's manual tool-change waiting state after the operator confirms.
func (s *Service) ContinueToolChange() (MachineActionResult, error) {
	display := strings.TrimSpace(protocol.ContinueToolChangeLine())
	out, err := s.sendToolContinueAction(display, protocol.ContinueToolChangeLine())
	res := MachineActionResult{
		Action:  "continue_tool_change",
		Command: display,
		Output:  out,
		Message: "Tool-change continue command sent; machine confirmation was not available.",
	}
	if err != nil {
		res.Message = err.Error()
		return res, err
	}
	return res, nil
}

// DropCurrentTool mirrors the controller's M6T-1 drop-tool action.
func (s *Service) DropCurrentTool() (MachineActionResult, error) {
	display := strings.TrimSpace(protocol.ChangeToolLine(-1))
	out, err := s.sendToolMachineAction(display, protocol.ChangeToolLine(-1))
	res := MachineActionResult{
		Action:  "drop_tool",
		ToolID:  -1,
		Command: display,
		Output:  out,
		Message: "Drop-tool command sent; machine confirmation was not available.",
	}
	if err != nil {
		res.Message = err.Error()
		return res, err
	}
	return res, nil
}

// CalibrateCurrentTool mirrors the controller's M491 current-tool calibration.
func (s *Service) CalibrateCurrentTool() (MachineActionResult, error) {
	display := strings.TrimSpace(protocol.CalibrateCurrentToolLine())
	out, err := s.sendToolMachineAction(display, protocol.CalibrateCurrentToolLine())
	res := MachineActionResult{
		Action:  "calibrate_tool",
		Command: display,
		Output:  out,
		Message: "Calibration command sent; machine confirmation was not available.",
	}
	if err != nil {
		res.Message = err.Error()
		return res, err
	}
	return res, nil
}

// AutoZProbe mirrors the official controller's M495 auto Z probe action. The
// proxy uses the current work XY as the probe point and zero X/Y probe offsets.
func (s *Service) AutoZProbe() (MachineActionResult, error) {
	res := MachineActionResult{Action: "auto_z_probe"}
	var out string
	var display string
	err := s.arb.WithMachine(true, func(c *client.Conn) error {
		st, err := s.queryRecoveryStatus(c)
		if err != nil {
			if errors.Is(err, ErrMachineStatusStale) {
				return err
			}
			return fmt.Errorf("%w: %v", ErrMachineStatusStale, err)
		}
		if st.State != machine.Idle {
			return fmt.Errorf("%w: machine reports %s", ErrMachineStatusStale, statusSummary(st))
		}
		workX, workY, ok := currentWorkXY(st)
		if !ok {
			return fmt.Errorf("%w: current work XY is unavailable", ErrProbeUnavailable)
		}
		wire := protocol.AutoZProbeLine(workX, workY, 0, 0)
		display = strings.TrimSpace(wire)
		s.gcodeLog.Append(gcodelog.DirSend, gcodelog.SourceAPI, display)
		o, e := c.SendConsoleCommand(wire, client.GcodeOpts{Cap: gcodeReplyCap})
		out = o
		if e != nil {
			return e
		}
		s.refreshStatusBestEffort(c)
		return nil
	})
	res.Command = display
	res.Output = out
	res.Message = "Auto Z probe command sent; machine confirmation was not available."
	if out != "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, out)
	}
	if err != nil {
		res.Message = err.Error()
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "error: "+err.Error())
		return res, err
	}
	if out == "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "sent: no reply observed")
	}
	return res, nil
}

func currentWorkXY(st machine.Status) (float64, float64, bool) {
	x, okX := finiteAxisValue(st.WPos, "x")
	y, okY := finiteAxisValue(st.WPos, "y")
	return x, y, okX && okY
}

func finiteAxisValue(values machine.AxisValues, axis string) (float64, bool) {
	if values == nil {
		return 0, false
	}
	v, ok := values[strings.ToLower(axis)]
	return v, ok && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func (s *Service) sendConsoleMachineAction(displayLine, wireLine string) (string, error) {
	s.gcodeLog.Append(gcodelog.DirSend, gcodelog.SourceAPI, displayLine)
	var out string
	err := s.arb.WithMachine(true, func(c *client.Conn) error {
		o, e := c.SendConsoleCommand(ensureWireLine(wireLine), client.GcodeOpts{Cap: gcodeReplyCap})
		out = o
		return e
	})
	if out != "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, out)
	}
	if err != nil {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "error: "+err.Error())
	} else if out == "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "sent: no reply observed")
	}
	return out, err
}

func (s *Service) sendToolMachineAction(displayLine, wireLine string) (string, error) {
	s.gcodeLog.Append(gcodelog.DirSend, gcodelog.SourceAPI, displayLine)
	var out string
	err := s.arb.WithMachine(true, func(c *client.Conn) error {
		o, e := c.SendConsoleCommand(ensureWireLine(wireLine), client.GcodeOpts{Cap: gcodeReplyCap})
		out = o
		if e != nil {
			return e
		}
		s.refreshStatusBestEffort(c)
		return nil
	})
	if out != "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, out)
	}
	if err != nil {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "error: "+err.Error())
	} else if out == "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "sent: no reply observed")
	}
	return out, err
}

func (s *Service) sendToolContinueAction(displayLine, wireLine string) (string, error) {
	s.gcodeLog.Append(gcodelog.DirSend, gcodelog.SourceAPI, displayLine)
	var out string
	err := s.arb.WithMachine(false, func(c *client.Conn) error {
		st, err := s.queryRecoveryStatus(c)
		if err != nil {
			if errors.Is(err, ErrMachineStatusStale) {
				return err
			}
			return fmt.Errorf("%w: %v", ErrMachineStatusStale, err)
		}
		if st.State != machine.Tool {
			return fmt.Errorf("%w: machine reports %s", ErrToolChangeUnavailable, statusSummary(st))
		}
		o, e := c.SendConsoleCommand(ensureWireLine(wireLine), client.GcodeOpts{Cap: gcodeReplyCap})
		out = o
		if e != nil {
			return e
		}
		s.refreshStatusBestEffort(c)
		return nil
	})
	if out != "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, out)
	}
	if err != nil {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "error: "+err.Error())
	} else if out == "" {
		s.gcodeLog.Append(gcodelog.DirRecv, gcodelog.SourceAPI, "sent: no reply observed")
	}
	return out, err
}

func (s *Service) refreshStatusBestEffort(c *client.Conn) {
	_, _ = s.queryRecoveryStatus(c)
}

func ensureWireLine(line string) string {
	if line == "" || line[len(line)-1] == '\n' {
		return line
	}
	return line + "\n"
}

type previewParser struct {
	unit                  float64
	absolute              bool
	arcAbsolute           bool
	motion                int
	plane                 int
	pos                   [4]float64
	currentTool           int
	tools                 map[int]bool
	preview               GcodePreview
	bounds                GcodeBounds
	haveBounds            bool
	cycleStarted          bool
	cycleRetractToInitial bool
	cycleInitialZ         float64
	cycleSticky           cycleSticky
}

type gword struct {
	letter byte
	value  float64
}

type cycleSticky struct {
	z float64
	r float64
	f float64
	q float64
	p float64
}

const (
	previewPlaneXY = iota
	previewPlaneXZ
	previewPlaneYZ
)

// ParseGcodePreview scans the Carvera-supported explicit motion surface into a
// bounded segment list: G0/G1/G2/G3, G38.2-G38.6, G17-G19 planes, G90/G91,
// G90.1/G91.1 arc centers, inch/mm units, A-axis moves, G92 coordinate resets,
// and firmware-supported G80-G83/G98/G99 drilling cycles.
func ParseGcodePreview(r io.Reader) (GcodePreview, error) {
	p := previewParser{
		unit:     1,
		absolute: true,
		motion:   -1,
		tools:    map[int]bool{},
	}
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			p.preview.LineCount++
			p.parseLine(line, p.preview.LineCount)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return GcodePreview{}, err
		}
	}
	if p.haveBounds {
		b := p.bounds
		p.preview.Bounds = &b
	}
	for tool := range p.tools {
		p.preview.Tools = append(p.preview.Tools, tool)
	}
	sort.Ints(p.preview.Tools)
	return p.preview, nil
}

func (p *previewParser) parseLine(line string, lineNo int) {
	words := parseGcodeWords(stripGcodeComments(line))
	if len(words) == 0 {
		return
	}
	values := map[byte]float64{}
	hasValue := map[byte]bool{}
	hasAxis := false
	lineMotion := -1
	cycleCode := 0
	setPosition := false
	for _, w := range words {
		switch w.letter {
		case 'G':
			code, subcode := splitGCode(w.value)
			switch code {
			case 0, 1, 2, 3:
				lineMotion = code
				p.motion = code
			case 17:
				p.plane = previewPlaneXY
			case 18:
				p.plane = previewPlaneXZ
			case 19:
				p.plane = previewPlaneYZ
			case 20:
				p.unit = 25.4
			case 21:
				p.unit = 1
			case 38:
				if subcode >= 2 && subcode <= 6 {
					lineMotion = 38
				}
			case 80:
				p.endCycle(lineNo)
			case 81, 82, 83:
				cycleCode = code
			case 90:
				if subcode == 1 {
					p.arcAbsolute = true
				} else {
					p.absolute = true
				}
			case 91:
				if subcode == 1 {
					p.arcAbsolute = false
				} else {
					p.absolute = false
				}
			case 92:
				setPosition = true
			case 98:
				p.startCycle(true)
			case 99:
				p.startCycle(false)
			}
		case 'T':
			tool := int(math.Round(w.value))
			p.currentTool = tool
			p.tools[tool] = true
		case 'M':
			if int(math.Round(w.value)) == 321 {
				p.currentTool = 7
				p.tools[7] = true
			}
		case 'X', 'Y', 'Z', 'I', 'J', 'K', 'R', 'Q', 'F':
			values[w.letter] = w.value * p.unit
			hasValue[w.letter] = true
			if w.letter == 'X' || w.letter == 'Y' || w.letter == 'Z' {
				hasAxis = true
			}
		case 'A':
			values[w.letter] = -w.value
			hasValue[w.letter] = true
			hasAxis = true
			p.preview.Has4Axis = true
		case 'L', 'P', 'S':
			values[w.letter] = w.value
			hasValue[w.letter] = true
		}
	}
	if setPosition {
		p.setPosition(values, hasValue)
		return
	}
	if cycleCode != 0 {
		p.runCycle(cycleCode, values, hasValue, lineNo)
		return
	}
	if lineMotion < 0 {
		lineMotion = p.motion
	}
	hasArcCenter := hasValue['I'] || hasValue['J'] || hasValue['K'] || hasValue['R']
	if lineMotion < 0 {
		return
	}
	if !hasAxis && !(hasArcCenter && (lineMotion == 2 || lineMotion == 3)) {
		return
	}
	target := p.targetFromValues(values, hasValue)
	switch lineMotion {
	case 0:
		p.addLinearMove("rapid", lineNo, target)
	case 1:
		p.addLinearMove("cut", lineNo, target)
	case 2, 3:
		p.addArcMove(lineMotion == 2, lineNo, target, values, hasValue)
	case 38:
		p.addLinearMove("probe", lineNo, target)
	}
	p.pos = target
}

func splitGCode(v float64) (int, int) {
	code := int(math.Trunc(v))
	subcode := int(math.Round((v - float64(code)) * 10))
	return code, subcode
}

func (p *previewParser) targetFromValues(values map[byte]float64, hasValue map[byte]bool) [4]float64 {
	target := p.pos
	for _, axis := range []struct {
		letter byte
		index  int
	}{
		{'X', 0},
		{'Y', 1},
		{'Z', 2},
		{'A', 3},
	} {
		if !hasValue[axis.letter] {
			continue
		}
		v := values[axis.letter]
		if p.absolute {
			target[axis.index] = v
		} else {
			target[axis.index] += v
		}
	}
	return target
}

func (p *previewParser) setPosition(values map[byte]float64, hasValue map[byte]bool) {
	if len(hasValue) == 0 {
		p.pos = [4]float64{}
		return
	}
	for _, axis := range []struct {
		letter byte
		index  int
	}{
		{'X', 0},
		{'Y', 1},
		{'Z', 2},
		{'A', 3},
	} {
		if hasValue[axis.letter] {
			p.pos[axis.index] = values[axis.letter]
		}
	}
}

func (p *previewParser) startCycle(retractToInitial bool) {
	p.cycleStarted = true
	p.cycleRetractToInitial = retractToInitial
	p.cycleInitialZ = p.pos[2]
	p.cycleSticky = cycleSticky{}
}

func (p *previewParser) endCycle(lineNo int) {
	if p.cycleStarted && !p.cycleRetractToInitial {
		target := p.pos
		target[2] = p.cycleInitialZ
		p.addLinearMove("rapid", lineNo, target)
		p.pos = target
	}
	p.cycleStarted = false
}

func (p *previewParser) runCycle(code int, values map[byte]float64, hasValue map[byte]bool, lineNo int) {
	if !p.cycleStarted || !p.absolute {
		return
	}
	p.updateCycleSticky(values, hasValue)
	xy := p.pos
	if hasValue['X'] {
		xy[0] = values['X']
	}
	if hasValue['Y'] {
		xy[1] = values['Y']
	}
	p.addLinearMove("rapid", lineNo, xy)
	p.pos = xy

	retract := p.pos
	retract[2] = p.cycleSticky.r
	p.addLinearMove("rapid", lineNo, retract)
	p.pos = retract

	if code == 83 && p.cycleSticky.q > 0 {
		p.runPeckCycle(lineNo)
	} else {
		drill := p.pos
		drill[2] = p.cycleSticky.z
		p.addLinearMove("cut", lineNo, drill)
		p.pos = drill
	}

	finalRetract := p.pos
	if p.cycleRetractToInitial {
		finalRetract[2] = p.cycleInitialZ
	} else {
		finalRetract[2] = p.cycleSticky.r
	}
	p.addLinearMove("rapid", lineNo, finalRetract)
	p.pos = finalRetract
}

func (p *previewParser) updateCycleSticky(values map[byte]float64, hasValue map[byte]bool) {
	if hasValue['Z'] {
		p.cycleSticky.z = values['Z']
	}
	if hasValue['R'] {
		p.cycleSticky.r = values['R']
	}
	if hasValue['F'] {
		p.cycleSticky.f = values['F']
	}
	if hasValue['Q'] {
		p.cycleSticky.q = values['Q']
	}
	if hasValue['P'] {
		p.cycleSticky.p = values['P']
	}
}

func (p *previewParser) runPeckCycle(lineNo int) {
	if p.cycleSticky.q <= 0 {
		return
	}
	for z := p.cycleSticky.r - p.cycleSticky.q; z > p.cycleSticky.z; z -= p.cycleSticky.q {
		drill := p.pos
		drill[2] = z
		p.addLinearMove("cut", lineNo, drill)
		p.pos = drill
		retract := p.pos
		retract[2] = p.cycleSticky.r
		p.addLinearMove("rapid", lineNo, retract)
		p.pos = retract
	}
	drill := p.pos
	drill[2] = p.cycleSticky.z
	p.addLinearMove("cut", lineNo, drill)
	p.pos = drill
}

func (p *previewParser) addLinearMove(kind string, lineNo int, target [4]float64) {
	if samePreviewPoint(p.pos, target) {
		return
	}
	p.preview.MoveCount++
	p.addSegment(GcodeSegment{Kind: kind, Line: lineNo, Tool: p.currentTool, From: p.pos, To: target})
}

func (p *previewParser) addArcMove(clockwise bool, lineNo int, target [4]float64, values map[byte]float64, hasValue map[byte]bool) {
	offset, ok := p.arcOffset(clockwise, target, values, hasValue)
	if !ok {
		p.addLinearMove("arc", lineNo, target)
		return
	}
	u, v, w := p.planeAxes()
	start := p.pos
	radius := math.Hypot(offset[u], offset[v])
	if radius <= 0.000001 {
		p.addLinearMove("arc", lineNo, target)
		return
	}
	centerU := start[u] + offset[u]
	centerV := start[v] + offset[v]
	r0U := -offset[u]
	r0V := -offset[v]
	rtU := target[u] - centerU
	rtV := target[v] - centerV
	angularTravel := 0.0
	if nearlyEqual(start[u], target[u]) && nearlyEqual(start[v], target[v]) {
		if clockwise {
			angularTravel = -2 * math.Pi
		} else {
			angularTravel = 2 * math.Pi
		}
	} else {
		angularTravel = math.Atan2(r0U*rtV-r0V*rtU, r0U*rtU+r0V*rtV)
		effectiveClockwise := clockwise
		if w == 1 {
			effectiveClockwise = !effectiveClockwise
		}
		if effectiveClockwise {
			if angularTravel > 0 {
				angularTravel -= 2 * math.Pi
			}
		} else if angularTravel < 0 {
			angularTravel += 2 * math.Pi
		}
	}
	travel := math.Hypot(angularTravel*radius, math.Abs(target[w]-start[w]))
	if travel <= 0.000001 && nearlyEqual(start[3], target[3]) {
		return
	}
	arcSegment := previewArcSegment
	if previewArcError > 0 && 2*radius > previewArcError {
		minErrSegment := 2 * math.Sqrt(previewArcError*(2*radius-previewArcError))
		if arcSegment < minErrSegment {
			arcSegment = minErrSegment
		}
	}
	if arcSegment < 0.0001 {
		arcSegment = 0.5
	}
	segments := int(math.Floor(travel / arcSegment))
	if segments < 1 {
		segments = 1
	}
	p.preview.MoveCount++
	startAngle := math.Atan2(start[v]-centerV, start[u]-centerU)
	prev := start
	for i := 1; i <= segments; i++ {
		t := float64(i) / float64(segments)
		next := start
		angle := startAngle + angularTravel*t
		next[u] = centerU + radius*math.Cos(angle)
		next[v] = centerV + radius*math.Sin(angle)
		next[w] = start[w] + (target[w]-start[w])*t
		next[3] = start[3] + (target[3]-start[3])*t
		if i == segments {
			next = target
		}
		if !samePreviewPoint(prev, next) {
			p.addSegment(GcodeSegment{Kind: "arc", Line: lineNo, Tool: p.currentTool, From: prev, To: next})
		}
		prev = next
	}
}

func (p *previewParser) arcOffset(clockwise bool, target [4]float64, values map[byte]float64, hasValue map[byte]bool) ([3]float64, bool) {
	var offset [3]float64
	if hasValue['R'] {
		return p.arcOffsetFromRadius(clockwise, target, values['R'])
	}
	seen := false
	for _, word := range []struct {
		letter byte
		axis   int
	}{
		{'I', 0},
		{'J', 1},
		{'K', 2},
	} {
		if !hasValue[word.letter] {
			continue
		}
		seen = true
		if p.arcAbsolute {
			offset[word.axis] = values[word.letter] - p.pos[word.axis]
		} else {
			offset[word.axis] = values[word.letter]
		}
	}
	return offset, seen
}

func (p *previewParser) arcOffsetFromRadius(clockwise bool, target [4]float64, radiusWord float64) ([3]float64, bool) {
	var offset [3]float64
	u, v, _ := p.planeAxes()
	startU := p.pos[u]
	startV := p.pos[v]
	targetU := target[u]
	targetV := target[v]
	du := targetU - startU
	dv := targetV - startV
	chord := math.Hypot(du, dv)
	if chord <= 0.000001 {
		return offset, false
	}
	radius := math.Abs(radiusWord)
	if radius <= 0.000001 {
		return offset, false
	}
	halfChord := chord / 2
	if radius < halfChord {
		radius = halfChord
	}
	oc := math.Sqrt(math.Max(radius*radius-halfChord*halfChord, 0))
	if clockwise {
		oc = -oc
	}
	if radiusWord < 0 {
		oc = -oc
	}
	centerU := 0.5*(startU+targetU) - oc*dv/chord
	centerV := 0.5*(startV+targetV) + oc*du/chord
	offset[u] = centerU - startU
	offset[v] = centerV - startV
	return offset, true
}

func (p *previewParser) planeAxes() (int, int, int) {
	switch p.plane {
	case previewPlaneXZ:
		return 0, 2, 1
	case previewPlaneYZ:
		return 1, 2, 0
	default:
		return 0, 1, 2
	}
}

func samePreviewPoint(a, b [4]float64) bool {
	for i := 0; i < len(a); i++ {
		if !nearlyEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func nearlyEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.0000001
}

func previewSegmentDistance(from, to [4]float64) float64 {
	dx := to[0] - from[0]
	dy := to[1] - from[1]
	dz := to[2] - from[2]
	d := math.Sqrt(dx*dx + dy*dy + dz*dz)
	if d <= 0.0000001 {
		d = math.Abs(to[3] - from[3])
	}
	return d
}

func (p *previewParser) addSegment(seg GcodeSegment) {
	d := previewSegmentDistance(seg.From, seg.To)
	seg.DistanceStart = p.preview.TotalDistance
	p.preview.TotalDistance += d
	seg.DistanceEnd = p.preview.TotalDistance
	p.includeBounds(seg.From)
	p.includeBounds(seg.To)
	if len(p.preview.Segments) >= maxPreviewSegments {
		p.preview.Truncated = true
		return
	}
	p.preview.Segments = append(p.preview.Segments, seg)
	p.preview.PlottedSegments = len(p.preview.Segments)
}

func (p *previewParser) includeBounds(pos [4]float64) {
	if !p.haveBounds {
		p.bounds.Min = [3]float64{pos[0], pos[1], pos[2]}
		p.bounds.Max = [3]float64{pos[0], pos[1], pos[2]}
		p.bounds.MinA = pos[3]
		p.bounds.MaxA = pos[3]
		p.haveBounds = true
		return
	}
	for i := 0; i < 3; i++ {
		if pos[i] < p.bounds.Min[i] {
			p.bounds.Min[i] = pos[i]
		}
		if pos[i] > p.bounds.Max[i] {
			p.bounds.Max[i] = pos[i]
		}
	}
	if pos[3] < p.bounds.MinA {
		p.bounds.MinA = pos[3]
	}
	if pos[3] > p.bounds.MaxA {
		p.bounds.MaxA = pos[3]
	}
}

func stripGcodeComments(line string) string {
	var b strings.Builder
	inParen := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inParen {
			if c == ')' {
				inParen = false
			}
			continue
		}
		switch c {
		case '(':
			inParen = true
		case ';':
			return b.String()
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func parseGcodeWords(line string) []gword {
	var out []gword
	for i := 0; i < len(line); {
		c := line[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c < 'A' || c > 'Z' {
			i++
			continue
		}
		i++
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		start := i
		if i < len(line) && (line[i] == '+' || line[i] == '-') {
			i++
		}
		digits := false
		exponent := false
		for i < len(line) {
			ch := line[i]
			if ch >= '0' && ch <= '9' {
				digits = true
				i++
				continue
			}
			if ch == '.' {
				i++
				continue
			}
			if (ch == 'e' || ch == 'E') && digits && !exponent {
				exponent = true
				i++
				if i < len(line) && (line[i] == '+' || line[i] == '-') {
					i++
				}
				continue
			}
			break
		}
		if !digits {
			continue
		}
		v, err := strconv.ParseFloat(line[start:i], 64)
		if err != nil {
			continue
		}
		out = append(out, gword{letter: c, value: v})
	}
	return out
}
