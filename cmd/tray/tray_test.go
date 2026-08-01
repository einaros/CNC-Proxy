//go:build tray

package main

import (
	"testing"

	"github.com/uwin/cnc-proxy/internal/traymgr"
)

func TestWebDAVMountEnableRequestUsesDesiredStateAfterMountFailure(t *testing.T) {
	status := traymgr.WebDAVMountStatus{
		Desired:     true,
		Mounted:     false,
		ErrorAction: "remount",
		Error:       "service unavailable",
	}
	if webDAVMountEnableRequest(status) {
		t.Fatal("failed desired mount should request disable, not another enable")
	}
}

func TestWebDAVMountEnableRequestRetriesFailedUnmount(t *testing.T) {
	status := traymgr.WebDAVMountStatus{
		Desired:     false,
		Mounted:     true,
		ErrorAction: "unmount",
		Error:       "drive is busy",
	}
	if webDAVMountEnableRequest(status) {
		t.Fatal("still-mounted disabled mount should retry unmount, not enable")
	}
}

func TestWebDAVMountEnableRequestEnablesCleanDisabledMount(t *testing.T) {
	if !webDAVMountEnableRequest(traymgr.WebDAVMountStatus{}) {
		t.Fatal("clean disabled mount should request enable")
	}
}
