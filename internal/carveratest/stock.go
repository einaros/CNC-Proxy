package carveratest

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	defaultStockResolutionMM = 1.0
	minStockResolutionMM     = 0.2
	maxStockResolutionMM     = 5.0
	maxStockCells            = 120000

	fakeToolShapeFlat = "flat"
	fakeToolShapeBall = "ball"
	fakeToolShapeVBit = "v_bit"

	defaultFakeVBitAngleDeg = 60.0
)

// SimulationSettingsUpdate is the sidecar/test API for changing fake carving
// replay behavior. Nil fields leave the existing setting unchanged.
type SimulationSettingsUpdate struct {
	Enabled      *bool    `json:"enabled,omitempty"`
	ShowVectors  *bool    `json:"show_vectors,omitempty"`
	SpeedScale   *float64 `json:"speed_scale,omitempty"`
	ResolutionMM *float64 `json:"resolution_mm,omitempty"`
	ToolShape    *string  `json:"tool_shape,omitempty"`
	ToolAngleDeg *float64 `json:"tool_angle_deg,omitempty"`
}

type SnapshotSimulation struct {
	Enabled          bool       `json:"enabled"`
	ShowVectors      bool       `json:"show_vectors"`
	SpeedScale       float64    `json:"speed_scale"`
	ResolutionMM     float64    `json:"resolution_mm"`
	ToolShape        string     `json:"tool_shape"`
	ToolAngleDeg     float64    `json:"tool_angle_deg"`
	HasStock         bool       `json:"has_stock"`
	StockID          string     `json:"stock_id,omitempty"`
	StockVersion     int64      `json:"stock_version,omitempty"`
	StockName        string     `json:"stock_name,omitempty"`
	RemovedVolumeMM3 float64    `json:"removed_volume_mm3,omitempty"`
	UpdatedAt        *time.Time `json:"updated_at,omitempty"`
}

type StockState struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Version          int64      `json:"version"`
	ResolutionMM     float64    `json:"resolution_mm"`
	Bounds           fakeBounds `json:"bounds"`
	XMin             float64    `json:"x_min"`
	XMax             float64    `json:"x_max"`
	YMin             float64    `json:"y_min"`
	YMax             float64    `json:"y_max"`
	BaseZ            float64    `json:"base_z"`
	TopZ             float64    `json:"top_z"`
	CellsX           int        `json:"cells_x"`
	CellsY           int        `json:"cells_y"`
	StepX            float64    `json:"step_x"`
	StepY            float64    `json:"step_y"`
	Heights          []float64  `json:"heights"`
	RemovedVolumeMM3 float64    `json:"removed_volume_mm3"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type fakeStock struct {
	ID               string
	Name             string
	Version          int64
	ResolutionMM     float64
	Bounds           fakeBounds
	XMin             float64
	XMax             float64
	YMin             float64
	YMax             float64
	BaseZ            float64
	TopZ             float64
	CellsX           int
	CellsY           int
	StepX            float64
	StepY            float64
	Heights          []float64
	RemovedVolumeMM3 float64
	UpdatedAt        time.Time
}

type fakeCuttingTool struct {
	Shape      string
	DiameterMM float64
	AngleDeg   float64
	Probe      bool
}

type fakeStockSegment struct {
	start   time.Time
	end     time.Time
	fromTip fakeVec3
	toTip   fakeVec3
	tool    fakeCuttingTool
	applied float64
}

func (m *FakeMachine) UpdateSimulationSettings(update SimulationSettingsUpdate) (SnapshotSimulation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rebuildStock := false
	if update.Enabled != nil {
		m.simEnabled = *update.Enabled
		if !m.simEnabled {
			m.stockSegments = nil
		}
	}
	if update.ShowVectors != nil {
		m.simShowVectors = *update.ShowVectors
	}
	if update.SpeedScale != nil {
		if !fakeFinite(*update.SpeedScale) || *update.SpeedScale <= 0 {
			return SnapshotSimulation{}, errors.New("speed_scale must be greater than zero")
		}
		m.simSpeedScale = math.Min(*update.SpeedScale, 200)
	}
	if update.ResolutionMM != nil {
		res, err := normalizeStockResolution(*update.ResolutionMM)
		if err != nil {
			return SnapshotSimulation{}, err
		}
		if math.Abs(res-m.simResolutionLocked()) > 0.0001 {
			m.simResolutionMM = res
			rebuildStock = true
		}
	}
	if update.ToolShape != nil {
		shape, err := normalizeFakeToolShape(*update.ToolShape)
		if err != nil {
			return SnapshotSimulation{}, err
		}
		m.simToolShape = shape
	}
	if update.ToolAngleDeg != nil {
		if !fakeFinite(*update.ToolAngleDeg) || *update.ToolAngleDeg < 10 || *update.ToolAngleDeg > 160 {
			return SnapshotSimulation{}, errors.New("tool_angle_deg must be between 10 and 160")
		}
		m.simToolAngleDeg = *update.ToolAngleDeg
	}
	if rebuildStock && m.probeModel != nil {
		if err := m.resetStockFromModelLocked(m.probeModel); err != nil {
			return SnapshotSimulation{}, err
		}
	}
	return m.snapshotSimulationLocked(), nil
}

func (m *FakeMachine) ResetStock() (SnapshotSimulation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.probeModel == nil {
		return SnapshotSimulation{}, errors.New("no stock model loaded")
	}
	if !m.fileOpsIdleLocked(time.Now()) {
		return SnapshotSimulation{}, errors.New("machine is not idle")
	}
	if err := m.resetStockFromModelLocked(m.probeModel); err != nil {
		return SnapshotSimulation{}, err
	}
	return m.snapshotSimulationLocked(), nil
}

func (m *FakeMachine) StockState() (StockState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.advanceMotionLocked(time.Now())
	if m.stock == nil {
		return StockState{}, false
	}
	return m.stock.state(), true
}

func (m *FakeMachine) StockSTL() ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.advanceMotionLocked(time.Now())
	if m.stock == nil {
		return nil, false
	}
	return m.stock.stl(), true
}

func (m *FakeMachine) snapshotSimulationLocked() SnapshotSimulation {
	out := SnapshotSimulation{
		Enabled:      m.simEnabled,
		ShowVectors:  m.simShowVectors,
		SpeedScale:   m.simSpeedScaleLocked(),
		ResolutionMM: m.simResolutionLocked(),
		ToolShape:    m.simToolShapeLocked(),
		ToolAngleDeg: m.simToolAngleLocked(),
	}
	if m.stock != nil {
		out.HasStock = true
		out.StockID = m.stock.ID
		out.StockVersion = m.stock.Version
		out.StockName = m.stock.Name
		out.RemovedVolumeMM3 = m.stock.RemovedVolumeMM3
		updated := m.stock.UpdatedAt
		out.UpdatedAt = &updated
	}
	return out
}

func (m *FakeMachine) resetStockFromModelLocked(model *fakeProbeModel) error {
	if model == nil {
		m.stock = nil
		m.stockSegments = nil
		return nil
	}
	stock, err := buildFakeStock(model, m.simResolutionLocked())
	if err != nil {
		return err
	}
	m.stock = stock
	m.stockSegments = nil
	return nil
}

func buildFakeStock(model *fakeProbeModel, requestedResolution float64) (*fakeStock, error) {
	if model == nil || len(model.Triangles) == 0 {
		return nil, errors.New("no stock model loaded")
	}
	b := model.Bounds
	width := b.Max.X - b.Min.X
	depth := b.Max.Y - b.Min.Y
	if width <= 0 || depth <= 0 || !fakeFinite(width) || !fakeFinite(depth) {
		return nil, errors.New("stock model must have non-zero X/Y size")
	}
	res, err := normalizeStockResolution(requestedResolution)
	if err != nil {
		return nil, err
	}
	cellsX := int(math.Ceil(width/res)) + 1
	cellsY := int(math.Ceil(depth/res)) + 1
	if cellsX < 2 {
		cellsX = 2
	}
	if cellsY < 2 {
		cellsY = 2
	}
	for cellsX*cellsY > maxStockCells {
		res *= 1.25
		cellsX = int(math.Ceil(width/res)) + 1
		cellsY = int(math.Ceil(depth/res)) + 1
	}
	stepX := width / float64(cellsX-1)
	stepY := depth / float64(cellsY-1)
	heights := make([]float64, cellsX*cellsY)
	covered := make([]bool, cellsX*cellsY)
	for i := range heights {
		heights[i] = b.Min.Z
	}
	top := b.Min.Z
	for _, tri := range model.Triangles {
		minX := math.Min(tri.A.X, math.Min(tri.B.X, tri.C.X))
		maxX := math.Max(tri.A.X, math.Max(tri.B.X, tri.C.X))
		minY := math.Min(tri.A.Y, math.Min(tri.B.Y, tri.C.Y))
		maxY := math.Max(tri.A.Y, math.Max(tri.B.Y, tri.C.Y))
		x0 := clampInt(int(math.Floor((minX-b.Min.X)/stepX))-1, 0, cellsX-1)
		x1 := clampInt(int(math.Ceil((maxX-b.Min.X)/stepX))+1, 0, cellsX-1)
		y0 := clampInt(int(math.Floor((minY-b.Min.Y)/stepY))-1, 0, cellsY-1)
		y1 := clampInt(int(math.Ceil((maxY-b.Min.Y)/stepY))+1, 0, cellsY-1)
		for y := y0; y <= y1; y++ {
			my := b.Min.Y + float64(y)*stepY
			if y == cellsY-1 {
				my = b.Max.Y
			}
			for x := x0; x <= x1; x++ {
				mx := b.Min.X + float64(x)*stepX
				if x == cellsX-1 {
					mx = b.Max.X
				}
				z, ok := triangleZAtXY(tri, mx, my)
				if !ok {
					continue
				}
				idx := y*cellsX + x
				if stockTopSample(z, b) {
					covered[idx] = true
				}
				if z > heights[idx] {
					heights[idx] = z
				}
				if z > top {
					top = z
				}
			}
		}
	}
	// Edge-only triangles can miss all grid vertices at coarse resolutions. Seed
	// each triangle's projected centroid so thin imported stock still appears.
	for _, tri := range model.Triangles {
		cx := (tri.A.X + tri.B.X + tri.C.X) / 3
		cy := (tri.A.Y + tri.B.Y + tri.C.Y) / 3
		x := clampInt(int(math.Round((cx-b.Min.X)/stepX)), 0, cellsX-1)
		y := clampInt(int(math.Round((cy-b.Min.Y)/stepY)), 0, cellsY-1)
		if z, ok := triangleZAtXY(tri, cx, cy); ok {
			idx := y*cellsX + x
			if stockTopSample(z, b) {
				covered[idx] = true
			}
			if z > heights[idx] {
				heights[idx] = z
			}
			if z > top {
				top = z
			}
		}
	}
	fillUncoveredStockHeights(heights, covered, cellsX, cellsY, top)
	now := time.Now()
	return &fakeStock{
		ID:           model.ID,
		Name:         model.Name,
		Version:      1,
		ResolutionMM: res,
		Bounds:       model.Bounds,
		XMin:         b.Min.X,
		XMax:         b.Max.X,
		YMin:         b.Min.Y,
		YMax:         b.Max.Y,
		BaseZ:        b.Min.Z,
		TopZ:         top,
		CellsX:       cellsX,
		CellsY:       cellsY,
		StepX:        stepX,
		StepY:        stepY,
		Heights:      heights,
		UpdatedAt:    now,
	}, nil
}

func fillUncoveredStockHeights(heights []float64, covered []bool, cellsX, cellsY int, fallbackTop float64) {
	if len(heights) == 0 || len(heights) != len(covered) || cellsX <= 0 || cellsY <= 0 {
		return
	}
	queue := make([]int, 0, len(heights))
	for i, ok := range covered {
		if ok {
			queue = append(queue, i)
		}
	}
	if len(queue) == 0 {
		for i := range heights {
			heights[i] = fallbackTop
		}
		return
	}
	for head := 0; head < len(queue); head++ {
		idx := queue[head]
		x := idx % cellsX
		y := idx / cellsX
		for _, next := range []int{
			neighborStockIndex(x-1, y, cellsX, cellsY),
			neighborStockIndex(x+1, y, cellsX, cellsY),
			neighborStockIndex(x, y-1, cellsX, cellsY),
			neighborStockIndex(x, y+1, cellsX, cellsY),
		} {
			if next < 0 || covered[next] {
				continue
			}
			covered[next] = true
			heights[next] = heights[idx]
			queue = append(queue, next)
		}
	}
}

func stockTopSample(z float64, b fakeBounds) bool {
	return math.Abs(b.Max.Z-b.Min.Z) < 1e-7 || z > b.Min.Z+1e-7
}

func neighborStockIndex(x, y, cellsX, cellsY int) int {
	if x < 0 || y < 0 || x >= cellsX || y >= cellsY {
		return -1
	}
	return y*cellsX + x
}

func triangleZAtXY(tri fakeTriangle, x, y float64) (float64, bool) {
	x1, y1 := tri.A.X, tri.A.Y
	x2, y2 := tri.B.X, tri.B.Y
	x3, y3 := tri.C.X, tri.C.Y
	den := (y2-y3)*(x1-x3) + (x3-x2)*(y1-y3)
	if math.Abs(den) < 1e-9 {
		return 0, false
	}
	a := ((y2-y3)*(x-x3) + (x3-x2)*(y-y3)) / den
	b := ((y3-y1)*(x-x3) + (x1-x3)*(y-y3)) / den
	c := 1 - a - b
	if a < -1e-7 || b < -1e-7 || c < -1e-7 {
		return 0, false
	}
	z := a*tri.A.Z + b*tri.B.Z + c*tri.C.Z
	if !fakeFinite(z) {
		return 0, false
	}
	return z, true
}

func (m *FakeMachine) maybeQueueStockCutLocked(start, end time.Time, fromM, toM []float64, feedMMMin float64) {
	if !m.simEnabled || m.stock == nil || feedMMMin <= 0 || m.insertedTool == nil || m.insertedTool.Probe {
		return
	}
	tool := m.currentCuttingToolLocked()
	if tool.DiameterMM <= 0 {
		return
	}
	from := m.toolTipFromMachinePositionLocked(fromM)
	to := m.toolTipFromMachinePositionLocked(toM)
	seg := fakeStockSegment{
		start:   start,
		end:     end,
		fromTip: from,
		toTip:   to,
		tool:    tool,
	}
	if !end.After(start) {
		m.applyStockSegmentRangeLocked(seg, 0, 1)
		return
	}
	m.stockSegments = append(m.stockSegments, seg)
}

func (m *FakeMachine) advanceStockSimulationLocked(now time.Time) {
	if len(m.stockSegments) == 0 || m.stock == nil {
		return
	}
	out := m.stockSegments[:0]
	for _, seg := range m.stockSegments {
		target := 0.0
		switch {
		case !now.After(seg.start):
			target = 0
		case !now.Before(seg.end):
			target = 1
		default:
			target = now.Sub(seg.start).Seconds() / seg.end.Sub(seg.start).Seconds()
		}
		target = clamp01(target)
		if target > seg.applied+1e-6 {
			m.applyStockSegmentRangeLocked(seg, seg.applied, target)
			seg.applied = target
		}
		if seg.applied < 1-1e-6 {
			out = append(out, seg)
		}
	}
	m.stockSegments = out
}

func (m *FakeMachine) applyStockSegmentRangeLocked(seg fakeStockSegment, fromT, toT float64) {
	if m.stock == nil || toT <= fromT {
		return
	}
	distance := distanceVec(seg.fromTip, seg.toTip) * (toT - fromT)
	step := math.Max(m.stock.ResolutionMM*0.5, math.Max(seg.tool.DiameterMM*0.20, 0.2))
	samples := int(math.Ceil(distance / step))
	if samples < 1 {
		samples = 1
	}
	if samples > 20000 {
		samples = 20000
	}
	changed := false
	removed := 0.0
	for i := 0; i <= samples; i++ {
		t := fromT + (toT-fromT)*float64(i)/float64(samples)
		p := lerpVec(seg.fromTip, seg.toTip, t)
		r, ok := m.stock.carveAt(p, seg.tool)
		if r > 0 {
			removed += r
			changed = true
		}
		if ok {
			changed = true
		}
	}
	if changed {
		m.stock.RemovedVolumeMM3 += removed
		m.stock.Version++
		m.stock.UpdatedAt = time.Now()
	}
}

func (s *fakeStock) carveAt(tip fakeVec3, tool fakeCuttingTool) (float64, bool) {
	if s == nil || len(s.Heights) == 0 {
		return 0, false
	}
	radius := tool.DiameterMM / 2
	if radius <= 0 || !fakeFinite(radius) {
		return 0, false
	}
	x0 := int(math.Floor((tip.X - radius - s.XMin) / s.StepX))
	x1 := int(math.Ceil((tip.X + radius - s.XMin) / s.StepX))
	y0 := int(math.Floor((tip.Y - radius - s.YMin) / s.StepY))
	y1 := int(math.Ceil((tip.Y + radius - s.YMin) / s.StepY))
	if x1 < 0 || y1 < 0 || x0 >= s.CellsX || y0 >= s.CellsY {
		return 0, false
	}
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 >= s.CellsX {
		x1 = s.CellsX - 1
	}
	if y1 >= s.CellsY {
		y1 = s.CellsY - 1
	}
	area := s.StepX * s.StepY
	removed := 0.0
	changed := false
	for yi := y0; yi <= y1; yi++ {
		y := s.YMin + float64(yi)*s.StepY
		for xi := x0; xi <= x1; xi++ {
			x := s.XMin + float64(xi)*s.StepX
			d := math.Hypot(x-tip.X, y-tip.Y)
			if d > radius+1e-9 {
				continue
			}
			surface, ok := cutterSurfaceZ(tip.Z, d, radius, tool)
			if !ok {
				continue
			}
			if surface < s.BaseZ {
				surface = s.BaseZ
			}
			idx := yi*s.CellsX + xi
			if surface < s.Heights[idx]-1e-7 {
				removed += (s.Heights[idx] - surface) * area
				s.Heights[idx] = surface
				changed = true
			}
		}
	}
	return removed, changed
}

func cutterSurfaceZ(tipZ, radial, radius float64, tool fakeCuttingTool) (float64, bool) {
	switch tool.Shape {
	case fakeToolShapeBall:
		if radial > radius {
			return 0, false
		}
		return tipZ + radius - math.Sqrt(math.Max(radius*radius-radial*radial, 0)), true
	case fakeToolShapeVBit:
		if radial > radius {
			return 0, false
		}
		angle := tool.AngleDeg
		if !fakeFinite(angle) || angle <= 0 {
			angle = defaultFakeVBitAngleDeg
		}
		tanHalf := math.Tan((angle * math.Pi / 180) / 2)
		if tanHalf <= 0 {
			return tipZ, true
		}
		return tipZ + radial/tanHalf, true
	default:
		return tipZ, true
	}
}

func (s *fakeStock) firstTipContact(start, end fakeVec3) (float64, fakeVec3, bool) {
	if s == nil || len(s.Heights) == 0 {
		return 0, fakeVec3{}, false
	}
	distance := distanceVec(start, end)
	step := math.Max(math.Min(s.StepX, s.StepY)*0.25, 0.1)
	if step <= 0 || !fakeFinite(step) {
		step = 0.25
	}
	samples := int(math.Ceil(distance / step))
	if samples < 1 {
		samples = 1
	}
	if samples > 4096 {
		samples = 4096
	}
	prevT := 0.0
	prevDistance := 0.0
	prevOK := false
	for i := 0; i <= samples; i++ {
		t := float64(i) / float64(samples)
		p := lerpVec(start, end, t)
		surface, ok := s.surfaceZAt(p.X, p.Y)
		if !ok {
			prevOK = false
			continue
		}
		d := p.Z - surface
		if d <= 0 {
			if !prevOK {
				p.Z = surface
				return t, p, true
			}
			lo, hi := prevT, t
			for n := 0; n < 32; n++ {
				mid := (lo + hi) / 2
				mp := lerpVec(start, end, mid)
				ms, ok := s.surfaceZAt(mp.X, mp.Y)
				if !ok {
					lo = mid
					continue
				}
				if mp.Z-ms <= 0 {
					hi = mid
				} else {
					lo = mid
				}
			}
			p = lerpVec(start, end, hi)
			if surface, ok = s.surfaceZAt(p.X, p.Y); ok {
				p.Z = surface
			}
			return hi, p, true
		}
		prevT = t
		prevDistance = d
		prevOK = fakeFinite(prevDistance)
	}
	return 0, fakeVec3{}, false
}

func (s *fakeStock) maxSurfaceZInDisk(x, y, radius float64) (float64, bool) {
	if s == nil || len(s.Heights) == 0 || radius < 0 || !fakeFinite(radius) || s.CellsX < 2 || s.CellsY < 2 {
		return 0, false
	}
	if x+radius < s.XMin-1e-9 || x-radius > s.XMax+1e-9 || y+radius < s.YMin-1e-9 || y-radius > s.YMax+1e-9 {
		return 0, false
	}
	x0 := clampInt(int(math.Floor((x-radius-s.XMin)/s.StepX))-1, 0, s.CellsX-2)
	x1 := clampInt(int(math.Ceil((x+radius-s.XMin)/s.StepX))+1, 0, s.CellsX-2)
	y0 := clampInt(int(math.Floor((y-radius-s.YMin)/s.StepY))-1, 0, s.CellsY-2)
	y1 := clampInt(int(math.Ceil((y+radius-s.YMin)/s.StepY))+1, 0, s.CellsY-2)
	best := math.Inf(-1)
	for yi := y0; yi <= y1; yi++ {
		for xi := x0; xi <= x1; xi++ {
			p00 := s.point(xi, yi)
			p10 := s.point(xi+1, yi)
			p11 := s.point(xi+1, yi+1)
			p01 := s.point(xi, yi+1)
			for _, tri := range []fakeTriangle{
				{A: p00, B: p10, C: p11},
				{A: p00, B: p11, C: p01},
			} {
				if !triangleXYMayOverlapDisk(tri, x, y, radius) {
					continue
				}
				if z, ok := triangleMaxZInDisk(tri, x, y, radius); ok && z > best {
					best = z
				}
			}
		}
	}
	if !fakeFinite(best) {
		return 0, false
	}
	return best, true
}

func (s *fakeStock) surfaceZAt(x, y float64) (float64, bool) {
	if s == nil || len(s.Heights) == 0 || s.CellsX <= 0 || s.CellsY <= 0 || s.StepX <= 0 || s.StepY <= 0 {
		return 0, false
	}
	if x < s.XMin-1e-9 || x > s.XMax+1e-9 || y < s.YMin-1e-9 || y > s.YMax+1e-9 {
		return 0, false
	}
	fx := (x - s.XMin) / s.StepX
	fy := (y - s.YMin) / s.StepY
	if fx < 0 {
		fx = 0
	}
	if fy < 0 {
		fy = 0
	}
	maxX := float64(s.CellsX - 1)
	maxY := float64(s.CellsY - 1)
	if fx > maxX {
		fx = maxX
	}
	if fy > maxY {
		fy = maxY
	}
	x0 := clampInt(int(math.Floor(fx)), 0, s.CellsX-1)
	y0 := clampInt(int(math.Floor(fy)), 0, s.CellsY-1)
	x1 := clampInt(x0+1, 0, s.CellsX-1)
	y1 := clampInt(y0+1, 0, s.CellsY-1)
	tx := fx - float64(x0)
	ty := fy - float64(y0)
	h00 := s.Heights[y0*s.CellsX+x0]
	h10 := s.Heights[y0*s.CellsX+x1]
	h01 := s.Heights[y1*s.CellsX+x0]
	h11 := s.Heights[y1*s.CellsX+x1]
	h0 := h00*(1-tx) + h10*tx
	h1 := h01*(1-tx) + h11*tx
	return h0*(1-ty) + h1*ty, true
}

func (m *FakeMachine) currentCuttingToolLocked() fakeCuttingTool {
	shape := m.simToolShapeLocked()
	angle := m.simToolAngleLocked()
	diameter := 0.0
	probe := false
	if m.insertedTool != nil {
		diameter = m.insertedTool.DiameterMM
		probe = m.insertedTool.Probe
	}
	return fakeCuttingTool{
		Shape:      shape,
		DiameterMM: diameter,
		AngleDeg:   angle,
		Probe:      probe,
	}
}

func (m *FakeMachine) toolTipFromMachinePositionLocked(pos []float64) fakeVec3 {
	p := fakeVec3{}
	if len(pos) > 0 {
		p.X = pos[0]
	}
	if len(pos) > 1 {
		p.Y = pos[1]
	}
	if len(pos) > 2 {
		p.Z = pos[2]
	}
	if m.insertedTool != nil {
		p.Z -= m.insertedTool.StickoutMM
	}
	return p
}

func normalizeStockResolution(v float64) (float64, error) {
	if !fakeFinite(v) || v <= 0 {
		return 0, errors.New("resolution_mm must be greater than zero")
	}
	if v < minStockResolutionMM {
		v = minStockResolutionMM
	}
	if v > maxStockResolutionMM {
		v = maxStockResolutionMM
	}
	return v, nil
}

func normalizeFakeToolShape(shape string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(shape))
	key = strings.NewReplacer("-", "_", " ", "_").Replace(key)
	switch key {
	case "", fakeToolShapeFlat, "flat_end_mill", "end_mill":
		return fakeToolShapeFlat, nil
	case fakeToolShapeBall, "ball_end_mill", "ballnose", "ball_nose":
		return fakeToolShapeBall, nil
	case fakeToolShapeVBit, "vbit", "v", "carving", "carving_bit", "engraving", "engraving_bit":
		return fakeToolShapeVBit, nil
	default:
		return "", fmt.Errorf("unsupported tool_shape %q", shape)
	}
}

func clampInt(v, minV, maxV int) int {
	if v < minV {
		return minV
	}
	if v > maxV {
		return maxV
	}
	return v
}

func (m *FakeMachine) simSpeedScaleLocked() float64 {
	if m.simSpeedScale <= 0 || !fakeFinite(m.simSpeedScale) {
		return 1
	}
	return m.simSpeedScale
}

func (m *FakeMachine) simResolutionLocked() float64 {
	if m.simResolutionMM <= 0 || !fakeFinite(m.simResolutionMM) {
		return defaultStockResolutionMM
	}
	return m.simResolutionMM
}

func (m *FakeMachine) simToolShapeLocked() string {
	if m.simToolShape == "" {
		return fakeToolShapeFlat
	}
	return m.simToolShape
}

func (m *FakeMachine) simToolAngleLocked() float64 {
	if m.simToolAngleDeg <= 0 || !fakeFinite(m.simToolAngleDeg) {
		return defaultFakeVBitAngleDeg
	}
	return m.simToolAngleDeg
}

func (m *FakeMachine) scaleDurationLocked(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	scale := m.simSpeedScaleLocked()
	if scale <= 1 {
		return d
	}
	scaled := time.Duration(float64(d) / scale)
	if scaled < time.Millisecond {
		return time.Millisecond
	}
	return scaled
}

func (m *FakeMachine) fakeMoveDurationLocked(delta map[byte]float64, feedMMMin float64) time.Duration {
	return m.scaleDurationLocked(fakeMoveDuration(delta, feedMMMin))
}

func distanceVec(a, b fakeVec3) float64 {
	return math.Hypot(math.Hypot(a.X-b.X, a.Y-b.Y), a.Z-b.Z)
}

func (s *fakeStock) state() StockState {
	return StockState{
		ID:               s.ID,
		Name:             s.Name,
		Version:          s.Version,
		ResolutionMM:     s.ResolutionMM,
		Bounds:           s.Bounds,
		XMin:             s.XMin,
		XMax:             s.XMax,
		YMin:             s.YMin,
		YMax:             s.YMax,
		BaseZ:            s.BaseZ,
		TopZ:             s.TopZ,
		CellsX:           s.CellsX,
		CellsY:           s.CellsY,
		StepX:            s.StepX,
		StepY:            s.StepY,
		Heights:          append([]float64(nil), s.Heights...),
		RemovedVolumeMM3: s.RemovedVolumeMM3,
		UpdatedAt:        s.UpdatedAt,
	}
}

func (s *fakeStock) stl() []byte {
	var b bytes.Buffer
	b.WriteString("solid fakemachine-stock\n")
	for y := 0; y < s.CellsY-1; y++ {
		for x := 0; x < s.CellsX-1; x++ {
			p00 := s.point(x, y)
			p10 := s.point(x+1, y)
			p11 := s.point(x+1, y+1)
			p01 := s.point(x, y+1)
			writeSTLFacet(&b, p00, p10, p11)
			writeSTLFacet(&b, p00, p11, p01)
		}
	}
	for x := 0; x < s.CellsX-1; x++ {
		writeStockWall(&b, s.point(x, 0), s.point(x+1, 0), s.BaseZ)
		writeStockWall(&b, s.point(x+1, s.CellsY-1), s.point(x, s.CellsY-1), s.BaseZ)
	}
	for y := 0; y < s.CellsY-1; y++ {
		writeStockWall(&b, s.point(0, y+1), s.point(0, y), s.BaseZ)
		writeStockWall(&b, s.point(s.CellsX-1, y), s.point(s.CellsX-1, y+1), s.BaseZ)
	}
	p0 := fakeVec3{s.XMin, s.YMin, s.BaseZ}
	p1 := fakeVec3{s.XMax, s.YMin, s.BaseZ}
	p2 := fakeVec3{s.XMax, s.YMax, s.BaseZ}
	p3 := fakeVec3{s.XMin, s.YMax, s.BaseZ}
	writeSTLFacet(&b, p0, p2, p1)
	writeSTLFacet(&b, p0, p3, p2)
	b.WriteString("endsolid fakemachine-stock\n")
	return b.Bytes()
}

func (s *fakeStock) point(x, y int) fakeVec3 {
	mx := s.XMin + float64(x)*s.StepX
	my := s.YMin + float64(y)*s.StepY
	if x == s.CellsX-1 {
		mx = s.XMax
	}
	if y == s.CellsY-1 {
		my = s.YMax
	}
	return fakeVec3{X: mx, Y: my, Z: s.Heights[y*s.CellsX+x]}
}

func writeStockWall(b *bytes.Buffer, topA, topB fakeVec3, baseZ float64) {
	baseA := fakeVec3{X: topA.X, Y: topA.Y, Z: baseZ}
	baseB := fakeVec3{X: topB.X, Y: topB.Y, Z: baseZ}
	writeSTLFacet(b, baseA, topA, topB)
	writeSTLFacet(b, baseA, topB, baseB)
}

func writeSTLFacet(b *bytes.Buffer, a, c, d fakeVec3) {
	n := unitNormal(a, c, d)
	b.WriteString("  facet normal ")
	writeFloat(b, n.X)
	b.WriteByte(' ')
	writeFloat(b, n.Y)
	b.WriteByte(' ')
	writeFloat(b, n.Z)
	b.WriteString("\n    outer loop\n      vertex ")
	writeSTLVertex(b, a)
	b.WriteString("\n      vertex ")
	writeSTLVertex(b, c)
	b.WriteString("\n      vertex ")
	writeSTLVertex(b, d)
	b.WriteString("\n    endloop\n  endfacet\n")
}

func writeSTLVertex(b *bytes.Buffer, p fakeVec3) {
	writeFloat(b, p.X)
	b.WriteByte(' ')
	writeFloat(b, p.Y)
	b.WriteByte(' ')
	writeFloat(b, p.Z)
}

func writeFloat(b *bytes.Buffer, v float64) {
	b.WriteString(strconv.FormatFloat(v, 'f', 6, 64))
}

func unitNormal(a, b, c fakeVec3) fakeVec3 {
	n := crossVec(subVec(b, a), subVec(c, a))
	l := math.Hypot(math.Hypot(n.X, n.Y), n.Z)
	if l <= 0 || !fakeFinite(l) {
		return fakeVec3{}
	}
	return fakeVec3{X: n.X / l, Y: n.Y / l, Z: n.Z / l}
}
