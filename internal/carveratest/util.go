package carveratest

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/uwin/cnc-proxy/internal/quicklz"
)

// decompressLZ decompresses a controller-format .lz container (the byte stream
// produced by quicklz.CompressStream), mirroring the firmware's decompress().
func decompressLZ(data []byte) ([]byte, error) {
	var out bytes.Buffer
	if err := quicklz.DecompressStream(&out, data); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// compressLZ produces a controller-format .lz container for data.
func compressLZ(data []byte) []byte {
	var out bytes.Buffer
	_, _ = quicklz.CompressStream(&out, bytes.NewReader(data))
	return out.Bytes()
}

func md5hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func itoa(n int) string { return strconv.Itoa(n) }

// lastSegment returns the final path component, decoding 0x01-escaped spaces.
func lastSegment(p string) string {
	p = strings.ReplaceAll(p, "\x01", " ")
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// secondField returns the decoded second whitespace-separated token of a
// command line, e.g. the path in "rm <path> -e".
func secondField(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ""
	}
	return strings.ReplaceAll(fields[1], "\x01", " ")
}

// md5Target returns the path argument of a "md5sum <path> -e" command.
func md5Target(line string) string {
	return secondField(line)
}

// lsDir extracts and decodes the directory argument of "ls -e -s <dir>". It is
// the last whitespace-separated token that isn't a flag.
func lsDir(line string) string {
	fields := strings.Fields(line)
	dir := ""
	for _, f := range fields[1:] { // skip "ls"
		if strings.HasPrefix(f, "-") {
			continue
		}
		dir = f
	}
	return strings.ReplaceAll(dir, "\x01", " ")
}

// parentOf returns the parent directory of a machine-absolute path, without a
// trailing slash. parentOf("/sd/gcodes/a.nc") == "/sd/gcodes".
func parentOf(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return "/"
}
