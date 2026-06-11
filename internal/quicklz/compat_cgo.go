//go:build cgo_compat

// Cgo bridge to the actual firmware QuickLZ implementation, used only by the
// compatibility test (build tag cgo_compat). It compiles the vendored quicklz.c
// so we can prove the Go port interoperates byte-for-byte with the firmware.
//
// Run with:
//
//	CGO_ENABLED=1 go test -mod=mod -tags cgo_compat ./internal/quicklz/
package quicklz

/*
#cgo CFLAGS: -O2 -I${SRCDIR}/../../vendor/CarveraFirmware/src/modules/utils/player
#include <stdlib.h>
#include "quicklz.h"
#include "quicklz.c"

static size_t c_compress(const unsigned char *in, size_t n, unsigned char *out) {
    qlz_state_compress *st = calloc(1, sizeof(qlz_state_compress));
    size_t r = qlz_compress(in, (char*)out, n, st);
    free(st);
    return r;
}
static size_t c_decompress(const unsigned char *in, unsigned char *out) {
    qlz_state_decompress *st = calloc(1, sizeof(qlz_state_decompress));
    size_t r = qlz_decompress((const char*)in, out, st);
    free(st);
    return r;
}
*/
import "C"

import "unsafe"

// firmwareCompress compresses in using the firmware C implementation.
func firmwareCompress(in []byte) []byte {
	if len(in) == 0 {
		return nil
	}
	out := make([]byte, len(in)+512)
	n := C.c_compress((*C.uchar)(unsafe.Pointer(&in[0])), C.size_t(len(in)), (*C.uchar)(unsafe.Pointer(&out[0])))
	return out[:int(n)]
}

// firmwareDecompress decompresses in using the firmware C implementation. hint
// bounds the output buffer (>= original size).
func firmwareDecompress(in []byte, hint int) []byte {
	out := make([]byte, hint+512)
	n := C.c_decompress((*C.uchar)(unsafe.Pointer(&in[0])), (*C.uchar)(unsafe.Pointer(&out[0])))
	return out[:int(n)]
}
