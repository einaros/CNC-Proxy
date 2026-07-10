package traymgr

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/uwin/cnc-proxy/internal/jog"
)

func TestSaveConfigRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "tray.json")
	cfg := DefaultConfig()
	cfg.AdminToken = "secret"
	cfg.SourceDir = "/tmp/cnc-src"
	cfg.AutoStart = true
	cfg.WebDAVMount.Enabled = true
	cfg.Flags["name"] = "Shop CNC"
	cfg.Flags["machine-transport"] = "usb"
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if want := normalizeConfig(cfg); !reflect.DeepEqual(loaded, want) {
		t.Fatalf("round-tripped config = %+v, want %+v", loaded, want)
	}
}

func TestDefaultConfigIncludesEveryProxyOption(t *testing.T) {
	cfg := DefaultConfig()
	for _, opt := range ProxyOptions {
		if _, ok := cfg.Flags[opt.Name]; !ok {
			t.Fatalf("default config missing %s", opt.Name)
		}
	}
}

func TestJogDefaultsMatchProxy(t *testing.T) {
	cfg := DefaultConfig()
	jogDefaults := jog.DefaultConfig()
	if got := cfg.Flags["jog-tick"]; got != jogDefaults.Tick.String() {
		t.Fatalf("jog tick default = %q, want %q", got, jogDefaults.Tick)
	}
	if got := cfg.Flags["jog-status-interval"]; got != jogDefaults.StatusInterval.String() {
		t.Fatalf("jog status default = %q, want %q", got, jogDefaults.StatusInterval)
	}
	if got := cfg.Flags["jog-deadman-timeout"]; got != jogDefaults.DeadmanTimeout.String() {
		t.Fatalf("jog deadman default = %q, want %q", got, jogDefaults.DeadmanTimeout)
	}
}

func TestProxyArgsIncludesConfiguredFlags(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Flags["machine-transport"] = "usb"
	cfg.Flags["usb-device"] = "COM3"
	cfg.Flags["advertise"] = "true"
	cfg.Flags["name"] = "Shop CNC"
	args := strings.Join(ProxyArgs(cfg), "\x00")
	for _, want := range []string{
		"-machine-transport\x00usb",
		"-usb-device\x00COM3",
		"-advertise=true",
		"-name\x00Shop CNC",
	} {
		if !strings.Contains(args, want) {
			t.Fatalf("args missing %q in %q", want, args)
		}
	}
}

func TestProxyArgsUsesDynamicOptions(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Flags["future-bool"] = "true"
	cfg.Flags["future-name"] = "enabled"
	options := []ProxyOption{
		{Name: "future-bool", Label: "Future Bool", Type: OptionBool, Default: "false"},
		{Name: "future-name", Label: "Future Name", Type: OptionString, Default: ""},
	}
	args := strings.Join(ProxyArgsForOptions(cfg, options), "\x00")
	for _, want := range []string{"-future-bool=true", "-future-name\x00enabled"} {
		if !strings.Contains(args, want) {
			t.Fatalf("dynamic args missing %q in %q", want, args)
		}
	}
}

func TestProxyOptionsForConfigUsesSourceSchema(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cmd", "proxy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/proxyschema\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainSrc := `package main

import (
	"encoding/json"
	"flag"
	"os"
)

func main() {
	printSchema := flag.Bool("print-config-schema", false, "")
	flag.Parse()
	if *printSchema {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"options": []map[string]any{{
			"name": "future-field",
			"label": "Future Field",
			"type": "string",
			"default": "next",
		}}})
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "cmd", "proxy", "main.go"), []byte(mainSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.SourceDir = dir
	opts, source := ProxyOptionsForConfig(context.Background(), cfg)
	if source != "source" {
		t.Fatalf("option source = %q, want source", source)
	}
	if len(opts) != 1 || opts[0].Name != "future-field" {
		t.Fatalf("options = %+v, want future-field from source", opts)
	}
}

func TestProxyOptionsExposeDiscreteChoices(t *testing.T) {
	for _, tc := range []struct {
		name    string
		choices []string
	}{
		{name: "machine-transport", choices: []string{"tcp", "usb"}},
		{name: "jog-motion", choices: []string{"instant", "g53"}},
	} {
		opt, ok := proxyOption(tc.name)
		if !ok {
			t.Fatalf("option %q not found", tc.name)
		}
		if !slices.Equal(opt.Choices, tc.choices) {
			t.Fatalf("%s choices = %v, want %v", tc.name, opt.Choices, tc.choices)
		}
	}
}

func TestValidateConfigRejectsUnknownChoice(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Flags["machine-transport"] = "bluetooth"
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "machine-transport") {
		t.Fatalf("ValidateConfig error = %v, want machine-transport choice error", err)
	}
}

func TestValidateConfigRequiresTokenForRemoteManagerBind(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AdminListen = "0.0.0.0:8430"
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "manager token") {
		t.Fatalf("remote manager bind error = %v, want manager token error", err)
	}
	cfg.AdminToken = "secret"
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("remote manager bind with token: %v", err)
	}
}

func TestValidateConfigRejectsInvalidManagerListen(t *testing.T) {
	cfg := DefaultConfig()
	for _, addr := range []string{"127.0.0.1", "localhost:notaport", "example.com:8430"} {
		cfg.AdminListen = addr
		if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "manager listen") {
			t.Fatalf("ValidateConfig(%q) error = %v, want manager listen error", addr, err)
		}
	}
}

func proxyOption(name string) (ProxyOption, bool) {
	for _, opt := range ProxyOptions {
		if opt.Name == name {
			return opt, true
		}
	}
	return ProxyOption{}, false
}

func TestAPIBaseNormalizesWildcardBind(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Flags["api-addr"] = "0.0.0.0:8420"
	if got := APIBase(cfg); got != "http://127.0.0.1:8420" {
		t.Fatalf("APIBase = %q", got)
	}
}

func TestWebDAVBaseNormalizesWildcardBind(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Flags["dav-addr"] = "0.0.0.0:8421"
	if got := WebDAVBase(cfg); got != "http://127.0.0.1:8421/" {
		t.Fatalf("WebDAVBase = %q", got)
	}
}

func TestDefaultConfigIncludesWebDAVMountDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.WebDAVMount.Enabled {
		t.Fatal("default WebDAV mount should be disabled")
	}
	if cfg.WebDAVMount.MountPoint == "" && cfg.WebDAVMount.Drive == "" {
		t.Fatal("default WebDAV mount should have a mount point or drive")
	}
}

func TestManagerBaseNormalizesWildcardBind(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AdminListen = "0.0.0.0:8430"
	cfg.AdminToken = "secret"
	if got := ManagerBase(cfg); got != "http://127.0.0.1:8430" {
		t.Fatalf("ManagerBase = %q", got)
	}
}

func TestManagerURLsIncludeLoopbackForWildcardBind(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AdminListen = "0.0.0.0:8430"
	cfg.AdminToken = "secret"
	urls := ManagerURLs(cfg)
	if len(urls) == 0 || urls[0] != "http://127.0.0.1:8430" {
		t.Fatalf("ManagerURLs = %v, want first loopback URL", urls)
	}
}
