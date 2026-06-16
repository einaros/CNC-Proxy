package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtractPayloadRejectsTraversal(t *testing.T) {
	payload := makeZip(t, map[string]string{"../bad.txt": "bad"})
	if err := extractPayload(payload, filepath.Join(t.TempDir(), "install")); err == nil {
		t.Fatal("expected traversal payload to fail")
	}
}

func TestExtractPayloadWritesFiles(t *testing.T) {
	payload := makeZip(t, map[string]string{
		"cnc-tray.exe":  "tray",
		"cnc-proxy.exe": "proxy",
		"deploy.exe":    "deploy",
	})
	dir := t.TempDir()
	if err := extractPayload(payload, dir); err != nil {
		t.Fatalf("extractPayload: %v", err)
	}
	for _, name := range []string{"cnc-tray.exe", "cnc-proxy.exe", "deploy.exe"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s not extracted: %v", name, err)
		}
	}
}

func TestWindowsProxyBuildCommandTargetsInstalledProxy(t *testing.T) {
	cmd := windowsProxyBuildCommand(`C:\Users\operator\AppData\Local\CNC Proxy\cnc-proxy.exe`)
	for _, want := range []string{
		"go build -mod=mod",
		`-trimpath`,
		`-ldflags="-s -w -H=windowsgui"`,
		`-o "C:\Users\operator\AppData\Local\CNC Proxy\cnc-proxy.exe"`,
		"./cmd/proxy",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("build command %q missing %q", cmd, want)
		}
	}
}

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
