package machinetransport

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestNormalizeTCPAddress(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "empty discovery", in: "", want: ""},
		{name: "bare IPv4", in: "192.168.1.42", want: "192.168.1.42:2222"},
		{name: "bare hostname", in: "carvera.local", want: "carvera.local:2222"},
		{name: "explicit port", in: "192.168.1.42:12222", want: "192.168.1.42:12222"},
		{name: "bare IPv6", in: "2001:db8::42", want: "[2001:db8::42]:2222"},
		{name: "bracketed IPv6", in: "[2001:db8::42]", want: "[2001:db8::42]:2222"},
		{name: "IPv6 with port", in: "[2001:db8::42]:12222", want: "[2001:db8::42]:12222"},
		{name: "bad port", in: "192.168.1.42:notaport", wantErr: "port"},
		{name: "port too large", in: "192.168.1.42:70000", wantErr: "port"},
		{name: "URL is not an address", in: "http://192.168.1.42", wantErr: "host or host:port"},
		{name: "host with space", in: "shop carvera", wantErr: "invalid host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeTCPAddress(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("NormalizeTCPAddress(%q) error = %v, want containing %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeTCPAddress(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeTCPAddress(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPacketSizeForKind(t *testing.T) {
	if got := PacketSizeForKind(KindTCP); got != TCPPacketSize {
		t.Fatalf("tcp packet size = %d, want %d", got, TCPPacketSize)
	}
	if got := PacketSizeForKind(KindUSB); got != USBPacketSize {
		t.Fatalf("usb packet size = %d, want %d", got, USBPacketSize)
	}
	if got := PacketSizeForKind(""); got != TCPPacketSize {
		t.Fatalf("default packet size = %d, want %d", got, TCPPacketSize)
	}
}

func TestOpenTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
		close(accepted)
	}()

	opened, err := Open(Config{
		Kind:        KindTCP,
		TCPAddr:     func() (string, error) { return ln.Addr().String(), nil },
		DialTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Open TCP: %v", err)
	}
	defer opened.Conn.Close()
	if opened.Kind != KindTCP || opened.PacketSize != TCPPacketSize || opened.Label != ln.Addr().String() {
		t.Fatalf("opened = %+v", opened)
	}
	if c := <-accepted; c != nil {
		c.Close()
	} else {
		t.Fatal("listener did not accept TCP open")
	}
}

func TestOpenUSBRequiresDevice(t *testing.T) {
	if _, err := Open(Config{Kind: KindUSB}); err == nil {
		t.Fatal("USB open without device should fail")
	}
}
