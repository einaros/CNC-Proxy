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
	"os/exec"
	"runtime"
	"time"

	"fyne.io/systray"

	"github.com/uwin/cnc-proxy/internal/apiclient"
)

var (
	apiBase = flag.String("api", "http://127.0.0.1:8420", "proxy API base URL")
	poll    = flag.Duration("poll", 3*time.Second, "status poll interval")
)

func main() {
	flag.Parse()
	systray.Run(onReady, func() {})
}

func onReady() {
	systray.SetTitle("CNC")
	systray.SetTooltip("CNC Proxy")
	mState := systray.AddMenuItem("Machine: …", "Current machine state")
	mState.Disable()
	mSync := systray.AddMenuItem("Sync: …", "Pending sync operations")
	mSync.Disable()
	systray.AddSeparator()
	mOpen := systray.AddMenuItem("Open Web UI", "Open the proxy web UI in a browser")
	mQuit := systray.AddMenuItem("Quit", "Quit the tray app")

	client := apiclient.New(*apiBase)

	go func() {
		ticker := time.NewTicker(*poll)
		defer ticker.Stop()
		update(client, mState, mSync)
		for {
			select {
			case <-ticker.C:
				update(client, mState, mSync)
			case <-mOpen.ClickedCh:
				openBrowser(*apiBase)
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func update(client *apiclient.Client, mState, mSync *systray.MenuItem) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

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
