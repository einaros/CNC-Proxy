package carveratest

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const maxProbeModelTriangles = 500000

const (
	fakeProbeTipDiameterMM      = 2.0
	fakeProbeShoulderDiameterMM = 3.175
	fakeProbeShoulderOffsetMM   = fakeProbeTipDiameterMM
)

type fakeVec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type fakeTriangle struct {
	A fakeVec3
	B fakeVec3
	C fakeVec3
}

type fakeBounds struct {
	Min fakeVec3
	Max fakeVec3
}

type fakeModelPlacement struct {
	Offset      fakeVec3
	RotationDeg float64
}

type fakeProbeModel struct {
	ID              string
	Name            string
	Format          string
	SourceTriangles []fakeTriangle
	SourceBounds    fakeBounds
	Triangles       []fakeTriangle
	Bounds          fakeBounds
	Placement       fakeModelPlacement
	LoadedAt        time.Time
}

type ProbeModelMesh struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	Format       string                 `json:"format"`
	Triangles    int                    `json:"triangles"`
	Bounds       fakeBounds             `json:"bounds"`
	SourceBounds fakeBounds             `json:"source_bounds"`
	Placement    SnapshotModelPlacement `json:"placement"`
	Positions    []float64              `json:"positions"`
}

type fakeProbeResult struct {
	Command string
	Hit     bool
	Machine fakeVec3
	Source  string
	At      time.Time
}

type ModelPlacementUpdate struct {
	XMinMM      *float64 `json:"x_min_mm,omitempty"`
	YMinMM      *float64 `json:"y_min_mm,omitempty"`
	TopZMM      *float64 `json:"top_z_mm,omitempty"`
	RotationDeg *float64 `json:"rotation_deg,omitempty"`
}

func (m *FakeMachine) applyProbeMoveLocked(command string, values map[byte]float64, has map[byte]bool, hasG53 bool) {
	now := time.Now()
	calibrationProbe := isFakeCalibrationProbe(command)
	m.advanceMotionLocked(now)
	bracketed, state, fields, ok := parseFakeStatus(m.status)
	if !ok {
		return
	}
	mi := findFakeStatusField(fields, "MPos")
	if mi < 0 {
		return
	}
	mpos, ok := parseFakeAxisList(fields[mi].value)
	if !ok {
		return
	}
	mpos = ensureFakeAxisLen(mpos, 3)
	wpos := append([]float64(nil), mpos...)
	if wi := findFakeStatusField(fields, "WPos"); wi >= 0 {
		if vals, ok := parseFakeAxisList(fields[wi].value); ok {
			wpos = ensureFakeAxisLen(vals, 3)
		}
	}
	wco := []float64{mpos[0] - wpos[0], mpos[1] - wpos[1], mpos[2] - wpos[2]}
	target := append([]float64(nil), mpos...)
	axisCount := 0
	if calibrationProbe {
		if has['Z'] {
			target[2] += values['Z']
			axisCount = 1
		}
	} else {
		for axis, targetValue := range fakeAxisValues(values, has) {
			idx, ok := fakeAxisIndex[axis]
			if !ok || idx >= 3 {
				continue
			}
			axisCount++
			if hasG53 {
				target[idx] = targetValue
			} else if m.absolute {
				target[idx] = targetValue + wco[idx]
			} else {
				target[idx] += targetValue
			}
		}
	}
	if axisCount == 0 {
		target[2] = configFloat(m.config, "soft_endstop.z_min", -121)
	}

	start := fakeVec3{mpos[0], mpos[1], mpos[2]}
	end := fakeVec3{target[0], target[1], target[2]}
	contact, hit, source := m.firstProbeContactLocked(start, end)
	if calibrationProbe {
		contact, hit, source = m.firstCalibrationContactLocked(start, end)
		if hit {
			m.curToolMZ = contact.Z
		}
	}
	finalM := []float64{contact.X, contact.Y, contact.Z}
	finalW := []float64{contact.X - wco[0], contact.Y - wco[1], contact.Z - wco[2]}
	applyFakeAxesToFields(&fields, finalM, finalW)
	if state == "Run" {
		state = "Idle"
	}
	m.status = formatFakeStatus(bracketed, state, fields)
	m.lastProbe = &fakeProbeResult{
		Command: command,
		Hit:     hit,
		Machine: contact,
		Source:  source,
		At:      now,
	}
}

func isFakeCalibrationProbe(command string) bool {
	for _, w := range parseFakeGcodeWords(stripFakeGcodeComments(command)) {
		if w.letter != 'G' {
			continue
		}
		code, subcode := splitFakeGCode(w.value)
		if code == 38 && subcode == 6 {
			return true
		}
	}
	return false
}

func (m *FakeMachine) firstCalibrationContactLocked(start, end fakeVec3) (fakeVec3, bool, string) {
	if m.insertedTool == nil {
		return m.firstProbeContactLocked(start, end)
	}
	z := m.insertedToolCalibrationMZLocked(*m.insertedTool)
	if _, p, ok := segmentZPlaneContact(start, end, z); ok {
		return p, true, fakeCalibrationSwitchSource
	}
	return end, false, ""
}

func (m *FakeMachine) firstProbeContactLocked(start, end fakeVec3) (fakeVec3, bool, string) {
	bestT := math.Inf(1)
	best := end
	source := ""
	contactStart, contactEnd, contactOffsetZ, useToolTip := m.probeContactSegmentLocked(start, end)
	if useToolTip {
		if t, p, src, ok := m.firstProbeShapeContactLocked(contactStart, contactEnd); ok && t < bestT {
			bestT = t
			best = fakeVec3{X: p.X, Y: p.Y, Z: p.Z + contactOffsetZ}
			source = src
		}
	} else if m.probeModel != nil {
		for _, tri := range m.probeModel.Triangles {
			if t, p, ok := segmentTriangleIntersection(contactStart, contactEnd, tri); ok && t < bestT {
				bestT = t
				best = fakeVec3{X: p.X, Y: p.Y, Z: p.Z + contactOffsetZ}
				source = m.probeModel.Name
			}
		}
	}
	if t, p, ok := m.bedPlaneContactLocked(contactStart, contactEnd); ok && t < bestT {
		bestT = t
		best = fakeVec3{X: p.X, Y: p.Y, Z: p.Z + contactOffsetZ}
		source = "bed"
	}
	if math.IsInf(bestT, 1) {
		return end, false, ""
	}
	return best, true, source
}

type fakeProbeSection struct {
	RadiusMM    float64
	TipOffsetMM float64
	Description string
}

func (m *FakeMachine) firstProbeShapeContactLocked(startTip, endTip fakeVec3) (float64, fakeVec3, string, bool) {
	section := fakeProbeSection{RadiusMM: fakeProbeTipDiameterMM / 2, TipOffsetMM: 0, Description: "probe tip"}
	return m.firstProbeSectionContactLocked(startTip, endTip, section)
}

func (m *FakeMachine) firstProbeSectionContactLocked(startTip, endTip fakeVec3, section fakeProbeSection) (float64, fakeVec3, string, bool) {
	if math.Hypot(endTip.X-startTip.X, endTip.Y-startTip.Y) <= 1e-7 {
		return m.probeSectionContactAtXYLocked(startTip, endTip, startTip.X, startTip.Y, section)
	}
	const samples = 160
	prevT := 0.0
	prevOverlap := false
	for i := 0; i <= samples; i++ {
		t := float64(i) / samples
		p := lerpVec(startTip, endTip, t)
		surfaceZ, source, ok := m.maxProbeSurfaceZLocked(p.X, p.Y, section.RadiusMM)
		if !ok {
			prevOverlap = false
			prevT = t
			continue
		}
		overlap := p.Z <= surfaceZ-section.TipOffsetMM+1e-9
		if overlap {
			if !prevOverlap {
				lo, hi := prevT, t
				for n := 0; n < 32; n++ {
					mid := (lo + hi) / 2
					mp := lerpVec(startTip, endTip, mid)
					mz, _, ok := m.maxProbeSurfaceZLocked(mp.X, mp.Y, section.RadiusMM)
					if ok && mp.Z <= mz-section.TipOffsetMM+1e-9 {
						hi = mid
					} else {
						lo = mid
					}
				}
				p = lerpVec(startTip, endTip, hi)
				if surfaceZ, source, ok = m.maxProbeSurfaceZLocked(p.X, p.Y, section.RadiusMM); ok {
					p.Z = surfaceZ - section.TipOffsetMM
				}
				return hi, p, source, true
			}
			p.Z = surfaceZ - section.TipOffsetMM
			return t, p, source, true
		}
		prevOverlap = false
		prevT = t
	}
	return 0, fakeVec3{}, "", false
}

func (m *FakeMachine) probeSectionContactAtXYLocked(startTip, endTip fakeVec3, x, y float64, section fakeProbeSection) (float64, fakeVec3, string, bool) {
	surfaceZ, source, ok := m.maxProbeSurfaceZLocked(x, y, section.RadiusMM)
	if !ok {
		return 0, fakeVec3{}, "", false
	}
	contactTipZ := surfaceZ - section.TipOffsetMM
	if startTip.Z <= contactTipZ+1e-9 {
		p := startTip
		p.Z = contactTipZ
		return 0, p, source, true
	}
	dz := endTip.Z - startTip.Z
	if math.Abs(dz) < 1e-9 {
		return 0, fakeVec3{}, "", false
	}
	t := (contactTipZ - startTip.Z) / dz
	if t < -1e-9 || t > 1+1e-9 {
		return 0, fakeVec3{}, "", false
	}
	p := lerpVec(startTip, endTip, clamp01(t))
	p.Z = contactTipZ
	return clamp01(t), p, source, true
}

func (m *FakeMachine) maxProbeSurfaceZLocked(x, y, radius float64) (float64, string, bool) {
	best := math.Inf(-1)
	source := ""
	useStock := m.stock != nil && m.stock.RemovedVolumeMM3 > 1e-9
	if useStock {
		if z, ok := m.stock.maxSurfaceZInDisk(x, y, radius); ok && z > best {
			best = z
			source = m.stock.Name
		}
	} else if m.probeModel != nil {
		if z, ok := m.probeModel.maxSurfaceZInDisk(x, y, radius); ok && z > best {
			best = z
			source = m.probeModel.Name
		}
	}
	if !fakeFinite(best) && m.stock != nil {
		if z, ok := m.stock.maxSurfaceZInDisk(x, y, radius); ok && z > best {
			best = z
			source = m.stock.Name
		}
	}
	if z, ok := m.bedSurfaceZInDiskLocked(x, y, radius); ok && z > best {
		best = z
		source = "bed"
	}
	if !fakeFinite(best) {
		return 0, "", false
	}
	return best, source, true
}

func (m *FakeMachine) bedSurfaceZInDiskLocked(x, y, radius float64) (float64, bool) {
	xMax := configFloat(m.config, "soft_endstop.x_max", 0)
	yMax := configFloat(m.config, "soft_endstop.y_max", 0)
	xMin := xMax - configFloat(m.config, "coordinate.worksize_x", 300)
	yMin := yMax - configFloat(m.config, "coordinate.worksize_y", 200)
	if x+radius < xMin-1e-6 || x-radius > xMax+1e-6 || y+radius < yMin-1e-6 || y-radius > yMax+1e-6 {
		return 0, false
	}
	return configFloat(m.config, "soft_endstop.z_min", -121), true
}

func (m *FakeMachine) probeContactSegmentLocked(start, end fakeVec3) (fakeVec3, fakeVec3, float64, bool) {
	if m.insertedTool == nil || !m.insertedTool.Probe || !m.insertedTool.Calibrated {
		return start, end, 0, false
	}
	offset := m.insertedTool.StickoutMM
	tipStart := start
	tipStart.Z -= offset
	tipEnd := end
	tipEnd.Z -= offset
	return tipStart, tipEnd, offset, true
}

func (pm *fakeProbeModel) maxSurfaceZInDisk(x, y, radius float64) (float64, bool) {
	if pm == nil || radius < 0 || !fakeFinite(radius) {
		return 0, false
	}
	best := math.Inf(-1)
	for _, tri := range pm.Triangles {
		if !triangleXYMayOverlapDisk(tri, x, y, radius) {
			continue
		}
		if z, ok := triangleMaxZInDisk(tri, x, y, radius); ok && z > best {
			best = z
		}
	}
	if !fakeFinite(best) {
		return 0, false
	}
	return best, true
}

func triangleXYMayOverlapDisk(tri fakeTriangle, x, y, radius float64) bool {
	minX := math.Min(tri.A.X, math.Min(tri.B.X, tri.C.X)) - radius
	maxX := math.Max(tri.A.X, math.Max(tri.B.X, tri.C.X)) + radius
	minY := math.Min(tri.A.Y, math.Min(tri.B.Y, tri.C.Y)) - radius
	maxY := math.Max(tri.A.Y, math.Max(tri.B.Y, tri.C.Y)) + radius
	return x >= minX && x <= maxX && y >= minY && y <= maxY
}

func triangleMaxZInDisk(tri fakeTriangle, cx, cy, radius float64) (float64, bool) {
	best := math.Inf(-1)
	addCandidate := func(p fakeVec3) {
		if math.Hypot(p.X-cx, p.Y-cy) <= radius+1e-8 && pointInTriangleXY(tri, p.X, p.Y) {
			if p.Z > best {
				best = p.Z
			}
		}
	}
	for _, p := range []fakeVec3{tri.A, tri.B, tri.C} {
		addCandidate(p)
	}
	if z, ok := triangleZAtXY(tri, cx, cy); ok {
		addCandidate(fakeVec3{X: cx, Y: cy, Z: z})
	}
	for _, edge := range [][2]fakeVec3{{tri.A, tri.B}, {tri.B, tri.C}, {tri.C, tri.A}} {
		for _, p := range edgeDiskCandidates(edge[0], edge[1], cx, cy, radius) {
			addCandidate(p)
		}
	}
	if p, ok := triangleDiskGradientCandidate(tri, cx, cy, radius); ok {
		addCandidate(p)
	}
	if !fakeFinite(best) {
		return 0, false
	}
	return best, true
}

func edgeDiskCandidates(a, b fakeVec3, cx, cy, radius float64) []fakeVec3 {
	out := make([]fakeVec3, 0, 4)
	addAt := func(t float64) {
		if t < -1e-8 || t > 1+1e-8 {
			return
		}
		out = append(out, lerpVec(a, b, clamp01(t)))
	}
	addAt(0)
	addAt(1)
	dx := b.X - a.X
	dy := b.Y - a.Y
	den := dx*dx + dy*dy
	if den <= 1e-12 {
		return out
	}
	addAt(((cx-a.X)*dx + (cy-a.Y)*dy) / den)
	fx := a.X - cx
	fy := a.Y - cy
	A := den
	B := 2 * (fx*dx + fy*dy)
	C := fx*fx + fy*fy - radius*radius
	disc := B*B - 4*A*C
	if disc >= -1e-9 {
		if disc < 0 {
			disc = 0
		}
		root := math.Sqrt(disc)
		addAt((-B - root) / (2 * A))
		addAt((-B + root) / (2 * A))
	}
	return out
}

func triangleDiskGradientCandidate(tri fakeTriangle, cx, cy, radius float64) (fakeVec3, bool) {
	n := crossVec(subVec(tri.B, tri.A), subVec(tri.C, tri.A))
	if math.Abs(n.Z) < 1e-12 {
		return fakeVec3{}, false
	}
	gx := -n.X / n.Z
	gy := -n.Y / n.Z
	g := math.Hypot(gx, gy)
	if g <= 1e-12 {
		return fakeVec3{}, false
	}
	x := cx + radius*gx/g
	y := cy + radius*gy/g
	z, ok := triangleZAtXY(tri, x, y)
	if !ok {
		return fakeVec3{}, false
	}
	return fakeVec3{X: x, Y: y, Z: z}, true
}

func pointInTriangleXY(tri fakeTriangle, x, y float64) bool {
	x1, y1 := tri.A.X, tri.A.Y
	x2, y2 := tri.B.X, tri.B.Y
	x3, y3 := tri.C.X, tri.C.Y
	den := (y2-y3)*(x1-x3) + (x3-x2)*(y1-y3)
	if math.Abs(den) < 1e-12 {
		return pointOnSegmentXY(tri.A, tri.B, x, y) || pointOnSegmentXY(tri.B, tri.C, x, y) || pointOnSegmentXY(tri.C, tri.A, x, y)
	}
	a := ((y2-y3)*(x-x3) + (x3-x2)*(y-y3)) / den
	b := ((y3-y1)*(x-x3) + (x1-x3)*(y-y3)) / den
	c := 1 - a - b
	return a >= -1e-8 && b >= -1e-8 && c >= -1e-8
}

func pointOnSegmentXY(a, b fakeVec3, x, y float64) bool {
	dx := b.X - a.X
	dy := b.Y - a.Y
	length2 := dx*dx + dy*dy
	if length2 <= 1e-12 {
		return math.Hypot(x-a.X, y-a.Y) <= 1e-8
	}
	t := ((x-a.X)*dx + (y-a.Y)*dy) / length2
	if t < -1e-8 || t > 1+1e-8 {
		return false
	}
	px := a.X + clamp01(t)*dx
	py := a.Y + clamp01(t)*dy
	return math.Hypot(x-px, y-py) <= 1e-7
}

func segmentZPlaneContact(start, end fakeVec3, z float64) (float64, fakeVec3, bool) {
	if !fakeFinite(z) {
		return 0, fakeVec3{}, false
	}
	dz := end.Z - start.Z
	if math.Abs(dz) < 1e-9 {
		if math.Abs(start.Z-z) < 1e-9 {
			return 0, start, true
		}
		return 0, fakeVec3{}, false
	}
	t := (z - start.Z) / dz
	if t < -1e-9 || t > 1+1e-9 {
		return 0, fakeVec3{}, false
	}
	t = clamp01(t)
	return t, lerpVec(start, end, t), true
}

func (m *FakeMachine) bedPlaneContactLocked(start, end fakeVec3) (float64, fakeVec3, bool) {
	z := configFloat(m.config, "soft_endstop.z_min", -121)
	dz := end.Z - start.Z
	if math.Abs(dz) < 1e-9 {
		return 0, fakeVec3{}, false
	}
	t := (z - start.Z) / dz
	if t < -1e-9 || t > 1+1e-9 {
		return 0, fakeVec3{}, false
	}
	p := lerpVec(start, end, clamp01(t))
	xMax := configFloat(m.config, "soft_endstop.x_max", 0)
	yMax := configFloat(m.config, "soft_endstop.y_max", 0)
	xMin := xMax - configFloat(m.config, "coordinate.worksize_x", 300)
	yMin := yMax - configFloat(m.config, "coordinate.worksize_y", 200)
	if p.X < xMin-1e-6 || p.X > xMax+1e-6 || p.Y < yMin-1e-6 || p.Y > yMax+1e-6 {
		return 0, fakeVec3{}, false
	}
	return clamp01(t), p, true
}

func segmentTriangleIntersection(start, end fakeVec3, tri fakeTriangle) (float64, fakeVec3, bool) {
	dir := subVec(end, start)
	edge1 := subVec(tri.B, tri.A)
	edge2 := subVec(tri.C, tri.A)
	h := crossVec(dir, edge2)
	det := dotVec(edge1, h)
	if math.Abs(det) < 1e-9 {
		return 0, fakeVec3{}, false
	}
	invDet := 1 / det
	s := subVec(start, tri.A)
	u := invDet * dotVec(s, h)
	if u < -1e-9 || u > 1+1e-9 {
		return 0, fakeVec3{}, false
	}
	q := crossVec(s, edge1)
	v := invDet * dotVec(dir, q)
	if v < -1e-9 || u+v > 1+1e-9 {
		return 0, fakeVec3{}, false
	}
	t := invDet * dotVec(edge2, q)
	if t < -1e-9 || t > 1+1e-9 {
		return 0, fakeVec3{}, false
	}
	t = clamp01(t)
	return t, lerpVec(start, end, t), true
}

func subVec(a, b fakeVec3) fakeVec3 { return fakeVec3{a.X - b.X, a.Y - b.Y, a.Z - b.Z} }
func dotVec(a, b fakeVec3) float64  { return a.X*b.X + a.Y*b.Y + a.Z*b.Z }

func crossVec(a, b fakeVec3) fakeVec3 {
	return fakeVec3{
		X: a.Y*b.Z - a.Z*b.Y,
		Y: a.Z*b.X - a.X*b.Z,
		Z: a.X*b.Y - a.Y*b.X,
	}
}

func lerpVec(a, b fakeVec3, t float64) fakeVec3 {
	return fakeVec3{
		X: a.X + (b.X-a.X)*t,
		Y: a.Y + (b.Y-a.Y)*t,
		Z: a.Z + (b.Z-a.Z)*t,
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// LoadProbeModel parses a machine-coordinate triangle mesh used by fake probe
// moves. STL is supported directly; GLB and self-contained glTF 2.0 files are
// supported for triangle mesh primitives with POSITION accessors.
func (m *FakeMachine) LoadProbeModel(name string, data []byte) error {
	model, err := parseProbeModel(name, data)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	model.applyPlacement(m.defaultModelPlacementLocked(model))
	m.probeModel = model
	m.lastProbe = nil
	if err := m.resetStockFromModelLocked(model); err != nil {
		m.probeModel = nil
		return err
	}
	return nil
}

func (m *FakeMachine) ClearProbeModel() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.probeModel = nil
	m.lastProbe = nil
	m.stock = nil
	m.stockSegments = nil
}

func (m *FakeMachine) SetModelPlacement(update ModelPlacementUpdate) (*SnapshotProbeModel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.probeModel == nil {
		return nil, errors.New("no stock model loaded")
	}
	if !m.fileOpsIdleLocked(time.Now()) {
		return nil, errors.New("machine is not idle")
	}
	placement := m.probeModel.Placement
	offset := placement.Offset
	rotationDeg := placement.RotationDeg
	oldCenter := boundsCenterXY(m.probeModel.Bounds)
	src := m.probeModel.SourceBounds
	if update.RotationDeg != nil {
		if !fakeFinite(*update.RotationDeg) {
			return nil, errors.New("rotation_deg must be finite")
		}
		rotationDeg = normalizePlacementRotationDeg(*update.RotationDeg)
	}
	if update.RotationDeg != nil && update.XMinMM == nil && update.YMinMM == nil {
		b := transformedSourceBounds(src, offset, rotationDeg)
		nextCenter := boundsCenterXY(b)
		offset.X += oldCenter.X - nextCenter.X
		offset.Y += oldCenter.Y - nextCenter.Y
	}
	if update.XMinMM != nil {
		if !fakeFinite(*update.XMinMM) {
			return nil, errors.New("x_min_mm must be finite")
		}
		b := transformedSourceBounds(src, offset, rotationDeg)
		offset.X += *update.XMinMM - b.Min.X
	}
	if update.YMinMM != nil {
		if !fakeFinite(*update.YMinMM) {
			return nil, errors.New("y_min_mm must be finite")
		}
		b := transformedSourceBounds(src, offset, rotationDeg)
		offset.Y += *update.YMinMM - b.Min.Y
	}
	if update.TopZMM != nil {
		if !fakeFinite(*update.TopZMM) {
			return nil, errors.New("top_z_mm must be finite")
		}
		offset.Z = *update.TopZMM - src.Max.Z
	}
	m.probeModel.applyPlacement(fakeModelPlacement{Offset: offset, RotationDeg: rotationDeg})
	m.lastProbe = nil
	if err := m.resetStockFromModelLocked(m.probeModel); err != nil {
		return nil, err
	}
	return snapshotProbeModelLocked(m.probeModel), nil
}

func (m *FakeMachine) defaultModelPlacementLocked(model *fakeProbeModel) fakeModelPlacement {
	if model == nil {
		return fakeModelPlacement{}
	}
	xMax := configFloat(m.config, "soft_endstop.x_max", 0)
	yMax := configFloat(m.config, "soft_endstop.y_max", 0)
	xMin := xMax - configFloat(m.config, "coordinate.worksize_x", 300)
	yMin := yMax - configFloat(m.config, "coordinate.worksize_y", 200)
	bedZ := configFloat(m.config, "soft_endstop.z_min", -121)
	return fakeModelPlacement{
		Offset: fakeVec3{
			X: xMin - model.SourceBounds.Min.X,
			Y: yMin - model.SourceBounds.Min.Y,
			Z: bedZ - model.SourceBounds.Min.Z,
		},
	}
}

func (m *FakeMachine) ProbeModelMesh() (ProbeModelMesh, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.probeModel == nil {
		return ProbeModelMesh{}, false
	}
	return m.probeModel.mesh(), true
}

func (pm *fakeProbeModel) mesh() ProbeModelMesh {
	positions := make([]float64, 0, len(pm.Triangles)*9)
	for _, tri := range pm.Triangles {
		positions = append(positions,
			tri.A.X, tri.A.Y, tri.A.Z,
			tri.B.X, tri.B.Y, tri.B.Z,
			tri.C.X, tri.C.Y, tri.C.Z,
		)
	}
	return ProbeModelMesh{
		ID:           pm.ID,
		Name:         pm.Name,
		Format:       pm.Format,
		Triangles:    len(pm.Triangles),
		Bounds:       pm.Bounds,
		SourceBounds: pm.SourceBounds,
		Placement:    snapshotModelPlacement(pm),
		Positions:    positions,
	}
}

func parseProbeModel(name string, data []byte) (*fakeProbeModel, error) {
	if len(data) == 0 {
		return nil, errors.New("probe model is empty")
	}
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".stl":
		return parseProbeSTL(name, data)
	case ".glb":
		return parseProbeGLB(name, data)
	case ".gltf":
		return parseProbeGLTF(name, data, nil)
	default:
		if bytes.HasPrefix(data, []byte{'g', 'l', 'T', 'F'}) {
			return parseProbeGLB(name, data)
		}
		if bytes.Contains(bytes.ToLower(data[:min(len(data), 512)]), []byte("facet")) {
			return parseProbeSTL(name, data)
		}
		return nil, fmt.Errorf("unsupported probe model type %q", ext)
	}
}

func newProbeModel(name, format string, triangles []fakeTriangle) (*fakeProbeModel, error) {
	if len(triangles) == 0 {
		return nil, errors.New("probe model contains no triangles")
	}
	if len(triangles) > maxProbeModelTriangles {
		return nil, fmt.Errorf("probe model has %d triangles, max %d", len(triangles), maxProbeModelTriangles)
	}
	bounds := trianglesBounds(triangles)
	now := time.Now()
	return &fakeProbeModel{
		ID:              strconv.FormatInt(now.UnixNano(), 36),
		Name:            name,
		Format:          format,
		SourceTriangles: append([]fakeTriangle(nil), triangles...),
		SourceBounds:    bounds,
		Triangles:       append([]fakeTriangle(nil), triangles...),
		Bounds:          bounds,
		LoadedAt:        now,
	}, nil
}

func (pm *fakeProbeModel) applyPlacement(placement fakeModelPlacement) {
	if pm == nil {
		return
	}
	placement.RotationDeg = normalizePlacementRotationDeg(placement.RotationDeg)
	pm.Placement = placement
	pm.Triangles = transformTriangles(pm.SourceTriangles, pm.SourceBounds, placement)
	pm.Bounds = transformedSourceBounds(pm.SourceBounds, placement.Offset, placement.RotationDeg)
	pm.ID = strconv.FormatInt(time.Now().UnixNano(), 36)
}

func transformTriangles(src []fakeTriangle, bounds fakeBounds, placement fakeModelPlacement) []fakeTriangle {
	out := make([]fakeTriangle, len(src))
	for i, tri := range src {
		out[i] = fakeTriangle{
			A: transformSourcePoint(tri.A, bounds, placement.Offset, placement.RotationDeg),
			B: transformSourcePoint(tri.B, bounds, placement.Offset, placement.RotationDeg),
			C: transformSourcePoint(tri.C, bounds, placement.Offset, placement.RotationDeg),
		}
	}
	return out
}

func transformedSourceBounds(bounds fakeBounds, offset fakeVec3, rotationDeg float64) fakeBounds {
	corners := []fakeVec3{
		{X: bounds.Min.X, Y: bounds.Min.Y, Z: bounds.Min.Z},
		{X: bounds.Max.X, Y: bounds.Min.Y, Z: bounds.Min.Z},
		{X: bounds.Max.X, Y: bounds.Max.Y, Z: bounds.Min.Z},
		{X: bounds.Min.X, Y: bounds.Max.Y, Z: bounds.Min.Z},
		{X: bounds.Min.X, Y: bounds.Min.Y, Z: bounds.Max.Z},
		{X: bounds.Max.X, Y: bounds.Min.Y, Z: bounds.Max.Z},
		{X: bounds.Max.X, Y: bounds.Max.Y, Z: bounds.Max.Z},
		{X: bounds.Min.X, Y: bounds.Max.Y, Z: bounds.Max.Z},
	}
	out := fakeBounds{
		Min: fakeVec3{X: math.Inf(1), Y: math.Inf(1), Z: math.Inf(1)},
		Max: fakeVec3{X: math.Inf(-1), Y: math.Inf(-1), Z: math.Inf(-1)},
	}
	placement := fakeModelPlacement{Offset: offset, RotationDeg: rotationDeg}
	for _, corner := range corners {
		p := transformSourcePoint(corner, bounds, placement.Offset, placement.RotationDeg)
		out.Min.X = math.Min(out.Min.X, p.X)
		out.Min.Y = math.Min(out.Min.Y, p.Y)
		out.Min.Z = math.Min(out.Min.Z, p.Z)
		out.Max.X = math.Max(out.Max.X, p.X)
		out.Max.Y = math.Max(out.Max.Y, p.Y)
		out.Max.Z = math.Max(out.Max.Z, p.Z)
	}
	return out
}

func transformSourcePoint(p fakeVec3, bounds fakeBounds, offset fakeVec3, rotationDeg float64) fakeVec3 {
	center := boundsCenterXY(bounds)
	theta := normalizePlacementRotationDeg(rotationDeg) * math.Pi / 180
	c, s := math.Cos(theta), math.Sin(theta)
	dx := p.X - center.X
	dy := p.Y - center.Y
	return fakeVec3{
		X: center.X + dx*c - dy*s + offset.X,
		Y: center.Y + dx*s + dy*c + offset.Y,
		Z: p.Z + offset.Z,
	}
}

func boundsCenterXY(bounds fakeBounds) fakeVec3 {
	return fakeVec3{
		X: (bounds.Min.X + bounds.Max.X) / 2,
		Y: (bounds.Min.Y + bounds.Max.Y) / 2,
	}
}

func normalizePlacementRotationDeg(v float64) float64 {
	if !fakeFinite(v) {
		return 0
	}
	v = math.Mod(v, 360)
	if v >= 180 {
		v -= 360
	}
	if v < -180 {
		v += 360
	}
	if math.Abs(v) < 0.0000001 {
		return 0
	}
	return v
}

func parseProbeSTL(name string, data []byte) (*fakeProbeModel, error) {
	if tris, ok := parseBinarySTL(data); ok {
		return newProbeModel(name, "stl", tris)
	}
	tris, err := parseASCIISTL(data)
	if err != nil {
		return nil, err
	}
	return newProbeModel(name, "stl", tris)
}

func parseBinarySTL(data []byte) ([]fakeTriangle, bool) {
	if len(data) < 84 {
		return nil, false
	}
	count := int(binary.LittleEndian.Uint32(data[80:84]))
	want := 84 + count*50
	if count <= 0 || want > len(data) {
		return nil, false
	}
	tris := make([]fakeTriangle, 0, count)
	off := 84
	for i := 0; i < count; i++ {
		off += 12 // normal
		a := fakeVec3{float64(math.Float32frombits(binary.LittleEndian.Uint32(data[off:]))), float64(math.Float32frombits(binary.LittleEndian.Uint32(data[off+4:]))), float64(math.Float32frombits(binary.LittleEndian.Uint32(data[off+8:])))}
		off += 12
		b := fakeVec3{float64(math.Float32frombits(binary.LittleEndian.Uint32(data[off:]))), float64(math.Float32frombits(binary.LittleEndian.Uint32(data[off+4:]))), float64(math.Float32frombits(binary.LittleEndian.Uint32(data[off+8:])))}
		off += 12
		c := fakeVec3{float64(math.Float32frombits(binary.LittleEndian.Uint32(data[off:]))), float64(math.Float32frombits(binary.LittleEndian.Uint32(data[off+4:]))), float64(math.Float32frombits(binary.LittleEndian.Uint32(data[off+8:])))}
		off += 14 // vertex plus attribute byte count
		if finiteVec(a) && finiteVec(b) && finiteVec(c) {
			tris = append(tris, fakeTriangle{A: a, B: b, C: c})
		}
	}
	return tris, len(tris) > 0
}

func parseASCIISTL(data []byte) ([]fakeTriangle, error) {
	lines := strings.Split(string(data), "\n")
	verts := make([]fakeVec3, 0, 3)
	tris := []fakeTriangle{}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 4 || !strings.EqualFold(fields[0], "vertex") {
			continue
		}
		x, errX := strconv.ParseFloat(fields[1], 64)
		y, errY := strconv.ParseFloat(fields[2], 64)
		z, errZ := strconv.ParseFloat(fields[3], 64)
		if errX != nil || errY != nil || errZ != nil || !fakeFinite(x) || !fakeFinite(y) || !fakeFinite(z) {
			return nil, fmt.Errorf("invalid STL vertex %q", line)
		}
		verts = append(verts, fakeVec3{x, y, z})
		if len(verts) == 3 {
			tris = append(tris, fakeTriangle{A: verts[0], B: verts[1], C: verts[2]})
			verts = verts[:0]
		}
	}
	if len(tris) == 0 {
		return nil, errors.New("STL contains no triangles")
	}
	return tris, nil
}

func parseProbeGLB(name string, data []byte) (*fakeProbeModel, error) {
	if len(data) < 20 || !bytes.Equal(data[:4], []byte{'g', 'l', 'T', 'F'}) {
		return nil, errors.New("invalid GLB header")
	}
	if version := binary.LittleEndian.Uint32(data[4:8]); version != 2 {
		return nil, fmt.Errorf("unsupported GLB version %d", version)
	}
	total := int(binary.LittleEndian.Uint32(data[8:12]))
	if total > len(data) {
		return nil, errors.New("truncated GLB")
	}
	var jsonChunk, binChunk []byte
	for off := 12; off+8 <= total; {
		n := int(binary.LittleEndian.Uint32(data[off : off+4]))
		typ := binary.LittleEndian.Uint32(data[off+4 : off+8])
		off += 8
		if off+n > total {
			return nil, errors.New("truncated GLB chunk")
		}
		chunk := data[off : off+n]
		off += n
		switch typ {
		case 0x4e4f534a:
			jsonChunk = bytes.TrimRight(chunk, " \t\r\n\x00")
		case 0x004e4942:
			binChunk = chunk
		}
	}
	if len(jsonChunk) == 0 {
		return nil, errors.New("GLB missing JSON chunk")
	}
	return parseProbeGLTF(name, jsonChunk, binChunk)
}

type gltfDoc struct {
	Scene       *int             `json:"scene"`
	Scenes      []gltfScene      `json:"scenes"`
	Nodes       []gltfNode       `json:"nodes"`
	Meshes      []gltfMesh       `json:"meshes"`
	Buffers     []gltfBuffer     `json:"buffers"`
	BufferViews []gltfBufferView `json:"bufferViews"`
	Accessors   []gltfAccessor   `json:"accessors"`
}

type gltfScene struct {
	Nodes []int `json:"nodes"`
}

type gltfNode struct {
	Mesh        *int      `json:"mesh"`
	Children    []int     `json:"children"`
	Matrix      []float64 `json:"matrix"`
	Translation []float64 `json:"translation"`
	Rotation    []float64 `json:"rotation"`
	Scale       []float64 `json:"scale"`
}

type gltfMesh struct {
	Primitives []gltfPrimitive `json:"primitives"`
}

type gltfPrimitive struct {
	Attributes map[string]int `json:"attributes"`
	Indices    *int           `json:"indices"`
	Mode       *int           `json:"mode"`
}

type gltfBuffer struct {
	URI        string `json:"uri"`
	ByteLength int    `json:"byteLength"`
}

type gltfBufferView struct {
	Buffer     int `json:"buffer"`
	ByteOffset int `json:"byteOffset"`
	ByteLength int `json:"byteLength"`
	ByteStride int `json:"byteStride"`
}

type gltfAccessor struct {
	BufferView    *int   `json:"bufferView"`
	ByteOffset    int    `json:"byteOffset"`
	ComponentType int    `json:"componentType"`
	Count         int    `json:"count"`
	Type          string `json:"type"`
}

func parseProbeGLTF(name string, jsonData, glbBin []byte) (*fakeProbeModel, error) {
	var doc gltfDoc
	if err := json.Unmarshal(jsonData, &doc); err != nil {
		return nil, fmt.Errorf("parse glTF JSON: %w", err)
	}
	buffers, err := gltfBuffers(doc, glbBin)
	if err != nil {
		return nil, err
	}
	var tris []fakeTriangle
	identity := identityMatrix()
	visited := map[int]bool{}
	roots := gltfSceneRoots(doc)
	for _, root := range roots {
		if err := gatherGLTFNodeTriangles(doc, buffers, root, identity, visited, &tris); err != nil {
			return nil, err
		}
	}
	if len(roots) == 0 {
		for i := range doc.Meshes {
			if err := gatherGLTFMeshTriangles(doc, buffers, i, identity, &tris); err != nil {
				return nil, err
			}
		}
	}
	return newProbeModel(name, "gltf", tris)
}

func gltfSceneRoots(doc gltfDoc) []int {
	if doc.Scene != nil && *doc.Scene >= 0 && *doc.Scene < len(doc.Scenes) {
		return append([]int(nil), doc.Scenes[*doc.Scene].Nodes...)
	}
	if len(doc.Scenes) > 0 {
		return append([]int(nil), doc.Scenes[0].Nodes...)
	}
	return nil
}

func gltfBuffers(doc gltfDoc, glbBin []byte) ([][]byte, error) {
	out := make([][]byte, len(doc.Buffers))
	for i, buf := range doc.Buffers {
		switch {
		case buf.URI == "" && i == 0 && glbBin != nil:
			out[i] = glbBin
		case strings.HasPrefix(buf.URI, "data:"):
			b, err := decodeDataURI(buf.URI)
			if err != nil {
				return nil, fmt.Errorf("decode glTF buffer %d: %w", i, err)
			}
			out[i] = b
		default:
			return nil, fmt.Errorf("glTF buffer %d uses external URI %q; upload GLB or a self-contained data URI glTF", i, buf.URI)
		}
		if buf.ByteLength > 0 && len(out[i]) < buf.ByteLength {
			return nil, fmt.Errorf("glTF buffer %d is truncated", i)
		}
	}
	return out, nil
}

func decodeDataURI(uri string) ([]byte, error) {
	meta, data, ok := strings.Cut(uri, ",")
	if !ok {
		return nil, errors.New("malformed data URI")
	}
	if strings.Contains(meta, ";base64") {
		return base64.StdEncoding.DecodeString(data)
	}
	s, err := url.QueryUnescape(data)
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

func gatherGLTFNodeTriangles(doc gltfDoc, buffers [][]byte, idx int, parent [16]float64, visited map[int]bool, tris *[]fakeTriangle) error {
	if idx < 0 || idx >= len(doc.Nodes) {
		return fmt.Errorf("glTF node %d out of range", idx)
	}
	if visited[idx] {
		return nil
	}
	visited[idx] = true
	node := doc.Nodes[idx]
	world := multiplyMatrix(parent, nodeMatrix(node))
	if node.Mesh != nil {
		if err := gatherGLTFMeshTriangles(doc, buffers, *node.Mesh, world, tris); err != nil {
			return err
		}
	}
	for _, child := range node.Children {
		if err := gatherGLTFNodeTriangles(doc, buffers, child, world, visited, tris); err != nil {
			return err
		}
	}
	return nil
}

func gatherGLTFMeshTriangles(doc gltfDoc, buffers [][]byte, idx int, world [16]float64, tris *[]fakeTriangle) error {
	if idx < 0 || idx >= len(doc.Meshes) {
		return fmt.Errorf("glTF mesh %d out of range", idx)
	}
	for _, prim := range doc.Meshes[idx].Primitives {
		mode := 4
		if prim.Mode != nil {
			mode = *prim.Mode
		}
		if mode != 4 {
			continue
		}
		posIdx, ok := prim.Attributes["POSITION"]
		if !ok {
			continue
		}
		positions, err := gltfReadVec3Accessor(doc, buffers, posIdx, world)
		if err != nil {
			return err
		}
		indices, err := gltfReadIndices(doc, buffers, prim.Indices, len(positions))
		if err != nil {
			return err
		}
		for i := 0; i+2 < len(indices); i += 3 {
			a, b, c := indices[i], indices[i+1], indices[i+2]
			if a < 0 || b < 0 || c < 0 || a >= len(positions) || b >= len(positions) || c >= len(positions) {
				return errors.New("glTF primitive index out of range")
			}
			*tris = append(*tris, fakeTriangle{A: positions[a], B: positions[b], C: positions[c]})
		}
	}
	return nil
}

func gltfReadVec3Accessor(doc gltfDoc, buffers [][]byte, idx int, world [16]float64) ([]fakeVec3, error) {
	if idx < 0 || idx >= len(doc.Accessors) {
		return nil, fmt.Errorf("glTF accessor %d out of range", idx)
	}
	acc := doc.Accessors[idx]
	if acc.BufferView == nil || acc.ComponentType != 5126 || acc.Type != "VEC3" {
		return nil, fmt.Errorf("glTF POSITION accessor %d must be FLOAT VEC3", idx)
	}
	view, buf, off, stride, err := gltfAccessorBytes(doc, buffers, acc)
	if err != nil {
		return nil, err
	}
	if stride == 0 {
		stride = 12
	}
	out := make([]fakeVec3, acc.Count)
	for i := 0; i < acc.Count; i++ {
		p := off + i*stride
		if p+12 > view.ByteLength+view.ByteOffset || p+12 > len(buf) {
			return nil, fmt.Errorf("glTF POSITION accessor %d is truncated", idx)
		}
		v := fakeVec3{
			X: float64(math.Float32frombits(binary.LittleEndian.Uint32(buf[p : p+4]))),
			Y: float64(math.Float32frombits(binary.LittleEndian.Uint32(buf[p+4 : p+8]))),
			Z: float64(math.Float32frombits(binary.LittleEndian.Uint32(buf[p+8 : p+12]))),
		}
		if !finiteVec(v) {
			return nil, fmt.Errorf("glTF POSITION accessor %d contains non-finite coordinate", idx)
		}
		out[i] = transformPoint(world, v)
	}
	return out, nil
}

func gltfReadIndices(doc gltfDoc, buffers [][]byte, idx *int, vertexCount int) ([]int, error) {
	if idx == nil {
		out := make([]int, vertexCount)
		for i := range out {
			out[i] = i
		}
		return out, nil
	}
	if *idx < 0 || *idx >= len(doc.Accessors) {
		return nil, fmt.Errorf("glTF index accessor %d out of range", *idx)
	}
	acc := doc.Accessors[*idx]
	if acc.BufferView == nil || acc.Type != "SCALAR" {
		return nil, fmt.Errorf("glTF index accessor %d must be SCALAR", *idx)
	}
	view, buf, off, stride, err := gltfAccessorBytes(doc, buffers, acc)
	if err != nil {
		return nil, err
	}
	width := 0
	switch acc.ComponentType {
	case 5121:
		width = 1
	case 5123:
		width = 2
	case 5125:
		width = 4
	default:
		return nil, fmt.Errorf("unsupported glTF index component type %d", acc.ComponentType)
	}
	if stride == 0 {
		stride = width
	}
	out := make([]int, acc.Count)
	for i := 0; i < acc.Count; i++ {
		p := off + i*stride
		if p+width > view.ByteLength+view.ByteOffset || p+width > len(buf) {
			return nil, fmt.Errorf("glTF index accessor %d is truncated", *idx)
		}
		switch width {
		case 1:
			out[i] = int(buf[p])
		case 2:
			out[i] = int(binary.LittleEndian.Uint16(buf[p:]))
		case 4:
			out[i] = int(binary.LittleEndian.Uint32(buf[p:]))
		}
	}
	return out, nil
}

func gltfAccessorBytes(doc gltfDoc, buffers [][]byte, acc gltfAccessor) (gltfBufferView, []byte, int, int, error) {
	viewIdx := *acc.BufferView
	if viewIdx < 0 || viewIdx >= len(doc.BufferViews) {
		return gltfBufferView{}, nil, 0, 0, fmt.Errorf("glTF bufferView %d out of range", viewIdx)
	}
	view := doc.BufferViews[viewIdx]
	if view.Buffer < 0 || view.Buffer >= len(buffers) {
		return gltfBufferView{}, nil, 0, 0, fmt.Errorf("glTF buffer %d out of range", view.Buffer)
	}
	buf := buffers[view.Buffer]
	off := view.ByteOffset + acc.ByteOffset
	if off < 0 || off > len(buf) || view.ByteOffset+view.ByteLength > len(buf) {
		return gltfBufferView{}, nil, 0, 0, errors.New("glTF accessor outside buffer")
	}
	return view, buf, off, view.ByteStride, nil
}

func identityMatrix() [16]float64 {
	return [16]float64{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
}

func nodeMatrix(node gltfNode) [16]float64 {
	if len(node.Matrix) == 16 {
		var m [16]float64
		copy(m[:], node.Matrix)
		return m
	}
	t := fakeVec3{}
	if len(node.Translation) >= 3 {
		t = fakeVec3{node.Translation[0], node.Translation[1], node.Translation[2]}
	}
	s := fakeVec3{1, 1, 1}
	if len(node.Scale) >= 3 {
		s = fakeVec3{node.Scale[0], node.Scale[1], node.Scale[2]}
	}
	q := [4]float64{0, 0, 0, 1}
	if len(node.Rotation) >= 4 {
		copy(q[:], node.Rotation[:4])
	}
	return composeTRS(t, q, s)
}

func composeTRS(t fakeVec3, q [4]float64, s fakeVec3) [16]float64 {
	x, y, z, w := q[0], q[1], q[2], q[3]
	l := math.Sqrt(x*x + y*y + z*z + w*w)
	if l > 0 {
		x, y, z, w = x/l, y/l, z/l, w/l
	}
	x2, y2, z2 := x+x, y+y, z+z
	xx, xy, xz := x*x2, x*y2, x*z2
	yy, yz, zz := y*y2, y*z2, z*z2
	wx, wy, wz := w*x2, w*y2, w*z2
	return [16]float64{
		(1 - (yy + zz)) * s.X, (xy + wz) * s.X, (xz - wy) * s.X, 0,
		(xy - wz) * s.Y, (1 - (xx + zz)) * s.Y, (yz + wx) * s.Y, 0,
		(xz + wy) * s.Z, (yz - wx) * s.Z, (1 - (xx + yy)) * s.Z, 0,
		t.X, t.Y, t.Z, 1,
	}
}

func multiplyMatrix(a, b [16]float64) [16]float64 {
	var out [16]float64
	for col := 0; col < 4; col++ {
		for row := 0; row < 4; row++ {
			out[col*4+row] =
				a[0*4+row]*b[col*4+0] +
					a[1*4+row]*b[col*4+1] +
					a[2*4+row]*b[col*4+2] +
					a[3*4+row]*b[col*4+3]
		}
	}
	return out
}

func transformPoint(m [16]float64, v fakeVec3) fakeVec3 {
	return fakeVec3{
		X: m[0]*v.X + m[4]*v.Y + m[8]*v.Z + m[12],
		Y: m[1]*v.X + m[5]*v.Y + m[9]*v.Z + m[13],
		Z: m[2]*v.X + m[6]*v.Y + m[10]*v.Z + m[14],
	}
}

func trianglesBounds(tris []fakeTriangle) fakeBounds {
	minV := fakeVec3{math.Inf(1), math.Inf(1), math.Inf(1)}
	maxV := fakeVec3{math.Inf(-1), math.Inf(-1), math.Inf(-1)}
	for _, tri := range tris {
		for _, v := range []fakeVec3{tri.A, tri.B, tri.C} {
			minV.X = math.Min(minV.X, v.X)
			minV.Y = math.Min(minV.Y, v.Y)
			minV.Z = math.Min(minV.Z, v.Z)
			maxV.X = math.Max(maxV.X, v.X)
			maxV.Y = math.Max(maxV.Y, v.Y)
			maxV.Z = math.Max(maxV.Z, v.Z)
		}
	}
	return fakeBounds{Min: minV, Max: maxV}
}

func finiteVec(v fakeVec3) bool {
	return fakeFinite(v.X) && fakeFinite(v.Y) && fakeFinite(v.Z)
}
