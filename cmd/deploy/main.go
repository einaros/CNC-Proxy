// Command deploy uploads a source tree zip to a remote CNC tray manager.
package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
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
	flag.Parse()

	zipPath, err := zipSource(*source)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zip:", err)
		os.Exit(1)
	}
	defer os.Remove(zipPath)
	res, err := upload(*target, *token, *restart, zipPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "deploy:", err)
		os.Exit(1)
	}
	fmt.Printf("deployment completed: source=%s backup=%s restarted=%v\n", res.Result.SourceDir, res.Result.BackupDir, res.Result.Restarted)
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
		SourceDir string `json:"source_dir"`
		BackupDir string `json:"backup_dir"`
		BuildLog  string `json:"build_log"`
		Restarted bool   `json:"restarted"`
	} `json:"result"`
	Error string `json:"error"`
}

func upload(target, token string, restart bool, zipPath string) (deployResponse, error) {
	u := strings.TrimRight(target, "/") + "/api/deploy"
	if !restart {
		u += "?restart=false"
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
		return out, fmt.Errorf("status %d: %s", resp.StatusCode, out.Error)
	}
	return out, nil
}

func envDefault(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback
}
