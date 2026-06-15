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
	apiBase    = flag.String("api", "http://127.0.0.1:8420", "proxy API base URL")
	authUser   = flag.String("auth-user", envDefault("CNC_AUTH_USER", "cnc"), "HTTP Basic Auth username")
	authToken  = flag.String("auth-token", envDefault("CNC_AUTH_TOKEN", ""), "HTTP Basic Auth token/password")
	poll       = flag.Duration("poll", 3*time.Second, "status poll interval")
	configPath = flag.String("config", traymgr.DefaultConfigPath(), "tray manager config path")
	explicit   = map[string]bool{}
)

func main() {
	flag.Parse()
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
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
	mManager := systray.AddMenuItem("Open Manager", "Open the tray manager configuration UI")
	systray.AddSeparator()
	mStart := systray.AddMenuItem("Start Proxy", "Start the managed cnc-proxy process")
	mRestart := systray.AddMenuItem("Restart Proxy", "Restart the managed cnc-proxy process")
	mStop := systray.AddMenuItem("Stop Proxy", "Stop the managed cnc-proxy process")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit the tray app")

	ctx, cancel := context.WithCancel(context.Background())
	app, err := traymgr.NewApp(*configPath, traymgr.OSNotifier{})
	if err != nil {
		systray.SetTitle("CNC ⚠")
		mState.SetTitle("Manager error: " + err.Error())
	} else {
		go func() {
			if err := app.Run(ctx); err != nil {
				systray.SetTitle("CNC ⚠")
				mState.SetTitle("Manager stopped: " + err.Error())
			}
		}()
	}

	go func() {
		ticker := time.NewTicker(*poll)
		defer ticker.Stop()
		update(app, mState, mSync, mProc)
		for {
			select {
			case <-ticker.C:
				update(app, mState, mSync, mProc)
			case <-mOpen.ClickedCh:
				openBrowser(currentAPIBase(app))
			case <-mManager.ClickedCh:
				openBrowser(managerURL(app))
			case <-mStart.ClickedCh:
				if app != nil {
					_ = app.Supervisor.Start()
					update(app, mState, mSync, mProc)
				}
			case <-mRestart.ClickedCh:
				if app != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					_ = app.Supervisor.Restart(ctx)
					cancel()
					update(app, mState, mSync, mProc)
				}
			case <-mStop.ClickedCh:
				if app != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					_ = app.Supervisor.Stop(ctx)
					cancel()
					update(app, mState, mSync, mProc)
				}
			case <-mQuit.ClickedCh:
				cancel()
				systray.Quit()
				return
			}
		}
	}()
}

func update(app *traymgr.App, mState, mSync, mProc *systray.MenuItem) {
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
		return
	}
	mState.SetTitle(fmt.Sprintf("Machine: %s (%s)", displayState(st.State), st.Mode))

	files, err := client.Files(ctx)
	if err != nil {
		mSync.SetTitle("Sync: —")
		return
	}
	pending := apiclient.PendingCount(files)
	if pending == 0 {
		systray.SetTitle("CNC ✓")
		mSync.SetTitle("Sync: up to date")
	} else {
		systray.SetTitle(fmt.Sprintf("CNC ⟳%d", pending))
		mSync.SetTitle(fmt.Sprintf("Sync: %d pending", pending))
	}
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
