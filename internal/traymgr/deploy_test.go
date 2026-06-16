package traymgr

import (
	"archive/zip"
	"os"
	"path/filepath"
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
