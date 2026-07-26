package traymgr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

type ProcessState struct {
	Running   bool      `json:"running"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	LastExit  string    `json:"last_exit,omitempty"`
}

type Supervisor struct {
	mu       sync.Mutex
	deployMu sync.Mutex
	cfg      Config
	cmd      *exec.Cmd
	done     chan error
	started  time.Time
	lastExit string
	logPath  string
}

func NewSupervisor(cfg Config, logPath string) *Supervisor {
	return &Supervisor{cfg: normalizeConfig(cfg), logPath: logPath}
}

func (s *Supervisor) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return normalizeConfig(s.cfg)
}

func (s *Supervisor) SetConfig(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = normalizeConfig(cfg)
}

func (s *Supervisor) State() ProcessState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := ProcessState{LastExit: s.lastExit}
	if s.cmd != nil && s.cmd.Process != nil {
		st.Running = true
		st.PID = s.cmd.Process.Pid
		st.StartedAt = s.started
	}
	return st
}

func (s *Supervisor) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil && s.cmd.Process != nil {
		return nil
	}
	cfg := normalizeConfig(s.cfg)
	options, _ := ProxyOptionsForConfig(context.Background(), cfg)
	cfg = normalizeConfigWithOptions(cfg, options)
	if err := ValidateConfigWithOptions(cfg, options); err != nil {
		return err
	}
	cmd := exec.Command(cfg.ProxyBinary, ProxyArgsForOptions(cfg, options)...)
	configureBackgroundCommand(cmd)
	if dir, err := proxyWorkingDir(cfg); err != nil {
		return err
	} else if dir != "" {
		cmd.Dir = dir
	}
	if s.logPath != "" {
		if err := os.MkdirAll(filepath.Dir(s.logPath), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		cmd.Stdout = f
		cmd.Stderr = f
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	s.cmd = cmd
	s.done = done
	s.started = time.Now()
	s.lastExit = ""
	go s.wait(cmd, done)
	return nil
}

func proxyWorkingDir(cfg Config) (string, error) {
	dir := stringsTrim(cfg.SourceDir)
	if dir == "" {
		return "", nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("source_dir is not a directory: %s", dir)
	}
	return dir, nil
}

func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	cmd := s.cmd
	done := s.done
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		_ = killProcess(cmd)
	} else {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			_ = killProcess(cmd)
		}
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		_ = killProcess(cmd)
		// The kill forces the process to exit; wait for the reaper so the
		// supervisor is never left half-reaped (s.cmd still set for a dead
		// process). Restart relies on this: Start must observe "not running"
		// after Stop returns, even on the deadline path.
		<-done
		return ctx.Err()
	}
}

func (s *Supervisor) Restart(ctx context.Context) error {
	if err := s.Stop(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return s.Start()
}

// StartAfterDeployment removes only stale processes whose executable is
// one of the configured managed proxy binary's deployment paths. A relaunched
// manager cannot inherit its predecessor's in-memory child-process handle, so
// without this recovery an orphaned old binary can keep the API port and make
// a freshly built proxy exit immediately while deployment appears successful.
func (s *Supervisor) StartAfterDeployment(ctx context.Context) error {
	readyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := stopStaleManagedProxyProcesses(readyCtx, s.Config()); err != nil {
		return err
	}
	if err := s.Start(); err != nil {
		return err
	}
	return s.waitUntilProxyReady(readyCtx)
}

func (s *Supervisor) waitUntilProxyReady(ctx context.Context) error {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	healthURL := APIBase(s.Config()) + "/healthz"
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var healthySince time.Time
	for {
		state := s.State()
		if !state.Running {
			if state.LastExit == "" {
				return errors.New("proxy stopped before becoming ready")
			}
			return fmt.Errorf("proxy stopped before becoming ready: %s", state.LastExit)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		healthy := err == nil && resp.StatusCode == http.StatusOK
		if resp != nil {
			resp.Body.Close()
		}
		if healthy {
			if healthySince.IsZero() {
				healthySince = time.Now()
			} else if time.Since(healthySince) >= 300*time.Millisecond {
				return nil
			}
		} else {
			healthySince = time.Time{}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for rebuilt proxy readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *Supervisor) Build(ctx context.Context) (string, error) {
	cfg := s.Config()
	if stringsTrim(cfg.SourceDir) == "" {
		return "", errors.New("source_dir must be set before building")
	}
	build, err := prepareBuild(ctx, cfg)
	if err != nil {
		return "", err
	}
	defer build.cleanup()
	cmd := build.cmd
	cmd.Dir = cfg.SourceDir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), err
	}
	if build.promote != nil {
		if err := build.promote(); err != nil {
			return out.String(), err
		}
		fmt.Fprintf(&out, "installed %s\n", build.outputPath)
	}
	return out.String(), nil
}

func (s *Supervisor) wait(cmd *exec.Cmd, done chan<- error) {
	err := cmd.Wait()
	// Clear the supervisor state BEFORE signaling done: Stop returns when
	// done fires, and Restart immediately calls Start, which must not observe
	// the exited command as still running (that would make the restart a
	// silent no-op).
	s.mu.Lock()
	if s.cmd == cmd {
		s.cmd = nil
		s.done = nil
	}
	if err != nil {
		s.lastExit = err.Error()
	} else {
		s.lastExit = "clean exit"
	}
	s.mu.Unlock()
	done <- err
}

func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

type preparedBuild struct {
	cmd        *exec.Cmd
	outputPath string
	stagedPath string
	promote    func() error
	cleanup    func()
}

func prepareBuild(ctx context.Context, cfg Config) (preparedBuild, error) {
	cfg = normalizeConfig(cfg)
	if isManagedProxyBuildCommand(cfg.BuildCommand) {
		outputPath, err := managedBuildOutputPath(cfg)
		if err != nil {
			return preparedBuild{}, err
		}
		stagedPath, err := stagedBuildOutputPath(outputPath)
		if err != nil {
			return preparedBuild{}, err
		}
		cmd := exec.CommandContext(ctx, "go", managedProxyBuildArgs(stagedPath)...)
		configureBackgroundCommand(cmd)
		return preparedBuild{
			cmd:        cmd,
			outputPath: outputPath,
			stagedPath: stagedPath,
			promote: func() error {
				return promoteStagedBinary(stagedPath, outputPath)
			},
			cleanup: func() {
				_ = os.Remove(stagedPath)
			},
		}, nil
	}
	cmd, err := shellCommand(ctx, cfg.BuildCommand)
	if err != nil {
		return preparedBuild{}, err
	}
	configureBackgroundCommand(cmd)
	return preparedBuild{cmd: cmd, cleanup: func() {}}, nil
}

func managedProxyBuildArgs(outputPath string) []string {
	return managedProxyBuildArgsForGOOS(outputPath, runtime.GOOS)
}

func managedProxyBuildArgsForGOOS(outputPath, goos string) []string {
	args := []string{"build", "-mod=mod"}
	if goos == "windows" {
		args = append(args, "-trimpath", "-ldflags=-s -w -H=windowsgui")
	}
	return append(args, "-o", outputPath, "./cmd/proxy")
}

func buildCommand(ctx context.Context, cfg Config) (*exec.Cmd, error) {
	build, err := prepareBuild(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return build.cmd, nil
}

func managedBuildOutputPath(cfg Config) (string, error) {
	sourceDir := stringsTrim(cfg.SourceDir)
	if sourceDir == "" {
		return "", errors.New("source_dir must be set before building")
	}
	proxyBinary := stringsTrim(cfg.ProxyBinary)
	if proxyBinary == "" {
		return "", errors.New("proxy binary must not be empty")
	}
	if filepath.IsAbs(proxyBinary) {
		return proxyBinary, nil
	}
	return filepath.Join(sourceDir, proxyBinary), nil
}

func stagedBuildOutputPath(outputPath string) (string, error) {
	return stagedBuildOutputPathWithPrefix(outputPath, ".cnc-proxy-build-")
}

func stagedBuildOutputPathWithPrefix(outputPath, prefix string) (string, error) {
	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ext := filepath.Ext(outputPath)
	if ext == "" && runtime.GOOS == "windows" {
		ext = ".exe"
	}
	for i := 0; i < 100; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s%d-%d%s", prefix, time.Now().UnixNano(), i, ext))
		f, err := os.OpenFile(candidate, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if closeErr := f.Close(); closeErr != nil {
				_ = os.Remove(candidate)
				return "", closeErr
			}
			if err := os.Remove(candidate); err != nil {
				return "", err
			}
			return candidate, nil
		}
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("could not reserve staged build path under %s", dir)
}

func promoteStagedBinary(stagedPath, outputPath string) error {
	if stringsTrim(stagedPath) == "" {
		return errors.New("staged build path must not be empty")
	}
	if stringsTrim(outputPath) == "" {
		return errors.New("proxy binary path must not be empty")
	}
	backupPath := ""
	if _, err := os.Stat(outputPath); err == nil {
		backupPath = outputPath + ".previous-" + time.Now().Format("20060102-150405")
		if err := os.Rename(outputPath, backupPath); err != nil {
			return fmt.Errorf("cannot replace proxy binary %s; stop any running proxy process and retry: %w", outputPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(stagedPath, outputPath); err != nil {
		if backupPath != "" {
			_ = os.Rename(backupPath, outputPath)
		}
		return fmt.Errorf("install built proxy binary %s: %w", outputPath, err)
	}
	if backupPath != "" {
		_ = os.Remove(backupPath)
	}
	return nil
}

func IsManagedProxyBuildCommand(command string) bool {
	fields, ok := splitCommandFields(command)
	if !ok || len(fields) < 6 {
		return false
	}
	if !strings.EqualFold(fields[0], "go") ||
		fields[1] != "build" ||
		fields[len(fields)-1] != "./cmd/proxy" {
		return false
	}
	seenMod := false
	for i := 2; i < len(fields)-1; {
		field := fields[i]
		switch {
		case field == "-mod=mod":
			seenMod = true
			i++
		case field == "-trimpath":
			i++
		case field == "-ldflags":
			if i+1 >= len(fields)-1 || !isManagedProxyLDFlags(fields[i+1]) {
				return false
			}
			i += 2
		case strings.HasPrefix(field, "-ldflags="):
			if !isManagedProxyLDFlags(strings.TrimPrefix(field, "-ldflags=")) {
				return false
			}
			i++
		case field == "-o":
			if !seenMod {
				return false
			}
			outputFields := fields[i+1 : len(fields)-1]
			if len(outputFields) == 0 {
				return false
			}
			for _, outputField := range outputFields[1:] {
				if strings.HasPrefix(outputField, "-") {
					return false
				}
			}
			base := path.Base(strings.ReplaceAll(outputFields[len(outputFields)-1], "\\", "/"))
			return base == "cnc-proxy" || base == "cnc-proxy.exe"
		default:
			return false
		}
	}
	return false
}

func isManagedProxyBuildCommand(command string) bool {
	return IsManagedProxyBuildCommand(command)
}

func isManagedProxyLDFlags(value string) bool {
	value = strings.Join(strings.Fields(value), " ")
	return value == "-s -w -H=windowsgui" || value == "-H=windowsgui"
}

func splitCommandFields(command string) ([]string, bool) {
	var fields []string
	var field strings.Builder
	inQuote := false
	haveField := false
	for i := 0; i < len(command); i++ {
		c := command[i]
		if c == '\\' && i+1 < len(command) && command[i+1] == '"' {
			inQuote = !inQuote
			haveField = true
			i++
			continue
		}
		if c == '"' {
			inQuote = !inQuote
			haveField = true
			continue
		}
		if (c == ' ' || c == '\t' || c == '\r' || c == '\n') && !inQuote {
			if haveField {
				fields = append(fields, field.String())
				field.Reset()
				haveField = false
			}
			continue
		}
		field.WriteByte(c)
		haveField = true
	}
	if inQuote {
		return nil, false
	}
	if haveField {
		fields = append(fields, field.String())
	}
	return fields, true
}

func shellCommand(ctx context.Context, command string) (*exec.Cmd, error) {
	if stringsTrim(command) == "" {
		return nil, errors.New("build command must not be empty")
	}
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/C", command), nil
	}
	return exec.CommandContext(ctx, "/bin/sh", "-c", command), nil
}

func stringsTrim(s string) string {
	return string(bytes.TrimSpace([]byte(s)))
}

func (s *Supervisor) String() string {
	st := s.State()
	if st.Running {
		return fmt.Sprintf("running pid=%d", st.PID)
	}
	if st.LastExit != "" {
		return "stopped: " + st.LastExit
	}
	return "stopped"
}
