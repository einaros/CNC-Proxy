package machinetransport

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"go.bug.st/serial"
)

const defaultUSBBaud = 115200

var serialResetSleep = time.Sleep

type serialPort interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	SetReadTimeout(t time.Duration) error
	ResetInputBuffer() error
	SetDTR(dtr bool) error
	Close() error
}

func openUSB(cfg Config) (*Opened, error) {
	if cfg.USBDevice == "" {
		return nil, errors.New("usb machine transport requires -usb-device")
	}
	baud := cfg.USBBaud
	if baud <= 0 {
		baud = defaultUSBBaud
	}
	mode := &serial.Mode{
		BaudRate: baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(cfg.USBDevice, mode)
	if err != nil {
		return nil, err
	}
	conn, err := newSerialConn(port, cfg.USBResetOnOpen)
	if err != nil {
		port.Close()
		return nil, err
	}
	return &Opened{
		Conn:       conn,
		Label:      fmt.Sprintf("%s@%d", cfg.USBDevice, baud),
		Kind:       KindUSB,
		PacketSize: USBPacketSize,
	}, nil
}

type serialConn struct {
	port serialPort

	writeMu sync.Mutex
	readMu  sync.Mutex

	deadlineMu sync.Mutex
	deadline   time.Time
}

func newSerialConn(port serialPort, resetOnOpen bool) (*serialConn, error) {
	c := &serialConn{port: port}
	if !resetOnOpen {
		if err := port.ResetInputBuffer(); err != nil {
			return nil, err
		}
		return c, nil
	}
	if err := port.SetDTR(false); err != nil {
		return nil, err
	}
	serialResetSleep(500 * time.Millisecond)
	if err := port.ResetInputBuffer(); err != nil {
		return nil, err
	}
	if err := port.SetDTR(true); err != nil {
		return nil, err
	}
	serialResetSleep(500 * time.Millisecond)
	return c, nil
}

func (c *serialConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	timeout, err := c.readTimeout()
	if err != nil {
		return 0, err
	}
	if err := c.port.SetReadTimeout(timeout); err != nil {
		return 0, err
	}
	n, err := c.port.Read(p)
	if n == 0 && err == nil && timeout != serial.NoTimeout {
		return 0, timeoutError{}
	}
	return n, err
}

func (c *serialConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	written := 0
	for written < len(p) {
		n, err := c.port.Write(p[written:])
		if n > 0 {
			written += n
		}
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrNoProgress
		}
	}
	return written, nil
}

func (c *serialConn) SetReadDeadline(t time.Time) error {
	c.deadlineMu.Lock()
	defer c.deadlineMu.Unlock()
	c.deadline = t
	return nil
}

func (c *serialConn) Close() error { return c.port.Close() }

func (c *serialConn) readTimeout() (time.Duration, error) {
	c.deadlineMu.Lock()
	deadline := c.deadline
	c.deadlineMu.Unlock()
	if deadline.IsZero() {
		return serial.NoTimeout, nil
	}
	timeout := time.Until(deadline)
	if timeout <= 0 {
		return 0, timeoutError{}
	}
	return timeout, nil
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "machine serial read timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
