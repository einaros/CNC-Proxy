//go:build !windows && !darwin && !linux

package traymgr

type OSNotifier struct{}

func (OSNotifier) Notify(Notification) error { return nil }
