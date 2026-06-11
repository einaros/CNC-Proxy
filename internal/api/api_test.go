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

	"github.com/uwin/cnc-proxy/internal/client"
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
