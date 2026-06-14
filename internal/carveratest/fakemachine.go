// Package carveratest provides a fake Carvera machine that speaks the framed
// wire protocol, for use in tests across packages. It emulates the firmware's
// management commands and the firmware-driven upload handshake closely enough
// to exercise the client, arbiter, and sync engine end to end.
package carveratest

import (
	"net"
	"strings"
	"sync"
	"time"

	"github.com/uwin/cnc-proxy/internal/protocol"
)

// FakeMachine is a TCP server emulating a Carvera over the wire protocol.
type FakeMachine struct {
	ln net.Listener

	mu                sync.Mutex
	files             map[string][]byte // remote path -> contents (from uploads)
	dirs              map[string]bool   // created directories
	status            string            // payload for "?" (e.g. "<Idle|...>")
	failCmd           map[string]bool   // command prefixes to fail (for error-path tests)
	ftype             string            // advertised upload type ("lz" enables compression)
	compressDownloads bool              // if set, downloads send a .lz container
	gcodes            []string          // CTRL_MULTI gcode lines received (motion/MDI)
	controls          []byte            // CTRL_SINGLE control chars received (!, ~, 0x18)
	gcodeReplies      map[string]string // exact line -> textual reply payload
}

// New starts a FakeMachine listening on a random loopback port. Call Close when
// done.
func New() (*FakeMachine, error) { return NewOn("127.0.0.1:0") }

// NewOn starts a FakeMachine listening on the given address (e.g. ":2222" for a
// fixed port in manual testing).
func NewOn(addr string) (*FakeMachine, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	m := &FakeMachine{
		ln:           ln,
		files:        map[string][]byte{},
		dirs:         map[string]bool{},
		status:       "<Idle|MPos:0,0,0|WPos:0,0,0>",
		failCmd:      map[string]bool{},
		gcodeReplies: map[string]string{},
	}
	go m.serve()
	return m, nil
}

// Addr returns the host:port the machine listens on.
func (m *FakeMachine) Addr() string { return m.ln.Addr().String() }

// Close stops the machine.
func (m *FakeMachine) Close() { m.ln.Close() }

// SetStatus sets the payload returned for "?" queries.
func (m *FakeMachine) SetStatus(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = s
}

// SetFtype sets the upload type advertised via "ftype" ("lz" enables QuickLZ
// upload compression; "nc" or empty disables it).
func (m *FakeMachine) SetFtype(s string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ftype = s
}

// SetCompressDownloads makes downloads send a QuickLZ .lz container (as the
// firmware does when a .lz sidecar exists), while still reporting the
// uncompressed MD5. Used to exercise download-side decompression.
func (m *FakeMachine) SetCompressDownloads(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.compressDownloads = v
}

// FailCommand makes management commands with the given prefix return LOAD_ERROR.
func (m *FakeMachine) FailCommand(prefix string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failCmd[prefix] = true
}

// File returns the contents uploaded to a path.
func (m *FakeMachine) File(path string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[path]
	return b, ok
}

// HasDir reports whether a directory was created.
func (m *FakeMachine) HasDir(path string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dirs[path]
}

// Gcodes returns the CTRL_MULTI gcode/MDI lines the machine has received.
func (m *FakeMachine) Gcodes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.gcodes...)
}

// Controls returns the realtime control characters (!, ~, 0x18) received.
func (m *FakeMachine) Controls() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.controls...)
}

// SetGcodeReply makes the machine answer an exact gcode line with the given
// reply (without trailing CRLF), e.g. "ok C: X:1.0" for an M114. Lines with no
// configured reply get a bare "ok".
func (m *FakeMachine) SetGcodeReply(line, reply string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcodeReplies[line] = reply
}

func (m *FakeMachine) serve() {
	for {
		c, err := m.ln.Accept()
		if err != nil {
			return
		}
		go m.handle(c)
	}
}

func (m *FakeMachine) send(c net.Conn, cmd byte, payload string) {
	c.Write(protocol.Encode(cmd, []byte(payload)))
}

func (m *FakeMachine) handle(c net.Conn) {
	defer c.Close()
	var scan protocol.Scanner
	buf := make([]byte, 16*1024)

	const (
		modeIdle     = iota
		modeUpload   // controller -> machine (machine receives)
		modeDownload // machine -> controller (machine sends)
	)
	var (
		mode      int
		xferPath  string
		totalPkts uint32
		nextSeq   uint32
		received  []byte
		sendData  []byte // contents being sent during a download
	)
	const pktSize = 8192

	for {
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := c.Read(buf)
		if n > 0 {
			for _, f := range scan.Push(buf[:n]) {
				switch f.Cmd {
				case protocol.CmdCtrlSingle:
					if len(f.Data) == 1 {
						switch f.Data[0] {
						case '?':
							m.mu.Lock()
							s := m.status
							m.mu.Unlock()
							m.send(c, protocol.CmdStatusRes, s)
						case '!', '~', 0x18:
							// Realtime control: record it so tests can assert it
							// arrived. The firmware acts out-of-band and sends no
							// reply (halt does emit an ALARM line, but we keep the
							// fake minimal).
							m.mu.Lock()
							m.controls = append(m.controls, f.Data[0])
							m.mu.Unlock()
						}
					}
				case protocol.CmdCtrlMulti:
					m.handleManaged(c, string(f.Data))
				case protocol.CmdFileStart:
					line := strings.TrimSpace(string(f.Data))
					verb, arg := "", ""
					if fields := strings.SplitN(line, " ", 2); len(fields) == 2 {
						verb = fields[0]
						arg = strings.ReplaceAll(strings.TrimSpace(fields[1]), "\x01", " ")
					}
					xferPath = arg
					received = nil
					nextSeq = 0
					if verb == "download" {
						mode = modeDownload
						m.mu.Lock()
						plain := append([]byte(nil), m.files[xferPath]...)
						_, exists := m.files[xferPath]
						compress := m.compressDownloads
						m.mu.Unlock()
						if !exists {
							m.send(c, protocol.CmdFileCancel, "not found")
							mode = modeIdle
							break
						}
						// The reported MD5 is always of the uncompressed content.
						uncompressedMD5 := md5hex(plain)
						sendData = plain
						if compress {
							// Send a .lz container instead, as the firmware does
							// when a sidecar exists.
							sendData = compressLZ(plain)
						}
						m.send(c, protocol.CmdFileMD5, uncompressedMD5)
						totalPkts = uint32((len(sendData) + pktSize - 1) / pktSize)
						if totalPkts == 0 {
							totalPkts = 1
						}
					} else {
						mode = modeUpload
					}
				case protocol.CmdFileMD5:
					if mode == modeUpload {
						m.send(c, protocol.CmdFileView, "")
					}
				case protocol.CmdFileView:
					switch mode {
					case modeUpload:
						if len(f.Data) >= 6 {
							totalPkts = uint32(f.Data[0])<<24 | uint32(f.Data[1])<<16 | uint32(f.Data[2])<<8 | uint32(f.Data[3])
							nextSeq = 1
							m.requestSeq(c, nextSeq)
						}
					case modeDownload:
						// Controller requested the view; reply with totals.
						view := []byte{
							byte(totalPkts >> 24), byte(totalPkts >> 16), byte(totalPkts >> 8), byte(totalPkts),
							byte(pktSize >> 8), byte(pktSize & 0xFF),
						}
						m.send2(c, protocol.CmdFileView, view)
					}
				case protocol.CmdFileData:
					if mode == modeUpload && len(f.Data) >= 4 {
						seq := uint32(f.Data[0])<<24 | uint32(f.Data[1])<<16 | uint32(f.Data[2])<<8 | uint32(f.Data[3])
						if seq == nextSeq {
							received = append(received, f.Data[4:]...)
							if seq < totalPkts {
								nextSeq++
								m.requestSeq(c, nextSeq)
							} else {
								// Mirror the firmware: a .lz upload is decompressed and
								// stored under the stripped name.
								storePath := xferPath
								content := received
								if strings.HasSuffix(xferPath, ".lz") {
									storePath = strings.TrimSuffix(xferPath, ".lz")
									if dec, derr := decompressLZ(received); derr == nil {
										content = dec
									} else {
										m.send(c, protocol.CmdFileCancel, "decompress failed")
										mode = modeIdle
										continue
									}
								}
								m.mu.Lock()
								cp := make([]byte, len(content))
								copy(cp, content)
								m.files[storePath] = cp
								m.mu.Unlock()
								m.send(c, protocol.CmdFileEnd, "")
								mode = modeIdle
							}
						} else {
							m.requestSeq(c, nextSeq)
						}
					} else if mode == modeDownload && len(f.Data) >= 4 {
						// Controller is requesting packet `seq`; send it.
						seq := uint32(f.Data[0])<<24 | uint32(f.Data[1])<<16 | uint32(f.Data[2])<<8 | uint32(f.Data[3])
						m.sendDownloadPacket(c, sendData, pktSize, seq)
					}
				case protocol.CmdFileEnd:
					if mode == modeDownload {
						m.send(c, protocol.CmdFileEnd, "")
						mode = modeIdle
					}
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (m *FakeMachine) requestSeq(c net.Conn, seq uint32) {
	data := []byte{byte(seq >> 24), byte(seq >> 16), byte(seq >> 8), byte(seq)}
	c.Write(protocol.Encode(protocol.CmdFileData, data))
}

// send2 writes a frame with a raw byte payload.
func (m *FakeMachine) send2(c net.Conn, cmd byte, data []byte) {
	c.Write(protocol.Encode(cmd, data))
}

// sendDownloadPacket sends the 1-based packet `seq` of data as a FILE_DATA frame
// (seq prefix + chunk), mirroring the firmware's download sender.
func (m *FakeMachine) sendDownloadPacket(c net.Conn, data []byte, pktSize int, seq uint32) {
	off := int(seq-1) * pktSize
	if off < 0 || off > len(data) {
		return
	}
	end := off + pktSize
	if end > len(data) {
		end = len(data)
	}
	chunk := data[off:end]
	payload := make([]byte, 4+len(chunk))
	payload[0] = byte(seq >> 24)
	payload[1] = byte(seq >> 16)
	payload[2] = byte(seq >> 8)
	payload[3] = byte(seq)
	copy(payload[4:], chunk)
	c.Write(protocol.Encode(protocol.CmdFileData, payload))
}

func (m *FakeMachine) handleManaged(c net.Conn, line string) {
	line = strings.TrimSpace(line)
	m.mu.Lock()
	for prefix := range m.failCmd {
		if strings.HasPrefix(line, prefix) {
			m.mu.Unlock()
			m.send(c, protocol.CmdLoadError, "forced failure\r\n")
			return
		}
	}
	m.mu.Unlock()

	switch {
	case strings.HasPrefix(line, "ls"):
		// Synthesize a listing of the requested directory's DIRECT children,
		// from stored files/dirs. The dir is the last token of "ls -e -s <dir>".
		dir := lsDir(line)
		m.mu.Lock()
		var sb strings.Builder
		for p := range m.dirs {
			if parentOf(p) == dir {
				sb.WriteString(lastSegment(p) + "/ 0 20260101120000\r\n")
			}
		}
		for p, b := range m.files {
			if parentOf(p) == dir {
				sb.WriteString(lastSegment(p) + " " + itoa(len(b)) + " 20260101120000\r\n")
			}
		}
		payload := sb.String()
		m.mu.Unlock()
		if payload != "" {
			m.send(c, protocol.CmdLoadInfo, payload)
		}
		m.send(c, protocol.CmdLoadFinish, "Load directory finished.\r\n")
	case strings.HasPrefix(line, "md5sum"):
		// Mirror the firmware: md5sum does NOT parse flags and replies with a
		// single NORMAL_INFO line, not packetized LOAD_* frames.
		path := md5Target(line)
		m.mu.Lock()
		b, ok := m.files[path]
		m.mu.Unlock()
		if ok {
			m.send(c, protocol.CmdNormalInfo, md5hex(b)+" "+path+"\n")
		} else {
			m.send(c, protocol.CmdNormalInfo, "File not found: "+path+"\n")
		}
	case strings.HasPrefix(line, "ftype"):
		m.mu.Lock()
		ft := m.ftype
		m.mu.Unlock()
		if ft == "" {
			ft = "nc" // default: no compression
		}
		m.send(c, protocol.CmdNormalInfo, "ftype = "+ft+"\n")
	case strings.EqualFold(line, "diagnose"):
		// Real firmware emits DIAG_RES for diagnose, not NORMAL_INFO. Keep the
		// fake strict so tests catch callers that only listen for NORMAL_INFO.
		m.mu.Lock()
		m.gcodes = append(m.gcodes, line)
		reply, ok := m.gcodeReplies[line]
		m.mu.Unlock()
		if !ok {
			reply = "{S:0,0|I:0}"
		}
		m.send(c, protocol.CmdDiagRes, strings.TrimRight(reply, "\r\n")+"\n")
	case strings.HasPrefix(line, "rm"):
		path := secondField(line)
		m.mu.Lock()
		delete(m.files, path)
		m.mu.Unlock()
		m.send(c, protocol.CmdLoadFinish, "ok\r\n")
	case strings.HasPrefix(line, "mv"):
		m.send(c, protocol.CmdLoadFinish, "ok\r\n")
	case strings.HasPrefix(line, "mkdir"):
		path := secondField(line)
		m.mu.Lock()
		m.dirs[path] = true
		m.mu.Unlock()
		m.send(c, protocol.CmdLoadFinish, "ok\r\n")
	case isGcodeLine(line):
		// Motion / MDI / console gcode. The firmware dispatches it and replies
		// with NORMAL_INFO, not LOAD framing. Record it so tests can assert what
		// reached the machine. Reply behavior mirrors hardware (verified on a
		// real Carvera):
		//   - read-only queries (M114, version, …) DO get a reply line, e.g.
		//     "ok C: X:..." for M114 or "version = 1.0.5";
		//   - motion / state-changing gcode (G0/G1, $H, …) gets NO reply at all —
		//     the move just executes. The proxy must NOT wait for an "ok" on
		//     these, so the fake stays silent unless a reply is explicitly set.
		m.mu.Lock()
		m.gcodes = append(m.gcodes, line)
		reply, ok := m.gcodeReplies[line]
		m.mu.Unlock()
		resp, _ := protocol.ClassifyGcode(line)
		if ok {
			m.send(c, protocol.CmdNormalInfo, reply+"\r\n")
		} else if resp == protocol.ReplyExpected {
			// A reply-expected command with no explicitly-configured reply still
			// gets a bare ok, as the firmware answers these promptly.
			m.send(c, protocol.CmdNormalInfo, "ok\r\n")
		}
		// Otherwise (fire-and-forget: motion/modal/dwell): silent, like hardware.
	default:
		m.send(c, protocol.CmdLoadError, "unknown\r\n")
	}
}

// isGcodeLine reports whether a console line is a gcode/MDI command (G/M/T/S
// codes, a grbl '$' command, an N-numbered line, or a console-word query the
// firmware answers with NORMAL_INFO), as opposed to the filesystem/management
// commands handled explicitly above. It mirrors the real firmware closely
// enough that anything the proxy will send as CTRL_MULTI gets a NORMAL_INFO
// reply rather than a LOAD_ERROR (which the client ignores, hanging to timeout).
func isGcodeLine(line string) bool {
	if line == "" {
		return false
	}
	switch line[0] {
	case 'G', 'M', 'T', 'S', '$', 'g', 'm', 't', 's', 'N', 'n':
		return true
	}
	// Console-word queries (version, model, ftype, time, echo, mem, diagnose).
	if protocol.IsStatusQuery(line) {
		return true
	}
	return false
}
