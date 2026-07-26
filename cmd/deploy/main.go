// Command deploy uploads a source tree zip to a remote CNC tray manager.
package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	source := flag.String("source", ".", "source tree to deploy")
	target := flag.String("target", "http://127.0.0.1:8430", "tray manager URL, e.g. http://192.168.1.50:8430")
	token := flag.String("token", envDefault("CNC_TRAY_TOKEN", ""), "tray manager admin token")
	restart := flag.Bool("restart", true, "restart proxy after deployment")
	component := flag.String("component", "proxy", "deployment component: proxy, manager, or all")
	manager := flag.Bool("manager", false, "also upgrade the tray manager app (same as -component all)")
	managerOnly := flag.Bool("manager-only", false, "upgrade only the tray manager app (same as -component manager)")
	flag.Parse()
	selectedComponent := strings.ToLower(strings.TrimSpace(*component))
	if *manager {
		selectedComponent = "all"
	}
	if *managerOnly {
		selectedComponent = "manager"
	}
	if err := validateComponent(selectedComponent); err != nil {
		fmt.Fprintln(os.Stderr, "deploy:", err)
		os.Exit(1)
	}

	zipPath, err := zipSource(*source)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zip:", err)
		os.Exit(1)
	}
	defer os.Remove(zipPath)
	res, err := upload(*target, *token, *restart, selectedComponent, zipPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "deploy:", err)
		os.Exit(1)
	}
	fmt.Printf("deployment completed: source=%s restarted=%v\n", res.Result.SourceDir, res.Result.Restarted)
	if res.Result.ManagerUpgrade != nil {
		fmt.Printf("manager upgrade scheduled: target=%s proxy_start_on_relaunch=%v\n", res.Result.ManagerUpgrade.TargetBinary, res.Result.ManagerUpgrade.ProxyStartOnRelaunch)
		if res.Result.ManagerUpgrade.ProxyStartOnRelaunch {
			fmt.Println("proxy restart deferred until the upgraded manager relaunches")
		}
	}
}

func zipSource(source string) (string, error) {
	sourceAbs, err := filepath.Abs(source)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp("", "cnc-source-*.zip")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	zw := zip.NewWriter(tmp)
	err = filepath.WalkDir(sourceAbs, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(sourceAbs, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if shouldSkip(rel, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if d.IsDir() {
			_, err := zw.Create(name + "/")
			return err
		}
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		h.Name = name
		h.Method = zip.Deflate
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	})
	if closeErr := zw.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

func shouldSkip(rel string, d os.DirEntry) bool {
	base := filepath.Base(rel)
	switch base {
	case ".git", ".codex", ".claude", ".DS_Store", "vendor", "dist", "data", ".cnc-proxy", "node_modules",
		"cnc-proxy", "cnc-proxy.exe", "cnc-tray", "cnc-tray.exe", "deploy", "deploy.exe",
		"discoverybeacon", "discoverybeacon.exe":
		return true
	}
	if strings.HasSuffix(base, ".log") || strings.HasSuffix(base, ".tmp") {
		return true
	}
	return false
}

type deployResponse struct {
	OK     bool `json:"ok"`
	Result struct {
		SourceDir      string `json:"source_dir"`
		BuildLog       string `json:"build_log"`
		Restarted      bool   `json:"restarted"`
		ManagerUpgrade *struct {
			TargetBinary         string `json:"target_binary"`
			StagedBinary         string `json:"staged_binary"`
			BuildLog             string `json:"build_log"`
			RestartScheduled     bool   `json:"restart_scheduled"`
			ProxyStartOnRelaunch bool   `json:"proxy_start_on_relaunch"`
		} `json:"manager_upgrade"`
	} `json:"result"`
	Error string `json:"error"`
}

func upload(target, token string, restart bool, component, zipPath string) (deployResponse, error) {
	return uploadWithRetry(target, token, restart, component, zipPath, 8, 500*time.Millisecond)
}

func uploadWithRetry(target, token string, restart bool, component, zipPath string, attempts int, initialDelay time.Duration) (deployResponse, error) {
	if attempts < 1 {
		attempts = 1
	}
	var (
		out   deployResponse
		err   error
		delay = initialDelay
	)
	for attempt := 0; attempt < attempts; attempt++ {
		out, err = uploadOnce(target, token, restart, component, zipPath)
		if err == nil || !isTransientDeployError(err) {
			return out, err
		}
		if attempt == attempts-1 {
			break
		}
		fmt.Fprintf(os.Stderr, "deploy: remote files are still busy; retrying in %s\n", delay)
		if delay > 0 {
			time.Sleep(delay)
		}
		if delay < 4*time.Second {
			delay *= 2
		}
	}
	return out, err
}

type deployStatusError struct {
	status int
	detail string
}

func (e *deployStatusError) Error() string {
	return fmt.Sprintf("status %d: %s", e.status, e.detail)
}

func isTransientDeployError(err error) bool {
	var statusErr *deployStatusError
	if !errors.As(err, &statusErr) || statusErr.status < 500 {
		return false
	}
	detail := strings.ToLower(statusErr.detail)
	for _, fragment := range []string{
		"being used by another process",
		"used by another process",
		"cannot access the file",
		"access is denied",
		"sharing violation",
	} {
		if strings.Contains(detail, fragment) {
			return true
		}
	}
	return false
}

func uploadOnce(target, token string, restart bool, component, zipPath string) (deployResponse, error) {
	u := strings.TrimRight(target, "/") + "/api/deploy"
	q := url.Values{}
	if !restart {
		q.Set("restart", "false")
	}
	if component != "" && component != "proxy" {
		q.Set("component", component)
	}
	if encoded := q.Encode(); encoded != "" {
		u += "?" + encoded
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("source", filepath.Base(zipPath))
	if err != nil {
		return deployResponse{}, err
	}
	f, err := os.Open(zipPath)
	if err != nil {
		return deployResponse{}, err
	}
	if _, err := io.Copy(part, f); err != nil {
		f.Close()
		return deployResponse{}, err
	}
	f.Close()
	if err := mw.Close(); err != nil {
		return deployResponse{}, err
	}
	req, err := http.NewRequest(http.MethodPost, u, &body)
	if err != nil {
		return deployResponse{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("X-CNC-Tray-Token", token)
	}
	client := &http.Client{Timeout: 20 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return deployResponse{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return deployResponse{}, err
	}
	var out deployResponse
	if len(b) > 0 {
		if err := json.Unmarshal(b, &out); err != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return deployResponse{}, err
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if out.Error == "" {
			out.Error = strings.TrimSpace(string(b))
		}
		if out.Error == "" {
			out.Error = resp.Status
		}
		return out, &deployStatusError{status: resp.StatusCode, detail: out.Error}
	}
	return out, nil
}

func validateComponent(component string) error {
	switch component {
	case "proxy", "manager", "tray", "all", "both":
		return nil
	default:
		return fmt.Errorf("component must be proxy, manager, or all")
	}
}

func envDefault(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}
