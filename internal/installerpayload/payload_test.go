package installerpayload

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAppendAndRead(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub.exe")
	payload := filepath.Join(dir, "payload.zip")
	out := filepath.Join(dir, "installer.exe")
	if err := os.WriteFile(stub, []byte("MZstub"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := []byte("zipdata")
	if err := os.WriteFile(payload, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Append(stub, payload, out); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := Read(out)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("payload = %q, want %q", got, want)
	}
}

func TestReadNoPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.exe")
	if err := os.WriteFile(path, []byte("MZplain"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Read(path)
	if !errors.Is(err, ErrNoPayload) {
		t.Fatalf("Read err = %v, want ErrNoPayload", err)
	}
}
