// Command wininstaller installs the Windows CNC Proxy tray distribution.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/uwin/cnc-proxy/internal/installerpayload"
	"github.com/uwin/cnc-proxy/internal/traymgr"
)

func main() {
	dir := flag.String("dir", defaultInstallDir(), "install directory")
	startup := flag.Bool("startup", true, "start tray app when the user logs in")
	start := flag.Bool("start", true, "start tray app after installation")
	overwriteConfig := flag.Bool("overwrite-config", false, "replace existing tray config")
	remote := flag.Bool("remote", false, "listen on 0.0.0.0:8430 and require/generate a manager token")
	var managerListen string
	var managerToken string
	flag.StringVar(&managerListen, "manager-listen", "", "tray manager listen address")
	flag.StringVar(&managerListen, "admin-listen", "", "deprecated alias for -manager-listen")
	flag.StringVar(&managerToken, "manager-token", "", "tray manager token")
	flag.StringVar(&managerToken, "admin-token", "", "deprecated alias for -manager-token")
	flag.Parse()

	if runtime.GOOS != "windows" {
		fmt.Fprintln(os.Stderr, "warning: this installer is intended to run on Windows")
	}

	payload, err := readPayload()
	if err != nil {
		fatal(err)
	}
	installDir, err := filepath.Abs(*dir)
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		fatal(err)
	}
	if err := extractPayload(payload, installDir); err != nil {
		fatal(err)
	}

	cfgPath := traymgr.DefaultConfigPath()
	cfg, err := traymgr.LoadConfig(cfgPath)
	if err != nil {
		fatal(err)
	}
	if *overwriteConfig || !fileExists(cfgPath) {
		cfg = traymgr.DefaultConfig()
	}
	cfg.ProxyBinary = filepath.Join(installDir, "cnc-proxy.exe")
	cfg.SourceDir = filepath.Join(installDir, "source")
	if *overwriteConfig ||
		!fileExists(cfgPath) ||
		strings.TrimSpace(cfg.BuildCommand) == "" ||
		cfg.BuildCommand == traymgr.DefaultConfig().BuildCommand ||
		traymgr.IsManagedProxyBuildCommand(cfg.BuildCommand) {
		cfg.BuildCommand = windowsProxyBuildCommand(cfg.ProxyBinary)
	}
	cfg.AutoStart = *startup
	if managerListen != "" {
		cfg.AdminListen = managerListen
	} else if *remote {
		cfg.AdminListen = "0.0.0.0:8430"
	}
	if managerToken != "" {
		cfg.AdminToken = managerToken
	} else if *remote && strings.TrimSpace(cfg.AdminToken) == "" {
		token, err := randomToken()
		if err != nil {
			fatal(err)
		}
		cfg.AdminToken = token
	}
	if err := traymgr.SaveConfig(cfgPath, cfg); err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(cfg.SourceDir, 0o755); err != nil {
		fatal(err)
	}

	trayExe := filepath.Join(installDir, "cnc-tray.exe")
	if *startup {
		if err := enableStartup(trayExe, cfgPath); err != nil {
			fmt.Fprintln(os.Stderr, "warning: startup registration failed:", err)
		}
	} else {
		_ = disableStartup()
	}
	if *start {
		if err := exec.Command(trayExe, "-config", cfgPath).Start(); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not start tray app:", err)
		}
	}

	fmt.Println("CNC Proxy tray installed")
	fmt.Println("Install dir:", installDir)
	fmt.Println("Config:", cfgPath)
	fmt.Println("Manager:", managerURL(cfg.AdminListen))
	if cfg.AdminToken != "" {
		fmt.Println("Manager token:", cfg.AdminToken)
	}
}

func readPayload() ([]byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return installerpayload.Read(exe)
}

func extractPayload(payload []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return err
	}
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		name := filepath.Clean(filepath.FromSlash(f.Name))
		if name == "." || filepath.IsAbs(name) || filepath.VolumeName(name) != "" || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("payload contains unsafe path %q", f.Name)
		}
		target := filepath.Join(destAbs, name)
		targetAbs, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		if targetAbs != destAbs && !strings.HasPrefix(targetAbs, destAbs+string(filepath.Separator)) {
			return fmt.Errorf("payload escapes install directory with path %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(targetAbs, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(targetAbs), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(targetAbs, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, copyErr := io.Copy(out, rc)
		closeErr := out.Close()
		rc.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func enableStartup(trayExe, configPath string) error {
	cmdLine := fmt.Sprintf(`"%s" -config "%s"`, trayExe, configPath)
	return exec.Command("reg", "add", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "/v", "CNCProxyTray", "/t", "REG_SZ", "/d", cmdLine, "/f").Run()
}

func disableStartup() error {
	return exec.Command("reg", "delete", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "/v", "CNCProxyTray", "/f").Run()
}

func windowsProxyBuildCommand(proxyBinary string) string {
	return "go build -mod=mod -trimpath -ldflags=" + quoteWindowsArg("-s -w -H=windowsgui") + " -o " + quoteWindowsArg(proxyBinary) + " ./cmd/proxy"
}

func quoteWindowsArg(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
}

func defaultInstallDir() string {
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return filepath.Join(v, "CNC Proxy")
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "CNC Proxy")
	}
	return "CNC Proxy"
}

func managerURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func randomToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "installer:", err)
	os.Exit(1)
}
