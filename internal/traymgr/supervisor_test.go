package traymgr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRestartAlwaysLeavesProcessRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell script as the supervised proxy")
	}
	run := func(t *testing.T, trapLine string, stopTimeout time.Duration, iterations int) {
		dir := t.TempDir()
		proxy := filepath.Join(dir, "proxy.sh")
		// The stub answers -print-config-schema (Start probes the binary for
		// the proxy option schema) and writes a pid marker once its TERM trap
		// is installed so the test never signals a not-yet-ready shell.
		script := "#!/bin/sh\n" +
			"if [ \"$1\" = \"-print-config-schema\" ]; then exit 0; fi\n" +
			trapLine + "\n" +
			": > \"" + dir + "/ready.$$\"\n" +
			"while true; do sleep 1 & wait $!; done\n"
		if err := os.WriteFile(proxy, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		waitReady := func(pid int) {
			t.Helper()
			marker := filepath.Join(dir, fmt.Sprintf("ready.%d", pid))
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(marker); err == nil {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("proxy script pid %d never became ready", pid)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		cfg := DefaultConfig()
		cfg.ProxyBinary = proxy
		sup := NewSupervisor(cfg, "")
		if err := sup.Start(); err != nil {
			t.Fatalf("start proxy script: %v", err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = sup.Stop(ctx)
		})
		prev := sup.State().PID
		if prev == 0 {
			t.Fatal("initial start left no running process")
		}
		waitReady(prev)
		for i := 0; i < iterations; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			err := sup.Restart(ctx)
			cancel()
			if err != nil {
				t.Fatalf("Restart #%d: %v", i, err)
			}
			st := sup.State()
			if !st.Running {
				t.Fatalf("Restart #%d reported success but no process is running (state=%+v)", i, st)
			}
			if st.PID == prev {
				t.Fatalf("Restart #%d reported success but PID %d is unchanged: restart was a no-op", i, prev)
			}
			prev = st.PID
			waitReady(prev)
		}
	}
	t.Run("term", func(t *testing.T) {
		run(t, "trap 'exit 0' TERM INT", 5*time.Second, 10)
	})
	// A proxy that ignores SIGTERM forces Stop onto its deadline/kill path;
	// Restart must still leave a freshly started process rather than
	// no-opping against the half-reaped old one.
	t.Run("kill_after_stop_deadline", func(t *testing.T) {
		run(t, "trap '' TERM", 500*time.Millisecond, 5)
	})
}

func TestStartAfterDeploymentRejectsImmediateProxyExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell script as the supervised proxy")
	}
	dir := t.TempDir()
	proxy := filepath.Join(dir, "proxy.sh")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"-print-config-schema\" ]; then exit 0; fi\n" +
		"exit 23\n"
	if err := os.WriteFile(proxy, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.ProxyBinary = proxy
	sup := NewSupervisor(cfg, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := sup.StartAfterDeployment(ctx)
	if err == nil {
		t.Fatal("StartAfterDeployment reported success for a proxy that exited immediately")
	}
	if !strings.Contains(err.Error(), "stopped before becoming ready") {
		t.Fatalf("StartAfterDeployment error = %q", err)
	}
}

func TestProxyWorkingDirIgnoresMissingSourceDir(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SourceDir = filepath.Join(t.TempDir(), "missing")
	dir, err := proxyWorkingDir(cfg)
	if err != nil {
		t.Fatalf("proxyWorkingDir: %v", err)
	}
	if dir != "" {
		t.Fatalf("proxyWorkingDir = %q, want empty", dir)
	}
}

func TestProxyWorkingDirRejectsFileSourceDir(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SourceDir = filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(cfg.SourceDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := proxyWorkingDir(cfg); err == nil {
		t.Fatal("proxyWorkingDir should reject a source_dir that is a file")
	}
}

func TestProxyWorkingDirUsesExistingSourceDir(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SourceDir = t.TempDir()
	dir, err := proxyWorkingDir(cfg)
	if err != nil {
		t.Fatalf("proxyWorkingDir: %v", err)
	}
	if dir != cfg.SourceDir {
		t.Fatalf("proxyWorkingDir = %q, want %q", dir, cfg.SourceDir)
	}
}

func TestManagedProxyBuildCommandRecognizesInstallerCommands(t *testing.T) {
	for _, command := range []string{
		`go build -mod=mod -o "C:\Users\operator\AppData\Local\CNC Proxy\cnc-proxy.exe" ./cmd/proxy`,
		`go build -mod=mod -o \"C:\Users\operator\AppData\Local\CNC Proxy\cnc-proxy.exe\" ./cmd/proxy`,
		`go build -mod=mod -trimpath -ldflags="-s -w -H=windowsgui" -o "C:\Users\operator\AppData\Local\CNC Proxy\cnc-proxy.exe" ./cmd/proxy`,
		`go build -mod=mod -trimpath -ldflags=-H=windowsgui -o "C:\Users\operator\AppData\Local\CNC Proxy\cnc-proxy.exe" ./cmd/proxy`,
		`go build -mod=mod -o C:\Users\operator\AppData\Local\CNC Proxy\cnc-proxy.exe ./cmd/proxy`,
		`go build -mod=mod -o cnc-proxy.exe ./cmd/proxy`,
	} {
		t.Run(command, func(t *testing.T) {
			if !isManagedProxyBuildCommand(command) {
				t.Fatalf("isManagedProxyBuildCommand(%q) = false, want true", command)
			}
		})
	}
}

func TestPrepareBuildStagesManagedProxyBuildOutput(t *testing.T) {
	sourceDir := t.TempDir()
	installDir := filepath.Join(t.TempDir(), "CNC Proxy")
	proxyBinary := filepath.Join(installDir, "cnc-proxy.exe")
	cfg := DefaultConfig()
	cfg.SourceDir = sourceDir
	cfg.ProxyBinary = proxyBinary
	cfg.BuildCommand = `go build -mod=mod -o "` + proxyBinary + `" ./cmd/proxy`
	build, err := prepareBuild(context.Background(), cfg)
	if err != nil {
		t.Fatalf("prepareBuild: %v", err)
	}
	defer build.cleanup()
	if build.outputPath != proxyBinary {
		t.Fatalf("outputPath = %q, want %q", build.outputPath, proxyBinary)
	}
	if build.stagedPath == "" {
		t.Fatal("stagedPath is empty")
	}
	if build.stagedPath == proxyBinary {
		t.Fatal("managed build should not write directly to the installed proxy binary")
	}
	if filepath.Dir(build.stagedPath) != installDir {
		t.Fatalf("staged build dir = %q, want %q", filepath.Dir(build.stagedPath), installDir)
	}
	if !strings.HasPrefix(filepath.Base(build.stagedPath), ".cnc-proxy-build-") {
		t.Fatalf("staged build name = %q", filepath.Base(build.stagedPath))
	}
	if _, err := os.Stat(build.stagedPath); !os.IsNotExist(err) {
		t.Fatalf("staged path should be reserved but absent before go build, stat err=%v", err)
	}
	wantArgs := append([]string{"go"}, managedProxyBuildArgs(build.stagedPath)...)
	if got := strings.Join(build.cmd.Args, "\x00"); got != strings.Join(wantArgs, "\x00") {
		t.Fatalf("cmd.Args = %#v, want %#v", build.cmd.Args, wantArgs)
	}
	if build.promote == nil {
		t.Fatal("managed build should have a promote step")
	}
}

func TestPromoteStagedBinaryReplacesExistingOutput(t *testing.T) {
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "cnc-proxy.exe")
	stagedPath := filepath.Join(dir, ".cnc-proxy-build-test.exe")
	if err := os.WriteFile(outputPath, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagedPath, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := promoteStagedBinary(stagedPath, outputPath); err != nil {
		t.Fatalf("promoteStagedBinary: %v", err)
	}
	b, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new" {
		t.Fatalf("output content = %q, want new", string(b))
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("staged path should be moved away, stat err=%v", err)
	}
	matches, err := filepath.Glob(outputPath + ".previous-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("successful promotion should remove backup files, found %v", matches)
	}
}

func TestManagedProxyBuildArgsForWindowsUsesGUISubsystem(t *testing.T) {
	got := managedProxyBuildArgsForGOOS(`C:\CNC Proxy\.cnc-proxy-build.exe`, "windows")
	want := []string{
		"build",
		"-mod=mod",
		"-trimpath",
		"-ldflags=-s -w -H=windowsgui",
		"-o",
		`C:\CNC Proxy\.cnc-proxy-build.exe`,
		"./cmd/proxy",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("managedProxyBuildArgsForGOOS(windows) = %#v, want %#v", got, want)
	}
}

func TestManagedManagerBuildArgsForWindowsUsesTrayTagAndGUISubsystem(t *testing.T) {
	got := managedManagerBuildArgsForGOOS(`C:\CNC Proxy\.cnc-tray-build.exe`, "windows")
	want := []string{
		"build",
		"-mod=mod",
		"-trimpath",
		"-tags",
		"tray",
		"-ldflags=-s -w -H=windowsgui",
		"-o",
		`C:\CNC Proxy\.cnc-tray-build.exe`,
		"./cmd/tray",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("managedManagerBuildArgsForGOOS(windows) = %#v, want %#v", got, want)
	}
}

func TestStagedBuildOutputPathWithPrefixUsesRequestedPrefix(t *testing.T) {
	outputPath := filepath.Join(t.TempDir(), "cnc-tray.exe")
	staged, err := stagedBuildOutputPathWithPrefix(outputPath, ".cnc-tray-build-")
	if err != nil {
		t.Fatalf("stagedBuildOutputPathWithPrefix: %v", err)
	}
	if filepath.Dir(staged) != filepath.Dir(outputPath) {
		t.Fatalf("staged dir = %q, want %q", filepath.Dir(staged), filepath.Dir(outputPath))
	}
	if !strings.HasPrefix(filepath.Base(staged), ".cnc-tray-build-") {
		t.Fatalf("staged base = %q", filepath.Base(staged))
	}
}

func TestBuildCommandLeavesCustomCommandAsShellCommand(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BuildCommand = `go build -mod=mod -tags custom -o cnc-proxy.exe ./cmd/proxy`
	cmd, err := buildCommand(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if len(cmd.Args) == 0 {
		t.Fatal("buildCommand returned no command arguments")
	}
	if cmd.Args[len(cmd.Args)-1] != cfg.BuildCommand {
		t.Fatalf("custom build command was not passed through shell: %#v", cmd.Args)
	}
}
