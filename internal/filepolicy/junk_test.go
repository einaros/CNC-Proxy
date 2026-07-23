package filepolicy

import (
	"path/filepath"
	"testing"
)

func TestIsJunk(t *testing.T) {
	for _, name := range []string{"._part.nc", "/sub/.DS_Store", "Thumbs.db", "/.Trashes"} {
		if !IsJunk(name) {
			t.Errorf("IsJunk(%q) = false", name)
		}
	}
	for _, name := range []string{"part.nc", "/sub/.fixture.nc", "DS_Store.nc"} {
		if IsJunk(name) {
			t.Errorf("IsJunk(%q) = true", name)
		}
	}
}

func TestIsGcodePath(t *testing.T) {
	for _, name := range []string{"/sd/gcodes/part.nc", "/sd/gcodes/sub/part.nc"} {
		if !IsGcodePath(name) {
			t.Errorf("IsGcodePath(%q) = false", name)
		}
	}
	for _, name := range []string{"/sd/gcodes", "/sd/gcodes/../config", "/sd/gcodes/._part.nc", "part.nc", "/etc/passwd"} {
		if IsGcodePath(name) {
			t.Errorf("IsGcodePath(%q) = true", name)
		}
	}
}

func TestIsWithinDir(t *testing.T) {
	dir := t.TempDir()
	if !IsWithinDir(dir, filepath.Join(dir, "nested", "file")) {
		t.Fatal("nested cache file was rejected")
	}
	if IsWithinDir(dir, dir) || IsWithinDir(dir, filepath.Join(dir, "..", "outside")) {
		t.Fatal("cache directory or outside path was accepted")
	}
}
