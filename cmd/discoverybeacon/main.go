// Command discoverybeacon broadcasts a Carvera discovery record for a proxy
// that is reachable on this host.
//
// It is meant for Docker Desktop-style deployments where the proxy itself runs
// in a container, but LAN broadcasts cannot reach into or out of that container.
// The beacon runs natively on the host, learns the real machine from UDP
// discovery when -machine is omitted, broadcasts discovery packets pointing
// controllers at the Docker-published TCP relay port, and exposes a local TCP
// bridge that the Docker proxy can dial as CNC_MACHINE.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/uwin/cnc-proxy/internal/discovery"
)

type envLookup func(string) (string, bool)

type config struct {
	Name      string
	ProxyIP   string
	ProxyPort int
	Broadcast string
	Machine   string
	Bridge    string
	Interval  time.Duration
}

func main() {
	if err := run(os.Args[1:], os.LookupEnv); err != nil {
		log.Fatal(err)
	}
}

func run(args []string, getenv envLookup) error {
	cfg, err := parseConfig(args, getenv)
	if err != nil {
		return err
	}

	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		close(stop)
	}()

	disc := &discovery.Listener{}
	if cfg.Machine == "" {
		udp, err := discovery.OpenListenSocket()
		if err != nil {
			return fmt.Errorf("cannot bind UDP discovery port %d: %w", discovery.Port, err)
		}
		defer udp.Close()
		go disc.Listen(udp)
		log.Printf("discoverybeacon: listening for real machine broadcasts on udp/%d", discovery.Port)
	}

	if cfg.Bridge != "" {
		ln, err := net.Listen("tcp", cfg.Bridge)
		if err != nil {
			return fmt.Errorf("cannot listen on bridge address %s: %w", cfg.Bridge, err)
		}
		defer ln.Close()
		go closeOnStop(ln, stop)
		go runBridge(ln, machineDialer(cfg.Machine, disc), stop)
		log.Printf("discoverybeacon: machine bridge listening on %s", cfg.Bridge)
	}

	log.Printf("discoverybeacon: advertising %q at %s:%d to %s:%d every %s",
		cfg.Name, cfg.ProxyIP, cfg.ProxyPort, cfg.Broadcast, discovery.Port, cfg.Interval)

	adv := &discovery.Advertiser{
		Listener:       disc,
		ProxyIP:        cfg.ProxyIP,
		ProxyPort:      cfg.ProxyPort,
		Name:           cfg.Name,
		Interval:       cfg.Interval,
		RequireMachine: cfg.Machine == "",
	}
	return adv.Run(cfg.Broadcast, stop)
}

func parseConfig(args []string, getenv envLookup) (config, error) {
	portDefault, err := envInt(getenv, 2222, "CNC_BEACON_PROXY_PORT", "CNC_TCP_PORT")
	if err != nil {
		return config{}, err
	}
	intervalDefault, err := envDuration(getenv, time.Second, "CNC_BEACON_INTERVAL")
	if err != nil {
		return config{}, err
	}

	cfg := config{
		Name:      envString(getenv, "Carvera (proxy)", "CNC_BEACON_NAME", "CNC_NAME"),
		ProxyIP:   envString(getenv, "", "CNC_BEACON_PROXY_IP"),
		ProxyPort: portDefault,
		Broadcast: envString(getenv, "", "CNC_BEACON_BROADCAST"),
		Machine:   envString(getenv, "", "CNC_BEACON_MACHINE", "CNC_MACHINE"),
		Bridge:    envString(getenv, "127.0.0.1:12222", "CNC_BEACON_BRIDGE"),
		Interval:  intervalDefault,
	}

	fs := flag.NewFlagSet("discoverybeacon", flag.ContinueOnError)
	fs.StringVar(&cfg.Name, "name", cfg.Name, "advertised machine name")
	fs.StringVar(&cfg.ProxyIP, "proxy-ip", cfg.ProxyIP, "host LAN IP controllers should connect to; empty = auto")
	fs.IntVar(&cfg.ProxyPort, "proxy-port", cfg.ProxyPort, "proxy TCP port controllers should connect to")
	fs.IntVar(&cfg.ProxyPort, "tcp-port", cfg.ProxyPort, "alias for -proxy-port")
	fs.StringVar(&cfg.Broadcast, "broadcast", cfg.Broadcast, "UDP discovery destination; empty = auto-directed broadcast")
	fs.StringVar(&cfg.Machine, "machine", cfg.Machine, "fixed machine host[:port]; empty = learn from UDP discovery")
	fs.StringVar(&cfg.Bridge, "bridge", cfg.Bridge, "TCP address the Docker proxy should use as CNC_MACHINE; empty = disabled")
	fs.DurationVar(&cfg.Interval, "interval", cfg.Interval, "discovery announcement interval")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	return finalizeConfig(cfg)
}

func finalizeConfig(cfg config) (config, error) {
	cfg.Name = strings.TrimSpace(cfg.Name)
	cfg.ProxyIP = strings.TrimSpace(cfg.ProxyIP)
	cfg.Broadcast = strings.TrimSpace(cfg.Broadcast)
	cfg.Machine = strings.TrimSpace(cfg.Machine)
	cfg.Bridge = strings.TrimSpace(cfg.Bridge)

	if cfg.Name == "" {
		return config{}, fmt.Errorf("-name must not be empty")
	}
	if strings.Contains(cfg.Name, ",") {
		return config{}, fmt.Errorf("-name must not contain ',' (the discovery wire format is comma-separated)")
	}
	if cfg.ProxyPort <= 0 || cfg.ProxyPort > 65535 {
		return config{}, fmt.Errorf("-proxy-port must be between 1 and 65535")
	}
	if cfg.Interval <= 0 {
		return config{}, fmt.Errorf("-interval must be positive")
	}
	if cfg.Machine != "" {
		cfg.Machine = normalizeMachineAddr(cfg.Machine)
	}
	if cfg.Bridge != "" {
		if _, _, err := net.SplitHostPort(cfg.Bridge); err != nil {
			return config{}, fmt.Errorf("-bridge must be host:port, got %q: %w", cfg.Bridge, err)
		}
	}

	if cfg.ProxyIP == "" {
		ip, err := autoProxyIP(cfg.Machine)
		if err != nil {
			return config{}, err
		}
		cfg.ProxyIP = ip.String()
	}
	ip := net.ParseIP(cfg.ProxyIP)
	if ip == nil || ip.To4() == nil {
		return config{}, fmt.Errorf("-proxy-ip must be an IPv4 address, got %q", cfg.ProxyIP)
	}

	if cfg.Broadcast == "" {
		b, err := discovery.BroadcastFor(ip)
		if err != nil {
			return config{}, fmt.Errorf("cannot auto-derive -broadcast from -proxy-ip %s: %w; pass -broadcast explicitly", cfg.ProxyIP, err)
		}
		cfg.Broadcast = b.String()
	}
	return cfg, nil
}

func autoProxyIP(machine string) (net.IP, error) {
	if machine != "" {
		ip, err := discovery.LocalIPToward(hostOnly(machine))
		if err != nil {
			return nil, fmt.Errorf("cannot auto-derive -proxy-ip from -machine %q: %w; pass -proxy-ip explicitly", machine, err)
		}
		return ip, nil
	}
	ip, err := firstNonLoopbackIPv4()
	if err != nil {
		return nil, fmt.Errorf("cannot auto-derive -proxy-ip: %w; pass -proxy-ip explicitly", err)
	}
	return ip, nil
}

func normalizeMachineAddr(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, "2222")
}

func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		return strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	}
	return addr
}

func firstNonLoopbackIPv4() (net.IP, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip != nil && !ip.IsLoopback() {
				return ip, nil
			}
		}
	}
	return nil, fmt.Errorf("no non-loopback IPv4 interface found")
}

func machineDialer(fixed string, l *discovery.Listener) func() (string, error) {
	return func() (string, error) {
		if fixed != "" {
			return fixed, nil
		}
		if l == nil {
			return "", fmt.Errorf("no machine discovered yet")
		}
		m, ok := l.Latest()
		if !ok {
			return "", fmt.Errorf("no machine discovered yet")
		}
		return net.JoinHostPort(m.IP, strconv.Itoa(m.Port)), nil
	}
}

func runBridge(ln net.Listener, dialAddr func() (string, error), stop <-chan struct{}) {
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-stop:
			default:
				log.Printf("discoverybeacon: bridge accept failed: %v", err)
			}
			return
		}
		go bridgeConn(c, dialAddr)
	}
}

func closeOnStop(ln net.Listener, stop <-chan struct{}) {
	<-stop
	_ = ln.Close()
}

func bridgeConn(src net.Conn, dialAddr func() (string, error)) {
	defer src.Close()
	addr, err := dialAddr()
	if err != nil {
		log.Printf("discoverybeacon: bridge rejected %s: %v", src.RemoteAddr(), err)
		return
	}
	dst, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		log.Printf("discoverybeacon: bridge dial %s failed: %v", addr, err)
		return
	}
	defer dst.Close()

	done := make(chan struct{}, 2)
	go copyAndClose(dst, src, done)
	go copyAndClose(src, dst, done)
	<-done
}

func copyAndClose(dst, src net.Conn, done chan<- struct{}) {
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
	done <- struct{}{}
}

func envString(getenv envLookup, fallback string, keys ...string) string {
	for _, key := range keys {
		if v, ok := getenv(key); ok {
			return v
		}
	}
	return fallback
}

func envInt(getenv envLookup, fallback int, keys ...string) (int, error) {
	for _, key := range keys {
		if v, ok := getenv(key); ok {
			i, err := strconv.Atoi(v)
			if err != nil {
				return 0, fmt.Errorf("invalid %s=%q: %w", key, v, err)
			}
			return i, nil
		}
	}
	return fallback, nil
}

func envDuration(getenv envLookup, fallback time.Duration, keys ...string) (time.Duration, error) {
	for _, key := range keys {
		if v, ok := getenv(key); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				return 0, fmt.Errorf("invalid %s=%q: %w", key, v, err)
			}
			return d, nil
		}
	}
	return fallback, nil
}
