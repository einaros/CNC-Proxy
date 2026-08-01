//go:build windows

package traymgr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func stopStaleManagedProxyProcesses(ctx context.Context, cfg Config) error {
	expected := stringsTrim(cfg.ProxyBinary)
	if expected == "" {
		return errors.New("proxy binary path must not be empty")
	}
	if !filepath.IsAbs(expected) {
		expected = filepath.Join(stringsTrim(cfg.SourceDir), expected)
	}
	expected, err := filepath.Abs(expected)
	if err != nil {
		return err
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return fmt.Errorf("enumerate stale managed proxy processes: %w", err)
	}
	defer windows.CloseHandle(snapshot)

	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err := windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		if entry.ProcessID == 0 || entry.ProcessID == uint32(os.Getpid()) {
			continue
		}
		process, err := windows.OpenProcess(
			windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE,
			false,
			entry.ProcessID,
		)
		if err != nil {
			continue
		}
		imagePath, queryErr := windowsProcessImagePath(process)
		if queryErr != nil || !isManagedProxyDeploymentPath(imagePath, expected) {
			windows.CloseHandle(process)
			continue
		}
		if err := windows.TerminateProcess(process, 1); err != nil {
			windows.CloseHandle(process)
			return fmt.Errorf("stop stale managed proxy process %d: %w", entry.ProcessID, err)
		}
		for {
			status, err := windows.WaitForSingleObject(process, 100)
			if err != nil {
				windows.CloseHandle(process)
				return fmt.Errorf("wait for stale managed proxy process %d: %w", entry.ProcessID, err)
			}
			if status == windows.WAIT_OBJECT_0 {
				break
			}
			select {
			case <-ctx.Done():
				windows.CloseHandle(process)
				return fmt.Errorf("wait for stale managed proxy process %d: %w", entry.ProcessID, ctx.Err())
			default:
			}
		}
		windows.CloseHandle(process)
	}
	return nil
}

func windowsProcessImagePath(process windows.Handle) (string, error) {
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buffer[:size]), nil
}

func isManagedProxyDeploymentPath(candidate, expected string) bool {
	candidate = filepath.Clean(candidate)
	expected = filepath.Clean(expected)
	if strings.EqualFold(candidate, expected) {
		return true
	}
	if strings.EqualFold(filepath.Dir(candidate), filepath.Dir(expected)) {
		name := strings.ToLower(filepath.Base(candidate))
		expectedName := strings.ToLower(filepath.Base(expected))
		return strings.HasPrefix(name, expectedName+".previous-") ||
			strings.HasPrefix(name, ".cnc-proxy-build-")
	}
	return false
}
