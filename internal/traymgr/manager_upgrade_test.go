package traymgr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallManagerBinaryReplacesTargetAndLeavesStaged(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cnc-tray.exe")
	staged := filepath.Join(dir, ".cnc-tray-build-test.exe")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installManagerBinary(context.Background(), staged, target); err != nil {
		t.Fatalf("installManagerBinary: %v", err)
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new" {
		t.Fatalf("target content = %q, want new", string(b))
	}
	b, err = os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new" {
		t.Fatalf("staged content = %q, want staged file preserved", string(b))
	}
	matches, err := filepath.Glob(target + ".previous-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("successful install should remove backup files, found %v", matches)
	}
}
