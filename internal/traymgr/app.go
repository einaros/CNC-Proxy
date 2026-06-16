package traymgr

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

type App struct {
	ConfigPath string
	Supervisor *Supervisor
	Server     *Server
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
	return &App{ConfigPath: configPath, Supervisor: sup, Server: srv}, nil
}

func (a *App) Run(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(a.ConfigPath), 0o755); err != nil {
		return err
	}
	if a.Supervisor.Config().AutoStart {
		_ = a.Supervisor.Start()
	}
	return a.Server.ListenAndServe(ctx)
}
