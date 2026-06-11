package protocol

import (
	"strconv"
	"strings"
	"time"
)

// DirEntry is one row of an `ls -e -s` listing.
type DirEntry struct {
	Name  string
	IsDir bool
	Size  int64
	MTime time.Time // zero if unparseable
}

// ParseLsLine parses one detailed listing row of the form
// "name[/] size YYYYMMDDHHMMSS". It mirrors the controller's fillRemoteDir:
// whitespace-split into exactly three fields, 0x01 decoded back to space, a
// trailing slash marks a directory, and lines starting with '<' (a status
// report that leaked into the buffer) or '.' (hidden) are rejected. ok is false
// for any line that isn't a valid entry.
func ParseLsLine(line string) (DirEntry, bool) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" || line[0] == '<' {
		return DirEntry{}, false
	}
	f := strings.Fields(line)
	if len(f) != 3 {
		return DirEntry{}, false
	}
	name, sizeStr, dateStr := f[0], f[1], f[2]
	if strings.HasPrefix(name, ".") {
		return DirEntry{}, false
	}
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return DirEntry{}, false
	}
	if _, err := strconv.Atoi(dateStr); err != nil {
		return DirEntry{}, false
	}

	name = strings.ReplaceAll(name, "\x01", " ")
	isDir := strings.HasSuffix(name, "/")
	if isDir {
		name = strings.TrimSuffix(name, "/")
	}

	var mtime time.Time
	// Firmware emits local wall-clock time; parse without a zone.
	if t, err := time.ParseInLocation("20060102150405", dateStr, time.Local); err == nil {
		mtime = t
	}

	return DirEntry{Name: name, IsDir: isDir, Size: size, MTime: mtime}, true
}

// ParseListing parses a full listing payload (possibly spanning multiple
// LOAD_INFO frames concatenated) into entries, skipping non-entry lines.
func ParseListing(payload string) []DirEntry {
	var out []DirEntry
	for _, line := range strings.Split(payload, "\n") {
		if e, ok := ParseLsLine(line); ok {
			out = append(out, e)
		}
	}
	return out
}

// ParseFtype extracts the file type from an "ftype = <type>" info line (the
// firmware may prefix it with "#Info: " or similar). Returns false if the line
// isn't an ftype report.
func ParseFtype(line string) (string, bool) {
	i := strings.Index(line, "ftype")
	if i < 0 {
		return "", false
	}
	rest := line[i+len("ftype"):]
	eq := strings.IndexByte(rest, '=')
	if eq < 0 {
		return "", false
	}
	val := strings.TrimSpace(rest[eq+1:])
	// Take the first token (stop at whitespace/newline).
	if sp := strings.IndexAny(val, " \t\r\n"); sp >= 0 {
		val = val[:sp]
	}
	if val == "" {
		return "", false
	}
	return val, true
}

// ParseMd5Response extracts the hex digest from a `md5sum` reply of the form
// "<hexdigest> <filename>". Returns false if it doesn't look like one (e.g. the
// firmware's "File not found:" message).
func ParseMd5Response(payload string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(payload))
	if len(fields) < 1 {
		return "", false
	}
	digest := fields[0]
	if len(digest) != 32 {
		return "", false
	}
	for _, c := range digest {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return "", false
		}
	}
	return strings.ToLower(digest), true
}
