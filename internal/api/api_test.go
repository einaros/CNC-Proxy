package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"time"

	"github.com/coder/websocket"
	"github.com/uwin/cnc-proxy/internal/carveratest"
	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/httpauth"
	"github.com/uwin/cnc-proxy/internal/jog"
	"github.com/uwin/cnc-proxy/internal/machine"
	"github.com/uwin/cnc-proxy/internal/service"
	"github.com/uwin/cnc-proxy/internal/session"
	"github.com/uwin/cnc-proxy/internal/store"
)

// do performs a request and fails the test on transport error, returning the
// response for status/body assertions. Keeps the error-checking in one place.
func do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	return resp
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	return do(t, req)
}

func newTestServer(t *testing.T) (*httptest.Server, *service.Service) {
	t.Helper()
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	arb := session.New(session.Config{Dial: func() (*client.Conn, error) { return nil, io.EOF }})
	svc, err := service.New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(svc).Handler())
	t.Cleanup(srv.Close)
	return srv, svc
}

func TestPostFileRawBody(t *testing.T) {
	srv, _ := newTestServer(t)
	req, _ := http.NewRequest("POST", srv.URL+"/api/files?path=part.nc", strings.NewReader("G0 X0\n"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	var entry store.Entry
	json.NewDecoder(resp.Body).Decode(&entry)
	if entry.Path != "/sd/gcodes/part.nc" || entry.Sync != store.PendingUpload {
		t.Errorf("entry = %+v", entry)
	}
}

func TestMutatingAPIRejectsCrossOriginBrowserRequests(t *testing.T) {
	srv, _ := newTestServer(t)
	req, _ := http.NewRequest("POST", srv.URL+"/api/gcode", strings.NewReader(`{"line":"M114"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", resp.StatusCode)
	}

	req, _ = http.NewRequest("POST", srv.URL+"/api/gcode", strings.NewReader(`{"line":"M114"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)
	resp = do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatalf("same-origin request was rejected")
	}
}

func TestBackupExportRejectsCrossOriginBrowserRequests(t *testing.T) {
	srv, _ := newTestServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/api/backup", nil)
	req.Header.Set("Origin", "http://evil.example")
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin backup status = %d, want 403", resp.StatusCode)
	}
}

func TestPostFileUploadLimit(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	arb := session.New(session.Config{Dial: func() (*client.Conn, error) { return nil, io.EOF }})
	svc, err := service.New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewWithOptions(svc, Options{MaxUploadBytes: 4}).Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/api/files?path=too-big.nc", strings.NewReader("12345"))
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("upload status = %d, want 413", resp.StatusCode)
	}
}

func TestAuthenticatedAPIRequests(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	arb := session.New(session.Config{Dial: func() (*client.Conn, error) { return nil, io.EOF }})
	svc, err := service.New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpauth.Middleware(httpauth.Config{User: "operator", Token: "secret"}, New(svc).Handler()))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/files", nil)
	resp := do(t, req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", srv.URL+"/api/files", nil)
	req.SetBasicAuth("operator", "secret")
	resp = do(t, req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", srv.URL+"/api/events", nil)
	req.SetBasicAuth("operator", "secret")
	resp = do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("authenticated SSE status=%d content-type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	req, _ = http.NewRequest("GET", srv.URL+"/healthz", nil)
	resp = do(t, req)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Fatalf("healthz status=%d body=%q", resp.StatusCode, body)
	}
}

func TestPostFileMultipart(t *testing.T) {
	srv, _ := newTestServer(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "drawing.nc")
	fw.Write([]byte("G1 X5 Y5\n"))
	mw.Close()

	resp, err := http.Post(srv.URL+"/api/files", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	var entry store.Entry
	json.NewDecoder(resp.Body).Decode(&entry)
	if entry.Path != "/sd/gcodes/drawing.nc" {
		t.Errorf("path = %q", entry.Path)
	}
}

func TestPostFileMultipartPreservesExactBytes(t *testing.T) {
	srv, _ := newTestServer(t)
	content := []byte{'G', '0', ' ', 'X', '0', '\r', '\n', 0, 'G', '1', ' ', 'X', '5', '\n'}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "exact.nc")
	if _, err := fw.Write(content); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	resp, err := http.Post(srv.URL+"/api/files", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	var entry store.Entry
	if err := json.NewDecoder(resp.Body).Decode(&entry); err != nil {
		t.Fatal(err)
	}
	if entry.Size != int64(len(content)) {
		t.Fatalf("multipart entry size = %d, want %d", entry.Size, len(content))
	}

	gotResp := get(t, srv.URL+"/api/files/exact.nc")
	defer gotResp.Body.Close()
	got, err := io.ReadAll(gotResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("multipart content = %v, want %v", got, content)
	}
}

func TestGetFilesAndContent(t *testing.T) {
	srv, _ := newTestServer(t)
	http.Post(srv.URL+"/api/files?path=a.nc", "application/octet-stream", strings.NewReader("hello"))

	resp := get(t, srv.URL+"/api/files")
	var files []store.Entry
	json.NewDecoder(resp.Body).Decode(&files)
	resp.Body.Close()
	if len(files) != 1 || files[0].Path != "/sd/gcodes/a.nc" {
		t.Fatalf("files = %+v", files)
	}

	// Content endpoint serves from cache.
	resp2 := get(t, srv.URL+"/api/files/a.nc")
	body, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if string(body) != "hello" {
		t.Errorf("content = %q", body)
	}
}

func TestJobsEndpointIncludesDiagnostics(t *testing.T) {
	srv, _ := newTestServer(t)
	http.Post(srv.URL+"/api/files?path=a.nc", "application/octet-stream", strings.NewReader("hello"))

	resp := get(t, srv.URL+"/api/jobs")
	defer resp.Body.Close()
	var jobs []store.Job
	if err := json.NewDecoder(resp.Body).Decode(&jobs); err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].BlockedReason != "stale_status" || jobs[0].BlockedMessage == "" {
		t.Fatalf("jobs = %+v", jobs)
	}
}

func TestBackupEndpoints(t *testing.T) {
	srv, _ := newTestServer(t)
	http.Post(srv.URL+"/api/files?path=a.nc", "application/octet-stream", strings.NewReader("hello"))

	resp := get(t, srv.URL+"/api/backup")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Disposition"), "cnc-proxy-backup.json") {
		t.Fatalf("backup export status=%d disposition=%q", resp.StatusCode, resp.Header.Get("Content-Disposition"))
	}
	var backup service.Backup
	if err := json.NewDecoder(resp.Body).Decode(&backup); err != nil {
		t.Fatal(err)
	}
	if backup.Version != 1 || len(backup.State.Entries) != 1 {
		t.Fatalf("backup = %+v", backup)
	}

	body, _ := json.Marshal(backup)
	req, _ := http.NewRequest("POST", srv.URL+"/api/backup/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	imp := do(t, req)
	defer imp.Body.Close()
	if imp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(imp.Body)
		t.Fatalf("backup import status=%d body=%s", imp.StatusCode, b)
	}

	backup.Version = 0
	body, _ = json.Marshal(backup)
	req, _ = http.NewRequest("POST", srv.URL+"/api/backup/import", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	bad := do(t, req)
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsupported backup version status=%d, want 400", bad.StatusCode)
	}
}

// TestSpacedFilenameRoundTrip ensures a filename with a space (which the web UI
// percent-encodes) can be uploaded, read back, and deleted through the API.
func TestSpacedFilenameRoundTrip(t *testing.T) {
	srv, _ := newTestServer(t)
	http.Post(srv.URL+"/api/files?path=my%20part.nc", "application/octet-stream", strings.NewReader("data"))

	// Read it back via a percent-encoded path.
	resp := get(t, srv.URL+"/api/files/my%20part.nc")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "data" {
		t.Fatalf("get spaced: status=%d body=%q", resp.StatusCode, body)
	}

	// Delete it via a percent-encoded path.
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/files/my%20part.nc", nil)
	dresp := do(t, req)
	defer dresp.Body.Close()
	if dresp.StatusCode != http.StatusAccepted {
		t.Errorf("delete spaced: status=%d", dresp.StatusCode)
	}
}

func TestDeleteEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)
	http.Post(srv.URL+"/api/files?path=a.nc", "application/octet-stream", strings.NewReader("x"))

	req, _ := http.NewRequest("DELETE", srv.URL+"/api/files/a.nc", nil)
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}

	// Deleting a non-existent file 404s.
	req2, _ := http.NewRequest("DELETE", srv.URL+"/api/files/missing.nc", nil)
	resp2 := do(t, req2)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("missing delete status = %d, want 404", resp2.StatusCode)
	}
}

func TestFileRetryAndDiscardEndpoints(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	arb := session.New(session.Config{Dial: func() (*client.Conn, error) { return nil, io.EOF }})
	svc, err := service.New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(svc).Handler())
	defer srv.Close()

	respUpload := postRaw(t, srv.URL+"/api/files?path=bad.nc", "x")
	respUpload.Body.Close()
	if respUpload.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d", respUpload.StatusCode)
	}
	if err := st.UpdateJob(1, func(j *store.Job) {
		j.State = store.Failed
		j.Attempts = 8
		j.LastError = "upload failed"
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEntrySync("/sd/gcodes/bad.nc", store.Error, "upload failed"); err != nil {
		t.Fatal(err)
	}

	retry := postJSON(t, srv.URL+"/api/files/retry", map[string]int64{"job_id": 1})
	retry.Body.Close()
	if retry.StatusCode != http.StatusAccepted {
		t.Fatalf("retry status = %d", retry.StatusCode)
	}
	if got := st.ListJobs()[0]; got.State != store.Queued || got.Attempts != 0 || got.LastError != "" {
		t.Fatalf("job after retry = %+v", got)
	}
	if got, _ := st.GetEntry("/sd/gcodes/bad.nc"); got.Sync != store.PendingUpload || got.Error != "" {
		t.Fatalf("entry after retry = %+v", got)
	}

	if err := st.UpdateJob(1, func(j *store.Job) {
		j.State = store.Failed
		j.Attempts = 8
		j.LastError = "upload failed again"
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEntrySync("/sd/gcodes/bad.nc", store.Error, "upload failed again"); err != nil {
		t.Fatal(err)
	}
	discard := postJSON(t, srv.URL+"/api/files/discard", map[string]string{"path": "bad.nc"})
	discard.Body.Close()
	if discard.StatusCode != http.StatusAccepted {
		t.Fatalf("discard status = %d", discard.StatusCode)
	}
	if _, ok := st.GetEntry("/sd/gcodes/bad.nc"); ok {
		t.Fatal("entry should be discarded")
	}
}

func TestRenameEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)
	http.Post(srv.URL+"/api/files?path=a.nc", "application/octet-stream", strings.NewReader("x"))

	body, _ := json.Marshal(map[string]string{"from": "a.nc", "to": "b.nc"})
	req, _ := http.NewRequest("POST", srv.URL+"/api/files/rename", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}
}

func TestMachineEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)
	reqUpload, _ := http.NewRequest("POST", srv.URL+"/api/files?path=queued.nc", strings.NewReader("x"))
	reqUpload.Header.Set("Content-Type", "application/octet-stream")
	respUpload := do(t, reqUpload)
	respUpload.Body.Close()
	if respUpload.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d", respUpload.StatusCode)
	}
	resp := get(t, srv.URL+"/api/machine")
	defer resp.Body.Close()
	var st service.MachineStatus
	json.NewDecoder(resp.Body).Decode(&st)
	if st.Mode != "owner" {
		t.Errorf("mode = %q, want owner", st.Mode)
	}
	if st.PendingJobs != 1 {
		t.Errorf("pending_jobs = %d, want 1", st.PendingJobs)
	}
}

func TestMachineStatusEndpointRichFields(t *testing.T) {
	srv, _, tr := serverWithMachine(t)
	tr.ObserveStatusPayload("<Idle|MPos:1,2,3|WPos:4,5,6|F:0,100,100>")
	resp := get(t, srv.URL+"/api/machine/status")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var st service.MachineStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.State != machine.Idle || st.MPos["x"] != 1 || st.WPos["z"] != 6 || st.Feed == nil {
		t.Fatalf("machine status = %+v", st)
	}
}

func TestRunsEndpointDerivesObservedRun(t *testing.T) {
	srv, _, tr := serverWithMachine(t)
	tr.Observe(machine.Idle)
	resp := postJSON(t, srv.URL+"/api/gcode", map[string]string{"line": "play /sd/gcodes/a.nc"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("play command status = %d", resp.StatusCode)
	}
	tr.ObserveStatusPayload("<Run|MPos:0,0,0|F:100,200,100|S:5000,12000,80|P:1,10,1>")
	tr.ObserveStatusPayload("<Idle|MPos:0,0,0>")

	var runs []struct {
		File string `json:"file"`
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		r := get(t, srv.URL+"/api/runs")
		json.NewDecoder(r.Body).Decode(&runs)
		r.Body.Close()
		if len(runs) == 1 && runs[0].File == "/sd/gcodes/a.nc" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("runs = %+v, want observed file", runs)
}

func TestWebUIServed(t *testing.T) {
	srv, _ := newTestServer(t)

	resp := get(t, srv.URL+"/")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<!DOCTYPE html>") {
		t.Errorf("index status=%d body-start=%.30q", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `id="tab-files"`) || !strings.Contains(string(body), `id="control-view"`) {
		t.Errorf("index missing lazy tab markup")
	}
	if !strings.Contains(string(body), `id="jog-plot"`) || !strings.Contains(string(body), `id="status-connection"`) {
		t.Errorf("index missing jog visualization or connection status")
	}
	for _, want := range []string{`[hidden] { display: none !important; }`, `id="status-bar"`, `id="notice-clear"`, `.status-item`, `.jobs-head`, `.job-recovery`, `id="status-fields"`, `id="alarm-panel"`, `id="alarm-recover"`, `id="alarm-feedback"`, `data-control-action="recover"`, `id="ctl-home-main"`, `data-control-action="home"`, `data-gcode="M114"`, `id="log-filter"`, `id="gcode-history"`, `id="file-summary"`, `id="tool-panel"`, `id="tool-set"`, `id="active-gcode-panel"`, `id="gcode-preview"`, `id="gcode-timeline"`, `type="module"`, `/app.js?v=gcode-3d-1`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("index missing %s", want)
		}
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("index Cache-Control = %q, want no-store", got)
	}
	for _, want := range []string{`id="file-browser"`, `id="folder-tree"`, `id="breadcrumbs"`, `id="folder-up"`, `id="folder-new"`, `id="current-folder"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("index missing %s", want)
		}
	}
	for _, want := range []string{`id="macro-toolbar"`, `id="macro-panel"`, `id="macro-manager"`, `id="macro-save"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("index missing %s", want)
		}
	}
	for _, want := range []string{`id="gamepad-settings"`, `id="gamepad-axis-x"`, `id="gamepad-speed-z"`, `id="gamepad-macro-bindings"`, `id="gamepad-add-macro"`, `id="jog-buttons"`, `id="jog-target-pos"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("index missing %s", want)
		}
	}
	for _, want := range []string{`class="metric-grid"`, `class="jog-body"`, `class="metric metric--primary"`, `class="table-scroll"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("index missing layout marker %s", want)
		}
	}

	js := get(t, srv.URL+"/app.js")
	jsBody, _ := io.ReadAll(js.Body)
	js.Body.Close()
	if js.StatusCode != http.StatusOK || !strings.Contains(string(jsBody), "EventSource") {
		t.Errorf("app.js status=%d", js.StatusCode)
	}
	if !strings.Contains(string(jsBody), "/api/events?scope=control") || !strings.Contains(string(jsBody), "/api/events?scope=files") {
		t.Errorf("app.js missing scoped event streams")
	}
	if !strings.Contains(string(jsBody), `clearNotice("control-sse")`) || !strings.Contains(string(jsBody), `clearNotice("files-sse")`) {
		t.Errorf("app.js missing stream reconnect notice clearing")
	}
	if !strings.Contains(string(jsBody), `setNotice("Machine status unavailable: " + e.message, "error", "machine-status")`) || !strings.Contains(string(jsBody), `clearNotice("machine-status")`) {
		t.Errorf("app.js missing machine status notice lifecycle")
	}
	if !strings.Contains(string(jsBody), "refreshJobs") || !strings.Contains(string(jsBody), "/api/jobs") {
		t.Errorf("app.js missing active job diagnostic refresh")
	}
	if !strings.Contains(string(jsBody), "renderJogPlot") || !strings.Contains(string(jsBody), "jogPanelMessage") || !strings.Contains(string(jsBody), "/api/machine/status") || !strings.Contains(string(jsBody), "motion_estimated") {
		t.Errorf("app.js missing jog plot, jog status messaging, or cache-only status polling")
	}
	if got := js.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("app.js Cache-Control = %q, want no-store", got)
	}
	three := get(t, srv.URL+"/three.module.min.js")
	threeBody, _ := io.ReadAll(three.Body)
	three.Body.Close()
	if three.StatusCode != http.StatusOK || !strings.Contains(string(threeBody[:min(len(threeBody), 200)]), "Three.js Authors") {
		t.Errorf("three.module.min.js status=%d", three.StatusCode)
	}
	if got := three.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("three.module.min.js Cache-Control = %q, want no-store", got)
	}
	for _, want := range []string{"rememberCommand", "navigateCommandHistory", "renderStatusFields", "renderAlarmPanel", "HALT_REASON", "controlPendingText", "controlSuccessText", "confirmControl", "bindDataControlButtons", "data-control-action", "renderFileSummary", "lineMatchesFilter", "selectActiveGcode", "runActiveGcode", "drawGcodePreview", "THREE.WebGLRenderer", "gcodeWorldPoint", "/api/gcode/active", "/api/tool/current", "/api/tool/calibrate"} {
		if !strings.Contains(string(jsBody), want) {
			t.Errorf("app.js missing %s", want)
		}
	}
	if strings.Contains(string(jsBody), "Emergency halt the machine?") {
		t.Errorf("app.js must not confirm emergency halt")
	}
	for _, want := range []string{"directoryRows", "renderFolderTree", "renderFolderChrome", "openDir", "doMkdir", "joinRelPath", "retryJob", "discardFile", "/api/files/retry", "/api/files/discard"} {
		if !strings.Contains(string(jsBody), want) {
			t.Errorf("app.js missing %s", want)
		}
	}
	for _, want := range []string{"loadUISettings", "saveUISettings", "renderMacroButtons", "runMacro", "/api/ui/settings"} {
		if !strings.Contains(string(jsBody), want) {
			t.Errorf("app.js missing %s", want)
		}
	}
	for _, want := range []string{"defaultGamepadSettings", "renderGamepadSettings", "mappedAxis", "handleGamepadMacroButtons", "addGamepadMacroBinding", "pressedButtonList", "gamepadLabel", "Xbox-compatible gamepad", "standard gamepad"} {
		if !strings.Contains(string(jsBody), want) {
			t.Errorf("app.js missing %s", want)
		}
	}
	for _, want := range []string{"scheduleJogReconnect", "clearJogReconnect", "preferredPadIndex", "visibilitychange"} {
		if !strings.Contains(string(jsBody), want) {
			t.Errorf("app.js missing jog reconnect behavior %s", want)
		}
	}
}

func TestUISettingsAPI(t *testing.T) {
	srv, _ := newTestServer(t)

	resp := get(t, srv.URL+"/api/ui/settings")
	var initial store.UISettings
	json.NewDecoder(resp.Body).Decode(&initial)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || initial.Log.Filter != "all" || !initial.Log.Autoscroll {
		t.Fatalf("initial settings status=%d value=%+v", resp.StatusCode, initial)
	}
	if initial.Macros == nil || initial.MacroButtons == nil || initial.Gamepad.SlowButtons == nil || initial.Gamepad.MacroButtons == nil {
		t.Fatalf("initial settings should use empty arrays, got %+v", initial)
	}
	if initial.Gamepad.Axes.Y.Axis != 1 || !initial.Gamepad.Axes.Y.Invert || initial.Gamepad.Axes.Z.Axis != 3 {
		t.Fatalf("initial gamepad defaults = %+v", initial.Gamepad)
	}

	body := `{
		"macros":[{"id":"m1","name":"Probe","lines":["G38.2 Z-5 F50","G10 L20 P1 Z0"],"color":"#44c27b"}],
		"macro_buttons":[{"id":"b1","macro_id":"m1","region":"toolbar","order":2}],
		"log":{"filter":"jog","autoscroll":false},
		"gamepad":{
			"axes":{
				"x":{"axis":2,"invert":false,"scale":0.5},
				"y":{"axis":1,"invert":false,"scale":0.75},
				"z":{"axis":3,"invert":true,"scale":0.25}
			},
			"deadman_button":7,
			"slow_buttons":[6],
			"macro_buttons":[{"id":"gp1","button":1,"macro_id":"m1"}]
		}
	}`
	req, _ := http.NewRequest("PUT", srv.URL+"/api/ui/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp = do(t, req)
	var saved store.UISettings
	json.NewDecoder(resp.Body).Decode(&saved)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(saved.Macros) != 1 || len(saved.MacroButtons) != 1 {
		t.Fatalf("saved settings status=%d value=%+v", resp.StatusCode, saved)
	}
	if saved.Log.Filter != "jog" || saved.Log.Autoscroll {
		t.Fatalf("saved log settings = %+v", saved.Log)
	}
	if saved.Gamepad.Axes.X.Axis != 2 || saved.Gamepad.Axes.X.Scale != 0.5 || saved.Gamepad.DeadmanButton != 7 {
		t.Fatalf("saved gamepad settings = %+v", saved.Gamepad)
	}
	if len(saved.Gamepad.MacroButtons) != 1 || saved.Gamepad.MacroButtons[0].Button != 1 {
		t.Fatalf("saved gamepad macro buttons = %+v", saved.Gamepad.MacroButtons)
	}

	resp = get(t, srv.URL+"/api/ui/settings")
	var got store.UISettings
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if len(got.Macros) != 1 || got.Macros[0].Name != "Probe" || got.MacroButtons[0].Region != "toolbar" {
		t.Fatalf("round trip settings = %+v", got)
	}
	if got.Gamepad.Axes.Z.Scale != 0.25 || len(got.Gamepad.SlowButtons) != 1 || got.Gamepad.SlowButtons[0] != 6 {
		t.Fatalf("round trip gamepad settings = %+v", got.Gamepad)
	}
}

func TestUISettingsAPIRejectsInvalidMacro(t *testing.T) {
	srv, _ := newTestServer(t)
	req, _ := http.NewRequest("PUT", srv.URL+"/api/ui/settings", strings.NewReader(`{"macros":[{"id":"bad","name":"Bad","lines":["   "]}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestUISettingsAPIRejectsInvalidGamepad(t *testing.T) {
	srv, _ := newTestServer(t)
	body := `{
		"macros":[{"id":"m1","name":"Position","lines":["M114"]}],
		"gamepad":{"axes":{"x":{"axis":99,"scale":1}}}
	}`
	req, _ := http.NewRequest("PUT", srv.URL+"/api/ui/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := do(t, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// serverWithMachine wires the API to a real fake machine + tracker so gcode and
// control endpoints can be exercised end to end with controllable state.
func serverWithMachine(t *testing.T) (*httptest.Server, *carveratest.FakeMachine, *machine.Tracker) {
	t.Helper()
	srv, m, tr, _, _ := serverWithMachineState(t)
	return srv, m, tr
}

func serverWithMachineState(t *testing.T) (*httptest.Server, *carveratest.FakeMachine, *machine.Tracker, *service.Service, *store.Store) {
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
		Dial: func() (*client.Conn, error) {
			return client.Dial(m.Addr(), 2*time.Second, client.WithUploadStartDelay(0))
		},
	})
	svc, err := service.New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(svc).Handler())
	t.Cleanup(srv.Close)
	return srv, m, tr, svc, st
}

func serverWithJog(t *testing.T, auth bool) (*httptest.Server, *carveratest.FakeMachine, *machine.Tracker) {
	t.Helper()
	m, err := carveratest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	status := "<Idle|MPos:0,0,0|WPos:0,0,0|F:0,0,100|S:0,0,100>"
	m.SetStatus(status)
	st, _ := store.Open(filepath.Join(t.TempDir(), "state.json"))
	tr := machine.NewTracker()
	if !tr.ObserveStatusPayload(status) {
		t.Fatal("status precondition failed")
	}
	arb := session.New(session.Config{
		Tracker:     tr,
		StateMaxAge: time.Second,
		Dial: func() (*client.Conn, error) {
			return client.Dial(m.Addr(), 2*time.Second, client.WithUploadStartDelay(0))
		},
	})
	svc, err := service.New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	cfg := jog.DefaultConfig()
	cfg.Tick = 20 * time.Millisecond
	cfg.StatusInterval = 40 * time.Millisecond
	cfg.DeadmanTimeout = 120 * time.Millisecond
	h := NewWithOptions(svc, Options{Jog: jog.New(arb, cfg)}).Handler()
	if auth {
		h = httpauth.Middleware(httpauth.Config{User: "operator", Token: "secret"}, h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, m, tr
}

func TestJogCapabilities(t *testing.T) {
	srv, _, _ := serverWithJog(t, false)
	resp := get(t, srv.URL+"/api/jog/capabilities")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var caps jog.Capabilities
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	if !caps.Enabled || caps.Axes[0] != "x" || !caps.Availability.Available {
		t.Fatalf("capabilities = %+v", caps)
	}
}

func TestJogWebSocketAuth(t *testing.T) {
	srv, _, _ := serverWithJog(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsURL(srv.URL), nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated dial err=%v resp=%v", err, resp)
	}

	c, _, err := websocket.Dial(ctx, wsURL(srv.URL), &websocket.DialOptions{
		HTTPHeader: basicAuthHeader("operator", "secret"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	if ev := readWSEvent(t, c, "hello"); ev.Type != "hello" || ev.Capabilities == nil || !ev.Capabilities.Availability.Available {
		t.Fatalf("event = %+v", ev)
	}
}

func TestJogWebSocketBadAxis(t *testing.T) {
	srv, _, _ := serverWithJog(t, false)
	c := dialWS(t, srv.URL)
	defer c.Close(websocket.StatusNormalClosure, "")
	readWSEvent(t, c, "hello")
	writeWS(t, c, map[string]any{"type": "input", "seq": 1, "deadman": true, "axes": map[string]float64{"a": 1}})
	ev := readWSEvent(t, c, "error")
	if ev.Code != jog.CodeBadInput {
		t.Fatalf("error = %+v", ev)
	}
}

func TestJogWebSocketDuplicateSession(t *testing.T) {
	srv, _, _ := serverWithJog(t, false)
	first := dialWS(t, srv.URL)
	defer first.Close(websocket.StatusNormalClosure, "")
	readWSEvent(t, first, "hello")

	second := dialWS(t, srv.URL)
	defer second.Close(websocket.StatusNormalClosure, "")
	ev := readWSEvent(t, second, "error")
	if ev.Code != jog.CodeBusy {
		t.Fatalf("second session error = %+v", ev)
	}
}

func TestJogWebSocketArmAndInput(t *testing.T) {
	srv, m, _ := serverWithJog(t, false)
	c := dialWS(t, srv.URL)
	defer c.Close(websocket.StatusNormalClosure, "")
	readWSEvent(t, c, "hello")
	writeWS(t, c, map[string]any{"type": "arm", "seq": 1})
	readWSEvent(t, c, "ack")
	writeWS(t, c, map[string]any{"type": "input", "seq": 2, "deadman": true, "axes": map[string]float64{"x": 1}})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, line := range m.Gcodes() {
			if strings.HasPrefix(line, "$J X") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no jog command observed: %v", m.Gcodes())
}

func TestJogWebSocketStatusTimeoutDoesNotCloseSession(t *testing.T) {
	srv, m, _ := serverWithJog(t, false)
	c := dialWS(t, srv.URL)
	defer c.Close(websocket.StatusNormalClosure, "")
	readWSEvent(t, c, "hello")
	writeWS(t, c, map[string]any{"type": "arm", "seq": 1})
	readWSEvent(t, c, "ack")

	m.SetStatus("<Run|MPos:0,0,0|WPos:0,0,0>")
	m.SetStatusReplyDelay(500 * time.Millisecond)
	writeWS(t, c, map[string]any{"type": "input", "seq": 2, "deadman": true, "axes": map[string]float64{"x": 1}})
	ev := readWSEvent(t, c, "error")
	if ev.Code != jog.CodeStatusWaiting {
		t.Fatalf("error = %+v, want status_waiting", ev)
	}

	writeWS(t, c, map[string]any{"type": "disarm", "seq": 3})
	ack := readWSEvent(t, c, "ack")
	if ack.Seq != 3 {
		t.Fatalf("ack = %+v, want disarm ack", ack)
	}
}

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http") + "/api/jog/ws"
}

func basicAuthHeader(user, pass string) http.Header {
	h := http.Header{}
	token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	h.Set("Authorization", "Basic "+token)
	return h
}

func dialWS(t *testing.T, serverURL string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(serverURL), nil)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func writeWS(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func readWSEvent(t *testing.T, c *websocket.Conn, typ string) jog.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		_, b, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read websocket event %q: %v", typ, err)
		}
		var ev jog.Event
		if err := json.Unmarshal(b, &ev); err != nil {
			t.Fatalf("decode event %q: %v", string(b), err)
		}
		if ev.Type == typ {
			return ev
		}
	}
	t.Fatalf("timeout waiting for websocket event %q", typ)
	return jog.Event{}
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return do(t, req)
}

func postRaw(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	return do(t, req)
}

func TestPostGcodeMotionGatedByState(t *testing.T) {
	srv, m, tr := serverWithMachine(t)

	// Running: a motion command is rejected with 503 and never reaches the machine.
	m.SetStatus("<Run|MPos:0,0,0|WPos:0,0,0>")
	tr.Observe(machine.Run)
	resp := postJSON(t, srv.URL+"/api/gcode", map[string]string{"line": "G91 G0 X-10"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("motion during Run: status = %d, want 503", resp.StatusCode)
	}
	if g := m.Gcodes(); len(g) != 0 {
		t.Fatalf("motion leaked to machine: %v", g)
	}

	// Idle: accepted.
	m.SetStatus("<Idle|MPos:0,0,0|WPos:0,0,0>")
	tr.Observe(machine.Idle)
	resp2 := postJSON(t, srv.URL+"/api/gcode", map[string]string{"line": "G91 G0 X-10"})
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("motion during Idle: status = %d, want 200", resp2.StatusCode)
	}
}

func TestPostGcodeQueryNotGated(t *testing.T) {
	srv, m, tr := serverWithMachine(t)
	m.SetGcodeReply("M114", "ok C: X:1.0")
	tr.Observe(machine.Run) // still running

	resp := postJSON(t, srv.URL+"/api/gcode", map[string]string{"line": "M114"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query during Run: status = %d, want 200", resp.StatusCode)
	}
	var out map[string]string
	json.NewDecoder(resp.Body).Decode(&out)
	if out["output"] != "C: X:1.0" {
		t.Errorf("output = %q", out["output"])
	}
}

func TestActiveGcodeEndpoints(t *testing.T) {
	srv, m, tr, _, st := serverWithMachineState(t)
	tr.Observe(machine.Idle)

	up := postRaw(t, srv.URL+"/api/files?path=my%20part.nc", "G90\nG0 X0 Y0\nG1 X5 Y5\n")
	up.Body.Close()
	if up.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d", up.StatusCode)
	}
	if err := st.SetEntrySync("/sd/gcodes/my part.nc", store.Synced, ""); err != nil {
		t.Fatal(err)
	}

	selectResp := postJSON(t, srv.URL+"/api/gcode/active", map[string]string{"path": "my part.nc"})
	defer selectResp.Body.Close()
	if selectResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(selectResp.Body)
		t.Fatalf("select status=%d body=%s", selectResp.StatusCode, b)
	}
	var active service.ActiveGcode
	if err := json.NewDecoder(selectResp.Body).Decode(&active); err != nil {
		t.Fatal(err)
	}
	if active.Path != "/sd/gcodes/my part.nc" || !active.Runnable || active.Preview == nil || active.Preview.MoveCount != 1 {
		t.Fatalf("active = %+v", active)
	}

	req, _ := http.NewRequest("POST", srv.URL+"/api/gcode/active/run", nil)
	runResp := do(t, req)
	defer runResp.Body.Close()
	if runResp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(runResp.Body)
		t.Fatalf("run status=%d body=%s", runResp.StatusCode, b)
	}
	if g := m.Gcodes(); len(g) != 1 || g[0] != "play /sd/gcodes/my part.nc" {
		t.Fatalf("machine gcodes = %v, want play command", g)
	}
}

func TestToolActionEndpoints(t *testing.T) {
	srv, m, tr := serverWithMachine(t)
	tr.Observe(machine.Idle)

	setResp := postJSON(t, srv.URL+"/api/tool/current", map[string]int{"tool_id": 4})
	setResp.Body.Close()
	if setResp.StatusCode != http.StatusAccepted {
		t.Fatalf("set tool status = %d", setResp.StatusCode)
	}
	req, _ := http.NewRequest("POST", srv.URL+"/api/tool/calibrate", nil)
	calResp := do(t, req)
	calResp.Body.Close()
	if calResp.StatusCode != http.StatusAccepted {
		t.Fatalf("calibrate status = %d", calResp.StatusCode)
	}
	if g := m.Gcodes(); len(g) != 2 || g[0] != "M493.2T4" || g[1] != "M491" {
		t.Fatalf("machine gcodes = %v, want tool commands", g)
	}
}

func TestPostControl(t *testing.T) {
	srv, m, tr := serverWithMachine(t)
	tr.Observe(machine.Run) // control works even while running

	for _, action := range []string{"hold", "resume", "halt"} {
		resp := postJSON(t, srv.URL+"/api/control", map[string]string{"action": action})
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("control %q: status = %d, want 202", action, resp.StatusCode)
		}
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(m.Controls()) < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := m.Controls(); len(got) != 3 || got[0] != '!' || got[1] != '~' || got[2] != 0x18 {
		t.Errorf("controls = %v, want [! ~ 0x18]", got)
	}

	// Unknown action → 400.
	resp := postJSON(t, srv.URL+"/api/control", map[string]string{"action": "wiggle"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown action: status = %d, want 400", resp.StatusCode)
	}
}

func TestPostControlAlarmRecovery(t *testing.T) {
	srv, m, tr := serverWithMachine(t)
	tr.ObserveStatusPayload("<Alarm|MPos:0,0,0|H:10>")

	resp := postJSON(t, srv.URL+"/api/control", map[string]string{"action": "recover"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("recover status = %d, want 202", resp.StatusCode)
	}
	var result service.RecoveryResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if !result.Recovered || result.State != machine.Idle || !result.NeedsHome {
		t.Fatalf("recover result = %+v, want recovered Idle with needs_home", result)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, line := range m.Gcodes() {
			if line == "$X" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("unlock did not reach machine: %v", m.Gcodes())
}

func TestPostControlAlarmRecoveryRejectsWrongAction(t *testing.T) {
	srv, m, tr := serverWithMachine(t)
	tr.ObserveStatusPayload("<Alarm|MPos:0,0,0|H:21>")

	resp := postJSON(t, srv.URL+"/api/control", map[string]string{"action": "unlock"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("unlock hard fault status = %d, want 409", resp.StatusCode)
	}
	if g := m.Gcodes(); len(g) != 0 {
		t.Fatalf("wrong recovery action reached machine: %v", g)
	}

	resp = postJSON(t, srv.URL+"/api/control", map[string]string{"action": "reset"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("reset status = %d, want 202", resp.StatusCode)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, line := range m.Gcodes() {
			if line == "reset" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("reset did not reach machine: %v", m.Gcodes())
}

func TestPostControlHomeAllowedDuringUnlockableAlarm(t *testing.T) {
	srv, m, tr := serverWithMachine(t)
	tr.ObserveStatusPayload("<Alarm|MPos:0,0,0|H:10>")

	resp := postJSON(t, srv.URL+"/api/control", map[string]string{"action": "home"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("home status = %d, want 202", resp.StatusCode)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, line := range m.Gcodes() {
			if line == "$H" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("home did not reach machine: %v", m.Gcodes())
}

func TestPostControlHomeRejectsHardFaultAlarm(t *testing.T) {
	srv, m, tr := serverWithMachine(t)
	tr.ObserveStatusPayload("<Alarm|MPos:0,0,0|H:21>")

	resp := postJSON(t, srv.URL+"/api/control", map[string]string{"action": "home"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("home hard fault status = %d, want 409", resp.StatusCode)
	}
	if g := m.Gcodes(); len(g) != 0 {
		t.Fatalf("home reached machine despite hard fault: %v", g)
	}
}

func TestMachineStatusIncludesHaltReason(t *testing.T) {
	srv, _, tr := serverWithMachine(t)
	tr.ObserveStatusPayload("<Alarm|MPos:0,0,0|H:10>")

	resp := get(t, srv.URL+"/api/machine/status")
	defer resp.Body.Close()
	var st service.MachineStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.State != machine.Alarm || st.HaltReason == nil || st.HaltReason.Code != 10 || st.HaltReason.Recovery != "unlock" {
		t.Fatalf("machine status = %+v", st)
	}
}

func TestGcodeLogEndpointAndStream(t *testing.T) {
	srv, svc := newTestServer(t)

	// Open the SSE stream first so the live event (not just the snapshot)
	// carries the line.
	req, _ := http.NewRequest("GET", srv.URL+"/api/events", nil)
	resp := do(t, req)
	defer resp.Body.Close()
	// Consume the snapshot event.
	buf := make([]byte, 8192)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "event: snapshot") {
		t.Fatalf("expected snapshot first, got %q", buf[:n])
	}

	// Submitting gcode fails (no machine in this harness), but the attempt and
	// the error must still land in the shared log and stream to clients.
	body, _ := json.Marshal(map[string]string{"line": "M114"})
	greq, _ := http.NewRequest("POST", srv.URL+"/api/gcode", bytes.NewReader(body))
	greq.Header.Set("Content-Type", "application/json")
	gresp := do(t, greq)
	gresp.Body.Close()

	// The REST log endpoint has both lines.
	lresp := get(t, srv.URL+"/api/gcode/log")
	var lines []struct {
		Dir    string `json:"dir"`
		Source string `json:"source"`
		Text   string `json:"text"`
	}
	json.NewDecoder(lresp.Body).Decode(&lines)
	lresp.Body.Close()
	if len(lines) < 2 || lines[0].Dir != "send" || lines[0].Text != "M114" || lines[0].Source != "api" {
		t.Fatalf("log lines = %+v", lines)
	}
	if lines[1].Dir != "recv" || !strings.Contains(lines[1].Text, "error") {
		t.Errorf("expected error output line, got %+v", lines[1])
	}

	// The SSE stream carries the same lines as gcode events.
	var got string
	for !strings.Contains(got, "M114") {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			t.Fatalf("stream ended before gcode event: %q (%v)", got, err)
		}
	}
	if !strings.Contains(got, "event: gcode") {
		t.Errorf("expected gcode event, got %q", got)
	}

	// Lines appended directly (as the relay does for controller traffic) also
	// reach the same stream.
	svc.GcodeLog().Append("recv", "controller", "ok")
	got = ""
	for !strings.Contains(got, `"controller"`) {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			t.Fatalf("stream ended before controller line: %q (%v)", got, err)
		}
	}
}

func TestEventsSnapshot(t *testing.T) {
	srv, _ := newTestServer(t)
	http.Post(srv.URL+"/api/files?path=a.nc", "application/octet-stream", strings.NewReader("x"))

	req, _ := http.NewRequest("GET", srv.URL+"/api/events", nil)
	resp := do(t, req)
	defer resp.Body.Close()

	// Read the initial snapshot event.
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	got := string(buf[:n])
	if !strings.Contains(got, "event: snapshot") || !strings.Contains(got, "/sd/gcodes/a.nc") {
		t.Errorf("snapshot event missing expected content: %q", got)
	}
}

func TestEventsControlScopeOmitsCatalog(t *testing.T) {
	srv, _ := newTestServer(t)
	http.Post(srv.URL+"/api/files?path=a.nc", "application/octet-stream", strings.NewReader("x"))

	req, _ := http.NewRequest("GET", srv.URL+"/api/events?scope=control", nil)
	resp := do(t, req)
	defer resp.Body.Close()

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	got := string(buf[:n])
	if !strings.Contains(got, "event: snapshot") || !strings.Contains(got, `"machine"`) {
		t.Fatalf("control snapshot missing expected machine content: %q", got)
	}
	if strings.Contains(got, "/sd/gcodes/a.nc") || strings.Contains(got, `"files"`) || strings.Contains(got, `"jobs"`) {
		t.Errorf("control snapshot unexpectedly included catalog data: %q", got)
	}
	if !strings.Contains(got, `"gcode"`) {
		t.Errorf("control snapshot should include gcode history: %q", got)
	}
}

func TestEventsFilesScopeOmitsGcode(t *testing.T) {
	srv, svc := newTestServer(t)
	http.Post(srv.URL+"/api/files?path=a.nc", "application/octet-stream", strings.NewReader("x"))
	svc.GcodeLog().Append("send", "api", "M114")

	req, _ := http.NewRequest("GET", srv.URL+"/api/events?scope=files", nil)
	resp := do(t, req)
	defer resp.Body.Close()

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	got := string(buf[:n])
	if !strings.Contains(got, "/sd/gcodes/a.nc") || !strings.Contains(got, `"files"`) || !strings.Contains(got, `"jobs"`) {
		t.Fatalf("files snapshot missing expected catalog data: %q", got)
	}
	if strings.Contains(got, `"gcode"`) || strings.Contains(got, "M114") {
		t.Errorf("files snapshot unexpectedly included gcode data: %q", got)
	}
}
