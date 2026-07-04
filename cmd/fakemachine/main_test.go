package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/uwin/cnc-proxy/internal/carveratest"
)

func TestSidecarServesSnapshotAndAssets(t *testing.T) {
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.PutFile("/sd/gcodes/a.nc", []byte("G1 X1\n"))

	srv := httptest.NewServer(sidecarHandler(m))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/state status = %d", resp.StatusCode)
	}
	var snap carveratest.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snap.Status.Raw == "" || len(snap.Files) != 1 || snap.Files[0].Path != "/sd/gcodes/a.nc" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.MachineProfile.Model != "CA1" || snap.MachineProfile.WorkSizeXMM != 300 || snap.Config["coordinate.worksize_y"] != "200.0" {
		t.Fatalf("machine profile/config = %+v config=%v", snap.MachineProfile, snap.Config)
	}
	if snap.Status.Tool == nil || snap.Status.Tool.Active != 0 || snap.Status.Tool.Offset != 0 {
		t.Fatalf("tool status = %+v", snap.Status.Tool)
	}

	asset, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer asset.Body.Close()
	body, _ := io.ReadAll(asset.Body)
	if asset.StatusCode != http.StatusOK || !strings.Contains(string(body), "Fakemachine Sidecar") {
		t.Fatalf("index status=%d body=%q", asset.StatusCode, string(body[:min(len(body), 80)]))
	}
	index := string(body)
	for _, want := range []string{"tool Z0", `id="laser-toggle"`, `aria-pressed="false"`, "tool laser"} {
		if !strings.Contains(index, want) {
			t.Fatalf("sidecar index missing legend marker %q", want)
		}
	}
	for _, gone := range []string{"work zero", "WCS Z0", "tool tip", ".swatch.tip", "--tip", "probe laser"} {
		if strings.Contains(index, gone) {
			t.Fatalf("sidecar index still contains obsolete legend marker %q", gone)
		}
	}

	app, err := http.Get(srv.URL + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Body.Close()
	jsBody, _ := io.ReadAll(app.Body)
	js := string(jsBody)
	for _, want := range []string{
		"box(profile.workX, 12, profile.workY",
		"box(profile.workX, 4, profile.workY",
		"profile.workYMax, 1, 0.34, mat.tableGridMinor",
		"profile.workYMax, 10, 0.38, mat.tableGridMajor",
		"function steppedGridValues",
		"function visualWorkOffset(point, wpos, tool)",
		"z: point.z - number(wpos.z) - toolStickout(tool),",
		"let toolLaserEnabled = false",
		"function updateToolLaser(snap, point)",
		"const startY = toolLaserTipY(snap.inserted_tool)",
		"new THREE.Mesh(new THREE.CylinderGeometry(0.7, 0.7, 1",
		"laserBeam.renderOrder = 20",
		"const length = Math.max(1, startY - endY)",
		"parts.laserBeam.scale.set(1, length, 1)",
		"laserToggleButtonEl.addEventListener(\"click\"",
		"function panCamera(dx, dy)",
		"event.button === 1 || event.button === 2 || event.shiftKey || event.altKey",
		"mode: pan ? \"pan\" : \"orbit\"",
		"pointer.mode === \"pan\"",
		"canvas.addEventListener(\"contextmenu\"",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("sidecar app.js missing configured table/grid marker %q", want)
		}
	}
	for _, gone := range []string{"profile.workX + 30", "profile.workY + 28", "profile.workYMax, 25,", "new THREE.Vector3(0, -16, 0), new THREE.Vector3(0, 16, 0)", "const toolOffsetZ", "visualWorkOffset(point, wpos);", "toolTip:", "toolTipCursor", "createToolTipCursor", "toolTipOffset", "cutCursor", "cutRing", "cutDot", "laserDot", "TorusGeometry", "const laserBeam = new THREE.Line"} {
		if strings.Contains(js, gone) {
			t.Fatalf("sidecar app.js still contains oversized/old grid marker %q", gone)
		}
	}
}

func TestSidecarInsertsTool(t *testing.T) {
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	srv := httptest.NewServer(sidecarHandler(m))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/tool/insert", strings.NewReader(`{"kind":"tool_6_35"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("insert status=%d body=%q", resp.StatusCode, string(b))
	}
	var tool carveratest.SnapshotInsertedTool
	if err := json.NewDecoder(resp.Body).Decode(&tool); err != nil {
		t.Fatal(err)
	}
	if tool.Kind != "tool_6_35" || tool.ToolID != 3 || tool.DiameterMM != 6.35 {
		t.Fatalf("inserted tool = %+v", tool)
	}
	if !tool.SpindleLocked || tool.Calibrated {
		t.Fatalf("inserted tool lock/calibration = %+v", tool)
	}

	req, err = http.NewRequest(http.MethodPost, srv.URL+"/api/tool/stickout", strings.NewReader(`{"stickout_mm":40}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	lockedResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	lockedResp.Body.Close()
	if lockedResp.StatusCode != http.StatusConflict {
		t.Fatalf("locked stickout status = %d, want 409", lockedResp.StatusCode)
	}

	req, err = http.NewRequest(http.MethodPost, srv.URL+"/api/tool/lock", strings.NewReader(`{"locked":false}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	unlockResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockResp.Body.Close()
	if unlockResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(unlockResp.Body)
		t.Fatalf("unlock status=%d body=%q", unlockResp.StatusCode, string(b))
	}

	req, err = http.NewRequest(http.MethodPost, srv.URL+"/api/tool/stickout", strings.NewReader(`{"stickout_mm":40}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	depthResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer depthResp.Body.Close()
	if depthResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(depthResp.Body)
		t.Fatalf("stickout status=%d body=%q", depthResp.StatusCode, string(b))
	}
	if err := json.NewDecoder(depthResp.Body).Decode(&tool); err != nil {
		t.Fatal(err)
	}
	if tool.StickoutMM != 40 || tool.SpindleLocked || tool.Calibrated {
		t.Fatalf("adjusted tool = %+v", tool)
	}

	stateResp, err := http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer stateResp.Body.Close()
	var snap carveratest.Snapshot
	if err := json.NewDecoder(stateResp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snap.InsertedTool == nil || snap.InsertedTool.Kind != "tool_6_35" || snap.InsertedTool.StickoutMM != 40 {
		t.Fatalf("snapshot inserted tool = %+v", snap.InsertedTool)
	}
	if snap.Status.Tool == nil || snap.Status.Tool.Active != 3 {
		t.Fatalf("snapshot tool status = %+v", snap.Status.Tool)
	}
}

func TestSidecarStreamsStateEvents(t *testing.T) {
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	srv := httptest.NewServer(sidecarHandler(m))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content-type = %q", got)
	}
	buf := make([]byte, 4096)
	n, err := resp.Body.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf[:n]), "event: state") || !strings.Contains(string(buf[:n]), `"status"`) {
		t.Fatalf("event chunk = %q", string(buf[:n]))
	}
}

func TestSidecarUploadsProbeModelAndServesMesh(t *testing.T) {
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	srv := httptest.NewServer(sidecarHandler(m))
	defer srv.Close()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("model", "plate.stl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(sidecarPlaneSTL())); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/model", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status=%d body=%q", resp.StatusCode, string(b))
	}
	var model carveratest.SnapshotProbeModel
	if err := json.NewDecoder(resp.Body).Decode(&model); err != nil {
		t.Fatal(err)
	}
	if model.Name != "plate.stl" || model.Triangles != 2 {
		t.Fatalf("model = %+v", model)
	}

	meshResp, err := http.Get(srv.URL + "/api/model/mesh")
	if err != nil {
		t.Fatal(err)
	}
	defer meshResp.Body.Close()
	if meshResp.StatusCode != http.StatusOK {
		t.Fatalf("mesh status = %d", meshResp.StatusCode)
	}
	var mesh carveratest.ProbeModelMesh
	if err := json.NewDecoder(meshResp.Body).Decode(&mesh); err != nil {
		t.Fatal(err)
	}
	if mesh.ID != model.ID || mesh.Triangles != 2 || len(mesh.Positions) != 18 {
		t.Fatalf("mesh = %+v", mesh)
	}
}

func sidecarPlaneSTL() string {
	return strings.Join([]string{
		"solid plate",
		"facet normal 0 0 1",
		"outer loop",
		"vertex 0 0 -5",
		"vertex 10 0 -5",
		"vertex 10 10 -5",
		"endloop",
		"endfacet",
		"facet normal 0 0 1",
		"outer loop",
		"vertex 0 0 -5",
		"vertex 10 10 -5",
		"vertex 0 10 -5",
		"endloop",
		"endfacet",
		"endsolid plate",
	}, "\n")
}
