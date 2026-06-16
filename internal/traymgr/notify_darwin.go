//go:build darwin

package traymgr

import (
	"os/exec"
	"strconv"
)

type OSNotifier struct{}

func (OSNotifier) Notify(n Notification) error {
	cmd := exec.Command("osascript", "-e", "display notification "+strconv.Quote(n.Message)+" with title "+strconv.Quote(n.Title))
	configureBackgroundCommand(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
