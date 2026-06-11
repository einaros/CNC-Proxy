package protocol

import "strings"

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

// Single-character control frames (CTRL_SINGLE).
func QueryStatus() []byte { return Encode(CmdCtrlSingle, []byte{'?'}) }
func Halt() []byte        { return Encode(CmdCtrlSingle, []byte{0x18}) } // Ctrl-X
func FeedHold() []byte    { return Encode(CmdCtrlSingle, []byte{'!'}) }
func Resume() []byte      { return Encode(CmdCtrlSingle, []byte{'~'}) }
