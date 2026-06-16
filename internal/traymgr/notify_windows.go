//go:build windows

package traymgr

import (
	"encoding/base64"
	"os/exec"
	"strings"
	"unicode/utf16"
)

type OSNotifier struct{}

func (OSNotifier) Notify(n Notification) error {
	title := psQuote(n.Title)
	message := psQuote(n.Message)
	icon := "Info"
	switch strings.ToLower(n.Level) {
	case "error", "alarm", "warning":
		icon = "Warning"
	}
	script := `
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$n = New-Object System.Windows.Forms.NotifyIcon
$n.Icon = [System.Drawing.SystemIcons]::` + icon + `
$n.BalloonTipTitle = ` + title + `
$n.BalloonTipText = ` + message + `
$n.Visible = $true
$n.ShowBalloonTip(5000)
Start-Sleep -Seconds 6
$n.Dispose()
`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-EncodedCommand", encodePowerShell(script))
	return cmd.Start()
}

func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func encodePowerShell(s string) string {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		b[i*2] = byte(v)
		b[i*2+1] = byte(v >> 8)
	}
	return base64.StdEncoding.EncodeToString(b)
}
