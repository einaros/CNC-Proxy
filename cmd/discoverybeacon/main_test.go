package main

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestParseConfigUsesSharedDockerEnv(t *testing.T) {
	cfg, err := parseConfig([]string{
		"-proxy-ip", "127.0.0.1",
		"-broadcast", "127.255.255.255",
	}, fakeEnv(map[string]string{
		"CNC_MACHINE":  "192.168.1.42:2222",
		"CNC_NAME":     "Shop CNC",
		"CNC_TCP_PORT": "12222",
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Name != "Shop CNC" || cfg.ProxyPort != 12222 || cfg.Machine != "192.168.1.42:2222" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.Bridge != "127.0.0.1:12222" {
		t.Fatalf("bridge = %q", cfg.Bridge)
	}
}

func TestParseConfigBeaconEnvWinsOverSharedEnv(t *testing.T) {
	cfg, err := parseConfig([]string{
		"-proxy-ip", "127.0.0.1",
		"-broadcast", "127.255.255.255",
	}, fakeEnv(map[string]string{
		"CNC_NAME":              "Container Name",
		"CNC_BEACON_NAME":       "LAN Name",
		"CNC_TCP_PORT":          "2222",
		"CNC_BEACON_PROXY_PORT": "12222",
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Name != "LAN Name" || cfg.ProxyPort != 12222 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestParseConfigAllowsDiscoveryBridgeWithoutMachine(t *testing.T) {
	cfg, err := parseConfig([]string{
		"-proxy-ip", "127.0.0.1",
		"-broadcast", "127.255.255.255",
	}, fakeEnv(map[string]string{
		"CNC_NAME": "Shop CNC",
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Machine != "" || cfg.Bridge == "" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestParseConfigFlagsOverrideEnv(t *testing.T) {
	cfg, err := parseConfig([]string{
		"-name", "Flag Name",
		"-proxy-ip", "127.0.0.1",
		"-proxy-port", "3333",
		"-broadcast", "127.255.255.255",
		"-bridge", "",
		"-interval", "2s",
	}, fakeEnv(map[string]string{
		"CNC_BEACON_NAME":       "Env Name",
		"CNC_BEACON_PROXY_PORT": "2222",
		"CNC_BEACON_BRIDGE":     "127.0.0.1:12222",
		"CNC_BEACON_INTERVAL":   "1s",
	}))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.Name != "Flag Name" || cfg.ProxyPort != 3333 || cfg.Bridge != "" || cfg.Interval != 2*time.Second {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestFinalizeConfigRejectsCommaName(t *testing.T) {
	_, err := finalizeConfig(config{
		Name:      "Bad,Name",
		ProxyIP:   "127.0.0.1",
		ProxyPort: 2222,
		Broadcast: "127.255.255.255",
		Interval:  time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "must not contain") {
		t.Fatalf("err = %v", err)
	}
}

func TestFinalizeConfigRejectsBadBridge(t *testing.T) {
	_, err := finalizeConfig(config{
		Name:      "Shop CNC",
		ProxyIP:   "127.0.0.1",
		ProxyPort: 2222,
		Broadcast: "127.255.255.255",
		Bridge:    "12222",
		Interval:  time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "-bridge must be host:port") {
		t.Fatalf("err = %v", err)
	}
}

func TestFinalizeConfigDerivesLoopbackFromMachine(t *testing.T) {
	cfg, err := finalizeConfig(config{
		Name:      "Shop CNC",
		ProxyPort: 2222,
		Machine:   "127.0.0.1:12222",
		Interval:  time.Second,
	})
	if err != nil {
		t.Fatalf("finalizeConfig: %v", err)
	}
	if cfg.ProxyIP != "127.0.0.1" || cfg.Broadcast == "" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestFinalizeConfigAddsDefaultMachinePort(t *testing.T) {
	cfg, err := finalizeConfig(config{
		Name:      "Shop CNC",
		ProxyIP:   "127.0.0.1",
		ProxyPort: 2222,
		Broadcast: "127.255.255.255",
		Machine:   "192.168.1.42",
		Interval:  time.Second,
	})
	if err != nil {
		t.Fatalf("finalizeConfig: %v", err)
	}
	if cfg.Machine != "192.168.1.42:2222" {
		t.Fatalf("machine = %q", cfg.Machine)
	}
}

func TestHostOnly(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.42:2222": "192.168.1.42",
		"192.168.1.42":      "192.168.1.42",
		"[::1]:2222":        "::1",
	} {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMachineDialerFixed(t *testing.T) {
	addr, err := machineDialer("192.168.1.42:2222", nil)()
	if err != nil {
		t.Fatalf("machineDialer: %v", err)
	}
	if addr != "192.168.1.42:2222" {
		t.Fatalf("addr = %q", addr)
	}
}

func TestMachineDialerNeedsDiscovery(t *testing.T) {
	_, err := machineDialer("", nil)()
	if err == nil || !strings.Contains(err.Error(), "no machine discovered") {
		t.Fatalf("err = %v", err)
	}
}

func TestBridgeConnForwardsBytes(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	done := make(chan error, 1)
	go func() {
		c, err := target.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		buf := make([]byte, 4)
		if _, err := io.ReadFull(c, buf); err != nil {
			done <- err
			return
		}
		if string(buf) != "ping" {
			done <- errString("unexpected payload: " + string(buf))
			return
		}
		_, err = c.Write([]byte("pong"))
		done <- err
	}()

	client, bridgeSide := net.Pipe()
	defer client.Close()
	go bridgeConn(bridgeSide, func() (string, error) { return target.Addr().String(), nil })

	if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "pong" {
		t.Fatalf("reply = %q", buf)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func fakeEnv(values map[string]string) envLookup {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}
