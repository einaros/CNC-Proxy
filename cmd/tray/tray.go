//go:build tray

// Command tray is the menu-bar/tray status companion for the CNC proxy. It
// polls the proxy's HTTP API and shows machine state and sync activity, with
// quick actions to open the web UI.
//
// It is behind the `tray` build tag because the systray dependency uses cgo and
// native UI frameworks (Cocoa on macOS), which aren't available in headless
// builds/CI. Build it explicitly:
//
//	go build -mod=mod -tags tray ./cmd/tray
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"time"

	"fyne.io/systray"

	"github.com/uwin/cnc-proxy/internal/apiclient"
	"github.com/uwin/cnc-proxy/internal/trayicon"
	"github.com/uwin/cnc-proxy/internal/traymgr"
)

var (
	apiBase               = flag.String("api", "http://127.0.0.1:8420", "proxy API base URL")
	authUser              = flag.String("auth-user", envDefault("CNC_AUTH_USER", "cnc"), "HTTP Basic Auth username")
	authToken             = flag.String("auth-token", envDefault("CNC_AUTH_TOKEN", ""), "HTTP Basic Auth token/password")
	poll                  = flag.Duration("poll", 3*time.Second, "status poll interval")
	configPath            = flag.String("config", traymgr.DefaultConfigPath(), "tray manager config path")
	upgradeFinalize       = flag.Bool(traymgr.ManagerUpgradeFinalizeFlag, false, "finalize a staged tray manager upgrade")
	upgradeStaged         = flag.String(traymgr.ManagerUpgradeStagedFlag, "", "staged tray manager binary")
	upgradeTarget         = flag.String(traymgr.ManagerUpgradeTargetFlag, "", "installed tray manager binary to replace")
	upgradeCleanup        = flag.String(traymgr.ManagerUpgradeCleanupFlag, "", "staged tray manager binary to remove after relaunch")
	upgradeStartProxy     = flag.Bool(traymgr.ManagerUpgradeStartProxyFlag, false, "start the managed proxy after a tray manager upgrade")
	upgradeInstallTimeout = flag.Duration(traymgr.ManagerUpgradeInstallTimeoutFlag, 45*time.Second, "tray manager upgrade install timeout")
	explicit              = map[string]bool{}
)

func main() {
	flag.Parse()
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	if *upgradeFinalize {
		err := traymgr.FinalizeManagerUpgrade(context.Background(), traymgr.ManagerUpgradeFinalizeOptions{
			StagedBinary:   *upgradeStaged,
			TargetBinary:   *upgradeTarget,
			ConfigPath:     *configPath,
			StartProxy:     *upgradeStartProxy,
			InstallTimeout: *upgradeInstallTimeout,
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "manager upgrade:", err)
			os.Exit(1)
		}
		return
	}
	if *upgradeCleanup != "" {
		cleanup := *upgradeCleanup
		time.AfterFunc(3*time.Second, func() { _ = os.Remove(cleanup) })
	}
	systray.Run(onReady, func() {})
}

func onReady() {
	if icon, err := trayicon.Bytes(runtime.GOOS); err == nil {
		systray.SetIcon(icon)
	}
	systray.SetTitle("CNC")
	systray.SetTooltip("CNC Proxy")
	mState := systray.AddMenuItem("Machine: …", "Current machine state")
	mState.Disable()
	mSync := systray.AddMenuItem("Sync: …", "Pending sync operations")
	mSync.Disable()
	mProc := systray.AddMenuItem("Proxy: …", "Managed proxy process")
	mProc.Disable()
	systray.AddSeparator()
	mOpen := systray.AddMenuItem("Open Web UI", "Open the proxy web UI in a browser")
	mMount := systray.AddMenuItem("Mount WebDAV", "Mount the WebDAV file view")
	mRefreshMount := systray.AddMenuItem("Refresh WebDAV Mount", "Clear the native WebDAV mount and mount it again")
	mManager := systray.AddMenuItem("Open Manager", "Open the tray manager configuration UI")
	systray.AddSeparator()
	mStart := systray.AddMenuItem("Start Proxy", "Start the managed cnc-proxy process")
	mRestart := systray.AddMenuItem("Restart Proxy", "Restart the managed cnc-proxy process")
	mStop := systray.AddMenuItem("Stop Proxy", "Stop the managed cnc-proxy process")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit the tray app")

	ctx, cancel := context.WithCancel(context.Background())
	appErrCh := make(chan error, 1)
	managerExitCh := make(chan struct{}, 1)
	mountDone := make(chan webDAVMountResult, 1)
	app, err := traymgr.NewApp(*configPath, traymgr.OSNotifier{})
	if err != nil {
		systray.SetTitle("CNC ⚠")
		mState.SetTitle("Manager error: " + err.Error())
		updateMountItem(app, mMount)
		updateRefreshMountItem(app, mRefreshMount, false)
	} else {
		app.Server.SetManagerProcessExit(func() {
			select {
			case managerExitCh <- struct{}{}:
			default:
			}
		})
		if *upgradeStartProxy {
			startCtx, startCancel := context.WithTimeout(ctx, 15*time.Second)
			if err := app.Supervisor.StartAfterDeployment(startCtx); err != nil {
				systray.SetTitle("CNC ⚠")
				mState.SetTitle("Proxy restart failed: " + err.Error())
			}
			startCancel()
		}
		updateMountItem(app, mMount)
		updateRefreshMountItem(app, mRefreshMount, false)
		go func() {
			if err := app.Run(ctx); err != nil {
				select {
				case appErrCh <- err:
				case <-ctx.Done():
				}
			}
		}()
	}

	go func() {
		ticker := time.NewTicker(*poll)
		defer ticker.Stop()
		mountBusy := false
		var uploadTracker traymgr.UploadCompletionTracker
		var deletionTracker traymgr.WebDAVVisibleDeletionTracker
		var pendingWebDAVRefresh []string
		beginMountSet := func(enable bool) {
			if app == nil || mountBusy {
				return
			}
			mountBusy = true
			mMount.Disable()
			mRefreshMount.Disable()
			if enable {
				mMount.SetTitle("Mounting WebDAV…")
			} else {
				mMount.SetTitle("Unmounting WebDAV…")
			}
			go runWebDAVMountSet(ctx, app, enable, mountDone)
		}
		beginRemount := func() {
			if app == nil || mountBusy {
				return
			}
			mountBusy = true
			mMount.Disable()
			mRefreshMount.Disable()
			mMount.SetTitle("Mounting WebDAV…")
			go runWebDAVRemount(ctx, app, mountDone)
		}
		requestWebDAVRefresh := func(paths []string) {
			if app == nil {
				pendingWebDAVRefresh = nil
				return
			}
			if len(paths) > 0 {
				pendingWebDAVRefresh = append(pendingWebDAVRefresh, paths...)
			}
			if mountBusy || len(pendingWebDAVRefresh) == 0 {
				return
			}
			if !app.Supervisor.Config().WebDAVMount.Enabled {
				statusCtx, statusCancel := context.WithTimeout(context.Background(), 3*time.Second)
				status := app.WebDAVMountStatus(statusCtx)
				statusCancel()
				if !status.Mounted {
					pendingWebDAVRefresh = nil
					return
				}
			}
			paths = pendingWebDAVRefresh
			pendingWebDAVRefresh = nil
			app.NotifyWebDAVRefreshAfterDelete(paths)
			beginRemount()
		}
		update(app, mState, mSync, mProc, &uploadTracker, &deletionTracker)
		updateMountItem(app, mMount)
		updateRefreshMountItem(app, mRefreshMount, mountBusy)
		for {
			select {
			case <-ticker.C:
				deleted := update(app, mState, mSync, mProc, &uploadTracker, &deletionTracker)
				requestWebDAVRefresh(deleted)
				if !mountBusy {
					updateMountItem(app, mMount)
					updateRefreshMountItem(app, mRefreshMount, mountBusy)
				}
			case err := <-appErrCh:
				cancel()
				systray.SetTitle("CNC ⚠")
				mState.SetTitle("Manager stopped: " + err.Error())
			case <-managerExitCh:
				cancel()
				systray.Quit()
				time.AfterFunc(2*time.Second, func() { os.Exit(0) })
				return
			case <-mountDone:
				mountBusy = false
				updateMountItem(app, mMount)
				updateRefreshMountItem(app, mRefreshMount, mountBusy)
				requestWebDAVRefresh(nil)
			case <-mOpen.ClickedCh:
				openBrowser(currentAPIBase(app))
			case <-mMount.ClickedCh:
				if app != nil && !mountBusy {
					statusCtx, statusCancel := context.WithTimeout(context.Background(), 3*time.Second)
					status := app.WebDAVMountStatus(statusCtx)
					statusCancel()
					if status.Busy {
						updateMountItem(app, mMount)
						continue
					}
					beginMountSet(!status.Mounted)
				}
			case <-mRefreshMount.ClickedCh:
				beginRemount()
			case <-mManager.ClickedCh:
				openBrowser(managerURL(app))
			case <-mStart.ClickedCh:
				if app != nil {
					_ = app.Supervisor.Start()
					beginRemount()
					update(app, mState, mSync, mProc, &uploadTracker, &deletionTracker)
				}
			case <-mRestart.ClickedCh:
				if app != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					_ = app.Supervisor.Restart(ctx)
					cancel()
					beginRemount()
					update(app, mState, mSync, mProc, &uploadTracker, &deletionTracker)
				}
			case <-mStop.ClickedCh:
				if app != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					_ = app.Supervisor.Stop(ctx)
					cancel()
					update(app, mState, mSync, mProc, &uploadTracker, &deletionTracker)
				}
			case <-mQuit.ClickedCh:
				cancel()
				systray.Quit()
				return
			}
		}
	}()
}

type webDAVMountResult struct{}

func runWebDAVMountSet(parent context.Context, app *traymgr.App, enable bool, done chan<- webDAVMountResult) {
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	err := app.SetWebDAVMountEnabled(ctx, enable)
	cancel()
	sendWebDAVMountDone(done, err)
}

func runWebDAVRemount(parent context.Context, app *traymgr.App, done chan<- webDAVMountResult) {
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	err := app.RemountWebDAV(ctx)
	cancel()
	sendWebDAVMountDone(done, err)
}

func sendWebDAVMountDone(done chan<- webDAVMountResult, _ error) {
	select {
	case done <- webDAVMountResult{}:
	default:
	}
}

func updateMountItem(app *traymgr.App, item *systray.MenuItem) {
	if app == nil {
		item.Disable()
		return
	}
	if !traymgr.WebDAVMountSupported() {
		item.SetTitle("Mount WebDAV: unsupported")
		item.Disable()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	status := app.WebDAVMountStatus(ctx)
	cancel()
	if status.Busy {
		item.SetTitle("Mounting WebDAV…")
		item.Disable()
		return
	}
	if status.Error != "" {
		switch status.ErrorAction {
		case "unmount":
			item.SetTitle("WebDAV Unmount Failed")
		default:
			item.SetTitle("WebDAV Mount Failed")
		}
		item.Enable()
		return
	}
	if status.Mounted {
		item.SetTitle("Unmount WebDAV")
	} else if status.Desired {
		item.SetTitle("Retry WebDAV Mount")
	} else {
		item.SetTitle("Mount WebDAV")
	}
	item.Enable()
}

func updateRefreshMountItem(app *traymgr.App, item *systray.MenuItem, busy bool) {
	if app == nil || !traymgr.WebDAVMountSupported() {
		item.Disable()
		return
	}
	item.SetTitle("Refresh WebDAV Mount")
	if busy {
		item.Disable()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	status := app.WebDAVMountStatus(ctx)
	cancel()
	if status.Mounted {
		item.Enable()
	} else {
		item.Disable()
	}
}

func update(app *traymgr.App, mState, mSync, mProc *systray.MenuItem, uploadTracker *traymgr.UploadCompletionTracker, deletionTracker *traymgr.WebDAVVisibleDeletionTracker) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	if app != nil {
		mProc.SetTitle("Proxy: " + app.Supervisor.String())
	}
	client := currentClient(app)
	st, err := client.Machine(ctx)
	if err != nil {
		systray.SetTitle("CNC ⚠")
		mState.SetTitle("Machine: unreachable")
		mSync.SetTitle("Sync: —")
		return nil
	}
	mState.SetTitle(fmt.Sprintf("Machine: %s (%s)", displayState(st.State), st.Mode))

	files, err := client.Files(ctx)
	if err != nil {
		mSync.SetTitle("Sync: —")
		return nil
	}
	var deleted []string
	if deletionTracker != nil {
		deleted = deletionTracker.Observe(files)
	}
	pending := apiclient.PendingCount(files)
	if pending == 0 {
		systray.SetTitle("CNC ✓")
		mSync.SetTitle("Sync: up to date")
	} else {
		systray.SetTitle(fmt.Sprintf("CNC ⟳%d", pending))
		mSync.SetTitle(fmt.Sprintf("Sync: %d pending", pending))
	}

	jobs, err := client.Jobs(ctx)
	if err != nil || uploadTracker == nil {
		return deleted
	}
	for _, upload := range uploadTracker.Observe(jobs) {
		if app != nil {
			app.NotifyUploadCompleted(upload.Path)
		}
	}
	return deleted
}

func currentClient(app *traymgr.App) *apiclient.Client {
	user, token := *authUser, *authToken
	if app != nil && !explicit["auth-user"] && !explicit["auth-token"] {
		user, token = traymgr.Auth(app.Supervisor.Config())
	}
	return apiclient.NewWithAuth(currentAPIBase(app), user, token)
}

func currentAPIBase(app *traymgr.App) string {
	if explicit["api"] || app == nil {
		return *apiBase
	}
	return traymgr.APIBase(app.Supervisor.Config())
}

func managerURL(app *traymgr.App) string {
	if app == nil {
		return "http://127.0.0.1:8430"
	}
	addr := app.Supervisor.Config().AdminListen
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + host + ":" + port
}

func displayState(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	args = append(args, url)
	_ = exec.Command(cmd, args...).Start()
}

func envDefault(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}
