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

type fakeProbeModel struct {
	ID        string
	Name      string
	Format    string
	Triangles []fakeTriangle
	Bounds    fakeBounds
	LoadedAt  time.Time
}

type ProbeModelMesh struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Format    string     `json:"format"`
	Triangles int        `json:"triangles"`
	Bounds    fakeBounds `json:"bounds"`
	Positions []float64  `json:"positions"`
}

type fakeProbeResult struct {
	Command string
	Hit     bool
	Machine fakeVec3
	Source  string
	At      time.Time
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
	if m.probeModel != nil {
		for _, tri := range m.probeModel.Triangles {
			if t, p, ok := segmentTriangleIntersection(start, end, tri); ok && t < bestT {
				bestT = t
				best = p
				source = m.probeModel.Name
			}
		}
	}
	if t, p, ok := m.bedPlaneContactLocked(start, end); ok && t < bestT {
		bestT = t
		best = p
		source = "bed"
	}
	if math.IsInf(bestT, 1) {
		return end, false, ""
	}
	return best, true, source
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
	m.probeModel = model
	m.lastProbe = nil
	return nil
}

func (m *FakeMachine) ClearProbeModel() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.probeModel = nil
	m.lastProbe = nil
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
		ID:        pm.ID,
		Name:      pm.Name,
		Format:    pm.Format,
		Triangles: len(pm.Triangles),
		Bounds:    pm.Bounds,
		Positions: positions,
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
		ID:        strconv.FormatInt(now.UnixNano(), 36),
		Name:      name,
		Format:    format,
		Triangles: triangles,
		Bounds:    bounds,
		LoadedAt:  now,
	}, nil
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
