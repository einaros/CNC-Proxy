package traymgr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInstallManagerBinaryReplacesTargetKeepsBackupAndLeavesStaged(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cnc-tray.exe")
	staged := filepath.Join(dir, ".cnc-tray-build-test.exe")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := installManagerBinary(context.Background(), staged, target)
	if err != nil {
		t.Fatalf("installManagerBinary: %v", err)
	}
	if result.TargetBinary != target {
		t.Fatalf("TargetBinary = %q, want %q", result.TargetBinary, target)
	}
	if result.BackupBinary == "" {
		t.Fatal("BackupBinary is empty")
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
	b, err = os.ReadFile(result.BackupBinary)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "old" {
		t.Fatalf("backup content = %q, want old", string(b))
	}
	if err := cleanupManagerInstallBackup(result); err != nil {
		t.Fatalf("cleanupManagerInstallBackup: %v", err)
	}
	if _, err := os.Stat(result.BackupBinary); !os.IsNotExist(err) {
		t.Fatalf("backup still exists after cleanup: %v", err)
	}
}

func TestRollbackManagerUpgradeRestoresBackup(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "cnc-tray.exe")
	staged := filepath.Join(dir, ".cnc-tray-build-test.exe")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := installManagerBinary(context.Background(), staged, target)
	if err != nil {
		t.Fatalf("installManagerBinary: %v", err)
	}
	if err := rollbackManagerUpgrade(result); err != nil {
		t.Fatalf("rollbackManagerUpgrade: %v", err)
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "old" {
		t.Fatalf("target content after rollback = %q, want old", string(b))
	}
	if _, err := os.Stat(result.BackupBinary); !os.IsNotExist(err) {
		t.Fatalf("backup still exists after rollback: %v", err)
	}
}
