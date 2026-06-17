package traymgr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	ManagerUpgradeFinalizeFlag        = "manager-upgrade-finalize"
	ManagerUpgradeStagedFlag          = "manager-upgrade-staged"
	ManagerUpgradeTargetFlag          = "manager-upgrade-target"
	ManagerUpgradeCleanupFlag         = "manager-upgrade-cleanup"
	ManagerUpgradeStartProxyFlag      = "manager-upgrade-start-proxy"
	ManagerUpgradeInstallTimeoutFlag  = "manager-upgrade-install-timeout"
	defaultManagerUpgradeInstallLimit = 45 * time.Second
	defaultManagerUpgradeReadyLimit   = 20 * time.Second
)

type ManagerUpgradeResult struct {
	TargetBinary         string `json:"target_binary"`
	StagedBinary         string `json:"staged_binary,omitempty"`
	BuildLog             string `json:"build_log,omitempty"`
	RestartScheduled     bool   `json:"restart_scheduled"`
	ProxyStartOnRelaunch bool   `json:"proxy_start_on_relaunch"`
}

type ManagerUpgradeFinalizeOptions struct {
	StagedBinary   string
	TargetBinary   string
	ConfigPath     string
	StartProxy     bool
	InstallTimeout time.Duration
}

type managerInstallResult struct {
	TargetBinary string
	BackupBinary string
}

func (s *Supervisor) BuildManager(ctx context.Context, targetBinary string) (ManagerUpgradeResult, error) {
	cfg := s.Config()
	sourceDir := stringsTrim(cfg.SourceDir)
	if sourceDir == "" {
		return ManagerUpgradeResult{}, errors.New("source_dir must be set before building manager")
	}
	target, err := managerBinaryPath(targetBinary)
	if err != nil {
		return ManagerUpgradeResult{}, err
	}
	staged, err := stagedBuildOutputPathWithPrefix(target, ".cnc-tray-build-")
	if err != nil {
		return ManagerUpgradeResult{}, err
	}
	cmd := exec.CommandContext(ctx, "go", managedManagerBuildArgs(staged)...)
	configureBackgroundCommand(cmd)
	cmd.Dir = sourceDir
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		_ = os.Remove(staged)
		return ManagerUpgradeResult{TargetBinary: target, StagedBinary: staged, BuildLog: out.String()}, err
	}
	fmt.Fprintf(&out, "staged manager %s\n", staged)
	return ManagerUpgradeResult{TargetBinary: target, StagedBinary: staged, BuildLog: out.String()}, nil
}

func managerBinaryPath(path string) (string, error) {
	path = stringsTrim(path)
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		path = exe
	}
	return filepath.Abs(path)
}

func managedManagerBuildArgs(outputPath string) []string {
	return managedManagerBuildArgsForGOOS(outputPath, runtime.GOOS)
}

func managedManagerBuildArgsForGOOS(outputPath, goos string) []string {
	args := []string{"build", "-mod=mod"}
	if goos == "windows" {
		args = append(args, "-trimpath", "-tags", "tray", "-ldflags=-s -w -H=windowsgui")
	} else {
		args = append(args, "-tags", "tray")
	}
	return append(args, "-o", outputPath, "./cmd/tray")
}

func LaunchManagerUpgradeFinalizer(result ManagerUpgradeResult, configPath string) error {
	if stringsTrim(result.StagedBinary) == "" {
		return errors.New("staged manager binary is empty")
	}
	if stringsTrim(result.TargetBinary) == "" {
		return errors.New("target manager binary is empty")
	}
	args := []string{
		"-" + ManagerUpgradeFinalizeFlag,
		"-" + ManagerUpgradeStagedFlag, result.StagedBinary,
		"-" + ManagerUpgradeTargetFlag, result.TargetBinary,
	}
	if stringsTrim(configPath) != "" {
		args = append(args, "-config", configPath)
	}
	if result.ProxyStartOnRelaunch {
		args = append(args, "-"+ManagerUpgradeStartProxyFlag)
	}
	cmd := exec.Command(result.StagedBinary, args...)
	configureBackgroundCommand(cmd)
	return cmd.Start()
}

func FinalizeManagerUpgrade(ctx context.Context, opts ManagerUpgradeFinalizeOptions) error {
	if stringsTrim(opts.StagedBinary) == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		opts.StagedBinary = exe
	}
	if stringsTrim(opts.TargetBinary) == "" {
		return errors.New("target manager binary is required")
	}
	if opts.InstallTimeout <= 0 {
		opts.InstallTimeout = defaultManagerUpgradeInstallLimit
	}
	installCtx, cancel := context.WithTimeout(ctx, opts.InstallTimeout)
	defer cancel()
	if err := waitForManagerDown(installCtx, opts.ConfigPath); err != nil {
		return err
	}
	install, err := installManagerBinary(installCtx, opts.StagedBinary, opts.TargetBinary)
	if err != nil {
		return errors.Join(err, relaunchManagerAfterFailedUpgrade(opts.TargetBinary, opts.ConfigPath, opts.StagedBinary, opts.StartProxy))
	}
	cmd, done, err := launchManagerBinary(opts.TargetBinary, opts.ConfigPath, opts.StagedBinary, opts.StartProxy)
	if err != nil {
		return errors.Join(err, rollbackManagerUpgrade(install), relaunchManagerAfterFailedUpgrade(opts.TargetBinary, opts.ConfigPath, opts.StagedBinary, opts.StartProxy))
	}
	if err := waitForManagerReady(ctx, cmd, done, opts.ConfigPath, defaultManagerUpgradeReadyLimit); err != nil {
		return errors.Join(err, rollbackManagerUpgrade(install), relaunchManagerAfterFailedUpgrade(opts.TargetBinary, opts.ConfigPath, opts.StagedBinary, opts.StartProxy))
	}
	return cleanupManagerInstallBackup(install)
}

func installManagerBinary(ctx context.Context, stagedPath, targetPath string) (managerInstallResult, error) {
	stagedPath = stringsTrim(stagedPath)
	targetPath = stringsTrim(targetPath)
	if stagedPath == "" {
		return managerInstallResult{}, errors.New("staged manager binary is empty")
	}
	if targetPath == "" {
		return managerInstallResult{}, errors.New("target manager binary is empty")
	}
	var lastErr error
	for {
		if result, err := installManagerBinaryOnce(stagedPath, targetPath); err == nil {
			return result, nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return managerInstallResult{}, fmt.Errorf("install manager binary: %w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func installManagerBinaryOnce(stagedPath, targetPath string) (managerInstallResult, error) {
	info, err := os.Stat(stagedPath)
	if err != nil {
		return managerInstallResult{}, err
	}
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return managerInstallResult{}, err
	}
	tmp, err := os.CreateTemp(dir, ".cnc-tray-install-*.tmp")
	if err != nil {
		return managerInstallResult{}, err
	}
	tmpName := tmp.Name()
	in, err := os.Open(stagedPath)
	if err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return managerInstallResult{}, err
	}
	_, copyErr := io.Copy(tmp, in)
	closeInErr := in.Close()
	closeOutErr := tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmpName)
		return managerInstallResult{}, copyErr
	}
	if closeInErr != nil {
		_ = os.Remove(tmpName)
		return managerInstallResult{}, closeInErr
	}
	if closeOutErr != nil {
		_ = os.Remove(tmpName)
		return managerInstallResult{}, closeOutErr
	}
	if err := os.Chmod(tmpName, info.Mode()); err != nil {
		_ = os.Remove(tmpName)
		return managerInstallResult{}, err
	}
	backup := ""
	if _, err := os.Stat(targetPath); err == nil {
		backup = targetPath + ".previous-" + time.Now().Format("20060102-150405.000000000")
		if err := os.Rename(targetPath, backup); err != nil {
			_ = os.Remove(tmpName)
			return managerInstallResult{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tmpName)
		return managerInstallResult{}, err
	}
	if err := os.Rename(tmpName, targetPath); err != nil {
		if backup != "" {
			_ = os.Rename(backup, targetPath)
		}
		_ = os.Remove(tmpName)
		return managerInstallResult{}, err
	}
	return managerInstallResult{TargetBinary: targetPath, BackupBinary: backup}, nil
}

func launchManagerBinary(targetPath, configPath, cleanupPath string, startProxy bool) (*exec.Cmd, <-chan error, error) {
	args := managerLaunchArgs(configPath, cleanupPath, startProxy)
	cmd := exec.Command(targetPath, args...)
	configureBackgroundCommand(cmd)
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	return cmd, done, nil
}

func managerLaunchArgs(configPath, cleanupPath string, startProxy bool) []string {
	var args []string
	if stringsTrim(configPath) != "" {
		args = append(args, "-config", configPath)
	}
	if stringsTrim(cleanupPath) != "" {
		args = append(args, "-"+ManagerUpgradeCleanupFlag, cleanupPath)
	}
	if startProxy {
		args = append(args, "-"+ManagerUpgradeStartProxyFlag)
	}
	return args
}

func waitForManagerReady(ctx context.Context, cmd *exec.Cmd, done <-chan error, configPath string, limit time.Duration) error {
	if limit <= 0 {
		limit = defaultManagerUpgradeReadyLimit
	}
	readyCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	url := managerReadyURL(configPath)
	client := &http.Client{Timeout: time.Second}
	for {
		req, err := http.NewRequestWithContext(readyCtx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return nil
			}
		}
		select {
		case err := <-done:
			if err == nil {
				err = errors.New("clean exit")
			}
			return fmt.Errorf("relaunched manager exited before becoming ready: %w", err)
		case <-readyCtx.Done():
			if cmd != nil && cmd.Process != nil {
				_ = cmd.Process.Kill()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
				}
			}
			return fmt.Errorf("relaunched manager did not become ready: %w", readyCtx.Err())
		case <-ticker.C:
		}
	}
}

func waitForManagerDown(ctx context.Context, configPath string) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	url := managerReadyURL(configPath)
	client := &http.Client{Timeout: time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil
		}
		_ = resp.Body.Close()
		select {
		case <-ctx.Done():
			return fmt.Errorf("current manager did not stop before upgrade install: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func managerReadyURL(configPath string) string {
	cfg := DefaultConfig()
	if stringsTrim(configPath) != "" {
		if loaded, err := LoadConfig(configPath); err == nil {
			cfg = loaded
		}
	}
	return strings.TrimRight(ManagerBase(cfg), "/") + "/"
}

func relaunchManagerAfterFailedUpgrade(targetPath, configPath, cleanupPath string, startProxy bool) error {
	if stringsTrim(targetPath) == "" {
		return errors.New("cannot relaunch manager after failed upgrade: target manager binary is empty")
	}
	if _, _, err := launchManagerBinary(targetPath, configPath, cleanupPath, startProxy); err != nil {
		return fmt.Errorf("relaunch manager after failed upgrade: %w", err)
	}
	return nil
}

func rollbackManagerUpgrade(result managerInstallResult) error {
	if stringsTrim(result.BackupBinary) == "" {
		return nil
	}
	if err := os.Remove(result.TargetBinary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove failed manager binary %s: %w", result.TargetBinary, err)
	}
	if err := os.Rename(result.BackupBinary, result.TargetBinary); err != nil {
		return fmt.Errorf("restore previous manager binary %s: %w", result.TargetBinary, err)
	}
	return nil
}

func cleanupManagerInstallBackup(result managerInstallResult) error {
	if stringsTrim(result.BackupBinary) == "" {
		return nil
	}
	if err := os.Remove(result.BackupBinary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove previous manager binary backup %s: %w", result.BackupBinary, err)
	}
	return nil
}
