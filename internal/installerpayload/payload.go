// Package installerpayload appends and reads zip payloads stored as an overlay
// at the end of an executable.
package installerpayload

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

const Magic = "CNC_PROXY_INSTALLER_PAYLOAD_V1"

var ErrNoPayload = errors.New("installer payload not found")

func Append(stubPath, payloadPath, outPath string) error {
	stub, err := os.Open(stubPath)
	if err != nil {
		return err
	}
	defer stub.Close()
	payload, err := os.Open(payloadPath)
	if err != nil {
		return err
	}
	defer payload.Close()
	info, err := payload.Stat()
	if err != nil {
		return err
	}
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	outName := out.Name()
	if _, err := io.Copy(out, stub); err != nil {
		out.Close()
		os.Remove(outName)
		return err
	}
	if _, err := io.Copy(out, payload); err != nil {
		out.Close()
		os.Remove(outName)
		return err
	}
	var size [8]byte
	binary.LittleEndian.PutUint64(size[:], uint64(info.Size()))
	if _, err := out.Write(size[:]); err != nil {
		out.Close()
		os.Remove(outName)
		return err
	}
	if _, err := out.Write([]byte(Magic)); err != nil {
		out.Close()
		os.Remove(outName)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(outName)
		return err
	}
	return nil
}

func Read(exePath string) ([]byte, error) {
	f, err := os.Open(exePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	trailerLen := int64(8 + len(Magic))
	if info.Size() < trailerLen {
		return nil, ErrNoPayload
	}
	trailer := make([]byte, trailerLen)
	if _, err := f.ReadAt(trailer, info.Size()-trailerLen); err != nil {
		return nil, err
	}
	if string(trailer[8:]) != Magic {
		return nil, ErrNoPayload
	}
	size := int64(binary.LittleEndian.Uint64(trailer[:8]))
	if size <= 0 || size > info.Size()-trailerLen {
		return nil, fmt.Errorf("invalid installer payload size %d", size)
	}
	payload := make([]byte, size)
	if _, err := f.ReadAt(payload, info.Size()-trailerLen-size); err != nil {
		return nil, err
	}
	return payload, nil
}
