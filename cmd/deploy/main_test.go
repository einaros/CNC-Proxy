package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestIsTransientDeployErrorRecognizesWindowsSharingFailures(t *testing.T) {
	for _, detail := range []string{
		`rename C:\source C:\source.backup: The process cannot access the file because it is being used by another process.`,
		`remove C:\source\app.go: Access is denied.`,
		`move failed: sharing violation`,
	} {
		err := &deployStatusError{status: 500, detail: detail}
		if !isTransientDeployError(err) {
			t.Fatalf("isTransientDeployError(%q) = false, want true", detail)
		}
	}
}

func TestIsTransientDeployErrorRejectsBuildAndClientErrors(t *testing.T) {
	for _, err := range []error{
		&deployStatusError{status: 500, detail: "go build failed"},
		&deployStatusError{status: 400, detail: "access is denied"},
		errors.New("connection refused"),
	} {
		if isTransientDeployError(err) {
			t.Fatalf("isTransientDeployError(%v) = true, want false", err)
		}
	}
}

func TestUploadRetriesWindowsSharingFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := requests.Add(1); got == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ok":false,"error":"rename source: The process cannot access the file because it is being used by another process."}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"source_dir":"source","restarted":true}}`))
	}))
	defer server.Close()

	zipPath := filepath.Join(t.TempDir(), "source.zip")
	if err := os.WriteFile(zipPath, []byte("test archive body"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := uploadWithRetry(server.URL, "token", true, "all", zipPath, 2, 0)
	if err != nil {
		t.Fatalf("uploadWithRetry: %v", err)
	}
	if result.Result.SourceDir != "source" || !result.Result.Restarted {
		t.Fatalf("result = %+v", result.Result)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}
