package api

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"time"

	"github.com/uwin/cnc-proxy/internal/carveratest"
	"github.com/uwin/cnc-proxy/internal/client"
	"github.com/uwin/cnc-proxy/internal/httpauth"
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
	resp := get(t, srv.URL+"/api/machine")
	defer resp.Body.Close()
	var st service.MachineStatus
	json.NewDecoder(resp.Body).Decode(&st)
	if st.Mode != "owner" {
		t.Errorf("mode = %q, want owner", st.Mode)
	}
}

func TestWebUIServed(t *testing.T) {
	srv, _ := newTestServer(t)

	resp := get(t, srv.URL+"/")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<!DOCTYPE html>") {
		t.Errorf("index status=%d body-start=%.30q", resp.StatusCode, body)
	}

	js := get(t, srv.URL+"/app.js")
	jsBody, _ := io.ReadAll(js.Body)
	js.Body.Close()
	if js.StatusCode != http.StatusOK || !strings.Contains(string(jsBody), "EventSource") {
		t.Errorf("app.js status=%d", js.StatusCode)
	}
}

// serverWithMachine wires the API to a real fake machine + tracker so gcode and
// control endpoints can be exercised end to end with controllable state.
func serverWithMachine(t *testing.T) (*httptest.Server, *carveratest.FakeMachine, *machine.Tracker) {
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
	svc, err := service.New(st, arb)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(svc).Handler())
	t.Cleanup(srv.Close)
	return srv, m, tr
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return do(t, req)
}

func TestPostGcodeMotionGatedByState(t *testing.T) {
	srv, m, tr := serverWithMachine(t)

	// Running: a motion command is rejected with 503 and never reaches the machine.
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
