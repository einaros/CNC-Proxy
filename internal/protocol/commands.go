package protocol

import (
	"strconv"
	"strings"
)

// Wire-level character escaping. The controller replaces these characters in
// command arguments before sending, and the firmware decodes them (libs/
// utils.cpp). We must apply the identical mapping so paths with spaces or shell
// metacharacters survive the round trip.
var escaper = strings.NewReplacer(
	" ", "\x01",
	"?", "\x02",
	"*", "\x03",
	"!", "\x04",
	"~", "\x05",
)

// Escape applies the controller's argument escaping. Backslashes in paths are
// normalized to forward slashes first, matching the controller's behavior.
func Escape(s string) string {
	s = strings.ReplaceAll(s, "\\", "/")
	return escaper.Replace(s)
}

var unescaper = strings.NewReplacer(
	"\x01", " ",
	"\x02", "?",
	"\x03", "*",
	"\x04", "!",
	"\x05", "~",
)

// Unescape reverses the wire escaping, for displaying observed command lines.
func Unescape(s string) string {
	return unescaper.Replace(s)
}

// The file commands are sent as CTRL_MULTI text frames. The "-e" flag tells the
// firmware to packetize its response (LOAD_INFO/LOAD_FINISH/LOAD_ERROR) rather
// than stream raw text, which is what we parse. ls additionally uses -s for the
// detailed "name size YYYYMMDDHHMMSS" rows.

// LsCommand builds an `ls -e -s <dir>` frame for a detailed directory listing.
func LsCommand(dir string) []byte {
	return Encode(CmdCtrlMulti, []byte("ls -e -s "+Escape(dir)+"\n"))
}

// RmCommand builds an `rm <path> -e` frame.
func RmCommand(path string) []byte {
	return Encode(CmdCtrlMulti, []byte("rm "+Escape(path)+" -e\n"))
}

// MvCommand builds an `mv <from> <to> -e` frame.
func MvCommand(from, to string) []byte {
	return Encode(CmdCtrlMulti, []byte("mv "+Escape(from)+" "+Escape(to)+" -e\n"))
}

// MkdirCommand builds a `mkdir <dir> -e` frame.
func MkdirCommand(dir string) []byte {
	return Encode(CmdCtrlMulti, []byte("mkdir "+Escape(dir)+" -e\n"))
}

// Md5Command builds a `md5sum <path>` frame. Unlike ls/rm/mv/mkdir, the
// firmware's md5sum command does NOT parse flags — it treats the whole argument
// as the filename — and replies with a plain NORMAL_INFO line ("<hex> <path>"
// or "File not found: <path>"), not a packetized LOAD_* response. So no "-e".
func Md5Command(path string) []byte {
	return Encode(CmdCtrlMulti, []byte("md5sum "+Escape(path)+"\n"))
}

// FtypeCommand builds an `ftype` frame. The firmware replies with
// "ftype = <type>" (e.g. "lz") indicating the upload file type it supports.
func FtypeCommand() []byte {
	return Encode(CmdCtrlMulti, []byte("ftype\n"))
}

// UploadCommand builds the `upload <path>` frame that begins a file transfer.
// Unlike the management commands it is sent as a FILE_START frame and takes no
// -e flag; the transfer then proceeds via the MD5/VIEW/DATA/END handshake.
func UploadCommand(path string) []byte {
	return Encode(CmdFileStart, []byte("upload "+Escape(path)+"\n"))
}

// DownloadCommand builds the `download <path>` frame that begins a file
// transfer from the machine. It is also a FILE_START frame; the machine then
// drives the MD5/VIEW/DATA/END handshake as the sender.
func DownloadCommand(path string) []byte {
	return Encode(CmdFileStart, []byte("download "+Escape(path)+"\n"))
}

// PlayLine builds the controller-compatible console command that starts a file
// from SD. The controller sends this as CTRL_MULTI text, with path escaping.
func PlayLine(path string) string {
	return "play " + Escape(path) + "\n"
}

// SetCurrentToolLine builds the controller's "set current tool ID" command.
func SetCurrentToolLine(toolID int) string {
	return "M493.2T" + strconv.Itoa(toolID) + "\n"
}

// ChangeToolLine builds the controller's "change to tool and auto-calibrate"
// command.
func ChangeToolLine(toolID int) string {
	return "M6T" + strconv.Itoa(toolID) + "\n"
}

// ContinueToolChangeLine builds the controller's manual tool-change continue
// command, matching Controller.change() in the official controller.
func ContinueToolChangeLine() string {
	return "M490.2\n"
}

// CalibrateCurrentToolLine builds the controller's current-tool calibration
// command.
func CalibrateCurrentToolLine() string {
	return "M491\n"
}

// AutoZProbeLine builds the controller-compatible auto Z probe command. The
// official controller sends M495 with the work path X/Y origin and O/F probe
// offsets; zero offsets probe at the supplied work XY point.
func AutoZProbeLine(workX, workY, offsetX, offsetY float64) string {
	return "M495 X" + controllerFloat(workX) + "Y" + controllerFloat(workY) +
		"O" + controllerFloat(offsetX) + "F" + controllerFloat(offsetY) + "\n"
}

// Probe3DLine builds the controller-compatible wired 3D-probe command. The
// official controller maps outside corners to M480.1-.4, inside corners to
// M480.5-.8, bore/pocket centering to M480.9, and boss/block centering to
// M480.10.
func Probe3DLine(subcode int, xOffset, yOffset, zOffset, diameter float64) string {
	return "M480." + strconv.Itoa(subcode) +
		" X" + controllerFloat(xOffset) +
		" Y" + controllerFloat(yOffset) +
		" Z" + controllerFloat(zOffset) +
		" D" + controllerFloat(diameter) + "\n"
}

func controllerFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// Single-character control frames (CTRL_SINGLE).
func QueryStatus() []byte { return Encode(CmdCtrlSingle, []byte{'?'}) }
func Halt() []byte        { return Encode(CmdCtrlSingle, []byte{0x18}) } // Ctrl-X
func FeedHold() []byte    { return Encode(CmdCtrlSingle, []byte{'!'}) }
func Resume() []byte      { return Encode(CmdCtrlSingle, []byte{'~'}) }

// Realtime control characters (CTRL_SINGLE payloads). The firmware acts on
// these out-of-band, independent of the gcode stream.
const (
	CtrlFeedHold = '!'
	CtrlResume   = '~'
	CtrlHalt     = 0x18 // Ctrl-X emergency stop
)

// ControlLabel returns a human-readable label for a realtime control character
// and whether it is a recognized control. It is the single source of truth for
// these labels, shared by the relay's sniffing log and the API's injection log.
func ControlLabel(c byte) (string, bool) {
	switch c {
	case CtrlFeedHold:
		return "! (feed hold)", true
	case CtrlResume:
		return "~ (resume)", true
	case CtrlHalt:
		return "^X (halt)", true
	default:
		return "", false
	}
}

// Response classifies how the Carvera firmware replies to a CTRL_MULTI line
// over the WiFi protocol. This was established empirically against real hardware
// (firmware 1.0.5), not from the vendored source — the deployed binary does NOT
// emit the per-line "ok" the source's GcodeDispatch suggests, and a whole class
// of state-setting commands reply with nothing at all.
//
// The load-bearing property, verified on hardware: the firmware replies
// PROMPTLY or NEVER — there is no "silent for a while, then acks". Queries are
// answered on the idle loop within milliseconds; normal motion/modal/dwell
// commands produce no terminating frame ever. Probe motion is an explicit
// exception because the firmware reports the contacted point. This is why a
// fire-and-forget command must not wait for an "ok" (it would block until
// timeout, holding the arbiter's opMu and stalling everything behind it — the
// original "second move hangs" bug), and why a reply-expected read can safely
// bound itself with a short settle window instead of a long keepalive loop.
type Response int

const (
	// FireAndForget: the firmware sends no terminating reply. The command is
	// written and we only briefly drain for an immediate error/alarm line.
	// Covers normal motion (G0–G3), modal/state sets (G90/G91/G21/G94, M5/M9,
	// …), blocking waits (M400, G4 dwell), and anything unrecognized. Probe
	// motion (G30/G38.x) is an explicit reply-producing exception below.
	FireAndForget Response = iota
	// ReplyExpected: the firmware answers promptly, either "ok"/"ok <payload>"
	// (e.g. M114) or one-or-more output lines with no "ok" (e.g. version, $G).
	// The reader collects output until quiescence.
	ReplyExpected
)

// replyQueries are the gcode/console commands the firmware answers promptly and
// that only READ state — safe to inject regardless of machine state (even while
// a controller streams a program) because they neither move the machine nor
// change modal/persistent state. Verified on hardware to produce a reply.
//
// Matched against the lowercased first token, dotted subcodes (e.g. "m114.1")
// reduced to their base ("m114").
var replyQueries = map[string]bool{
	"m114": true, "m115": true, "m119": true, "m105": true,
	"m503":    true, // display live settings — pure read (M500 saves; not here)
	"version": true, "model": true, "ftype": true,
	"time": true, "echo": true, "mem": true, "diagnose": true,
	"progress": true,
}

// dollarQueries are the read-only grbl '$' commands. '$H' (home) and '$X'
// (unlock) are intentionally absent: they change machine state.
var dollarQueries = map[string]bool{
	"$": true, "$$": true, "$#": true, "$g": true, "$i": true, "$n": true,
}

// dualNatureReporters REPORT state when given with no setting argument (and
// then the firmware replies), but MUTATE state when given an argument (and then
// it is silent). Examples verified/derived: M220 (feed override), M221 (flow),
// M211 (soft-endstop enable + report), M204 (acceleration), M203 (max feed),
// M206 (home offset), M301 (PID). With no arg they behave like a query (reply,
// safe anytime); with an arg they behave like a mutating set (no reply, must be
// idle). settingLetters are the parameter letters whose presence means "set".
var dualNatureReporters = map[string]bool{
	"m220": true, "m221": true, "m211": true, "m204": true,
	"m203": true, "m206": true, "m301": true,
}

// normalizeLine lowercases, trims, and strips a leading "Nnnn" line number (the
// controller and gcode senders may prefix one; the firmware strips it too in
// GcodeDispatch before parsing). Returns the cleaned line.
func normalizeLine(line string) string {
	line = strings.TrimSpace(strings.ToLower(line))
	if len(line) > 1 && line[0] == 'n' && line[1] >= '0' && line[1] <= '9' {
		if i := strings.IndexAny(line, " \t"); i >= 0 {
			line = strings.TrimSpace(line[i+1:])
		}
	}
	return line
}

// firstToken returns the first whitespace-delimited token of a normalized line.
func firstToken(line string) string {
	tok := line
	if i := strings.IndexAny(tok, " \t"); i >= 0 {
		tok = tok[:i]
	}
	return tok
}

// hasSettingArg reports whether a dual-nature reporter line carries a setting
// argument (making it a mutating "set" rather than a bare "report"). It looks
// for any parameter word after the command token — i.e. a letter+value pair
// such as "s150", "x640", "p20". A bare "m220" has none; "m220 s150" does.
func hasSettingArg(line, tok string) bool {
	rest := strings.TrimSpace(strings.TrimPrefix(line, tok))
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c >= 'a' && c <= 'z' && i+1 < len(rest) {
			n := rest[i+1]
			if (n >= '0' && n <= '9') || n == '-' || n == '+' || n == '.' {
				return true
			}
		}
	}
	return false
}

// ClassifyGcode reports how the firmware will respond to a CTRL_MULTI line and
// whether the command must be idle-gated (run only on a fresh Idle machine).
// It is the single source of truth for both decisions:
//
//   - resp == ReplyExpected: caller should read the prompt reply (ok / payload /
//     output lines). resp == FireAndForget: caller writes and moves on.
//   - requiresIdle == false: safe to inject regardless of machine state (pure
//     reads). requiresIdle == true: motion/modal/SD-affecting; gate on Idle.
//
// The two axes are independent: a bare reporter (M220) is ReplyExpected and not
// idle-gated; a motion command is FireAndForget and idle-gated; a dual-nature
// SET (M220 S150) is FireAndForget and idle-gated.
func ClassifyGcode(line string) (resp Response, requiresIdle bool) {
	line = normalizeLine(line)
	if line == "" {
		return FireAndForget, true
	}
	tok := firstToken(line)

	if strings.HasPrefix(tok, "$") {
		if dollarQueries[tok] {
			return ReplyExpected, false
		}
		return FireAndForget, true // $H, $X, etc.
	}

	base := tok
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i] // reduce a dotted subcode (m114.1) to its base (m114)
	}

	if replyQueries[base] {
		return ReplyExpected, false
	}
	// Firmware probe commands report the contacted position as NORMAL_INFO
	// ([PRB:x,y,z:1] or Z:...), but they move the machine and must stay idle
	// gated.
	if base == "g30" || base == "g38" {
		return ReplyExpected, true
	}
	if dualNatureReporters[base] {
		if hasSettingArg(line, tok) {
			return FireAndForget, true // a "set": mutates, no reply
		}
		return ReplyExpected, false // a bare "report": replies, read-only
	}
	// Everything else — motion, modal sets, dwell, SD I/O, unknown — is silent
	// and state-affecting.
	return FireAndForget, true
}

// IsStatusQuery reports whether a console/gcode line is a read-only query safe
// to inject regardless of machine state. Retained as a thin wrapper over
// ClassifyGcode for callers that only care about the idle-gating axis.
func IsStatusQuery(line string) bool {
	_, requiresIdle := ClassifyGcode(line)
	return !requiresIdle
}
