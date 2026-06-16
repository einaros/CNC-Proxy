//go:build darwin

package traymgr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

func mountWebDAVNative(ctx context.Context, req webDAVMountRequest) error {
	ctx, cancel := withMountTimeout(ctx)
	defer cancel()
	if err := os.MkdirAll(req.MountPoint, 0o755); err != nil {
		return err
	}
	if mounted, err := mountPointMounted(ctx, req.MountPoint); err != nil {
		return err
	} else if mounted {
		return nil
	}
	args := []string{"-S", "-v", "CNC Proxy"}
	var stdin string
	if req.Password != "" {
		args = append(args, "-i")
		stdin = req.User + "\n" + req.Password + "\n"
	}
	args = append(args, sanitizedMountURL(req.URL), req.MountPoint)
	cmd := exec.CommandContext(ctx, "mount_webdav", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("mount webdav: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

func unmountWebDAVNative(ctx context.Context, req webDAVMountRequest) error {
	ctx, cancel := withMountTimeout(ctx)
	defer cancel()
	mounted, err := mountPointMounted(ctx, req.MountPoint)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	if out, err := exec.CommandContext(ctx, "umount", req.MountPoint).CombinedOutput(); err == nil {
		return nil
	} else if ctx.Err() != nil {
		return ctx.Err()
	} else if out2, err2 := exec.CommandContext(ctx, "diskutil", "unmount", req.MountPoint).CombinedOutput(); err2 != nil {
		return fmt.Errorf("unmount webdav: %w: %s; diskutil: %v: %s", err, strings.TrimSpace(string(out)), err2, strings.TrimSpace(string(out2)))
	}
	return nil
}

func webDAVMountedNative(ctx context.Context, req webDAVMountRequest) (bool, error) {
	ctx, cancel := withMountTimeout(ctx)
	defer cancel()
	return mountPointMounted(ctx, req.MountPoint)
}

func mountPointMounted(ctx context.Context, mountPoint string) (bool, error) {
	if strings.TrimSpace(mountPoint) == "" {
		return false, errors.New("webdav mount point is empty")
	}
	out, err := exec.CommandContext(ctx, "mount").Output()
	if err != nil {
		return false, err
	}
	needle := " on " + mountPoint + " "
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, needle) {
			return true, nil
		}
	}
	return false, nil
}
