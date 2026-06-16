package traymgr

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type webDAVMountRequest struct {
	URL        string
	User       string
	Password   string
	MountPoint string
	Drive      string
}

var (
	mountWebDAVNativeFunc   = mountWebDAVNative
	unmountWebDAVNativeFunc = unmountWebDAVNative
	webDAVMountedNativeFunc = webDAVMountedNative
)

func WebDAVMountSupported() bool {
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	default:
		return false
	}
}

func MountWebDAV(ctx context.Context, cfg Config) error {
	req := webDAVMountRequestFromConfig(cfg)
	if strings.TrimSpace(req.URL) == "" {
		return errors.New("webdav URL is empty")
	}
	if err := mountWebDAVNativeFunc(ctx, req); err != nil {
		return err
	}
	mounted, err := webDAVMountedNativeFunc(ctx, req)
	if err != nil {
		return err
	}
	if !mounted {
		return errors.New("webdav mount command completed but the mount is not present")
	}
	return nil
}

func UnmountWebDAV(ctx context.Context, cfg Config) error {
	req := webDAVMountRequestFromConfig(cfg)
	if err := unmountWebDAVNativeFunc(ctx, req); err != nil {
		return err
	}
	mounted, err := webDAVMountedNativeFunc(ctx, req)
	if err != nil {
		return err
	}
	if mounted {
		return errors.New("webdav unmount command completed but the mount is still present")
	}
	return nil
}

func WebDAVMounted(ctx context.Context, cfg Config) (bool, error) {
	if !WebDAVMountSupported() {
		return false, nil
	}
	req := webDAVMountRequestFromConfig(cfg)
	return webDAVMountedNativeFunc(ctx, req)
}

func webDAVMountRequestFromConfig(cfg Config) webDAVMountRequest {
	cfg = normalizeConfig(cfg)
	user, password := Auth(cfg)
	mount := normalizeWebDAVMountConfig(cfg.WebDAVMount)
	if runtime.GOOS == "windows" {
		return webDAVMountRequest{
			URL:   strings.TrimRight(ManagerBase(cfg), "/") + "/webdav/",
			Drive: mount.Drive,
		}
	}
	return webDAVMountRequest{
		URL:        WebDAVBase(cfg),
		User:       user,
		Password:   password,
		MountPoint: mount.MountPoint,
		Drive:      mount.Drive,
	}
}

func withMountTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, 20*time.Second)
}

func defaultWebDAVMountPoint() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(home, "CNC Proxy Files")
	}
	return "CNC Proxy Files"
}

func defaultWebDAVDrive() string {
	if runtime.GOOS == "windows" {
		return "*"
	}
	return ""
}

func sanitizedMountURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	return u.String()
}
