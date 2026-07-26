package traymgr

import (
	"archive/zip"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnzipSafeRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "bad.zip")
	if err := writeTestZip(zipPath, map[string]string{"../escape.txt": "bad"}); err != nil {
		t.Fatal(err)
	}
	if err := unzipSafe(zipPath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("expected traversal zip to fail")
	}
}

func TestUnzipSafeExtractsSourceRoot(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "src.zip")
	if err := writeTestZip(zipPath, map[string]string{
		"repo/go.mod":        "module example.com/test\n",
		"repo/cmd/proxy.go":  "package main\n",
		"repo/internal/a.go": "package internal\n",
	}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := unzipSafe(zipPath, out); err != nil {
		t.Fatalf("unzipSafe: %v", err)
	}
	root, err := sourceRoot(out)
	if err != nil {
		t.Fatalf("sourceRoot: %v", err)
	}
	if filepath.Base(root) != "repo" {
		t.Fatalf("root = %s, want repo", root)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("go.mod not extracted: %v", err)
	}
}

func TestDeployZipWithOptionsRequiresComponent(t *testing.T) {
	sup := NewSupervisor(DefaultConfig(), "")
	if _, err := sup.DeployZipWithOptions(context.Background(), "unused.zip", DeployOptions{}); err == nil {
		t.Fatal("DeployZipWithOptions should require at least one component")
	}
}

func TestInstallSourceZipMirrorsInPlaceWithoutBackup(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "source")
	if err := os.MkdirAll(filepath.Join(sourceDir, "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "go.mod"), []byte("old module\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "old", "stale.go"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	proxyBinary := filepath.Join(sourceDir, "cnc-proxy.exe")
	if err := os.WriteFile(proxyBinary, []byte("working binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Keeping the source directory open reproduces the class of Windows
	// handle that prevented the old whole-directory rename.
	held, err := os.Open(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	zipPath := filepath.Join(dir, "src.zip")
	if err := writeTestZip(zipPath, map[string]string{
		"go.mod":      "module example.com/new\n",
		"cmd/main.go": "package main\n",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.SourceDir = sourceDir
	cfg.ProxyBinary = proxyBinary
	sup := NewSupervisor(cfg, "")
	result, wasRunning, err := sup.installSourceZip(context.Background(), zipPath)
	if err != nil {
		t.Fatalf("installSourceZip: %v", err)
	}
	if wasRunning {
		t.Fatal("installSourceZip unexpectedly reported a running proxy")
	}
	if result.SourceDir != sourceDir {
		t.Fatalf("SourceDir = %q, want %q", result.SourceDir, sourceDir)
	}
	b, err := os.ReadFile(filepath.Join(sourceDir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "module example.com/new\n" {
		t.Fatalf("go.mod = %q", b)
	}
	if _, err := os.Stat(filepath.Join(sourceDir, "old", "stale.go")); !os.IsNotExist(err) {
		t.Fatalf("stale source file still exists: %v", err)
	}
	b, err = os.ReadFile(proxyBinary)
	if err != nil {
		t.Fatalf("protected proxy binary was removed: %v", err)
	}
	if string(b) != "working binary" {
		t.Fatalf("protected proxy binary = %q", b)
	}
	matches, err := filepath.Glob(sourceDir + ".backup-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("source backups created: %v", matches)
	}
}

func TestCleanupStaleDeploymentArtifactsRemovesOnlyKnownArtifacts(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "source")
	proxyBinary := filepath.Join(dir, "cnc-proxy.exe")
	managerBinary := filepath.Join(dir, "cnc-tray.exe")
	paths := []string{
		sourceDir + ".backup-20260726-122913",
		sourceDir + ".incoming-20260726-122913",
		filepath.Join(dir, ".source.incoming-old"),
		proxyBinary + ".previous-old",
		filepath.Join(dir, ".cnc-proxy-build-old.exe"),
		managerBinary + ".previous-old",
		filepath.Join(dir, ".cnc-tray-build-old.exe"),
		filepath.Join(dir, ".cnc-tray-install-old.tmp"),
	}
	for _, path := range paths {
		if strings.Contains(filepath.Base(path), "source.") {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(dir, "operator-notes.txt")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxyBinary, []byte("proxy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managerBinary, []byte("manager"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.SourceDir = sourceDir
	cfg.ProxyBinary = proxyBinary
	cleanupStaleDeploymentArtifacts(cfg, managerBinary)

	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale deployment artifact still exists at %s: %v", path, err)
		}
	}
	for _, path := range []string{proxyBinary, managerBinary, unrelated} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("wanted path removed at %s: %v", path, err)
		}
	}
}

func TestDeployComponentParsesSupportedValues(t *testing.T) {
	for _, tc := range []struct {
		query       string
		wantProxy   bool
		wantManager bool
	}{
		{query: "", wantProxy: true},
		{query: "?component=proxy", wantProxy: true},
		{query: "?component=manager", wantManager: true},
		{query: "?component=tray", wantManager: true},
		{query: "?component=all", wantProxy: true, wantManager: true},
		{query: "?component=both", wantProxy: true, wantManager: true},
	} {
		req := httptest.NewRequest("POST", "/api/deploy"+tc.query, nil)
		got, err := deployComponent(req)
		if err != nil {
			t.Fatalf("deployComponent(%q): %v", tc.query, err)
		}
		if got.BuildProxy != tc.wantProxy || got.BuildManager != tc.wantManager {
			t.Fatalf("deployComponent(%q) = %+v", tc.query, got)
		}
	}
}

func writeTestZip(path string, files map[string]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			zw.Close()
			return err
		}
		if _, err := w.Write([]byte(body)); err != nil {
			zw.Close()
			return err
		}
	}
	return zw.Close()
}
