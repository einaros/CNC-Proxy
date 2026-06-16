//go:build !windows

package traymgr

type OSNotifier struct{}

func (OSNotifier) Notify(Notification) error { return nil }
