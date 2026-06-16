//go:build linux

package traymgr

import "os/exec"

type OSNotifier struct{}

func (OSNotifier) Notify(n Notification) error {
	cmd := exec.Command("notify-send", n.Title, n.Message)
	configureBackgroundCommand(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
