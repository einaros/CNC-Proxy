// Package machinetransport opens the proxy's machine-side byte stream.
//
// The Carvera framed protocol is the same over WiFi/TCP and the firmware's
// USB serial console. This package keeps the transport choice below the client,
// relay, and arbiter layers so controller-facing behavior stays unchanged.
package machinetransport

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	KindTCP = "tcp"
	KindUSB = "usb"

	TCPPacketSize = 8192
	USBPacketSize = 128
)

// Conn is the minimal machine-side byte stream used by the protocol client and
// relay mux. A net.Conn satisfies it directly; the USB implementation adapts a
// serial.Port to the same shape.
type Conn interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	SetReadDeadline(t time.Time) error
	Close() error
}

// Config describes how to open a machine-side transport.
type Config struct {
	Kind string

	// TCPAddr resolves the machine host:port lazily. It is used only for TCP.
	TCPAddr func() (string, error)

	USBDevice      string
	USBBaud        int
	USBResetOnOpen bool

	DialTimeout time.Duration
}

// Opened is one live machine-side transport plus metadata callers need for
// logging and file-transfer packet sizing.
type Opened struct {
	Conn       Conn
	Label      string
	Kind       string
	PacketSize int
}

func NormalizeKind(kind string) string {
	if strings.TrimSpace(kind) == "" {
		return KindTCP
	}
	return strings.ToLower(strings.TrimSpace(kind))
}

func PacketSizeForKind(kind string) int {
	if NormalizeKind(kind) == KindUSB {
		return USBPacketSize
	}
	return TCPPacketSize
}

func ValidateKind(kind string) error {
	switch NormalizeKind(kind) {
	case KindTCP, KindUSB:
		return nil
	default:
		return fmt.Errorf("machine transport must be %q or %q", KindTCP, KindUSB)
	}
}

// Open connects to the configured machine transport.
func Open(cfg Config) (*Opened, error) {
	switch NormalizeKind(cfg.Kind) {
	case KindTCP:
		return openTCP(cfg)
	case KindUSB:
		return openUSB(cfg)
	default:
		return nil, fmt.Errorf("machine transport must be %q or %q", KindTCP, KindUSB)
	}
}

func openTCP(cfg Config) (*Opened, error) {
	if cfg.TCPAddr == nil {
		return nil, errors.New("machine tcp transport requires an address resolver")
	}
	addr, err := cfg.TCPAddr()
	if err != nil {
		return nil, err
	}
	timeout := cfg.DialTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	return &Opened{
		Conn:       c,
		Label:      addr,
		Kind:       KindTCP,
		PacketSize: TCPPacketSize,
	}, nil
}
