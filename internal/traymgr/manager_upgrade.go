package traymgr

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	if err := installManagerBinary(installCtx, opts.StagedBinary, opts.TargetBinary); err != nil {
		return err
	}
	args := []string{"-config", opts.ConfigPath, "-" + ManagerUpgradeCleanupFlag, opts.StagedBinary}
	if stringsTrim(opts.ConfigPath) == "" {
		args = []string{"-" + ManagerUpgradeCleanupFlag, opts.StagedBinary}
	}
	if opts.StartProxy {
		args = append(args, "-"+ManagerUpgradeStartProxyFlag)
	}
	cmd := exec.Command(opts.TargetBinary, args...)
	configureBackgroundCommand(cmd)
	return cmd.Start()
}

func installManagerBinary(ctx context.Context, stagedPath, targetPath string) error {
	stagedPath = stringsTrim(stagedPath)
	targetPath = stringsTrim(targetPath)
	if stagedPath == "" {
		return errors.New("staged manager binary is empty")
	}
	if targetPath == "" {
		return errors.New("target manager binary is empty")
	}
	var lastErr error
	for {
		if err := installManagerBinaryOnce(stagedPath, targetPath); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("install manager binary: %w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func installManagerBinaryOnce(stagedPath, targetPath string) error {
	info, err := os.Stat(stagedPath)
	if err != nil {
		return err
	}
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cnc-tray-install-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	in, err := os.Open(stagedPath)
	if err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	_, copyErr := io.Copy(tmp, in)
	closeInErr := in.Close()
	closeOutErr := tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmpName)
		return copyErr
	}
	if closeInErr != nil {
		_ = os.Remove(tmpName)
		return closeInErr
	}
	if closeOutErr != nil {
		_ = os.Remove(tmpName)
		return closeOutErr
	}
	if err := os.Chmod(tmpName, info.Mode()); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	backup := ""
	if _, err := os.Stat(targetPath); err == nil {
		backup = targetPath + ".previous-" + time.Now().Format("20060102-150405")
		if err := os.Rename(targetPath, backup); err != nil {
			_ = os.Remove(tmpName)
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, targetPath); err != nil {
		if backup != "" {
			_ = os.Rename(backup, targetPath)
		}
		_ = os.Remove(tmpName)
		return err
	}
	if backup != "" {
		_ = os.Remove(backup)
	}
	return nil
}
