package apiclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientReadsEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/machine", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"state":"Idle","mode":"owner","connected":true,"age_ms":100}`))
	})
	mux.HandleFunc("/api/files", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"path":"/sd/gcodes/a.nc","sync":"synced"},{"path":"/sd/gcodes/b.nc","sync":"pending_upload"}]`))
	})
	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":1,"kind":"upload","path":"/sd/gcodes/b.nc","state":"queued"}]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL)
	ctx := context.Background()

	m, err := c.Machine(ctx)
	if err != nil || m.State != "Idle" || m.Mode != "owner" {
		t.Fatalf("machine = %+v err=%v", m, err)
	}

	files, err := c.Files(ctx)
	if err != nil || len(files) != 2 {
		t.Fatalf("files = %+v err=%v", files, err)
	}
	if n := PendingCount(files); n != 1 {
		t.Errorf("PendingCount = %d, want 1", n)
	}

	jobs, err := c.Jobs(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].Kind != "upload" {
		t.Fatalf("jobs = %+v err=%v", jobs, err)
	}
}

func TestPendingCount(t *testing.T) {
	files := []File{
		{Path: "a", Sync: "synced"},
		{Path: "b", Sync: "remote_only"},
		{Path: "c", Sync: "uploading"},
		{Path: "d", Sync: "error"},
	}
	if n := PendingCount(files); n != 2 {
		t.Errorf("PendingCount = %d, want 2 (uploading + error)", n)
	}
}

func TestMachineUnreachable(t *testing.T) {
	c := New("http://127.0.0.1:1") // nothing listening
	if _, err := c.Machine(context.Background()); err == nil {
		t.Error("expected error for unreachable API")
	}
}
