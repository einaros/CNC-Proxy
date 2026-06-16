//go:build !darwin && !windows

package traymgr

import (
	"context"
	"fmt"
	"runtime"
)

func mountWebDAVNative(ctx context.Context, req webDAVMountRequest) error {
	return fmt.Errorf("webdav mounting is not supported on %s", runtime.GOOS)
}

func unmountWebDAVNative(ctx context.Context, req webDAVMountRequest) error {
	return nil
}

func webDAVMountedNative(ctx context.Context, req webDAVMountRequest) (bool, error) {
	return false, nil
}
