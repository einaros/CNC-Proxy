//go:build !windows

package traymgr

import "os/exec"

func configureBackgroundCommand(cmd *exec.Cmd) {}
