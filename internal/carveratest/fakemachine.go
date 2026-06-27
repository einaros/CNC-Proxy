// Package carveratest provides a fake Carvera machine that speaks the framed
// wire protocol, for use in tests across packages. It emulates the firmware's
// management commands and the firmware-driven upload handshake closely enough
// to exercise the client, arbiter, and sync engine end to end.
package carveratest

import (
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/uwin/cnc-proxy/internal/protocol"
)

// FakeMachine is a TCP server emulating a Carvera over the wire protocol.
type FakeMachine struct {
	ln net.Listener

	mu                 sync.Mutex
	files              map[string][]byte // remote path -> contents (from uploads)
	dirs               map[string]bool   // created directories
	status             string            // payload for "?" (e.g. "<Idle|...>")
	statusReplyDelay   time.Duration     // optional test hook: delay "?" replies
	dropStatusReplies  bool              // optional test hook: ignore "?" replies
	failCmd            map[string]bool   // command prefixes to fail (for error-path tests)
	ftype              string            // advertised upload type ("lz" enables compression)
	compressDownloads  bool              // if set, downloads send a .lz container
	downloadPacketSize int               // packet size reported/sent for downloads
	gcodes             []string          // CTRL_MULTI gcode lines received (motion/MDI)
	controls           []byte            // CTRL_SINGLE control chars received (!, ~, 0x18)
	gcodeReplies       map[string]string // exact line -> textual reply payload
	uploadPacketSizes  []int             // packet sizes advertised by upload senders
	unlockDoesNotClear bool              // test hook: $X replies but leaves status unchanged
	m999DoesNotClear   bool              // test hook: M999 replies but leaves status unchanged
	absolute           bool              // simulated modal distance mode for ordinary G0/G1 moves
	unit               float64           // simulated modal unit scale, mm per gcode unit
	motionMode         int               // simulated modal motion mode for ordinary axis words
	feedMMMin          float64           // simulated modal G1 feed, in mm/min
	motion             []fakeMotionSegment
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
		ln:                 ln,
		files:              map[string][]byte{},
		dirs:               map[string]bool{},
		status:             "<Idle|MPos:0,0,0|WPos:0,0,0>",
		failCmd:            map[string]bool{},
		gcodeReplies:       map[string]string{},
		downloadPacketSize: 8192,
		absolute:           true,
		unit:               1,
		motionMode:         fakeMotionRapid,
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
	m.motion = nil
}

// SetStatusReplyDelay delays replies to `?` status polls. It is a test hook for
// exercising jog/status behavior when firmware replies are delayed by motion.
func (m *FakeMachine) SetStatusReplyDelay(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statusReplyDelay = d
}

// SetDropStatusReplies makes the fake ignore `?` status polls. It is a test
// hook for exercising status timeout behavior.
func (m *FakeMachine) SetDropStatusReplies(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropStatusReplies = v
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

// SetDownloadPacketSize sets the FILE_VIEW packet size used when the fake
// machine sends downloads. Real firmware uses 8192 over WiFi and 128 over USB.
func (m *FakeMachine) SetDownloadPacketSize(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n > 0 {
		m.downloadPacketSize = n
	}
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

// UploadPacketSizes returns packet sizes advertised by upload senders in
// FILE_VIEW frames.
func (m *FakeMachine) UploadPacketSizes() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.uploadPacketSizes...)
}

// SetGcodeReply makes the machine answer an exact gcode line with the given
// reply (without trailing CRLF), e.g. "ok C: X:1.0" for an M114. Lines with no
// configured reply get a bare "ok".
func (m *FakeMachine) SetGcodeReply(line, reply string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gcodeReplies[line] = reply
}

// SetUnlockDoesNotClear makes $X leave the current status unchanged while still
// replying like firmware. Tests use this to exercise M999 recovery fallback.
func (m *FakeMachine) SetUnlockDoesNotClear(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unlockDoesNotClear = v
}

// SetM999DoesNotClear makes M999 leave the current status unchanged while still
// replying like firmware. Tests use this to exercise verified recovery failure.
func (m *FakeMachine) SetM999DoesNotClear(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m999DoesNotClear = v
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
	pktSize := 8192

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
							delay := m.statusReplyDelay
							drop := m.dropStatusReplies
							s := ""
							if !drop && delay <= 0 {
								s = m.statusAtLocked(time.Now())
							}
							m.mu.Unlock()
							if drop {
								break
							}
							if delay > 0 {
								go func() {
									time.Sleep(delay)
									m.mu.Lock()
									s := m.statusAtLocked(time.Now())
									m.mu.Unlock()
									m.send(c, protocol.CmdStatusRes, s)
								}()
								break
							}
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
						pktSize = m.downloadPacketSize
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
							ps := int(f.Data[4])<<8 | int(f.Data[5])
							m.mu.Lock()
							m.uploadPacketSizes = append(m.uploadPacketSizes, ps)
							m.mu.Unlock()
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
	case strings.HasPrefix(line, "config-get-all"):
		m.mu.Lock()
		m.gcodes = append(m.gcodes, line)
		reply, ok := m.gcodeReplies[line]
		m.mu.Unlock()
		if !ok {
			reply = "soft_endstop.x_min=-300\nsoft_endstop.y_min=-200\nsoft_endstop.z_min=-120\nalpha_max=0\nbeta_max=0\ngamma_max=0\nalpha_max_rate=3000\nbeta_max_rate=3000\ngamma_max_rate=2000\ndefault_seek_rate=3000"
		}
		m.send(c, protocol.CmdNormalInfo, strings.TrimRight(reply, "\r\n")+"\n")
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
	case strings.EqualFold(line, "reset"):
		// Firmware SimpleShell accepts "reset" as a console command and schedules
		// a reboot. Record it so alarm-recovery tests can assert it was sent.
		m.mu.Lock()
		m.gcodes = append(m.gcodes, line)
		m.mu.Unlock()
		m.send(c, protocol.CmdNormalInfo, "Rebooting machine in 3 seconds...\n")
	case strings.EqualFold(line, "M999"):
		m.mu.Lock()
		m.gcodes = append(m.gcodes, line)
		if !m.m999DoesNotClear {
			m.status = "<Idle|MPos:0,0,0|WPos:0,0,0>"
		}
		m.mu.Unlock()
		m.send(c, protocol.CmdNormalInfo, "WARNING: After HALT you should HOME before resume\nok\n")
	case line == "$X":
		m.mu.Lock()
		m.gcodes = append(m.gcodes, line)
		if !m.unlockDoesNotClear {
			m.status = "<Idle|MPos:0,0,0|WPos:0,0,0>"
		}
		m.mu.Unlock()
		m.send(c, protocol.CmdNormalInfo, "[Caution: Unlocked]\nok\n")
	case line == "$H":
		m.mu.Lock()
		m.gcodes = append(m.gcodes, line)
		m.status = "<Idle|MPos:0,0,0|WPos:0,0,0>"
		m.mu.Unlock()
		m.send(c, protocol.CmdNormalInfo, "ok\n")
	case strings.HasPrefix(strings.ToLower(line), "play "):
		m.mu.Lock()
		m.gcodes = append(m.gcodes, protocol.Unescape(line))
		m.mu.Unlock()
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
		m.applySimulatedGcodeLocked(line)
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
// codes, modal axis/feed words, a grbl '$' command, an N-numbered line, or a
// console-word query the firmware answers with NORMAL_INFO), as opposed to the
// filesystem/management commands handled explicitly above. It mirrors the real
// firmware closely enough that anything the proxy will send as CTRL_MULTI gets a
// NORMAL_INFO reply rather than a LOAD_ERROR (which the client ignores, hanging
// to timeout).
func isGcodeLine(line string) bool {
	if line == "" {
		return false
	}
	switch line[0] {
	case 'G', 'M', 'T', 'S', '$', 'N', 'X', 'Y', 'Z', 'A', 'B', 'C', 'F',
		'g', 'm', 't', 's', 'n', 'x', 'y', 'z', 'a', 'b', 'c', 'f':
		return true
	}
	// Console-word queries (version, model, ftype, time, echo, mem, diagnose).
	if protocol.IsStatusQuery(line) {
		return true
	}
	return false
}

type fakeGcodeWord struct {
	letter byte
	value  float64
}

type fakeStatusField struct {
	key   string
	value string
}

type fakeMotionSegment struct {
	start time.Time
	end   time.Time
	fromM []float64
	toM   []float64
	fromW []float64
	toW   []float64
}

const (
	fakeFirmwareMaxXYMMMin = 3000.0
	fakeFirmwareMaxZMMMin  = 2000.0
)

const (
	fakeMotionRapid = iota
	fakeMotionFeed
)

var fakeAxisLetters = []byte{'X', 'Y', 'Z', 'A', 'B', 'C'}

var fakeAxisIndex = map[byte]int{
	'X': 0,
	'Y': 1,
	'Z': 2,
	'A': 3,
	'B': 4,
	'C': 5,
}

// applySimulatedGcodeLocked updates the fake's status position for the motion
// commands CNC Proxy generates during manual/jog testing. It intentionally does
// not emit replies; fire-and-forget motion stays silent like the firmware.
func (m *FakeMachine) applySimulatedGcodeLocked(line string) {
	line = strings.TrimSpace(protocol.Unescape(line))
	if line == "" {
		return
	}
	if strings.HasPrefix(strings.ToUpper(line), "$J") {
		words := parseFakeGcodeWords(line)
		values, has := fakeWordValues(words, 1)
		delta := fakeAxisValues(values, has)
		scale := values['F']
		if !has['F'] || scale <= 0 {
			scale = 1
		}
		m.applyRelativeMoveLocked(delta, scale*fakeSelectedMachineMax(delta))
		return
	}

	words := parseFakeGcodeWords(stripFakeGcodeComments(line))
	if len(words) == 0 {
		return
	}
	hasG10 := false
	hasG53 := false
	lineMotionMode := -1
	var lineAbsolute *bool
	unit := m.unit
	if unit == 0 {
		unit = 1
	}
	for _, w := range words {
		if w.letter != 'G' {
			continue
		}
		code, subcode := splitFakeGCode(w.value)
		switch code {
		case 0:
			if subcode == 0 {
				lineMotionMode = fakeMotionRapid
			}
		case 1:
			if subcode == 0 {
				lineMotionMode = fakeMotionFeed
			}
		case 10:
			hasG10 = true
		case 20:
			unit = 25.4
		case 21:
			unit = 1
		case 53:
			hasG53 = true
		case 90:
			if subcode == 0 {
				v := true
				lineAbsolute = &v
			}
		case 91:
			if subcode == 0 {
				v := false
				lineAbsolute = &v
			}
		}
	}

	values, has := fakeWordValues(words, unit)
	m.unit = unit
	if lineAbsolute != nil {
		m.absolute = *lineAbsolute
	}
	if lineMotionMode >= 0 {
		m.motionMode = lineMotionMode
	}
	if has['F'] && values['F'] > 0 {
		m.feedMMMin = values['F']
	}

	if hasG10 && fakeNear(values['L'], 20) && fakeNear(values['P'], 0) {
		m.advanceMotionLocked(time.Now())
		m.applyWorkPositionLocked(fakeAxisValues(values, has))
		return
	}
	axes := fakeAxisValues(values, has)
	if len(axes) == 0 {
		return
	}
	feedMMMin := 0.0
	if m.motionMode == fakeMotionFeed {
		feedMMMin = m.feedMMMin
	}
	if hasG53 || m.absolute {
		m.applyAbsoluteMoveLocked(axes, feedMMMin)
		return
	}
	m.applyRelativeMoveLocked(axes, feedMMMin)
}

func (m *FakeMachine) applyRelativeMoveLocked(delta map[byte]float64, feedMMMin float64) {
	if len(delta) == 0 {
		return
	}
	now := time.Now()
	m.advanceMotionLocked(now)
	bracketed, state, fields, ok := parseFakeStatus(m.status)
	if !ok {
		return
	}
	mi := findFakeStatusField(fields, "MPos")
	if mi < 0 {
		return
	}
	mpos, ok := parseFakeAxisList(fields[mi].value)
	if !ok {
		return
	}
	wi := findFakeStatusField(fields, "WPos")
	wpos, haveWPos := []float64(nil), false
	if wi >= 0 {
		if vals, ok := parseFakeAxisList(fields[wi].value); ok {
			wpos, haveWPos = vals, true
		}
	}
	start := now
	if last := m.lastMotionSegmentLocked(); last != nil && last.end.After(now) {
		start = last.end
		mpos = append([]float64(nil), last.toM...)
		if haveWPos {
			wpos = append([]float64(nil), last.toW...)
		}
	}

	fromM := append([]float64(nil), mpos...)
	fromW := []float64(nil)
	if haveWPos {
		fromW = append([]float64(nil), wpos...)
	}
	for axis, d := range delta {
		idx, ok := fakeAxisIndex[axis]
		if !ok || !fakeFinite(d) {
			continue
		}
		mpos = ensureFakeAxisLen(mpos, idx+1)
		mpos[idx] += d
		if haveWPos {
			wpos = ensureFakeAxisLen(wpos, idx+1)
			wpos[idx] += d
		}
	}
	m.appendMotionLocked(bracketed, state, fields, start, fromM, mpos, fromW, wpos, fakeMoveDuration(delta, feedMMMin))
}

func (m *FakeMachine) applyAbsoluteMoveLocked(targets map[byte]float64, feedMMMin float64) {
	if len(targets) == 0 {
		return
	}
	now := time.Now()
	m.advanceMotionLocked(now)
	bracketed, state, fields, ok := parseFakeStatus(m.status)
	if !ok {
		return
	}
	mi := findFakeStatusField(fields, "MPos")
	if mi < 0 {
		return
	}
	mpos, ok := parseFakeAxisList(fields[mi].value)
	if !ok {
		return
	}
	wi := findFakeStatusField(fields, "WPos")
	wpos, haveWPos := []float64(nil), false
	if wi >= 0 {
		if vals, ok := parseFakeAxisList(fields[wi].value); ok {
			wpos, haveWPos = vals, true
		}
	}
	start := now
	if last := m.lastMotionSegmentLocked(); last != nil && last.end.After(now) {
		start = last.end
		mpos = append([]float64(nil), last.toM...)
		if haveWPos {
			wpos = append([]float64(nil), last.toW...)
		}
	}

	fromM := append([]float64(nil), mpos...)
	fromW := []float64(nil)
	if haveWPos {
		fromW = append([]float64(nil), wpos...)
	}
	delta := map[byte]float64{}
	for axis, target := range targets {
		idx, ok := fakeAxisIndex[axis]
		if !ok || !fakeFinite(target) {
			continue
		}
		mpos = ensureFakeAxisLen(mpos, idx+1)
		d := target - mpos[idx]
		mpos[idx] = target
		delta[axis] = d
		if haveWPos {
			wpos = ensureFakeAxisLen(wpos, idx+1)
			wpos[idx] += d
		}
	}
	m.appendMotionLocked(bracketed, state, fields, start, fromM, mpos, fromW, wpos, fakeMoveDuration(delta, feedMMMin))
}

func (m *FakeMachine) applyWorkPositionLocked(targets map[byte]float64) {
	if len(targets) == 0 {
		return
	}
	bracketed, state, fields, ok := parseFakeStatus(m.status)
	if !ok {
		return
	}
	wi := findFakeStatusField(fields, "WPos")
	wpos := []float64(nil)
	if wi >= 0 {
		vals, ok := parseFakeAxisList(fields[wi].value)
		if !ok {
			return
		}
		wpos = vals
	} else {
		mi := findFakeStatusField(fields, "MPos")
		if mi < 0 {
			return
		}
		vals, ok := parseFakeAxisList(fields[mi].value)
		if !ok {
			return
		}
		wpos = append([]float64(nil), vals...)
		fields = append(fields, fakeStatusField{key: "WPos"})
		wi = len(fields) - 1
	}

	for axis, target := range targets {
		idx, ok := fakeAxisIndex[axis]
		if !ok || !fakeFinite(target) {
			continue
		}
		wpos = ensureFakeAxisLen(wpos, idx+1)
		wpos[idx] = target
	}
	fields[wi].value = formatFakeAxisList(wpos)
	m.status = formatFakeStatus(bracketed, state, fields)
}

func (m *FakeMachine) statusAtLocked(now time.Time) string {
	m.advanceMotionLocked(now)
	return m.status
}

func (m *FakeMachine) advanceMotionLocked(now time.Time) {
	if len(m.motion) == 0 {
		return
	}
	bracketed, state, fields, ok := parseFakeStatus(m.status)
	if !ok {
		m.motion = nil
		return
	}
	for len(m.motion) > 0 && !now.Before(m.motion[0].end) {
		seg := m.motion[0]
		applyFakeAxesToFields(&fields, seg.toM, seg.toW)
		m.motion = m.motion[1:]
	}
	if len(m.motion) == 0 {
		m.status = formatFakeStatus(bracketed, state, fields)
		return
	}
	seg := m.motion[0]
	mpos := append([]float64(nil), seg.fromM...)
	wpos := append([]float64(nil), seg.fromW...)
	if !now.Before(seg.start) && seg.end.After(seg.start) {
		t := now.Sub(seg.start).Seconds() / seg.end.Sub(seg.start).Seconds()
		mpos = interpolateFakeAxes(seg.fromM, seg.toM, t)
		if seg.toW != nil {
			wpos = interpolateFakeAxes(seg.fromW, seg.toW, t)
		}
	}
	applyFakeAxesToFields(&fields, mpos, wpos)
	m.status = formatFakeStatus(bracketed, state, fields)
}

func (m *FakeMachine) appendMotionLocked(bracketed bool, state string, fields []fakeStatusField, start time.Time, fromM, toM, fromW, toW []float64, dur time.Duration) {
	if dur <= 0 {
		applyFakeAxesToFields(&fields, toM, toW)
		m.status = formatFakeStatus(bracketed, state, fields)
		return
	}
	m.motion = append(m.motion, fakeMotionSegment{
		start: start,
		end:   start.Add(dur),
		fromM: append([]float64(nil), fromM...),
		toM:   append([]float64(nil), toM...),
		fromW: append([]float64(nil), fromW...),
		toW:   append([]float64(nil), toW...),
	})
	m.status = formatFakeStatus(bracketed, state, fields)
}

func (m *FakeMachine) lastMotionSegmentLocked() *fakeMotionSegment {
	if len(m.motion) == 0 {
		return nil
	}
	return &m.motion[len(m.motion)-1]
}

func applyFakeAxesToFields(fields *[]fakeStatusField, mpos, wpos []float64) {
	if len(mpos) > 0 {
		if mi := findFakeStatusField(*fields, "MPos"); mi >= 0 {
			(*fields)[mi].value = formatFakeAxisList(mpos)
		}
	}
	if len(wpos) > 0 {
		if wi := findFakeStatusField(*fields, "WPos"); wi >= 0 {
			(*fields)[wi].value = formatFakeAxisList(wpos)
		}
	}
}

func parseFakeStatus(raw string) (bool, string, []fakeStatusField, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, "", nil, false
	}
	bracketed := strings.HasPrefix(raw, "<") && strings.HasSuffix(raw, ">")
	body := strings.TrimPrefix(raw, "<")
	body = strings.TrimSuffix(body, ">")
	parts := strings.Split(body, "|")
	state := strings.TrimSpace(parts[0])
	if state == "" {
		return false, "", nil, false
	}
	fields := make([]fakeStatusField, 0, len(parts)-1)
	for _, part := range parts[1:] {
		key, value, ok := strings.Cut(strings.TrimSpace(part), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		fields = append(fields, fakeStatusField{key: key, value: strings.TrimSpace(value)})
	}
	return bracketed, state, fields, true
}

func formatFakeStatus(bracketed bool, state string, fields []fakeStatusField) string {
	var b strings.Builder
	b.WriteString(state)
	for _, f := range fields {
		b.WriteByte('|')
		b.WriteString(f.key)
		b.WriteByte(':')
		b.WriteString(f.value)
	}
	if !bracketed {
		return b.String()
	}
	return "<" + b.String() + ">"
}

func findFakeStatusField(fields []fakeStatusField, key string) int {
	for i, f := range fields {
		if strings.EqualFold(f.key, key) {
			return i
		}
	}
	return -1
}

func parseFakeAxisList(s string) ([]float64, bool) {
	parts := strings.Split(s, ",")
	out := make([]float64, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.ParseFloat(part, 64)
		if err != nil || !fakeFinite(v) {
			return nil, false
		}
		out = append(out, v)
	}
	return out, len(out) > 0
}

func formatFakeAxisList(vals []float64) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		if math.Abs(v) < 0.00005 {
			v = 0
		}
		parts[i] = strconv.FormatFloat(v, 'f', 4, 64)
	}
	return strings.Join(parts, ",")
}

func ensureFakeAxisLen(vals []float64, n int) []float64 {
	if len(vals) >= n {
		return vals
	}
	out := make([]float64, n)
	copy(out, vals)
	return out
}

func interpolateFakeAxes(from, to []float64, t float64) []float64 {
	if t < 0 {
		t = 0
	} else if t > 1 {
		t = 1
	}
	n := len(from)
	if len(to) > n {
		n = len(to)
	}
	out := ensureFakeAxisLen(append([]float64(nil), from...), n)
	for i, target := range to {
		out[i] = out[i] + (target-out[i])*t
	}
	return out
}

func stripFakeGcodeComments(line string) string {
	var b strings.Builder
	inParen := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inParen {
			if c == ')' {
				inParen = false
			}
			continue
		}
		switch c {
		case '(':
			inParen = true
		case ';':
			return b.String()
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func parseFakeGcodeWords(line string) []fakeGcodeWord {
	var out []fakeGcodeWord
	for i := 0; i < len(line); {
		c := line[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c < 'A' || c > 'Z' {
			i++
			continue
		}
		i++
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		start := i
		if i < len(line) && (line[i] == '+' || line[i] == '-') {
			i++
		}
		digits := false
		exponent := false
		for i < len(line) {
			ch := line[i]
			if ch >= '0' && ch <= '9' {
				digits = true
				i++
				continue
			}
			if ch == '.' {
				i++
				continue
			}
			if (ch == 'e' || ch == 'E') && digits && !exponent {
				exponent = true
				i++
				if i < len(line) && (line[i] == '+' || line[i] == '-') {
					i++
				}
				continue
			}
			break
		}
		if !digits {
			continue
		}
		v, err := strconv.ParseFloat(line[start:i], 64)
		if err != nil || !fakeFinite(v) {
			continue
		}
		out = append(out, fakeGcodeWord{letter: c, value: v})
	}
	return out
}

func splitFakeGCode(v float64) (int, int) {
	code := int(math.Trunc(v))
	subcode := int(math.Round((v - float64(code)) * 10))
	return code, subcode
}

func fakeWordValues(words []fakeGcodeWord, unit float64) (map[byte]float64, map[byte]bool) {
	values := map[byte]float64{}
	has := map[byte]bool{}
	if unit == 0 {
		unit = 1
	}
	for _, w := range words {
		switch w.letter {
		case 'X', 'Y', 'Z', 'A', 'B', 'C':
			values[w.letter] = w.value * unit
			has[w.letter] = true
		case 'F':
			values[w.letter] = w.value * unit
			has[w.letter] = true
		case 'L', 'P':
			values[w.letter] = w.value
			has[w.letter] = true
		}
	}
	return values, has
}

func fakeAxisTargets(words []fakeGcodeWord, unit float64) map[byte]float64 {
	values, has := fakeWordValues(words, unit)
	return fakeAxisValues(values, has)
}

func fakeAxisValues(values map[byte]float64, has map[byte]bool) map[byte]float64 {
	out := map[byte]float64{}
	for _, axis := range fakeAxisLetters {
		if has[axis] && fakeFinite(values[axis]) {
			out[axis] = values[axis]
		}
	}
	return out
}

func fakeFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func fakeNear(v, target float64) bool {
	return math.Abs(v-target) < 0.000001
}

func fakeMoveDuration(delta map[byte]float64, feedMMMin float64) time.Duration {
	dist := fakeMoveDistance(delta)
	if dist == 0 {
		return 0
	}
	if feedMMMin <= 0 || !fakeFinite(feedMMMin) {
		feedMMMin = fakeSelectedMachineMax(delta)
	}
	if feedMMMin <= 0 {
		return 0
	}
	return time.Duration((dist / feedMMMin) * float64(time.Minute))
}

func fakeMoveDistance(delta map[byte]float64) float64 {
	sum := 0.0
	for _, d := range delta {
		sum += d * d
	}
	return math.Sqrt(sum)
}

func fakeSelectedMachineMax(delta map[byte]float64) float64 {
	maxRate := 0.0
	for axis, d := range delta {
		if d == 0 {
			continue
		}
		rate := fakeFirmwareMaxXYMMMin
		if axis == 'Z' {
			rate = fakeFirmwareMaxZMMMin
		}
		if maxRate == 0 || rate < maxRate {
			maxRate = rate
		}
	}
	return maxRate
}
