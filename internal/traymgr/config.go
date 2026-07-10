// Package traymgr implements the local tray-side manager for cnc-proxy.
//
// The manager owns process supervision and remote deployment because those are
// host-level capabilities. The proxy itself remains focused on machine I/O.
package traymgr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/uwin/cnc-proxy/internal/proxyconfig"
)

type OptionType = proxyconfig.OptionType

const (
	OptionString   = proxyconfig.OptionString
	OptionBool     = proxyconfig.OptionBool
	OptionInt      = proxyconfig.OptionInt
	OptionInt64    = proxyconfig.OptionInt64
	OptionFloat    = proxyconfig.OptionFloat
	OptionDuration = proxyconfig.OptionDuration
)

type ProxyOption = proxyconfig.Option

var ProxyOptions = proxyconfig.Options()

type Config struct {
	ProxyBinary  string            `json:"proxy_binary"`
	SourceDir    string            `json:"source_dir"`
	BuildCommand string            `json:"build_command"`
	AutoStart    bool              `json:"auto_start"`
	AdminListen  string            `json:"admin_listen"`
	AdminToken   string            `json:"admin_token"`
	WebDAVMount  WebDAVMountConfig `json:"webdav_mount"`
	Flags        map[string]string `json:"flags"`
}

type WebDAVMountConfig struct {
	Enabled    bool   `json:"enabled"`
	MountPoint string `json:"mount_point,omitempty"`
	Drive      string `json:"drive,omitempty"`
}

func DefaultConfig() Config {
	cfg := Config{
		ProxyBinary:  defaultProxyBinary(),
		SourceDir:    "",
		BuildCommand: defaultBuildCommand(),
		AutoStart:    false,
		AdminListen:  "127.0.0.1:8430",
		AdminToken:   "",
		WebDAVMount:  defaultWebDAVMountConfig(),
		Flags:        map[string]string{},
	}
	for _, opt := range ProxyOptions {
		cfg.Flags[opt.Name] = opt.Default
	}
	return cfg
}

func DefaultConfigPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "cnc-proxy", "tray-config.json")
	}
	return "tray-config.json"
}

func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return Config{}, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, err
	}
	cfg = normalizeConfig(cfg)
	return cfg, nil
}

func SaveConfig(path string, cfg Config) error {
	return saveConfig(path, cfg, ValidateConfig)
}

func SaveManagerConfig(path string, cfg Config) error {
	return saveConfig(path, cfg, ValidateManagerConfig)
}

func SaveProxyConfig(path string, cfg Config) error {
	return saveConfig(path, cfg, ValidateProxyConfig)
}

func SaveProxyConfigWithOptions(path string, cfg Config, options []ProxyOption) error {
	return saveConfig(path, cfg, func(c Config) error { return ValidateProxyConfigWithOptions(c, options) })
}

func saveConfig(path string, cfg Config, validate func(Config) error) error {
	cfg = normalizeConfig(cfg)
	if err := validate(cfg); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tray-config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	// fsync through rename (same discipline as store.flushLocked) so a crash
	// or power loss cannot leave an empty or missing tray config.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	// fsync the directory so the rename itself is durable.
	if dir, derr := os.Open(filepath.Dir(path)); derr == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}

func ValidateConfig(cfg Config) error {
	if err := ValidateManagerConfig(cfg); err != nil {
		return err
	}
	return ValidateProxyConfig(cfg)
}

func ValidateConfigWithOptions(cfg Config, options []ProxyOption) error {
	if err := ValidateManagerConfig(cfg); err != nil {
		return err
	}
	return ValidateProxyConfigWithOptions(cfg, options)
}

func ValidateProxyConfig(cfg Config) error {
	return ValidateProxyConfigWithOptions(cfg, ProxyOptions)
}

func ValidateProxyConfigWithOptions(cfg Config, options []ProxyOption) error {
	cfg = normalizeConfigWithOptions(cfg, options)
	for _, opt := range options {
		v := cfg.Flags[opt.Name]
		if err := validateOption(opt, v); err != nil {
			return err
		}
	}
	return nil
}

func ValidateManagerConfig(cfg Config) error {
	if strings.TrimSpace(cfg.ProxyBinary) == "" {
		return errors.New("proxy binary must not be empty")
	}
	if strings.TrimSpace(cfg.AdminListen) == "" {
		return errors.New("manager listen address must not be empty")
	}
	if err := validateListenAddr(cfg.AdminListen); err != nil {
		return err
	}
	if !isLoopbackBind(cfg.AdminListen) && strings.TrimSpace(cfg.AdminToken) == "" {
		return errors.New("manager listener beyond loopback requires manager token")
	}
	if err := validateWebDAVMountConfig(cfg.WebDAVMount); err != nil {
		return err
	}
	return nil
}

func ProxyArgs(cfg Config) []string {
	return ProxyArgsForOptions(cfg, ProxyOptions)
}

func ProxyArgsForOptions(cfg Config, options []ProxyOption) []string {
	cfg = normalizeConfigWithOptions(cfg, options)
	var args []string
	for _, opt := range options {
		v := strings.TrimSpace(cfg.Flags[opt.Name])
		if opt.Type == OptionBool {
			if v == "" {
				v = opt.Default
			}
			args = append(args, "-"+opt.Name+"="+v)
			continue
		}
		if v == "" && opt.Default == "" {
			continue
		}
		if v == "" {
			v = opt.Default
		}
		args = append(args, "-"+opt.Name, v)
	}
	return args
}

func APIBase(cfg Config) string {
	cfg = normalizeConfig(cfg)
	addr := cfg.Flags["api-addr"]
	if strings.TrimSpace(addr) == "" {
		addr = "127.0.0.1:8420"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func WebDAVBase(cfg Config) string {
	cfg = normalizeConfig(cfg)
	addr := cfg.Flags["dav-addr"]
	if strings.TrimSpace(addr) == "" {
		addr = "127.0.0.1:8421"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr + "/"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

func ManagerBase(cfg Config) string {
	cfg = normalizeConfig(cfg)
	addr := cfg.AdminListen
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func ManagerURLs(cfg Config) []string {
	cfg = normalizeConfig(cfg)
	host, port, err := net.SplitHostPort(cfg.AdminListen)
	if err != nil {
		return []string{ManagerBase(cfg)}
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return []string{ManagerBase(cfg)}
	}
	seen := map[string]bool{}
	var urls []string
	add := func(host string) {
		url := "http://" + net.JoinHostPort(host, port)
		if !seen[url] {
			seen[url] = true
			urls = append(urls, url)
		}
	}
	add("127.0.0.1")
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, addr := range addrs {
			ipn, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP.To4()
			if ip == nil || ip.IsLoopback() {
				continue
			}
			add(ip.String())
		}
	}
	return urls
}

func Auth(cfg Config) (user, token string) {
	cfg = normalizeConfig(cfg)
	user = cfg.Flags["auth-user"]
	if user == "" {
		user = "cnc"
	}
	return user, cfg.Flags["auth-token"]
}

func normalizeConfig(cfg Config) Config {
	return normalizeConfigWithOptions(cfg, ProxyOptions)
}

func normalizeConfigWithOptions(cfg Config, options []ProxyOption) Config {
	def := DefaultConfig()
	if cfg.ProxyBinary == "" {
		cfg.ProxyBinary = def.ProxyBinary
	}
	if cfg.BuildCommand == "" {
		cfg.BuildCommand = def.BuildCommand
	}
	if cfg.AdminListen == "" {
		cfg.AdminListen = def.AdminListen
	}
	cfg.WebDAVMount = normalizeWebDAVMountConfig(cfg.WebDAVMount)
	if cfg.Flags == nil {
		cfg.Flags = map[string]string{}
	}
	for _, opt := range options {
		if _, ok := cfg.Flags[opt.Name]; !ok {
			cfg.Flags[opt.Name] = opt.Default
			continue
		}
		cfg.Flags[opt.Name] = normalizeOptionValue(opt, cfg.Flags[opt.Name])
	}
	return cfg
}

func BuiltinProxyOptions() []ProxyOption {
	return cloneProxyOptions(ProxyOptions)
}

func ProxyOptionsForConfig(ctx context.Context, cfg Config) ([]ProxyOption, string) {
	if opts, err := proxyOptionsFromSource(ctx, cfg); err == nil {
		return opts, "source"
	}
	if opts, err := proxyOptionsFromBinary(ctx, cfg); err == nil {
		return opts, "binary"
	}
	return BuiltinProxyOptions(), "manager fallback"
}

func proxyOptionsFromSource(ctx context.Context, cfg Config) ([]ProxyOption, error) {
	dir := stringsTrim(cfg.SourceDir)
	if dir == "" {
		return nil, errors.New("source_dir is empty")
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, "cmd", "proxy")); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", "-mod=mod", "./cmd/proxy", "-print-config-schema")
	cmd.Dir = dir
	configureBackgroundCommand(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseProxyOptions(out)
}

func proxyOptionsFromBinary(ctx context.Context, cfg Config) ([]ProxyOption, error) {
	bin := stringsTrim(cfg.ProxyBinary)
	if bin == "" {
		return nil, errors.New("proxy binary is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-print-config-schema")
	if dir, err := proxyWorkingDir(cfg); err == nil && dir != "" {
		cmd.Dir = dir
	}
	configureBackgroundCommand(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return parseProxyOptions(out)
}

func parseProxyOptions(b []byte) ([]ProxyOption, error) {
	var schema proxyconfig.Schema
	if err := json.Unmarshal(b, &schema); err != nil {
		var direct []ProxyOption
		if err2 := json.Unmarshal(b, &direct); err2 != nil {
			return nil, err
		}
		schema.Options = direct
	}
	opts := cloneProxyOptions(schema.Options)
	if len(opts) == 0 {
		return nil, errors.New("proxy schema contains no options")
	}
	seen := map[string]bool{}
	for i := range opts {
		opts[i].Name = strings.TrimSpace(opts[i].Name)
		opts[i].Label = strings.TrimSpace(opts[i].Label)
		if opts[i].Name == "" {
			return nil, errors.New("proxy schema contains an option without a name")
		}
		if seen[opts[i].Name] {
			return nil, fmt.Errorf("proxy schema contains duplicate option %q", opts[i].Name)
		}
		seen[opts[i].Name] = true
		if opts[i].Label == "" {
			opts[i].Label = opts[i].Name
		}
		switch opts[i].Type {
		case OptionString, OptionBool, OptionInt, OptionInt64, OptionFloat, OptionDuration:
		case "":
			opts[i].Type = OptionString
		default:
			return nil, fmt.Errorf("proxy schema option %s has unknown type %q", opts[i].Name, opts[i].Type)
		}
	}
	return opts, nil
}

func cloneProxyOptions(options []ProxyOption) []ProxyOption {
	out := append([]ProxyOption(nil), options...)
	for i := range out {
		out[i].Choices = append([]string(nil), out[i].Choices...)
	}
	return out
}

func normalizeOptionValue(opt ProxyOption, value string) string {
	if len(opt.Choices) == 0 {
		return value
	}
	trimmed := strings.TrimSpace(value)
	for _, choice := range opt.Choices {
		if strings.EqualFold(trimmed, choice) {
			return choice
		}
	}
	return value
}

func validateOption(opt ProxyOption, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		value = opt.Default
	}
	if len(opt.Choices) > 0 {
		for _, choice := range opt.Choices {
			if value == choice {
				return nil
			}
		}
		return fmt.Errorf("%s must be one of: %s", opt.Name, strings.Join(opt.Choices, ", "))
	}
	switch opt.Type {
	case OptionBool:
		if _, err := strconv.ParseBool(value); err != nil {
			return fmt.Errorf("%s must be true or false", opt.Name)
		}
	case OptionInt:
		if _, err := strconv.Atoi(value); err != nil {
			return fmt.Errorf("%s must be an integer", opt.Name)
		}
	case OptionInt64:
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			return fmt.Errorf("%s must be an integer", opt.Name)
		}
	case OptionFloat:
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return fmt.Errorf("%s must be a number", opt.Name)
		}
	case OptionString, OptionDuration:
		if opt.Type == OptionDuration {
			if _, err := time.ParseDuration(value); err != nil {
				return fmt.Errorf("%s must be a duration", opt.Name)
			}
		}
	default:
		return fmt.Errorf("unknown option type %q for %s", opt.Type, opt.Name)
	}
	return nil
}

func isLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateListenAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("manager listen address must be host:port: %w", err)
	}
	if strings.TrimSpace(host) != "" && host != "localhost" && net.ParseIP(host) == nil {
		return fmt.Errorf("manager listen host must be an IP address or localhost: %q", host)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("manager listen port must be between 1 and 65535: %q", port)
	}
	return nil
}

func defaultWebDAVMountConfig() WebDAVMountConfig {
	return WebDAVMountConfig{
		Enabled:    false,
		MountPoint: defaultWebDAVMountPoint(),
		Drive:      defaultWebDAVDrive(),
	}
}

func normalizeWebDAVMountConfig(cfg WebDAVMountConfig) WebDAVMountConfig {
	if strings.TrimSpace(cfg.MountPoint) == "" {
		cfg.MountPoint = defaultWebDAVMountPoint()
	}
	if strings.TrimSpace(cfg.Drive) == "" {
		cfg.Drive = defaultWebDAVDrive()
	}
	cfg.MountPoint = strings.TrimSpace(cfg.MountPoint)
	cfg.Drive = strings.ToUpper(strings.TrimSpace(cfg.Drive))
	return cfg
}

func validateWebDAVMountConfig(cfg WebDAVMountConfig) error {
	cfg = normalizeWebDAVMountConfig(cfg)
	if strings.ContainsRune(cfg.MountPoint, 0) {
		return errors.New("webdav mount point contains an invalid character")
	}
	if cfg.Drive != "" {
		if cfg.Drive != "*" && (len(cfg.Drive) != 2 || cfg.Drive[1] != ':' || cfg.Drive[0] < 'A' || cfg.Drive[0] > 'Z') {
			return errors.New("webdav drive must be * or a Windows drive letter like Z:")
		}
	}
	return nil
}

func defaultProxyBinary() string {
	if runtime.GOOS == "windows" {
		return ".\\cnc-proxy.exe"
	}
	return "./cnc-proxy"
}

func defaultBuildCommand() string {
	out := "cnc-proxy"
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	return "go build -mod=mod -o " + out + " ./cmd/proxy"
}

func optionNames() []string {
	names := make([]string, 0, len(ProxyOptions))
	for _, opt := range ProxyOptions {
		names = append(names, opt.Name)
	}
	sort.Strings(names)
	return names
}
