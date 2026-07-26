package traymgr

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type DeployResult struct {
	SourceDir      string                `json:"source_dir"`
	BuildLog       string                `json:"build_log,omitempty"`
	Restarted      bool                  `json:"restarted"`
	ManagerUpgrade *ManagerUpgradeResult `json:"manager_upgrade,omitempty"`
}

type DeployOptions struct {
	BuildProxy    bool
	BuildManager  bool
	RestartProxy  bool
	ManagerBinary string
}

func (s *Supervisor) DeployZip(ctx context.Context, zipPath string, restart bool) (DeployResult, error) {
	return s.DeployZipWithOptions(ctx, zipPath, DeployOptions{BuildProxy: true, RestartProxy: restart})
}

func (s *Supervisor) DeployZipWithOptions(ctx context.Context, zipPath string, opts DeployOptions) (DeployResult, error) {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()

	if !opts.BuildProxy && !opts.BuildManager {
		return DeployResult{}, errors.New("at least one deployment component must be selected")
	}
	cleanupStaleDeploymentArtifacts(s.Config(), opts.ManagerBinary)
	result, wasRunning, err := s.installSourceZip(ctx, zipPath)
	if err != nil {
		return DeployResult{}, err
	}

	recoverProxy := func(deployErr error) error {
		if wasRunning {
			if startErr := s.Start(); startErr != nil {
				return errors.Join(deployErr, fmt.Errorf("restart previous proxy after failed deployment: %w", startErr))
			}
		}
		return deployErr
	}

	if opts.BuildManager {
		manager, err := s.BuildManager(ctx, opts.ManagerBinary)
		result.ManagerUpgrade = &manager
		if err != nil {
			return result, recoverProxy(err)
		}
	}

	if opts.BuildProxy {
		buildLog, buildErr := s.Build(ctx)
		result.BuildLog = buildLog
		if buildErr != nil {
			cleanupManagerUpgrade(result.ManagerUpgrade)
			return result, recoverProxy(buildErr)
		}
	}

	restartProxy := opts.RestartProxy || wasRunning
	if opts.BuildManager {
		result.ManagerUpgrade.ProxyStartOnRelaunch = restartProxy
		return result, nil
	}
	if restartProxy {
		if err := s.StartAfterDeployment(ctx); err != nil {
			return result, err
		}
		result.Restarted = true
	}
	return result, nil
}

func (s *Supervisor) installSourceZip(ctx context.Context, zipPath string) (DeployResult, bool, error) {
	cfg := s.Config()
	if strings.TrimSpace(cfg.SourceDir) == "" {
		return DeployResult{}, false, errors.New("source_dir must be set before deployment")
	}
	sourceDir, err := filepath.Abs(cfg.SourceDir)
	if err != nil {
		return DeployResult{}, false, err
	}
	parent := filepath.Dir(sourceDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return DeployResult{}, false, err
	}

	incoming, err := os.MkdirTemp(parent, "."+filepath.Base(sourceDir)+".incoming-")
	if err != nil {
		return DeployResult{}, false, err
	}
	defer os.RemoveAll(incoming)
	if err := unzipSafe(zipPath, incoming); err != nil {
		return DeployResult{}, false, err
	}
	root, err := sourceRoot(incoming)
	if err != nil {
		return DeployResult{}, false, err
	}

	wasRunning := s.State().Running
	if wasRunning {
		stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.Stop(stopCtx)
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return DeployResult{}, false, err
		}
	}

	protected := protectedSourcePaths(sourceDir, cfg.ProxyBinary)
	if err := mirrorSourceTree(ctx, root, sourceDir, protected); err != nil {
		if wasRunning {
			if startErr := s.Start(); startErr != nil {
				err = errors.Join(err, fmt.Errorf("restart previous proxy after source install failed: %w", startErr))
			}
		}
		return DeployResult{}, false, err
	}
	return DeployResult{SourceDir: sourceDir}, wasRunning, nil
}

func protectedSourcePaths(sourceDir, proxyBinary string) map[string]bool {
	protected := make(map[string]bool)
	binary := strings.TrimSpace(proxyBinary)
	if binary == "" {
		return protected
	}
	if !filepath.IsAbs(binary) {
		binary = filepath.Join(sourceDir, binary)
	}
	rel, err := filepath.Rel(sourceDir, binary)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return protected
	}
	for rel != "." && rel != "" {
		protected[filepath.Clean(rel)] = true
		rel = filepath.Dir(rel)
	}
	return protected
}

func mirrorSourceTree(ctx context.Context, sourceRoot, destRoot string, protected map[string]bool) error {
	sourceRoot, err := filepath.Abs(sourceRoot)
	if err != nil {
		return err
	}
	destRoot, err = filepath.Abs(destRoot)
	if err != nil {
		return err
	}
	if sourceRoot == destRoot {
		return errors.New("deployment source and destination must differ")
	}
	if err := retryDeploymentFileOp(ctx, func() error { return os.MkdirAll(destRoot, 0o755) }); err != nil {
		return err
	}

	desired := map[string]bool{".": true}
	err = filepath.WalkDir(sourceRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.Clean(rel)
		desired[rel] = true
		if rel == "." {
			return nil
		}
		target := filepath.Join(destRoot, rel)
		if d.IsDir() {
			return retryDeploymentFileOp(ctx, func() error { return os.MkdirAll(target, 0o755) })
		}
		return copyDeploymentFile(ctx, path, target)
	})
	if err != nil {
		return err
	}

	var stale []string
	if err := filepath.WalkDir(destRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		rel, err := filepath.Rel(destRoot, path)
		if err != nil || rel == "." {
			return nil
		}
		rel = filepath.Clean(rel)
		if !desired[rel] && !protected[rel] {
			stale = append(stale, path)
		}
		return nil
	}); err == nil {
		sort.Slice(stale, func(i, j int) bool {
			return strings.Count(stale[i], string(filepath.Separator)) > strings.Count(stale[j], string(filepath.Separator))
		})
		for _, path := range stale {
			// Stale files are not needed by the new source. Their cleanup is
			// deliberately best-effort so an unrelated scanner/editor handle
			// cannot turn a valid upgrade into an outage.
			_ = os.Remove(path)
		}
	}
	return nil
}

func copyDeploymentFile(ctx context.Context, sourcePath, targetPath string) error {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return err
	}
	if err := retryDeploymentFileOp(ctx, func() error { return os.MkdirAll(filepath.Dir(targetPath), 0o755) }); err != nil {
		return err
	}
	in, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(targetPath), ".cnc-deploy-file-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, info.Mode()); err != nil {
		return err
	}
	if err := retryDeploymentFileOp(ctx, func() error { return os.Rename(tmpName, targetPath) }); err != nil {
		return fmt.Errorf("replace source file %s: %w", targetPath, err)
	}
	return nil
}

func retryDeploymentFileOp(ctx context.Context, op func() error) error {
	var lastErr error
	delay := 50 * time.Millisecond
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := op(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), lastErr)
		case <-time.After(delay):
		}
		if delay < time.Second {
			delay *= 2
		}
	}
}

func cleanupStaleDeploymentArtifacts(cfg Config, managerBinary string) {
	sourceDir := strings.TrimSpace(cfg.SourceDir)
	if sourceDir != "" {
		if abs, err := filepath.Abs(sourceDir); err == nil {
			removeDeploymentMatches(abs+".backup-*", "")
			removeDeploymentMatches(abs+".incoming-*", "")
			removeDeploymentMatches(filepath.Join(filepath.Dir(abs), "."+filepath.Base(abs)+".incoming-*"), "")
		}
	}

	proxyBinary := strings.TrimSpace(cfg.ProxyBinary)
	if proxyBinary != "" {
		if !filepath.IsAbs(proxyBinary) && sourceDir != "" {
			proxyBinary = filepath.Join(sourceDir, proxyBinary)
		}
		if abs, err := filepath.Abs(proxyBinary); err == nil {
			removeDeploymentMatches(abs+".previous-*", abs)
			removeDeploymentMatches(filepath.Join(filepath.Dir(abs), ".cnc-proxy-build-*"), abs)
		}
	}

	if strings.TrimSpace(managerBinary) == "" {
		if current, err := managerBinaryPath(""); err == nil {
			managerBinary = current
		}
	}
	if abs, err := filepath.Abs(strings.TrimSpace(managerBinary)); err == nil && strings.TrimSpace(managerBinary) != "" {
		removeDeploymentMatches(abs+".previous-*", abs)
		removeDeploymentMatches(filepath.Join(filepath.Dir(abs), ".cnc-tray-build-*"), abs)
		removeDeploymentMatches(filepath.Join(filepath.Dir(abs), ".cnc-tray-install-*"), abs)
	}
}

func removeDeploymentMatches(pattern, keep string) {
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return
	}
	for _, match := range matches {
		if keep != "" && sameDeploymentPath(match, keep) {
			continue
		}
		_ = os.RemoveAll(match)
	}
}

func sameDeploymentPath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if filepath.Separator == '\\' {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func cleanupManagerUpgrade(result *ManagerUpgradeResult) {
	if result == nil || strings.TrimSpace(result.StagedBinary) == "" {
		return
	}
	_ = os.Remove(result.StagedBinary)
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
