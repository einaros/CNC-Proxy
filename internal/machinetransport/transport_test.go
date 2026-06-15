package machinetransport

import (
	"net"
	"testing"
	"time"
)

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
