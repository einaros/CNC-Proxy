// Package trayicon provides the small generated icon used by the native tray
// companion.
package trayicon

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Bytes returns image bytes suitable for fyne.io/systray on the requested OS.
// Windows wants ICO bytes; the other supported systray backends accept PNG.
func Bytes(goos string) ([]byte, error) {
	if goos == "windows" {
		return ico([]int{16, 32, 48}), nil
	}
	var b bytes.Buffer
	if err := png.Encode(&b, render(32)); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func render(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const samples = 4
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var pr, pg, pb, pa float64
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					nx := (float64(x) + (float64(sx)+0.5)/samples) / float64(size)
					ny := (float64(y) + (float64(sy)+0.5)/samples) / float64(size)
					c := sample(nx, ny)
					a := float64(c.A) / 255
					pr += float64(c.R) * a
					pg += float64(c.G) * a
					pb += float64(c.B) * a
					pa += a
				}
			}
			total := float64(samples * samples)
			if pa == 0 {
				continue
			}
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(math.Round(pr / pa)),
				G: uint8(math.Round(pg / pa)),
				B: uint8(math.Round(pb / pa)),
				A: uint8(math.Round(pa / total * 255)),
			})
		}
	}
	return img
}

func sample(x, y float64) color.NRGBA {
	dx, dy := x-0.5, y-0.5
	r := math.Hypot(dx, dy)
	if r > 0.485 {
		return color.NRGBA{}
	}
	if r > 0.43 {
		return color.NRGBA{R: 8, G: 48, B: 57, A: 255}
	}
	bg := color.NRGBA{
		R: uint8(20 - 5*y),
		G: uint8(108 - 34*y),
		B: uint8(126 - 28*y),
		A: 255,
	}

	cd := math.Hypot(x-0.46, y-0.50)
	cGap := x > 0.52 && math.Abs(y-0.50) < 0.15
	if cd > 0.205 && cd < 0.325 && !cGap {
		return color.NRGBA{R: 242, G: 249, B: 251, A: 255}
	}

	bit := distanceToSegment(x, y, 0.67, 0.23, 0.74, 0.72)
	if bit < 0.055 && y < 0.74 {
		if bit < 0.025 {
			return color.NRGBA{R: 255, G: 219, B: 99, A: 255}
		}
		return color.NRGBA{R: 242, G: 158, B: 32, A: 255}
	}
	if triangle(x, y, [2]float64{0.68, 0.70}, [2]float64{0.80, 0.70}, [2]float64{0.74, 0.84}) {
		return color.NRGBA{R: 242, G: 158, B: 32, A: 255}
	}
	return bg
}

func distanceToSegment(px, py, ax, ay, bx, by float64) float64 {
	vx, vy := bx-ax, by-ay
	wx, wy := px-ax, py-ay
	c := (wx*vx + wy*vy) / (vx*vx + vy*vy)
	if c < 0 {
		c = 0
	} else if c > 1 {
		c = 1
	}
	x, y := ax+c*vx, ay+c*vy
	return math.Hypot(px-x, py-y)
}

func triangle(x, y float64, a, b, c [2]float64) bool {
	d1 := sign(x, y, a, b)
	d2 := sign(x, y, b, c)
	d3 := sign(x, y, c, a)
	hasNeg := d1 < 0 || d2 < 0 || d3 < 0
	hasPos := d1 > 0 || d2 > 0 || d3 > 0
	return !(hasNeg && hasPos)
}

func sign(x, y float64, a, b [2]float64) float64 {
	return (x-b[0])*(a[1]-b[1]) - (a[0]-b[0])*(y-b[1])
}

func ico(sizes []int) []byte {
	images := make([][]byte, len(sizes))
	for i, size := range sizes {
		images[i] = dib(render(size))
	}

	dirLen := 6 + len(images)*16
	total := dirLen
	for _, img := range images {
		total += len(img)
	}
	out := make([]byte, total)
	binary.LittleEndian.PutUint16(out[2:], 1)
	binary.LittleEndian.PutUint16(out[4:], uint16(len(images)))

	offset := dirLen
	for i, img := range images {
		size := sizes[i]
		entry := out[6+i*16:]
		entry[0] = iconDirSize(size)
		entry[1] = iconDirSize(size)
		binary.LittleEndian.PutUint16(entry[4:], 1)
		binary.LittleEndian.PutUint16(entry[6:], 32)
		binary.LittleEndian.PutUint32(entry[8:], uint32(len(img)))
		binary.LittleEndian.PutUint32(entry[12:], uint32(offset))
		copy(out[offset:], img)
		offset += len(img)
	}
	return out
}

func iconDirSize(size int) byte {
	if size >= 256 {
		return 0
	}
	return byte(size)
}

func dib(img *image.NRGBA) []byte {
	size := img.Bounds().Dx()
	maskStride := ((size + 31) / 32) * 4
	pixelsLen := size * size * 4
	out := make([]byte, 40+pixelsLen+maskStride*size)

	binary.LittleEndian.PutUint32(out[0:], 40)
	binary.LittleEndian.PutUint32(out[4:], uint32(size))
	binary.LittleEndian.PutUint32(out[8:], uint32(size*2))
	binary.LittleEndian.PutUint16(out[12:], 1)
	binary.LittleEndian.PutUint16(out[14:], 32)
	binary.LittleEndian.PutUint32(out[20:], uint32(pixelsLen+maskStride*size))

	pos := 40
	for y := size - 1; y >= 0; y-- {
		for x := 0; x < size; x++ {
			c := img.NRGBAAt(x, y)
			out[pos+0] = c.B
			out[pos+1] = c.G
			out[pos+2] = c.R
			out[pos+3] = c.A
			pos += 4
		}
	}
	return out
}
