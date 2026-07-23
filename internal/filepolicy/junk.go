// Package filepolicy contains path rules shared by every file surface.
package filepolicy

import (
	"path"
	"path/filepath"
	"strings"
)

// IsJunk reports whether a path's final component is OS-generated metadata
// that must never enter the catalog, durable queue, or machine.
func IsJunk(name string) bool {
	base := path.Base(strings.Trim(path.Clean("/"+name), "/"))
	switch base {
	case ".DS_Store", ".localized", "Thumbs.db", "desktop.ini", ".fseventsd",
		".Spotlight-V100", ".Trashes", ".TemporaryItems", ".apdisk":
		return true
	}
	return strings.HasPrefix(base, "._")
}

// IsGcodePath reports whether name is a machine-absolute child of /sd/gcodes.
// The root itself is intentionally excluded: queue operations must never target
// the filesystem root exposed by the proxy.
func IsGcodePath(name string) bool {
	name = strings.ReplaceAll(name, "\\", "/")
	clean := path.Clean(name)
	return clean == name && strings.HasPrefix(clean, "/sd/gcodes/") && !IsJunk(clean)
}

// IsWithinDir reports whether name resolves to a file below dir. It rejects the
// directory itself and traversal through relative or absolute paths.
func IsWithinDir(dir, name string) bool {
	if dir == "" || name == "" {
		return false
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	target, err := filepath.Abs(name)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
