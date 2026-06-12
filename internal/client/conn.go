// Package client speaks the Carvera wire protocol to a machine over a TCP
// connection the proxy owns (owner mode). It implements the management commands
// (ls/rm/mv/mkdir/md5sum), a status query, and the upload handshake.
//
// This is the execution path the sync engine uses; it is only ever active when
// no controller is connected, so there is a single in-flight operation at a time.
package client

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/uwin/cnc-proxy/internal/protocol"
)

// WifiPacketSize is the upload chunk size used over WiFi, matching the
// controller (XMODEM.py wifiMode).
const WifiPacketSize = 8192

// transport is the minimal byte channel Conn needs. A net.Conn satisfies it
// directly (owner mode), and the relay's injection mux provides an
// implementation that shares the controller's machine socket (relay mode).
type transport interface {
	Write(p []byte) (int, error)
	Read(p []byte) (int, error)
	SetReadDeadline(t time.Time) error
	Close() error
}

// Conn wraps a frame transport with frame-level read/write and a resync scanner.
type Conn struct {
	c    transport
	scan protocol.Scanner
	// pending holds frames already parsed from a read that returned more than
	// one frame, so the next readFrame call drains them first.
	pending []protocol.Frame
}

// Dial connects to a machine at host:port.
func Dial(addr string, timeout time.Duration) (*Conn, error) {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	return &Conn{c: c}, nil
}

// New wraps an existing connection (useful for tests).
func New(c net.Conn) *Conn { return &Conn{c: c} }

// NewTransport wraps any frame transport, e.g. the relay injection mux.
func NewTransport(t transport) *Conn { return &Conn{c: t} }

// Close closes the underlying connection.
func (k *Conn) Close() error { return k.c.Close() }

func (k *Conn) writeFrame(b []byte) error {
	_, err := k.c.Write(b)
	return err
}

// readFrame returns the next protocol frame, blocking until one arrives or the
// deadline passes. Bytes that don't form a frame are buffered across calls.
func (k *Conn) readFrame(deadline time.Time) (protocol.Frame, error) {
	if len(k.pending) > 0 {
		f := k.pending[0]
		k.pending = k.pending[1:]
		return f, nil
	}
	buf := make([]byte, 16*1024)
	for {
		if err := k.c.SetReadDeadline(deadline); err != nil {
			return protocol.Frame{}, err
		}
		n, err := k.c.Read(buf)
		if n > 0 {
			frames := k.scan.Push(buf[:n])
			if len(frames) > 0 {
				k.pending = frames[1:]
				return frames[0], nil
			}
		}
		if err != nil {
			return protocol.Frame{}, err
		}
	}
}

// CommandResult is the outcome of a management command.
type CommandResult struct {
	Info    string // concatenated LOAD_INFO payloads (e.g. listing rows)
	Success bool   // LOAD_FINISH vs LOAD_ERROR
}

// runManaged sends a management command frame and collects response frames
// until LOAD_FINISH or LOAD_ERROR. Status reports (STATUS_RES) that arrive
// interleaved are ignored here; the caller's state tracker handles those via
// the relay/poll path.
func (k *Conn) runManaged(frame []byte, timeout time.Duration) (CommandResult, error) {
	if err := k.writeFrame(frame); err != nil {
		return CommandResult{}, err
	}
	deadline := time.Now().Add(timeout)
	var info []byte
	for {
		f, err := k.readFrame(deadline)
		if err != nil {
			return CommandResult{}, err
		}
		switch f.Cmd {
		case protocol.CmdLoadInfo:
			info = append(info, f.Data...)
		case protocol.CmdLoadFinish:
			return CommandResult{Info: string(info), Success: true}, nil
		case protocol.CmdLoadError:
			return CommandResult{Info: string(info), Success: false}, nil
		default:
			// Ignore status/diag/normal-info noise during a managed command.
		}
	}
}

// List runs `ls -e -s` and returns parsed directory entries.
func (k *Conn) List(dir string, timeout time.Duration) ([]protocol.DirEntry, error) {
	res, err := k.runManaged(protocol.LsCommand(dir), timeout)
	if err != nil {
		return nil, err
	}
	if !res.Success {
		return nil, fmt.Errorf("ls %q failed: %s", dir, res.Info)
	}
	return protocol.ParseListing(res.Info), nil
}

// Remove runs `rm <path> -e`.
func (k *Conn) Remove(path string, timeout time.Duration) error {
	res, err := k.runManaged(protocol.RmCommand(path), timeout)
	if err != nil {
		return err
	}
	if !res.Success {
		return fmt.Errorf("rm %q failed: %s", path, res.Info)
	}
	return nil
}

// Rename runs `mv <from> <to> -e`.
func (k *Conn) Rename(from, to string, timeout time.Duration) error {
	res, err := k.runManaged(protocol.MvCommand(from, to), timeout)
	if err != nil {
		return err
	}
	if !res.Success {
		return fmt.Errorf("mv %q->%q failed: %s", from, to, res.Info)
	}
	return nil
}

// Mkdir runs `mkdir <dir> -e`.
func (k *Conn) Mkdir(dir string, timeout time.Duration) error {
	res, err := k.runManaged(protocol.MkdirCommand(dir), timeout)
	if err != nil {
		return err
	}
	if !res.Success {
		return fmt.Errorf("mkdir %q failed: %s", dir, res.Info)
	}
	return nil
}

// Md5 runs `md5sum <path>` and returns the lowercase hex digest. The firmware
// replies with a single NORMAL_INFO line — "<hex> <path>" on success or
// "File not found: <path>" — rather than a packetized LOAD_* response, so this
// scans info lines rather than using the managed-command path.
func (k *Conn) Md5(path string, timeout time.Duration) (string, error) {
	if err := k.writeFrame(protocol.Md5Command(path)); err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	for {
		f, err := k.readFrame(deadline)
		if err != nil {
			return "", err
		}
		switch f.Cmd {
		case protocol.CmdNormalInfo, protocol.CmdLoadInfo:
			text := string(f.Data)
			if digest, ok := protocol.ParseMd5Response(text); ok {
				return digest, nil
			}
			if strings.Contains(text, "not found") || strings.Contains(strings.ToLower(text), "error") {
				return "", fmt.Errorf("md5sum %q: %s", path, strings.TrimSpace(text))
			}
			// Other info noise (e.g. a trailing upload message): keep reading.
		default:
			// Ignore STATUS_RES etc.
		}
	}
}

// Ftype sends `ftype` and returns the upload file type the firmware supports
// (e.g. "lz"). The reply arrives as an info line "ftype = <type>" rather than a
// LOAD_* frame, so we scan info-bearing frames for it.
func (k *Conn) Ftype(timeout time.Duration) (string, error) {
	if err := k.writeFrame(protocol.FtypeCommand()); err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	for {
		f, err := k.readFrame(deadline)
		if err != nil {
			return "", err
		}
		switch f.Cmd {
		case protocol.CmdNormalInfo, protocol.CmdLoadInfo, protocol.CmdStatusRes:
			if t, ok := protocol.ParseFtype(string(f.Data)); ok {
				return t, nil
			}
		}
	}
}

// SendGcode sends a single command line (CTRL_MULTI) and collects its response.
//
// Response shapes differ by command kind, so termination is not just "wait for
// ok": actual G/M codes get a terminating "ok\r\n" (or "ok <payload>", e.g.
// M114 position) via GcodeDispatch, but console-style commands (version, etc.)
// reply with output lines and NO "ok". To handle both without hanging, this
// returns on an "ok"/"error" line if one arrives, otherwise after a short
// quiescence window once at least one output line has been seen. quietWindow is
// how long to wait for more output before considering a no-ok command done.
func (k *Conn) SendGcode(line string, timeout time.Duration) (string, error) {
	if len(line) == 0 || line[len(line)-1] != '\n' {
		line += "\n"
	}
	if err := k.writeFrame(protocol.Encode(protocol.CmdCtrlMulti, []byte(line))); err != nil {
		return "", err
	}
	const quietWindow = 400 * time.Millisecond
	overall := time.Now().Add(timeout)
	var out []byte
	for {
		// Read deadline is the sooner of the overall timeout and (once we have
		// output) the quiescence window.
		rd := overall
		if len(out) > 0 && time.Now().Add(quietWindow).Before(rd) {
			rd = time.Now().Add(quietWindow)
		}
		f, err := k.readFrame(rd)
		if err != nil {
			// Many commands legitimately produce output with no terminating
			// "ok" (console commands like `version`), or the firmware closes
			// the stream after the output. Once we've collected any output, a
			// subsequent read error — whether a quiescence timeout or an
			// EOF/closed connection — marks the end of that output, not a
			// failure. Only surface the error when nothing was received at all.
			if len(out) > 0 {
				return string(out), nil
			}
			return "", err
		}
		switch f.Cmd {
		case protocol.CmdNormalInfo:
			text := string(f.Data)
			trimmed := strings.TrimRight(text, "\r\n")
			low := strings.ToLower(strings.TrimSpace(trimmed))
			switch {
			case low == "ok":
				return string(out), nil
			case strings.HasPrefix(low, "ok "):
				// "ok <payload>" — the firmware appended the command's result to
				// the ok line (e.g. M114 position). Capture the payload.
				payload := strings.TrimSpace(trimmed[len("ok "):])
				if len(out) > 0 {
					out = append(out, '\n')
				}
				out = append(out, payload...)
				return string(out), nil
			case strings.Contains(low, "error") || strings.Contains(low, "alarm"):
				return string(out), fmt.Errorf("machine: %s", trimmed)
			default:
				out = append(out, f.Data...)
			}
		default:
			// Ignore STATUS_RES and other frames during a gcode command.
		}
	}
}

// QueryState sends `?` and waits for the next STATUS_RES frame, returning its
// payload (e.g. "<Idle|...>"). The caller parses it via the machine package.
func (k *Conn) QueryState(timeout time.Duration) (string, error) {
	if err := k.writeFrame(protocol.QueryStatus()); err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	for {
		f, err := k.readFrame(deadline)
		if err != nil {
			return "", err
		}
		if f.Cmd == protocol.CmdStatusRes {
			return string(f.Data), nil
		}
	}
}

var ErrUploadCanceled = errors.New("upload canceled by machine")

// Upload transfers a file to remotePath. md5hex is the MD5 of the file's
// contents (the firmware stores it in a sidecar and the controller compares it
// for sync confirmation). size is the file length. The handshake is firmware-
// driven: it requests MD5, then a file "view", then each data packet by
// sequence number; we react to each request. progress, if non-nil, is called
// with the number of packets sent so far.
//
// Note: this primitive uploads the bytes it is given. The sync engine may wrap
// it with QuickLZ compression by passing a .lz container and the uncompressed
// MD5, matching the controller's large-upload behavior.
func (k *Conn) Upload(remotePath string, r io.ReaderAt, size int64, md5hex string, timeout time.Duration, progress func(sent, total uint32)) error {
	if err := k.writeFrame(protocol.UploadCommand(remotePath)); err != nil {
		return err
	}
	// Proactively send MD5, as the controller does.
	if err := k.writeFrame(protocol.Encode(protocol.CmdFileMD5, []byte(md5hex))); err != nil {
		return err
	}

	packetSize := int64(WifiPacketSize)
	totalPackets := uint32((size + packetSize - 1) / packetSize)
	if totalPackets == 0 {
		totalPackets = 1 // an empty file is still one (empty) packet to view
	}

	deadline := time.Now().Add(timeout)
	for {
		f, err := k.readFrame(deadline)
		if err != nil {
			return err
		}
		// Any progress resets the inactivity deadline.
		deadline = time.Now().Add(timeout)

		switch f.Cmd {
		case protocol.CmdFileCancel:
			return ErrUploadCanceled
		case protocol.CmdFileMD5:
			if err := k.writeFrame(protocol.Encode(protocol.CmdFileMD5, []byte(md5hex))); err != nil {
				return err
			}
		case protocol.CmdFileView:
			view := []byte{
				byte(totalPackets >> 24), byte(totalPackets >> 16), byte(totalPackets >> 8), byte(totalPackets),
				byte(WifiPacketSize >> 8), byte(WifiPacketSize & 0xFF),
			}
			if err := k.writeFrame(protocol.Encode(protocol.CmdFileView, view)); err != nil {
				return err
			}
		case protocol.CmdFileData:
			if len(f.Data) < 4 {
				continue
			}
			seq := uint32(f.Data[0])<<24 | uint32(f.Data[1])<<16 | uint32(f.Data[2])<<8 | uint32(f.Data[3])
			if err := k.sendDataPacket(r, size, packetSize, seq); err != nil {
				return err
			}
			if progress != nil {
				progress(seq, totalPackets)
			}
		case protocol.CmdFileEnd:
			if progress != nil {
				progress(totalPackets, totalPackets)
			}
			return nil
		default:
			// ignore
		}
	}
}

// sendDataPacket reads the chunk for the requested 1-based sequence and sends a
// FILE_DATA frame of seq(4 BE) + chunk.
func (k *Conn) sendDataPacket(r io.ReaderAt, size, packetSize int64, seq uint32) error {
	offset := int64(seq-1) * packetSize
	n := packetSize
	if offset+n > size {
		n = size - offset
	}
	if n < 0 {
		n = 0
	}
	chunk := make([]byte, n)
	if n > 0 {
		if _, err := r.ReadAt(chunk, offset); err != nil && err != io.EOF {
			return err
		}
	}
	data := make([]byte, 4+len(chunk))
	data[0] = byte(seq >> 24)
	data[1] = byte(seq >> 16)
	data[2] = byte(seq >> 8)
	data[3] = byte(seq)
	copy(data[4:], chunk)
	return k.writeFrame(protocol.Encode(protocol.CmdFileData, data))
}

var ErrDownloadCanceled = errors.New("download canceled by machine")

// reqSeq sends a FILE_DATA frame whose payload is just the 4-byte sequence
// number, which is how the receiver asks the machine for that packet.
func (k *Conn) reqSeq(seq uint32) error {
	data := []byte{byte(seq >> 24), byte(seq >> 16), byte(seq >> 8), byte(seq)}
	return k.writeFrame(protocol.Encode(protocol.CmdFileData, data))
}

// Download fetches remotePath from the machine into w. Here the proxy is the
// receiver and driver: the machine sends MD5 first, then we request the file
// view, then we pull each packet by sequence number until the last one, and
// acknowledge with FILE_END. It returns the machine-reported MD5 (of the
// uncompressed content) and the number of bytes written.
//
// The bytes written are exactly what the machine sends. If a .lz sidecar exists
// the machine sends compressed bytes; callers that need the plaintext must
// detect and decompress (see protocol.IsQuickLZ). For .nc/.gcode files without
// a sidecar the machine sends them uncompressed.
func (k *Conn) Download(remotePath string, w io.Writer, timeout time.Duration, progress func(recv, total uint32)) (md5hex string, written int64, err error) {
	if err := k.writeFrame(protocol.DownloadCommand(remotePath)); err != nil {
		return "", 0, err
	}

	const (
		stWaitMD5 = iota
		stWaitView
		stReadData
	)
	state := stWaitMD5
	var totalPackets, nextSeq uint32

	deadline := time.Now().Add(timeout)
	for {
		f, err := k.readFrame(deadline)
		if err != nil {
			return "", written, err
		}
		deadline = time.Now().Add(timeout)

		if f.Cmd == protocol.CmdFileCancel {
			return "", written, ErrDownloadCanceled
		}

		switch state {
		case stWaitMD5:
			if f.Cmd == protocol.CmdFileMD5 {
				md5hex = string(f.Data)
				// Request the file view (total packets + packet size).
				if err := k.writeFrame(protocol.Encode(protocol.CmdFileView, nil)); err != nil {
					return md5hex, written, err
				}
				state = stWaitView
			}
		case stWaitView:
			if f.Cmd == protocol.CmdFileView {
				if len(f.Data) < 6 {
					return md5hex, written, fmt.Errorf("download %q: short FILE_VIEW (%d bytes)", remotePath, len(f.Data))
				}
				totalPackets = uint32(f.Data[0])<<24 | uint32(f.Data[1])<<16 | uint32(f.Data[2])<<8 | uint32(f.Data[3])
				if totalPackets == 0 {
					return md5hex, written, fmt.Errorf("download %q: FILE_VIEW reports zero packets", remotePath)
				}
				nextSeq = 1
				if err := k.reqSeq(nextSeq); err != nil {
					return md5hex, written, err
				}
				state = stReadData
			}
		case stReadData:
			if f.Cmd == protocol.CmdFileEnd {
				// The machine ended the transfer before the last expected packet.
				// Accept what we received rather than blocking until timeout.
				return md5hex, written, nil
			}
			if f.Cmd != protocol.CmdFileData || len(f.Data) < 4 {
				continue
			}
			seq := uint32(f.Data[0])<<24 | uint32(f.Data[1])<<16 | uint32(f.Data[2])<<8 | uint32(f.Data[3])
			if seq != nextSeq {
				// Re-request the packet we still expect.
				if err := k.reqSeq(nextSeq); err != nil {
					return md5hex, written, err
				}
				continue
			}
			n, werr := w.Write(f.Data[4:])
			written += int64(n)
			if werr != nil {
				return md5hex, written, werr
			}
			if progress != nil {
				progress(seq, totalPackets)
			}
			if seq >= totalPackets {
				if err := k.writeFrame(protocol.Encode(protocol.CmdFileEnd, nil)); err != nil {
					return md5hex, written, err
				}
				return md5hex, written, nil
			}
			nextSeq++
			if err := k.reqSeq(nextSeq); err != nil {
				return md5hex, written, err
			}
		}
	}
}
