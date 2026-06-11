package protocol

// CRC16-CCITT (XMODEM variant): polynomial 0x1021, initial value 0x0000,
// no reflection, no final XOR. This must match the firmware (WifiProvider.cpp
// crc_table) and controller (XMODEM.py crctable) byte-for-byte, since the
// controller validates the CRC on responses we send it. The table is generated
// once at init from the polynomial and verified against known entries in tests.
var crcTable = func() [256]uint16 {
	var t [256]uint16
	for i := 0; i < 256; i++ {
		crc := uint16(i) << 8
		for bit := 0; bit < 8; bit++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc <<= 1
			}
		}
		t[i] = crc
	}
	return t
}()

// CRC16 computes the CCITT checksum over data. The controller/firmware compute
// the CRC over LEN(2) + CMD(1) + DATA(N) — i.e. the body excluding header,
// trailing CRC, and footer. Callers pass exactly those bytes.
func CRC16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		tmp := byte((crc >> 8)) ^ b
		crc = (crc << 8) ^ crcTable[tmp]
	}
	return crc
}
