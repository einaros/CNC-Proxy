package traymgr

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type recordingNotifier struct {
	got []Notification
}

func (r *recordingNotifier) Notify(n Notification) error {
	r.got = append(r.got, n)
	return nil
}

func TestServerNotifyRequiresTokenAndRecordsNotification(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AdminListen = "127.0.0.1:8430"
	cfg.AdminToken = "secret"
	configPath := filepath.Join(t.TempDir(), "tray.json")
	sup := NewSupervisor(cfg, "")
	rec := &recordingNotifier{}
	srv := NewServer(configPath, sup, rec)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, _ := json.Marshal(Notification{Title: "CNC", Message: "Done", Level: "info"})
	resp, err := http.Post(ts.URL+"/api/notify", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/notify", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CNC-Tray-Token", "secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status = %d", resp.StatusCode)
	}
	if len(rec.got) != 1 || rec.got[0].Message != "Done" {
		t.Fatalf("notifier got %+v", rec.got)
	}
	if got := srv.recentNotifications(); len(got) != 1 || got[0].Title != "CNC" {
		t.Fatalf("recent notifications = %+v", got)
	}
}

func TestServerPutConfigPersistsAndUpdatesSupervisor(t *testing.T) {
	cfg := DefaultConfig()
	configPath := filepath.Join(t.TempDir(), "tray.json")
	sup := NewSupervisor(cfg, "")
	srv := NewServer(configPath, sup, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cfg.Flags["name"] = "Shop CNC"
	body, _ := json.Marshal(cfg)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/config status = %d", resp.StatusCode)
	}
	loaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Flags["name"] != "Shop CNC" || sup.Config().Flags["name"] != "Shop CNC" {
		t.Fatalf("config was not persisted/applied: loaded=%q supervisor=%q", loaded.Flags["name"], sup.Config().Flags["name"])
	}
}

func TestServerPutConfigRestartsWhenManagerSettingsChange(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AdminListen = "127.0.0.1:8430"
	configPath := filepath.Join(t.TempDir(), "tray.json")
	sup := NewSupervisor(cfg, "")
	srv := NewServer(configPath, sup, nil)
	srv.restartLag = 0
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cfg.AdminListen = "127.0.0.1:8431"
	body, _ := json.Marshal(cfg)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/config status = %d", resp.StatusCode)
	}
	var got struct {
		ManagerRestarting bool   `json:"manager_restarting"`
		ManagerURL        string `json:"manager_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.ManagerRestarting {
		t.Fatal("manager_restarting = false, want true")
	}
	if got.ManagerURL != "http://127.0.0.1:8431" {
		t.Fatalf("manager_url = %q", got.ManagerURL)
	}
	select {
	case <-srv.restartCh:
	case <-time.After(time.Second):
		t.Fatal("manager restart was not scheduled")
	}
}

func TestServerPutManagerConfigIgnoresInvalidProxyFlagsAndRestarts(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Flags["machine-transport"] = "bluetooth"
	configPath := filepath.Join(t.TempDir(), "tray.json")
	sup := NewSupervisor(cfg, "")
	srv := NewServer(configPath, sup, nil)
	srv.restartLag = 0
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	cfg.AdminListen = "127.0.0.1:8432"
	body, _ := json.Marshal(cfg)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/manager/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/manager/config status = %d", resp.StatusCode)
	}
	if got := sup.Config().AdminListen; got != "127.0.0.1:8432" {
		t.Fatalf("AdminListen = %q", got)
	}
	if got := sup.Config().Flags["machine-transport"]; got != "bluetooth" {
		t.Fatalf("machine-transport = %q", got)
	}
	select {
	case <-srv.restartCh:
	case <-time.After(time.Second):
		t.Fatal("manager restart was not scheduled")
	}
}

func TestServerPutProxyConfigDoesNotTouchManagerSettingsOrRestart(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AdminListen = "0.0.0.0:8430"
	cfg.AdminToken = ""
	configPath := filepath.Join(t.TempDir(), "tray.json")
	sup := NewSupervisor(cfg, "")
	srv := NewServer(configPath, sup, nil)
	srv.restartLag = 0
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := []byte(`{"flags":{"machine-transport":"usb","usb-device":"COM3"}}`)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/proxy/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/proxy/config status = %d", resp.StatusCode)
	}
	var gotResp struct {
		ProxyRestarted bool `json:"proxy_restarted"`
		ProxyChanged   bool `json:"proxy_changed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gotResp); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotResp.ProxyRestarted {
		t.Fatal("stopped proxy should not be restarted")
	}
	if !gotResp.ProxyChanged {
		t.Fatal("proxy_changed = false, want true")
	}
	if got := sup.Config().AdminListen; got != "0.0.0.0:8430" {
		t.Fatalf("AdminListen changed to %q", got)
	}
	if got := sup.Config().AdminToken; got != "" {
		t.Fatalf("AdminToken changed to %q", got)
	}
	if got := sup.Config().Flags["machine-transport"]; got != "usb" {
		t.Fatalf("machine-transport = %q", got)
	}
	select {
	case <-srv.restartCh:
		t.Fatal("proxy save should not schedule manager restart")
	default:
	}
}

func TestServerPutProxyConfigRestartsRunningProxy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell script as the supervised proxy")
	}
	dir := t.TempDir()
	proxy := filepath.Join(dir, "proxy.sh")
	if err := os.WriteFile(proxy, []byte("#!/bin/sh\ntrap 'exit 0' TERM INT\nwhile true; do sleep 1 & wait $!; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.ProxyBinary = proxy
	configPath := filepath.Join(dir, "tray.json")
	sup := NewSupervisor(cfg, "")
	if err := sup.Start(); err != nil {
		t.Fatalf("start proxy script: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sup.Stop(ctx)
	})
	srv := NewServer(configPath, sup, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := []byte(`{"flags":{"name":"Shop CNC"}}`)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/api/proxy/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/proxy/config status = %d", resp.StatusCode)
	}
	var got struct {
		ProxyRestarted bool         `json:"proxy_restarted"`
		ProxyChanged   bool         `json:"proxy_changed"`
		Process        ProcessState `json:"process"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !got.ProxyChanged || !got.ProxyRestarted {
		t.Fatalf("restart response = %+v, want changed and restarted", got)
	}
	if !got.Process.Running || got.Process.PID == 0 {
		t.Fatalf("process = %+v, want running proxy after restart", got.Process)
	}
}

func TestServerRestartManagerEndpointSchedulesRestart(t *testing.T) {
	cfg := DefaultConfig()
	configPath := filepath.Join(t.TempDir(), "tray.json")
	sup := NewSupervisor(cfg, "")
	srv := NewServer(configPath, sup, nil)
	srv.restartLag = 0
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/manager/restart", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/manager/restart status = %d", resp.StatusCode)
	}
	select {
	case <-srv.restartCh:
	case <-time.After(time.Second):
		t.Fatal("manager restart was not scheduled")
	}
}
