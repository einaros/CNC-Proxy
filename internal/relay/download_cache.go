package relay

import (
	"encoding/binary"
	"io"
	"net"

	"github.com/uwin/cnc-proxy/internal/machinetransport"
	"github.com/uwin/cnc-proxy/internal/protocol"
)

type controllerDownload struct {
	reader     io.ReaderAt
	closer     io.Closer
	size       int64
	md5hex     string
	packetSize int64
	lastFrame  []byte
}

func newControllerDownload(reader io.ReaderAt, closer io.Closer, size int64, md5hex string) *controllerDownload {
	if size < 0 {
		size = 0
	}
	return &controllerDownload{
		reader:     reader,
		closer:     closer,
		size:       size,
		md5hex:     md5hex,
		packetSize: machinetransport.TCPPacketSize,
	}
}

func (d *controllerDownload) Start(c net.Conn) error {
	return d.write(c, protocol.CmdFileMD5, []byte(d.md5hex))
}

func (d *controllerDownload) Handle(c net.Conn, f protocol.Frame) (done bool, handled bool) {
	switch f.Cmd {
	case protocol.CmdFileRetry:
		if len(d.lastFrame) > 0 {
			_, _ = c.Write(d.lastFrame)
		}
		return false, true
	case protocol.CmdFileView:
		_ = d.writeView(c)
		return false, true
	case protocol.CmdFileData:
		if len(f.Data) < 4 {
			return false, true
		}
		seq := binary.BigEndian.Uint32(f.Data[:4])
		_ = d.writeData(c, seq)
		return false, true
	case protocol.CmdFileEnd:
		_ = d.write(c, protocol.CmdFileEnd, nil)
		return true, true
	case protocol.CmdFileCancel:
		return true, true
	default:
		return false, false
	}
}

func (d *controllerDownload) Close() {
	if d.closer != nil {
		_ = d.closer.Close()
	}
}

func (d *controllerDownload) writeView(c net.Conn) error {
	total := d.totalPackets()
	payload := make([]byte, 6)
	binary.BigEndian.PutUint32(payload[:4], total)
	binary.BigEndian.PutUint16(payload[4:6], uint16(d.packetSize))
	return d.write(c, protocol.CmdFileView, payload)
}

func (d *controllerDownload) writeData(c net.Conn, seq uint32) error {
	if seq == 0 {
		seq = 1
	}
	off := int64(seq-1) * d.packetSize
	if off > d.size {
		off = d.size
	}
	n := d.packetSize
	if off+n > d.size {
		n = d.size - off
	}
	if n < 0 {
		n = 0
	}
	payload := make([]byte, 4+int(n))
	binary.BigEndian.PutUint32(payload[:4], seq)
	if n > 0 {
		if _, err := d.reader.ReadAt(payload[4:], off); err != nil && err != io.EOF {
			return err
		}
	}
	return d.write(c, protocol.CmdFileData, payload)
}

func (d *controllerDownload) totalPackets() uint32 {
	if d.packetSize <= 0 {
		return 1
	}
	packets := (d.size + d.packetSize - 1) / d.packetSize
	if packets < 1 {
		return 1
	}
	if packets > int64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(packets)
}

func (d *controllerDownload) write(c net.Conn, cmd byte, payload []byte) error {
	frame := protocol.Encode(cmd, payload)
	d.lastFrame = frame
	_, err := c.Write(frame)
	return err
}
