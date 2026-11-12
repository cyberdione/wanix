package draw

import (
	"encoding/binary"
	"fmt"
	"image"
)

// Color constants from draw.h (RRGGBBAA in one u32).
const (
	DWhite = 0xFFFFFFFF
	DBlack = 0x000000FF
	DRed   = 0xFF0000FF
)

type chanInfo struct {
	depth int    // bits per pixel
	str   string // chantostr form
}

func chanDesc(c uint32) (chanInfo, error) {
	switch c {
	case ChanGREY1:
		return chanInfo{1, "k1"}, nil
	case ChanGREY8:
		return chanInfo{8, "k8"}, nil
	case ChanRGB24:
		return chanInfo{24, "r8g8b8"}, nil
	}
	return chanInfo{}, fmt.Errorf("unsupported channel format %#x", c)
}

// bytesPerLine per memdraw semantics: rows padded to a byte boundary.
func bytesPerLine(r image.Rectangle, depth int) int {
	return (r.Dx()*depth + 7) / 8
}

// Image is a per-client draw image. Pixel data is stored packed at native
// depth, one scanline after another with no inter-line padding beyond the
// byte alignment implied by bytesPerLine; for sub-byte depths the high-order
// bit of each byte is the leftmost pixel (image(6)).
type Image struct {
	id      uint32
	chanFmt uint32
	depth   int
	chanStr string
	repl    bool
	r       image.Rectangle
	clipr   image.Rectangle
	stride  int
	pix     []byte
	screen  uint32 // screen id if allocated on a screen, else 0
	refresh byte
	name    string // global name, if named via 'N'
}

func newImage(id uint32, chanFmt uint32, repl bool, r, clipr image.Rectangle, color uint32) *Image {
	ci, _ := chanDesc(chanFmt)
	stride := bytesPerLine(r, ci.depth)
	img := &Image{
		id:      id,
		chanFmt: chanFmt,
		depth:   ci.depth,
		chanStr: ci.str,
		repl:    repl,
		r:       r,
		clipr:   clipr,
		stride:  stride,
		pix:     make([]byte, stride*r.Dy()),
	}
	img.fill(r, color)
	return img
}

func (im *Image) offset(x, y int) (byteOff, bitOff int) {
	i := (y-im.r.Min.Y)*im.stride*8 + (x-im.r.Min.X)*im.depth
	return i / 8, i % 8
}

func (im *Image) getPixel(x, y int) uint32 {
	switch im.depth {
	case 1:
		bo, bi := im.offset(x, y)
		if im.pix[bo]&(0x80>>bi) != 0 {
			return 1
		}
		return 0
	case 8:
		bo, _ := im.offset(x, y)
		return uint32(im.pix[bo])
	case 24:
		bo, _ := im.offset(x, y)
		return uint32(im.pix[bo]) | uint32(im.pix[bo+1])<<8 | uint32(im.pix[bo+2])<<16
	}
	panic("bad depth")
}

func (im *Image) setPixel(x, y int, v uint32) {
	switch im.depth {
	case 1:
		bo, bi := im.offset(x, y)
		if v != 0 {
			im.pix[bo] |= 0x80 >> bi
		} else {
			im.pix[bo] &^= 0x80 >> bi
		}
	case 8:
		bo, _ := im.offset(x, y)
		im.pix[bo] = byte(v)
	case 24:
		bo, _ := im.offset(x, y)
		im.pix[bo] = byte(v)
		im.pix[bo+1] = byte(v >> 8)
		im.pix[bo+2] = byte(v >> 16)
	}
}

// colorToPixel converts an RRGGBBAA color value to this image's pixel format.
// Grey conversion uses a simple channel average; the kernel converts via the
// color map, which this slice does not replicate.
func (im *Image) colorToPixel(color uint32) uint32 {
	r := byte(color >> 24)
	g := byte(color >> 16)
	b := byte(color >> 8)
	switch im.chanFmt {
	case ChanRGB24:
		return uint32(b) | uint32(g)<<8 | uint32(r)<<16
	case ChanGREY8:
		return uint32((uint16(r) + uint16(g) + uint16(b)) / 3)
	case ChanGREY1:
		if (uint16(r)+uint16(g)+uint16(b))/3 >= 128 {
			return 1
		}
		return 0
	}
	return 0
}

func (im *Image) fill(r image.Rectangle, color uint32) {
	v := im.colorToPixel(color)
	r = r.Intersect(im.r)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			im.setPixel(x, y, v)
		}
	}
}

// pixelRGB returns the pixel at (x, y) as 0xRRGGBB.
func (im *Image) pixelRGB(x, y int) uint32 {
	v := im.getPixel(x, y)
	switch im.chanFmt {
	case ChanRGB24:
		return v // already B|G<<8|R<<16
	case ChanGREY8:
		return v * 0x010101
	case ChanGREY1:
		if v != 0 {
			return 0xFFFFFF
		}
		return 0
	}
	return 0
}

// rgbPixel converts 0xRRGGBB to this image's pixel format. Grey conversion
// uses a channel average; the kernel converts via the color map, which this
// slice does not replicate.
func (im *Image) rgbPixel(rgb uint32) uint32 {
	switch im.chanFmt {
	case ChanRGB24:
		return rgb
	case ChanGREY8:
		r := (rgb >> 16) & 0xff
		g := (rgb >> 8) & 0xff
		b := rgb & 0xff
		return (r + g + b) / 3
	case ChanGREY1:
		r := (rgb >> 16) & 0xff
		g := (rgb >> 8) & 0xff
		b := rgb & 0xff
		if (r+g+b)/3 >= 128 {
			return 1
		}
		return 0
	}
	return 0
}

// draw copies the rectangle dr (dst coordinates) from src, mapping dr.Min to
// sp in src coordinates. A replicated src tiles; otherwise reads outside
// src.r are transparent (skipped). Only dst pixels inside dst.clipr change.
// Mask content is not applied; the mask is treated as fully opaque.
func drawBlit(dst, src *Image, dr image.Rectangle, sp image.Point, op int) {
	dr = dr.Intersect(dst.clipr).Intersect(dst.r)
	if dr.Empty() {
		return
	}
	sr := src.r
	for y := dr.Min.Y; y < dr.Max.Y; y++ {
		for x := dr.Min.X; x < dr.Max.X; x++ {
			sx := sp.X + (x - dr.Min.X)
			sy := sp.Y + (y - dr.Min.Y)
			if src.repl {
				sx = sr.Min.X + mod(sx-sr.Min.X, sr.Dx())
				sy = sr.Min.Y + mod(sy-sr.Min.Y, sr.Dy())
			} else if !(image.Point{sx, sy}.In(sr)) {
				continue
			}
			if op == OpClear {
				dst.setPixel(x, y, 0)
				continue
			}
			dst.setPixel(x, y, dst.rgbPixel(src.pixelRGB(sx, sy)))
		}
	}
}

func mod(a, b int) int {
	if b <= 0 {
		return 0
	}
	a %= b
	if a < 0 {
		a += b
	}
	return a
}

// load writes uncompressed pixel data (image(6) byte order) into r.
func (im *Image) load(r image.Rectangle, data []byte) (int, error) {
	if !r.In(im.r) {
		return 0, fmt.Errorf("writeimage outside image")
	}
	need := bytesPerLine(r, im.depth) * r.Dy()
	if len(data) < need {
		return 0, ErrShortDraw
	}
	if im.depth == 24 {
		// contiguous byte copy per row
		w := r.Dx() * 3
		for row := 0; row < r.Dy(); row++ {
			srcOff := row * w
			dstOff := (r.Min.Y+row-im.r.Min.Y)*im.stride + (r.Min.X-im.r.Min.X)*3
			copy(im.pix[dstOff:dstOff+w], data[srcOff:srcOff+w])
		}
		return need, nil
	}
	// bit-accurate path for sub-byte/byte depths
	ls := bytesPerLine(r, im.depth)
	for row := 0; row < r.Dy(); row++ {
		for col := 0; col < r.Dx(); col++ {
			i := row*ls*8 + col*im.depth
			var v uint32
			switch im.depth {
			case 1:
				if data[i/8]&(0x80>>uint(i%8)) != 0 {
					v = 1
				}
			case 8:
				v = uint32(data[i/8])
			}
			im.setPixel(r.Min.X+col, r.Min.Y+row, v)
		}
	}
	return need, nil
}

// unload extracts uncompressed pixel data from r in image(6) byte order.
func (im *Image) unload(r image.Rectangle) ([]byte, error) {
	if !r.In(im.r) {
		return nil, fmt.Errorf("readimage outside image")
	}
	n := bytesPerLine(r, im.depth) * r.Dy()
	out := make([]byte, n)
	if im.depth == 24 {
		w := r.Dx() * 3
		for row := 0; row < r.Dy(); row++ {
			srcOff := (r.Min.Y+row-im.r.Min.Y)*im.stride + (r.Min.X-im.r.Min.X)*3
			copy(out[row*w:row*w+w], im.pix[srcOff:srcOff+w])
		}
		return out, nil
	}
	ls := bytesPerLine(r, im.depth)
	for row := 0; row < r.Dy(); row++ {
		for col := 0; col < r.Dx(); col++ {
			v := im.getPixel(r.Min.X+col, r.Min.Y+row)
			i := row*ls*8 + col*im.depth
			switch im.depth {
			case 1:
				if v != 0 {
					out[i/8] |= 0x80 >> uint(i%8)
				}
			case 8:
				out[i/8] = byte(v)
			}
		}
	}
	return out, nil
}

func bgLong(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func bgShort(b []byte) int   { return int(binary.LittleEndian.Uint16(b)) }

func bgRect(b []byte) image.Rectangle {
	return image.Rect(int(int32(bgLong(b))), int(int32(bgLong(b[4:]))),
		int(int32(bgLong(b[8:]))), int(int32(bgLong(b[12:]))))
}

func bgPoint(b []byte) image.Point {
	return image.Pt(int(int32(bgLong(b))), int(int32(bgLong(b[4:]))))
}
