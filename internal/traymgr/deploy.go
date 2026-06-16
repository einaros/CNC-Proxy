package traymgr

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type DeployResult struct {
	SourceDir string `json:"source_dir"`
	BackupDir string `json:"backup_dir,omitempty"`
	BuildLog  string `json:"build_log,omitempty"`
	Restarted bool   `json:"restarted"`
}

func (s *Supervisor) DeployZip(ctx context.Context, zipPath string, restart bool) (DeployResult, error) {
	cfg := s.Config()
	if strings.TrimSpace(cfg.SourceDir) == "" {
		return DeployResult{}, errors.New("source_dir must be set before deployment")
	}
	sourceDir, err := filepath.Abs(cfg.SourceDir)
	if err != nil {
		return DeployResult{}, err
	}
	parent := filepath.Dir(sourceDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return DeployResult{}, err
	}

	incoming := sourceDir + ".incoming-" + time.Now().Format("20060102-150405")
	if err := unzipSafe(zipPath, incoming); err != nil {
		os.RemoveAll(incoming)
		return DeployResult{}, err
	}
	root, err := sourceRoot(incoming)
	if err != nil {
		os.RemoveAll(incoming)
		return DeployResult{}, err
	}

	wasRunning := s.State().Running
	if wasRunning {
		stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.Stop(stopCtx)
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			os.RemoveAll(incoming)
			return DeployResult{}, err
		}
	}

	backup := ""
	if _, err := os.Stat(sourceDir); err == nil {
		backup = sourceDir + ".backup-" + time.Now().Format("20060102-150405")
		if err := os.Rename(sourceDir, backup); err != nil {
			os.RemoveAll(incoming)
			return DeployResult{}, err
		}
	}
	if err := os.Rename(root, sourceDir); err != nil {
		if backup != "" {
			_ = os.Rename(backup, sourceDir)
		}
		os.RemoveAll(incoming)
		return DeployResult{}, err
	}
	_ = os.RemoveAll(incoming)

	buildLog, buildErr := s.Build(ctx)
	if buildErr != nil {
		_ = os.RemoveAll(sourceDir)
		if backup != "" {
			_ = os.Rename(backup, sourceDir)
		}
		if wasRunning {
			_ = s.Start()
		}
		return DeployResult{SourceDir: sourceDir, BackupDir: backup, BuildLog: buildLog}, buildErr
	}

	result := DeployResult{SourceDir: sourceDir, BackupDir: backup, BuildLog: buildLog}
	if restart || wasRunning {
		if err := s.Start(); err != nil {
			return result, err
		}
		result.Restarted = true
	}
	return result, nil
}

func unzipSafe(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destAbs, 0o755); err != nil {
		return err
	}
	for _, f := range r.File {
		name := filepath.Clean(filepath.FromSlash(f.Name))
		if name == "." || strings.HasPrefix(name, ".."+string(filepath.Separator)) || filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
			return fmt.Errorf("zip contains unsafe path %q", f.Name)
		}
		target := filepath.Join(destAbs, name)
		targetAbs, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		if targetAbs != destAbs && !strings.HasPrefix(targetAbs, destAbs+string(filepath.Separator)) {
			return fmt.Errorf("zip escapes destination with path %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(targetAbs, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(targetAbs), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(targetAbs, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, copyErr := io.Copy(out, rc)
		closeErr := out.Close()
		rc.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func sourceRoot(dir string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return dir, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var dirs []os.DirEntry
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e)
		}
	}
	if len(dirs) == 1 {
		candidate := filepath.Join(dir, dirs[0].Name())
		if _, err := os.Stat(filepath.Join(candidate, "go.mod")); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("deployment zip must contain go.mod at its root or under one top-level directory")
}
