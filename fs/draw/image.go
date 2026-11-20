package draw

import (
	"encoding/binary"
	"fmt"
	"image"
)

// Color constants from draw.h (premultiplied RRGGBBAA in one u32;
// allocimage(2): e.g. 50% red is 0x7F00007F, not 0xFF00007F).
const (
	DWhite = 0xFFFFFFFF
	DBlack = 0x000000FF
	DRed   = 0xFF0000FF
)

// Channel types (draw.h enum: CRed..CIgnore).
const (
	chanRed   = 0
	chanGreen = 1
	chanBlue  = 2
	chanGrey  = 3
	chanAlpha = 4
)

// chanField is one channel of a pixel format: the channel type, its bit
// width, and its bit shift within the pixel word. Per draw(3), the channel
// descriptor is the concatenation of up to four bytes, each with the type
// in the high nibble and the bit count in the low nibble; the first channel
// of the string is the most significant descriptor byte. Per memdraw
// memsetchan, descriptor byte k (from the bottom) occupies bits 8*k of the
// pixel, so the last channel of the string is the least significant.
type chanField struct {
	ctype int
	nbits int
	shift int
}

// chanFormat is a supported pixel format.
type chanFormat struct {
	str    string
	depth  int
	fields []chanField
}

// chanValue computes the wire channel descriptor (draw.h CHANn packing:
// strtochan("r8g8b8") == 0x281808) and pixel depth for a field list given
// in descriptor order (first channel first).
func chanValue(fields []chanField) (value uint32, depth int) {
	for _, f := range fields {
		value = value<<8 | uint32(f.ctype)<<4 | uint32(f.nbits)
		depth += f.nbits
	}
	return value, depth
}

func makeFormat(str string, in ...chanField) chanFormat {
	// in is in descriptor order; shifts are from the last channel up.
	f := chanFormat{str: str, fields: make([]chanField, len(in))}
	shift := 0
	for i := len(in) - 1; i >= 0; i-- {
		f.fields[i] = in[i]
		f.fields[i].shift = shift
		shift += ((in[i].nbits + 7) / 8) * 8
	}
	f.depth = 0
	for _, c := range in {
		f.depth += c.nbits
	}
	return f
}

// Channel format constants (draw.h CHANn packing, strtochan values).
const (
	ChanGREY1  = 0x31       // k1
	ChanGREY8  = 0x38       // k8
	ChanALPHA8 = 0x48       // a8 (alpha-only; natural as a mask)
	ChanRGB24  = 0x081828   // r8g8b8
	ChanRGBA32 = 0x08182848 // r8g8b8a8
)

var chanFormats = map[uint32]chanFormat{
	ChanGREY1:  makeFormat("k1", chanField{chanGrey, 1, 0}),
	ChanGREY8:  makeFormat("k8", chanField{chanGrey, 8, 0}),
	ChanALPHA8: makeFormat("a8", chanField{chanAlpha, 8, 0}),
	ChanRGB24:  makeFormat("r8g8b8", chanField{chanRed, 8, 0}, chanField{chanGreen, 8, 0}, chanField{chanBlue, 8, 0}),
	ChanRGBA32: makeFormat("r8g8b8a8", chanField{chanRed, 8, 0}, chanField{chanGreen, 8, 0}, chanField{chanBlue, 8, 0}, chanField{chanAlpha, 8, 0}),
}

func chanDesc(c uint32) (chanFormat, error) {
	if f, ok := chanFormats[c]; ok {
		return f, nil
	}
	return chanFormat{}, fmt.Errorf("unsupported channel format %#x", c)
}

// bytesPerLine per memdraw semantics: rows padded to a byte boundary.
func bytesPerLine(r image.Rectangle, depth int) int {
	return (r.Dx()*depth + 7) / 8
}

// repTo8 replicates an n-bit channel value up to 8 bits (memdraw replbit).
func repTo8(v, n int) int {
	nb := n
	for nb < 8 {
		v |= v << nb
		nb *= 2
	}
	return v >> (nb - 8)
}

// rgb2k is memdraw's RGB2K: perfect approximation to the NTSC average
// .299r+.587g+.114b for 0 <= r,g,b < 256.
func rgb2k(r, g, b int) int {
	return ((156763*r + 307758*g + 59769*b) >> 19)
}

// Image is a per-client draw image. Pixel data is stored packed at native
// depth, one scanline after another with no inter-line padding beyond the
// byte alignment implied by bytesPerLine. For sub-byte depths the high-order
// bit of each byte is the leftmost pixel (image(6)). For whole-byte depths
// the pixel is a little-endian integer of depth/8 bytes whose channel layout
// follows the descriptor (color(6): e.g. r8g8b8 files order bytes blue,
// green, red; r8g8b8a8 order alpha, blue, green, red).
//
// Alpha-bearing pixels are stored PREMULTIPLIED, matching the pinned
// memdraw: compositing arithmetic operates on premultiplied channels, fill
// colors are premultiplied per allocimage(2), and load/unload ('y'/'r') pass
// the stored bytes through untouched.
type Image struct {
	id      uint32
	chanFmt uint32
	fmt     chanFormat
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

	// Font state, set by 'i' and 'l' (devdraw.c DImage fields).
	ascent int
	fchar  []FChar
}

// FChar is one font-cache character, as recorded by the 'l' opcode
// (devdraw.c FChar): the glyph bits' bounding box in the font image plus
// the baseline metrics.
type FChar struct {
	minx, maxx int // left/right edge of glyph bits in the font image
	miny, maxy int // first scan line / last scan line + 1
	left       int // offset of baseline (signed)
	width      int // width of baseline (advance)
}

func newImage(id uint32, chanFmt uint32, repl bool, r, clipr image.Rectangle, color uint32) *Image {
	f, _ := chanDesc(chanFmt)
	stride := bytesPerLine(r, f.depth)
	img := &Image{
		id:      id,
		chanFmt: chanFmt,
		fmt:     f,
		depth:   f.depth,
		chanStr: f.str,
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
	case 32:
		bo, _ := im.offset(x, y)
		return uint32(im.pix[bo]) | uint32(im.pix[bo+1])<<8 | uint32(im.pix[bo+2])<<16 | uint32(im.pix[bo+3])<<24
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
	case 32:
		bo, _ := im.offset(x, y)
		im.pix[bo] = byte(v)
		im.pix[bo+1] = byte(v >> 8)
		im.pix[bo+2] = byte(v >> 16)
		im.pix[bo+3] = byte(v >> 24)
	}
}

// rgbaAt reads the pixel at (x, y) as premultiplied 8-bit RGBA. Channel
// conversion follows memdraw imgtorgba: sub-byte values are replicated to 8
// bits, grey feeds r, g and b, absent channels default to alpha 255 and
// color 0 (imgtorgba's 0xAA garbage color default is replaced by 0 here, a
// deterministic choice for this slice).
func (im *Image) rgbaAt(x, y int) (r, g, b, a int) {
	v := im.getPixel(x, y)
	r, g, b, a = 0, 0, 0, 255
	for _, f := range im.fmt.fields {
		c := repTo8(int((v>>uint(f.shift))&((1<<uint(f.nbits))-1)), f.nbits)
		switch f.ctype {
		case chanRed:
			r = c
		case chanGreen:
			g = c
		case chanBlue:
			b = c
		case chanGrey:
			r, g, b = c, c, c
		case chanAlpha:
			a = c
		}
	}
	return
}

// pixelFromRGBA converts premultiplied 8-bit RGBA to this image's pixel
// format, following memdraw rgbatoimg: channels are truncated to their bit
// width and grey is computed with the NTSC average rgb2k.
func (im *Image) pixelFromRGBA(r, g, b, a int) uint32 {
	var v uint32
	for _, f := range im.fmt.fields {
		var c int
		switch f.ctype {
		case chanRed:
			c = r
		case chanGreen:
			c = g
		case chanBlue:
			c = b
		case chanGrey:
			c = rgb2k(r, g, b)
		case chanAlpha:
			c = a
		}
		c >>= 8 - f.nbits
		c &= (1 << uint(f.nbits)) - 1
		v |= uint32(c) << uint(f.shift)
	}
	return v
}

// setRGBA writes a premultiplied 8-bit RGBA pixel.
func (im *Image) setRGBA(x, y int, r, g, b, a int) {
	im.setPixel(x, y, im.pixelFromRGBA(r, g, b, a))
}

// maskAlpha returns the alpha value a pixel of this image contributes when
// used as a mask (draw(2)): the alpha channel if the format has one,
// otherwise the NTSC grey of the color.
func (im *Image) maskAlpha(x, y int) int {
	if im.hasField(chanAlpha) {
		_, _, _, a := im.rgbaAt(x, y)
		return a
	}
	r, g, b, _ := im.rgbaAt(x, y)
	return rgb2k(r, g, b)
}

func (im *Image) hasField(ctype int) bool {
	for _, f := range im.fmt.fields {
		if f.ctype == ctype {
			return true
		}
	}
	return false
}

// colorToPixel converts a premultiplied RRGGBBAA color value to this
// image's pixel format (memfillcolor -> rgbatoimg).
func (im *Image) colorToPixel(color uint32) uint32 {
	return im.pixelFromRGBA(int(color>>24)&0xff, int(color>>16)&0xff, int(color>>8)&0xff, int(color)&0xff)
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

// mul255 is memdraw's rounded multiply MUL: the nearest integer to x*y/255.
func mul255(x, y int) int {
	t := x*y + 128
	return (t + (t >> 8)) >> 8
}

// composite implements the raster of draw(3) 'd' per libmemdraw
// memimagedraw for the supported Porter-Duff ops:
//
//	Clear  dst = 0
//	S     dst = src*ma                      (per premultiplied channel)
//	SoverD dst = src*ma + dst*(1-sa*ma)
//
// where sa is the source alpha and ma the mask alpha: the alpha channel
// for alpha-bearing masks, else the NTSC grey of the mask color (draw(2)).
// Pixels are premultiplied (see the Image doc). Clipping follows memdraw
// drawclip: dr is cut to dst.r and dst.clipr; the draw rect is cut to the
// parts whose source and mask samples fall inside the src and mask
// rectangles (non-repl) and clip rectangles (always); repl images wrap
// within r (drawrepl). mask == nil is the memopaque stand-in used by the
// font ops.
func composite(dst, src, mask *Image, dr image.Rectangle, sp, mp image.Point, op int) {
	r := dr.Intersect(dst.r).Intersect(dst.clipr)
	if r.Empty() {
		return
	}
	if op != OpClear && op != OpS && op != OpSoverD {
		return
	}
	if src == dst {
		src = snapshot(src, sp.Add(r.Min.Sub(dr.Min)), r.Size())
	}
	if mask == dst {
		mask = snapshot(mask, mp.Add(r.Min.Sub(dr.Min)), r.Size())
	}

	// Map the destination rect into source space and clip it (drawclip):
	// non-repl sources clip to r, all sources clip to clipr.
	p0 := sp.Add(r.Min.Sub(dr.Min))
	sr := image.Rect(p0.X, p0.Y, p0.X+r.Dx(), p0.Y+r.Dy())
	if !src.repl {
		sr = sr.Intersect(src.r)
	}
	sr = sr.Intersect(src.clipr)
	if sr.Empty() {
		return
	}

	// The mask rect is congruent to sr in mask space; clipping it clips the
	// drawn area, and the clip is reflected back into the source rect.
	m0 := mp.Add(sr.Min.Sub(sp))
	if mask != nil {
		mr := image.Rect(m0.X, m0.Y, m0.X+sr.Dx(), m0.Y+sr.Dy())
		omr := mr
		if !mask.repl {
			mr = mr.Intersect(mask.r)
		}
		mr = mr.Intersect(mask.clipr)
		if mr.Empty() {
			return
		}
		sr = image.Rect(
			sr.Min.X+mr.Min.X-omr.Min.X, sr.Min.Y+mr.Min.Y-omr.Min.Y,
			sr.Max.X+mr.Max.X-omr.Max.X, sr.Max.Y+mr.Max.Y-omr.Max.Y)
		m0 = mr.Min
	}

	// Back to destination coordinates; r, sr and m0 are now congruent.
	r = sr.Add(r.Min.Sub(p0))

	for y := 0; y < r.Dy(); y++ {
		sy := sr.Min.Y + y
		if src.repl {
			sy = src.r.Min.Y + mod(sy-src.r.Min.Y, src.r.Dy())
		}
		my := m0.Y + y
		if mask != nil && mask.repl {
			my = mask.r.Min.Y + mod(my-mask.r.Min.Y, mask.r.Dy())
		}
		for x := 0; x < r.Dx(); x++ {
			sx := sr.Min.X + x
			if src.repl {
				sx = src.r.Min.X + mod(sx-src.r.Min.X, src.r.Dx())
			}
			dx, dy := r.Min.X+x, r.Min.Y+y
			ma := 255 // mask == nil: memopaque
			if mask != nil {
				mx := m0.X + x
				if mask.repl {
					mx = mask.r.Min.X + mod(mx-mask.r.Min.X, mask.r.Dx())
				}
				ma = mask.maskAlpha(mx, my)
			}
			switch op {
			case OpClear:
				dst.setRGBA(dx, dy, 0, 0, 0, 0)
			case OpS:
				srr, sgg, sbb, saa := src.rgbaAt(sx, sy)
				dst.setRGBA(dx, dy, mul255(ma, srr), mul255(ma, sgg), mul255(ma, sbb), mul255(ma, saa))
			case OpSoverD:
				srr, sgg, sbb, saa := src.rgbaAt(sx, sy)
				fd := 255 - mul255(saa, ma)
				drr, dgg, dbb, daa := dst.rgbaAt(dx, dy)
				dst.setRGBA(dx, dy,
					mul255(ma, srr)+mul255(fd, drr),
					mul255(ma, sgg)+mul255(fd, dgg),
					mul255(ma, sbb)+mul255(fd, dbb),
					mul255(ma, saa)+mul255(fd, daa))
			}
		}
	}
}

// snapshot copies the region of im that a draw onto im itself samples
// (aligned at p, spanning size), so reads never see writes from the same
// operation (memdraw buffers same-bitmap draws).
func snapshot(im *Image, p image.Point, size image.Point) *Image {
	sr := image.Rect(p.X, p.Y, p.X+size.X, p.Y+size.Y)
	if im.repl {
		sr = im.r // repl samples may wrap anywhere in r
	} else {
		sr = sr.Intersect(im.r)
	}
	if sr.Empty() {
		return im
	}
	data, err := im.unload(sr)
	if err != nil {
		return im
	}
	cp := newImage(0, im.chanFmt, im.repl, sr, im.clipr, 0)
	cp.load(sr, data)
	return cp
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
	if im.depth >= 8 {
		// whole-byte pixels: source rows are contiguous
		w := r.Dx() * im.depth / 8
		for row := 0; row < r.Dy(); row++ {
			srcOff := row * w
			dstOff := (r.Min.Y+row-im.r.Min.Y)*im.stride + (r.Min.X-im.r.Min.X)*im.depth/8
			copy(im.pix[dstOff:dstOff+w], data[srcOff:srcOff+w])
		}
		return need, nil
	}
	// bit-accurate path for sub-byte depths
	ls := bytesPerLine(r, im.depth)
	for row := 0; row < r.Dy(); row++ {
		for col := 0; col < r.Dx(); col++ {
			i := row*ls*8 + col*im.depth
			var v uint32
			if data[i/8]&(0x80>>uint(i%8)) != 0 {
				v = 1
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
	if im.depth >= 8 {
		w := r.Dx() * im.depth / 8
		for row := 0; row < r.Dy(); row++ {
			srcOff := (r.Min.Y+row-im.r.Min.Y)*im.stride + (r.Min.X-im.r.Min.X)*im.depth/8
			copy(out[row*w:row*w+w], im.pix[srcOff:srcOff+w])
		}
		return out, nil
	}
	ls := bytesPerLine(r, im.depth)
	for row := 0; row < r.Dy(); row++ {
		for col := 0; col < r.Dx(); col++ {
			v := im.getPixel(r.Min.X+col, r.Min.Y+row)
			i := row*ls*8 + col*im.depth
			if v != 0 {
				out[i/8] |= 0x80 >> uint(i%8)
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

// drawString renders the cache indices from the font image through src onto
// dst, following devdraw.c 's'/'x': each glyph is composited with the font
// image as mask (drawChar); 'x' first paints the background image under the
// full string extent (x from p.x to p.x+sum(width), y from the ascent line
// spanning the font image height) with an opaque mask. The caller has
// already swapped dst.clipr to the string clip rectangle.
func drawString(dst, src, bg *Image, bgq image.Point, font *Image, p, sp image.Point, indices []byte, op int) error {
	if bg != nil {
		r := image.Rect(p.X, p.Y-font.ascent, p.X, p.Y-font.ascent+font.r.Dy())
		for j := 0; j*2+1 < len(indices); j++ {
			ci := int(binary.LittleEndian.Uint16(indices[2*j:]))
			if ci >= len(font.fchar) {
				return ErrIndex
			}
			r.Max.X += font.fchar[ci].width
		}
		composite(dst, bg, nil, r, bgq, bgq, op)
	}
	for j := 0; j*2+1 < len(indices); j++ {
		ci := int(binary.LittleEndian.Uint16(indices[2*j:]))
		if ci >= len(font.fchar) {
			return ErrIndex
		}
		p, sp = drawChar(dst, src, p, sp, font, ci, op)
	}
	return nil
}

// drawChar composites one glyph (devdraw.c drawchar): the glyph rectangle
// in dst is placed at the pen plus the left bearing, vertically at
// baseline-ascent+miny, with the font image at (minx, miny) as mask and
// the source at sp plus the same offsets; the pen and source point advance
// by the character width.
func drawChar(dst, src *Image, p, sp image.Point, font *Image, index int, op int) (image.Point, image.Point) {
	fc := &font.fchar[index]
	r := image.Rect(p.X+fc.left, p.Y-(font.ascent-fc.miny),
		p.X+fc.left+(fc.maxx-fc.minx), p.Y-(font.ascent-fc.miny)+(fc.maxy-fc.miny))
	sp1 := image.Pt(sp.X+fc.left, sp.Y+fc.miny)
	composite(dst, src, font, r, sp1, image.Pt(fc.minx, fc.miny), op)
	return image.Pt(p.X+fc.width, p.Y), image.Pt(sp.X+fc.width, sp.Y)
}
