//go:build !windows

package traymgr

import "context"

func stopStaleManagedProxyProcesses(context.Context, Config) error {
	return nil
}
