package traymgr

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type App struct {
	ConfigPath string
	Supervisor *Supervisor
	Server     *Server

	mountRetry time.Duration

	mountMu sync.Mutex

	mu              sync.Mutex
	lastMountAction string
	lastMountError  string
}

type WebDAVMountStatus struct {
	Desired     bool   `json:"desired"`
	Mounted     bool   `json:"mounted"`
	Busy        bool   `json:"busy,omitempty"`
	ErrorAction string `json:"error_action,omitempty"`
	Error       string `json:"error,omitempty"`
}

func NewApp(configPath string, notifier Notifier) (*App, error) {
	if configPath == "" {
		configPath = DefaultConfigPath()
	}
	_, statErr := os.Stat(configPath)
	missing := errors.Is(statErr, os.ErrNotExist)
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return nil, err
	}
	if err := ValidateManagerConfig(cfg); err != nil {
		return nil, err
	}
	if err := ValidateConfig(cfg); err == nil {
		if err := SaveConfig(configPath, cfg); err != nil {
			return nil, err
		}
	} else if missing {
		return nil, err
	}
	sup := NewSupervisor(cfg, DefaultLogPath(configPath))
	srv := NewServer(configPath, sup, notifier)
	return &App{ConfigPath: configPath, Supervisor: sup, Server: srv, mountRetry: 10 * time.Second}, nil
}

func (a *App) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(a.ConfigPath), 0o755); err != nil {
		return err
	}
	if a.Supervisor.Config().AutoStart {
		_ = a.Supervisor.Start()
	}
	go a.webDAVMountLoop(ctx)
	return a.Server.ListenAndServe(ctx)
}

func (a *App) SetWebDAVMountEnabled(ctx context.Context, enabled bool) error {
	action := "unmount"
	if enabled {
		action = "mount"
	}
	requestCfg := a.Supervisor.Config()
	requestCfg.WebDAVMount.Enabled = enabled
	a.logWebDAVOperation("info", fmt.Sprintf("%s requested target=%s", action, webDAVMountLogTarget(requestCfg)))

	a.mountMu.Lock()
	defer a.mountMu.Unlock()

	next := a.Supervisor.Config()
	next.WebDAVMount.Enabled = enabled
	if err := SaveManagerConfig(a.ConfigPath, next); err != nil {
		a.recordWebDAVMountError(action, err)
		return err
	}
	a.Supervisor.SetConfig(next)
	var err error
	if enabled {
		a.logWebDAVOperation("info", "mount native call starting target="+webDAVMountLogTarget(next))
		err = MountWebDAV(ctx, next)
	} else {
		a.logWebDAVOperation("info", "unmount native call starting target="+webDAVMountLogTarget(next))
		err = UnmountWebDAV(ctx, next)
	}
	if err != nil {
		a.recordWebDAVMountError(action, err)
		return err
	}
	a.setLastMountError("", "")
	a.logWebDAVOperation("info", action+" verified target="+webDAVMountLogTarget(next))
	return nil
}

func (a *App) RemountWebDAV(ctx context.Context) error {
	return a.remountWebDAV(ctx)
}

func (a *App) remountWebDAV(ctx context.Context) error {
	a.mountMu.Lock()
	defer a.mountMu.Unlock()

	cfg := a.Supervisor.Config()
	if !cfg.WebDAVMount.Enabled {
		a.logWebDAVOperation("info", "remount skipped because WebDAV mount is disabled")
		return nil
	}
	a.logWebDAVOperation("info", "remount native call starting target="+webDAVMountLogTarget(cfg))
	if err := MountWebDAV(ctx, cfg); err != nil {
		a.recordWebDAVMountError("remount", err)
		return err
	}
	a.setLastMountError("", "")
	a.logWebDAVOperation("info", "remount verified target="+webDAVMountLogTarget(cfg))
	return nil
}

func (a *App) WebDAVMountStatus(ctx context.Context) WebDAVMountStatus {
	cfg := a.Supervisor.Config()
	st := WebDAVMountStatus{Desired: cfg.WebDAVMount.Enabled}
	if !a.mountMu.TryLock() {
		st.Busy = true
		return st
	}
	defer a.mountMu.Unlock()

	mounted, err := WebDAVMounted(ctx, cfg)
	st.Mounted = mounted
	if err != nil {
		st.Error = err.Error()
	}
	if st.Error == "" {
		st.ErrorAction, st.Error = a.lastWebDAVMountError()
	}
	return st
}

func (a *App) webDAVMountLoop(ctx context.Context) {
	if !WebDAVMountSupported() {
		return
	}
	if a.Server == nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-a.Server.Ready():
	}
	delay := a.mountRetry
	if delay <= 0 {
		delay = 10 * time.Second
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	lastNotified := ""
	lastAutoResult := ""
	autoStartLogged := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if a.Supervisor.Config().WebDAVMount.Enabled {
				cfg := a.Supervisor.Config()
				if !autoStartLogged {
					a.logWebDAVOperation("info", "automatic remount check starting target="+webDAVMountLogTarget(cfg))
					autoStartLogged = true
				}
				mountCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				err := a.remountWebDAVQuiet(mountCtx)
				cancel()
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					msg := err.Error()
					_, current := a.lastWebDAVMountError()
					a.setLastMountError("remount", msg)
					if msg != lastAutoResult {
						a.logWebDAVOperation("error", "automatic remount failed: "+msg)
						lastAutoResult = msg
					}
					if msg != lastNotified && msg != current {
						a.notifyWebDAVMount("error", "WebDAV remount failed: "+msg)
						lastNotified = msg
					}
				} else {
					a.setLastMountError("", "")
					if lastAutoResult != "verified" {
						a.logWebDAVOperation("info", "automatic remount verified target="+webDAVMountLogTarget(cfg))
						lastAutoResult = "verified"
					}
					lastNotified = ""
				}
			}
			timer.Reset(delay)
		}
	}
}

func (a *App) remountWebDAVQuiet(ctx context.Context) error {
	a.mountMu.Lock()
	defer a.mountMu.Unlock()

	cfg := a.Supervisor.Config()
	if !cfg.WebDAVMount.Enabled {
		return nil
	}
	return MountWebDAV(ctx, cfg)
}

func (a *App) recordWebDAVMountError(action string, err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	a.setLastMountError(action, msg)
	a.logWebDAVOperation("error", "WebDAV "+action+" failed: "+msg)
	a.notifyWebDAVMount("error", "WebDAV "+action+" failed: "+msg)
}

func (a *App) logWebDAVOperation(level, message string) {
	if a.Server == nil {
		return
	}
	a.Server.addManagerLog(level, "webdav", message)
}

func webDAVMountLogTarget(cfg Config) string {
	req := webDAVMountRequestFromConfig(cfg)
	var parts []string
	if strings.TrimSpace(req.URL) != "" {
		parts = append(parts, "url="+sanitizedMountURL(req.URL))
	}
	if strings.TrimSpace(req.Drive) != "" {
		parts = append(parts, "drive="+req.Drive)
	}
	if strings.TrimSpace(req.MountPoint) != "" {
		parts = append(parts, "mount_point="+req.MountPoint)
	}
	if len(parts) == 0 {
		return "<empty>"
	}
	return strings.Join(parts, " ")
}

func (a *App) notifyWebDAVMount(level, message string) {
	if a.Server == nil {
		return
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	n := Notification{Title: "CNC Proxy", Message: message, Level: level, Time: time.Now()}
	a.Server.addNotification(n)
	if a.Server.notifier != nil {
		_ = a.Server.notifier.Notify(n)
	}
}

func (a *App) setLastMountError(action, msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastMountAction = strings.TrimSpace(action)
	a.lastMountError = strings.TrimSpace(msg)
}

func (a *App) lastWebDAVMountError() (string, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastMountAction, a.lastMountError
}
