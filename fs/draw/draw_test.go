package draw

import (
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
	"strings"
	"testing"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/vfs"
)

func msg(parts ...any) []byte {
	var out []byte
	for _, p := range parts {
		switch v := p.(type) {
		case byte:
			out = append(out, v)
		case string:
			out = append(out, v...)
		case uint32:
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], v)
			out = append(out, b[:]...)
		case uint16:
			var b [2]byte
			binary.LittleEndian.PutUint16(b[:], v)
			out = append(out, b[:]...)
		case rect:
			for _, n := range [4]int32{v.minx, v.miny, v.maxx, v.maxy} {
				var b [4]byte
				binary.LittleEndian.PutUint32(b[:], uint32(n))
				out = append(out, b[:]...)
			}
		case pt:
			var b [8]byte
			binary.LittleEndian.PutUint32(b[0:], uint32(v.x))
			binary.LittleEndian.PutUint32(b[4:], uint32(v.y))
			out = append(out, b[:]...)
		case []byte:
			out = append(out, v...)
		default:
			panic(fmt.Sprintf("bad msg part %T", p))
		}
	}
	return out
}

type rect struct{ minx, miny, maxx, maxy int32 }
type pt struct{ x, y int32 }

// huge is the clipr libdraw sends for replicated images (alloc.c): huge but
// not infinite, so offsets leave it huge without overflow.
var huge = rect{-0x3FFFFFFF, -0x3FFFFFFF, 0x3FFFFFFF, 0x3FFFFFFF}

func wr(t *testing.T, f fs.File, b []byte) {
	t.Helper()
	n, err := fs.Write(f, b)
	if err != nil {
		t.Fatalf("write %q: %v", string(b[:1]), err)
	}
	if n != len(b) {
		t.Fatalf("short write: %d != %d", n, len(b))
	}
}

func openClient(t *testing.T, dev *Device) (ctl, data fs.File, id uint32) {
	t.Helper()
	f, err := dev.Open("new")
	if err != nil {
		t.Fatalf("open new: %v", err)
	}
	info := make([]byte, 145)
	n, err := f.Read(info)
	if err != nil {
		t.Fatalf("read ctl: %v", err)
	}
	if n != 144 {
		t.Fatalf("ctl read = %d bytes, want 144", n)
	}
	fields := strings.Fields(string(info[:n]))
	if len(fields) != 12 {
		t.Fatalf("ctl fields = %d, want 12: %q", len(fields), info[:n])
	}
	if _, err := fmt.Sscanf(fields[0], "%d", &id); err != nil {
		t.Fatalf("parse conn id: %v", err)
	}
	df, err := dev.Open(fmt.Sprintf("%d/data", id))
	if err != nil {
		t.Fatalf("open data: %v", err)
	}
	return f, df, id
}

func TestNewHandshake(t *testing.T) {
	dev := New()
	ctl, _, id := openClient(t, dev)
	defer ctl.Close()
	if id == 0 {
		t.Fatal("client id 0 reserved")
	}
	// info was consumed: second read must fail like the kernel (infoid=-1)
	if _, err := ctl.Read(make([]byte, 145)); err == nil {
		t.Fatal("second ctl read should fail")
	}
	// ctl write of 4 bytes selects image id for the next read
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], 0)
	wr(t, ctl, b[:])
	info := make([]byte, 144)
	if _, err := ctl.Read(info); err != nil {
		t.Fatalf("ctl read after select: %v", err)
	}
	fields := strings.Fields(string(info))
	if fields[2] != "r8g8b8" || fields[1] != "0" {
		t.Fatalf("unexpected ctl info: %q", info)
	}
}

func TestAllocDrawReadback(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	// white replicated 1x1 GREY1 (like libdraw initdraw display->white)
	wr(t, data, msg(byte('b'), uint32(1), uint32(0), byte(0), uint32(ChanGREY1), byte(1),
		rect{0, 0, 1, 1}, huge, uint32(DWhite)))
	// 10x10 RGB24 dst filled black
	wr(t, data, msg(byte('b'), uint32(2), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 10, 10}, rect{0, 0, 10, 10}, uint32(DBlack)))
	// draw replicated white over the whole dst
	wr(t, data, msg(byte('d'), uint32(2), uint32(1), uint32(1),
		rect{0, 0, 10, 10}, pt{0, 0}, pt{0, 0}))
	wr(t, data, msg(byte('v')))
	// read back
	wr(t, data, msg(byte('r'), uint32(2), rect{0, 0, 10, 10}))
	buf := make([]byte, 300)
	n, err := data.Read(buf)
	if err != nil {
		t.Fatalf("read pixels: %v", err)
	}
	if n != 300 {
		t.Fatalf("pixel read = %d, want 300", n)
	}
	for i := 0; i < 300; i += 3 {
		if buf[i] != 0xff || buf[i+1] != 0xff || buf[i+2] != 0xff {
			t.Fatalf("pixel %d = %x %x %x, want ff ff ff", i/3, buf[i], buf[i+1], buf[i+2])
		}
	}
	if _, err := data.Read(buf); err != io.EOF {
		t.Fatalf("second read = %v, want EOF", err)
	}
}

func TestLoadAndReadPixels(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	wr(t, data, msg(byte('b'), uint32(1), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 2, 1}, rect{0, 0, 2, 1}, uint32(DBlack)))
	// DRed is RRGGBBAA 0xFF0000FF; r8g8b8 pixel byte order is blue,green,red (image(6))
	wr(t, data, msg(byte('y'), uint32(1), rect{0, 0, 2, 1}, []byte{0x00, 0x00, 0xff, 0x00, 0x00, 0xff}))
	wr(t, data, msg(byte('r'), uint32(1), rect{0, 0, 2, 1}))
	buf := make([]byte, 6)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != string([]byte{0, 0, 0xff, 0, 0, 0xff}) {
		t.Fatalf("pixels = %x", buf)
	}
}

func TestSegmentedWrites(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	m := msg(byte('b'), uint32(1), uint32(0), byte(0), uint32(ChanGREY8), byte(0),
		rect{0, 0, 4, 4}, rect{0, 0, 4, 4}, uint32(DWhite))
	// split across three writes
	for _, chunk := range [][]byte{m[:10], m[10:30], m[30:]} {
		wr(t, data, chunk)
	}
	wr(t, data, msg(byte('r'), uint32(1), rect{0, 0, 4, 4}))
	buf := make([]byte, 16)
	if n, err := data.Read(buf); n != 16 || err != nil {
		t.Fatalf("read = %d, %v", n, err)
	}
	for i, b := range buf {
		if b != 0xff {
			t.Fatalf("byte %d = %x, want ff", i, b)
		}
	}
}

func TestMalformedAndUnsupported(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	if _, err := fs.Write(data, []byte{'Z'}); err == nil {
		t.Fatal("unknown opcode accepted")
	}
	if _, err := fs.Write(data, msg(byte('D'), byte(1))); err == nil {
		t.Fatal("unsupported opcode D accepted")
	}
	// truncated 'b' (50 of 51 bytes) buffers; then a garbage completion fails
	m := msg(byte('b'), uint32(1), uint32(0), byte(0), uint32(ChanGREY8), byte(0),
		rect{0, 0, 4, 4}, rect{0, 0, 4, 4}, uint32(DWhite))
	if _, err := fs.Write(data, m[:len(m)-1]); err != nil {
		t.Fatalf("partial message should buffer: %v", err)
	}
	if _, err := fs.Write(data, m[len(m)-1:]); err != nil {
		t.Fatalf("completing buffered message failed: %v", err)
	}
	// bad channel format
	if _, err := fs.Write(data, msg(byte('b'), uint32(9), uint32(0), byte(0), uint32(0x99), byte(0),
		rect{0, 0, 1, 1}, rect{0, 0, 1, 1}, uint32(0))); err == nil {
		t.Fatal("unsupported channel accepted")
	}
	// duplicate id (id 1 was allocated by the segmented write above)
	if _, err := fs.Write(data, msg(byte('b'), uint32(1), uint32(0), byte(0), uint32(ChanGREY8), byte(0),
		rect{0, 0, 4, 4}, rect{0, 0, 4, 4}, uint32(DWhite))); err == nil {
		t.Fatal("duplicate image id accepted")
	}
	// free it, then 'd' against freed id fails
	wr(t, data, msg(byte('f'), uint32(1)))
	if _, err := fs.Write(data, msg(byte('d'), uint32(1), uint32(1), uint32(1),
		rect{0, 0, 1, 1}, pt{0, 0}, pt{0, 0})); err == nil {
		t.Fatal("draw with freed id accepted")
	}
}

func TestIndependentClientsAndNames(t *testing.T) {
	dev := New()
	ctl1, data1, id1 := openClient(t, dev)
	defer ctl1.Close()
	defer data1.Close()
	ctl2, data2, id2 := openClient(t, dev)
	defer ctl2.Close()
	defer data2.Close()
	if id1 == id2 {
		t.Fatal("client ids not unique")
	}

	// both clients can use image id 5 independently (per-client id space)
	wr(t, data1, msg(byte('b'), uint32(5), uint32(0), byte(0), uint32(ChanGREY8), byte(0),
		rect{0, 0, 2, 2}, rect{0, 0, 2, 2}, uint32(DWhite)))
	wr(t, data2, msg(byte('b'), uint32(5), uint32(0), byte(0), uint32(ChanGREY8), byte(0),
		rect{0, 0, 2, 2}, rect{0, 0, 2, 2}, uint32(DBlack)))

	// name client1's image and attach from client2
	wr(t, data1, msg(byte('N'), uint32(5), byte(1), byte(7), "winname"))
	wr(t, data2, msg(byte('n'), uint32(3), byte(7), "winname"))
	// 'n' sets infoid; a ctl pread returns the named image's info
	info := make([]byte, 144)
	if _, err := ctl2.Read(info); err != nil {
		t.Fatalf("ctl read after n: %v", err)
	}
	fields := strings.Fields(string(info))
	if fields[1] != "3" || fields[2] != "k8" || fields[6] != "2" {
		t.Fatalf("attached info = %q", info)
	}
	// attach of unknown name fails
	if _, err := fs.Write(data2, msg(byte('n'), uint32(4), byte(4), "nope")); err == nil {
		t.Fatal("attach unknown name accepted")
	}
}

func TestScreenAllocAndWindowImage(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	wr(t, data, msg(byte('b'), uint32(1), uint32(0), byte(0), uint32(ChanGREY1), byte(1),
		rect{0, 0, 1, 1}, huge, uint32(DWhite)))
	// screen on display image 0 with white fill
	wr(t, data, msg(byte('A'), uint32(1), uint32(0), uint32(1), byte(0)))
	// window image on the screen must match the screen chan and not repl
	wr(t, data, msg(byte('b'), uint32(2), uint32(1), byte(0), uint32(ChanRGB24), byte(0),
		rect{4, 4, 104, 104}, rect{4, 4, 104, 104}, uint32(DBlack)))
	if _, err := fs.Write(data, msg(byte('b'), uint32(3), uint32(1), byte(0), uint32(ChanGREY8), byte(0),
		rect{0, 0, 1, 1}, rect{0, 0, 1, 1}, uint32(0))); err == nil {
		t.Fatal("window image with mismatched chan accepted")
	}
	if _, err := fs.Write(data, msg(byte('A'), uint32(1), uint32(0), uint32(1), byte(0))); err == nil {
		t.Fatal("duplicate screen id accepted")
	}
	wr(t, data, msg(byte('F'), uint32(1)))
}

func TestCloseClunkFreesClient(t *testing.T) {
	dev := New()
	ctl, data, id := openClient(t, dev)
	wr(t, data, msg(byte('b'), uint32(5), uint32(0), byte(0), uint32(ChanGREY8), byte(0),
		rect{0, 0, 1, 1}, rect{0, 0, 1, 1}, uint32(DWhite)))
	wr(t, data, msg(byte('N'), uint32(5), byte(1), byte(4), "gone"))
	ctl.Close()
	// client survives while its data file is still open (fids are refcounted)
	df, err := dev.Open(fmt.Sprintf("%d/data", id))
	if err != nil {
		t.Fatalf("client gone while data file open: %v", err)
	}
	df.Close()
	data.Close()
	if _, err := dev.Open(fmt.Sprintf("%d/data", id)); err == nil {
		t.Fatal("client not freed after last close")
	}
	// names owned by the freed client are gone
	ctl2, data2, _ := openClient(t, dev)
	defer ctl2.Close()
	defer data2.Close()
	if _, err := fs.Write(data2, msg(byte('n'), uint32(1), byte(4), "gone")); err == nil {
		t.Fatal("stale name survived client close")
	}
}

func TestResourceLimits(t *testing.T) {
	dev := New()
	dev.MaxClients = 1
	dev.MaxImages = 2
	dev.MaxPixels = 16
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	if _, err := dev.Open("new"); err == nil {
		t.Fatal("second client accepted past MaxClients")
	}
	// display image id 0 occupies one slot; one more allowed
	wr(t, data, msg(byte('b'), uint32(1), uint32(0), byte(0), uint32(ChanGREY8), byte(0),
		rect{0, 0, 4, 4}, rect{0, 0, 4, 4}, uint32(0)))
	if _, err := fs.Write(data, msg(byte('b'), uint32(2), uint32(0), byte(0), uint32(ChanGREY8), byte(0),
		rect{0, 0, 4, 4}, rect{0, 0, 4, 4}, uint32(0))); err == nil {
		t.Fatal("image count past MaxImages accepted")
	}
	if _, err := fs.Write(data, msg(byte('b'), uint32(3), uint32(0), byte(0), uint32(ChanGREY8), byte(0),
		rect{0, 0, 8, 8}, rect{0, 0, 8, 8}, uint32(0))); err == nil {
		t.Fatal("oversized image accepted")
	}
}

func TestNamespaceBinding(t *testing.T) {
	dev := New()
	ns := vfs.New(context.Background())
	if err := ns.Bind(dev, ".", "#i"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// bound after /dev, the device yields /dev/draw/new as with Plan 9 #i
	if err := ns.Bind(ns, "#i", "dev"); err != nil {
		t.Fatalf("bind dev: %v", err)
	}
	f, err := ns.Open("dev/draw/new")
	if err != nil {
		t.Fatalf("open dev/draw/new: %v", err)
	}
	defer f.Close()
	info := make([]byte, 144)
	if n, err := f.Read(info); n != 144 || err != nil {
		t.Fatalf("read = %d, %v", n, err)
	}
}

func TestRefreshEOF(t *testing.T) {
	dev := New()
	ctl, _, id := openClient(t, dev)
	defer ctl.Close()
	rf, err := dev.Open(fmt.Sprintf("%d/refresh", id))
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	if _, err := rf.Read(make([]byte, 60)); err != io.EOF {
		t.Fatalf("refresh read = %v, want EOF (no notifications in this slice)", err)
	}
}

func TestColormap(t *testing.T) {
	dev := New()
	ctl, _, id := openClient(t, dev)
	defer ctl.Close()
	cf, err := dev.Open(fmt.Sprintf("%d/colormap", id))
	if err != nil {
		t.Fatal(err)
	}
	defer cf.Close()
	data, err := io.ReadAll(readerFunc(cf.Read))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 256 {
		t.Fatalf("colormap lines = %d, want 256", len(lines))
	}
	// index 0 is black (v=0)
	if !strings.HasSuffix(lines[0], "          0           0           0") {
		t.Fatalf("colormap[0] = %q", lines[0])
	}
	for i, line := range lines {
		var idx, r, g, b uint64
		if _, err := fmt.Sscanf(line, "%d %d %d %d", &idx, &r, &g, &b); err != nil || int(idx) != i {
			t.Fatalf("colormap line %d malformed: %q", i, line)
		}
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// strtochan re-implements the pinned libdraw chan.c strtochan for the test's
// reference derivation of wire channel descriptor values.
func strtochan(s string) uint32 {
	names := "rgbkamx"
	var c uint32
	for i := 0; i+1 < len(s); i += 2 {
		t := strings.IndexByte(names, s[i])
		if t < 0 || s[i+1] < '0' || s[i+1] > '9' {
			return 0
		}
		c = c<<8 | uint32(t)<<4 | uint32(s[i+1]-'0')
	}
	return c
}

// TestChanValues pins the wire channel descriptor values to the pinned
// strtochan (sys/src/libdraw/chan.c): the first channel of the string is the
// most significant descriptor byte (r8g8b8 == 0x281808).
func TestChanValues(t *testing.T) {
	for s, v := range map[string]uint32{
		"k1":       ChanGREY1,
		"k8":       ChanGREY8,
		"a8":       ChanALPHA8,
		"r8g8b8":   ChanRGB24,
		"r8g8b8a8": ChanRGBA32,
	} {
		if got := strtochan(s); got != v {
			t.Errorf("strtochan(%q) = %#x, constant = %#x", s, got, v)
		}
	}
}

func TestAlphaImageAllocAndCtl(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	for i, tc := range []struct {
		chanv uint32
		str   string
		depth int
	}{{ChanALPHA8, "a8", 8}, {ChanRGBA32, "r8g8b8a8", 32}} {
		id := uint32(i + 1)
		wr(t, data, msg(byte('b'), id, uint32(0), byte(0), tc.chanv, byte(0),
			rect{0, 0, 2, 1}, rect{0, 0, 2, 1}, uint32(0)))
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], id)
		wr(t, ctl, b[:])
		info := make([]byte, 144)
		if _, err := ctl.Read(info); err != nil {
			t.Fatalf("ctl read %s: %v", tc.str, err)
		}
		if fields := strings.Fields(string(info)); fields[2] != tc.str {
			t.Errorf("ctl chan = %q, want %q", fields[2], tc.str)
		}
	}
	// other alpha orders stay unsupported in this slice
	for _, bad := range []uint32{strtochan("argb8a8"), strtochan("a8r8g8b8"), strtochan("k4"), strtochan("r8g8b8x8")} {
		if _, err := fs.Write(data, msg(byte('b'), uint32(9), uint32(0), byte(0), bad, byte(0),
			rect{0, 0, 1, 1}, rect{0, 0, 1, 1}, uint32(0))); err == nil {
			t.Errorf("unsupported chan %#x accepted", bad)
		}
	}
}

// TestRGBA32ByteOrder checks image(6) byte order for r8g8b8a8: the pixel is
// a little-endian 32-bit word with red most significant (color(6)), so file
// bytes are alpha, blue, green, red.
func TestRGBA32ByteOrder(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	wr(t, data, msg(byte('b'), uint32(1), uint32(0), byte(0), uint32(ChanRGBA32), byte(0),
		rect{0, 0, 2, 2}, rect{0, 0, 2, 2}, uint32(DBlack)))
	// two pixels: (a=E0,b=C0,g=80,r=40) and (a=00,b=10,g=20,r=30), two rows
	px := []byte{0xE0, 0xC0, 0x80, 0x40, 0x00, 0x10, 0x20, 0x30}
	wr(t, data, msg(byte('y'), uint32(1), rect{0, 0, 2, 2}, append(append([]byte{}, px...), px...)))
	wr(t, data, msg(byte('r'), uint32(1), rect{0, 0, 2, 2}))
	buf := make([]byte, 16)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte{}, px...), px...)
	if string(buf) != string(want) {
		t.Fatalf("readback = %x, want %x", buf, want)
	}
}

// TestRGBA32FillPremultiplied: 'b' fill colors are premultiplied RRGGBBAA
// (allocimage(2)) and reach the channels via rgbatoimg truncation.
func TestRGBA32FillPremultiplied(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	wr(t, data, msg(byte('b'), uint32(1), uint32(0), byte(0), uint32(ChanRGBA32), byte(0),
		rect{0, 0, 1, 1}, rect{0, 0, 1, 1}, uint32(0x4080C0E0)))
	wr(t, data, msg(byte('r'), uint32(1), rect{0, 0, 1, 1}))
	buf := make([]byte, 4)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	// word = r<<24|g<<16|b<<8|a = 0x4080C0E0, little-endian bytes a,b,g,r
	if string(buf) != string([]byte{0xE0, 0xC0, 0x80, 0x40}) {
		t.Fatalf("fill pixel = %x, want e0 c0 80 40", buf)
	}
}

// TestA8ImageRoundtrip: an alpha-only image loads and unloads bytes verbatim.
func TestA8ImageRoundtrip(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	wr(t, data, msg(byte('b'), uint32(1), uint32(0), byte(0), uint32(ChanALPHA8), byte(0),
		rect{0, 0, 5, 1}, rect{0, 0, 5, 1}, uint32(0)))
	wr(t, data, msg(byte('y'), uint32(1), rect{0, 0, 5, 1}, []byte{0x00, 0x40, 0x80, 0xC0, 0xFF}))
	wr(t, data, msg(byte('r'), uint32(1), rect{0, 0, 5, 1}))
	buf := make([]byte, 5)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != string([]byte{0x00, 0x40, 0x80, 0xC0, 0xFF}) {
		t.Fatalf("a8 readback = %x", buf)
	}
}

// hugeClipRepl allocates a replicated 1x1 image like libdraw's
// display->white/opaque (huge clipr, repl).
func allocOpaque(t *testing.T, data fs.File, id uint32, chanv uint32, color uint32) {
	t.Helper()
	wr(t, data, msg(byte('b'), id, uint32(0), byte(0), chanv, byte(1),
		rect{0, 0, 1, 1}, huge, color))
}

// TestCompositeSoverDAlphaSource: premultiplied 50% red source over an
// opaque black destination. Porter-Duff: fd = 255-MUL(sa,ma) = 255-127 =
// 128; out.r = MUL(255,127)+MUL(128,0) = 127 (memdraw MUL rounding).
func TestCompositeSoverDAlphaSource(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	allocOpaque(t, data, 1, ChanRGBA32, 0x7F00007F) // 50% red (premultiplied)
	allocOpaque(t, data, 3, ChanGREY1, DWhite)
	wr(t, data, msg(byte('b'), uint32(2), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 4, 1}, rect{0, 0, 4, 1}, uint32(DBlack)))
	wr(t, data, msg(byte('d'), uint32(2), uint32(1), uint32(3),
		rect{0, 0, 4, 1}, pt{0, 0}, pt{0, 0}))
	wr(t, data, msg(byte('r'), uint32(2), rect{0, 0, 4, 1}))
	buf := make([]byte, 12)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if buf[i*3] != 0x00 || buf[i*3+1] != 0x00 || buf[i*3+2] != 0x7F {
			t.Fatalf("pixel %d = %x %x %x, want 00 00 7f", i, buf[i*3], buf[i*3+1], buf[i*3+2])
		}
	}
}

// TestCompositeMaskModulation: opaque white source through a k8 mask row;
// SoverD onto black gives grey = ma (MUL(ma,255)+MUL(255-ma,0)).
func TestCompositeMaskModulation(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	allocOpaque(t, data, 1, ChanRGB24, DWhite)
	wr(t, data, msg(byte('b'), uint32(2), uint32(0), byte(0), uint32(ChanGREY8), byte(0),
		rect{0, 0, 4, 1}, rect{0, 0, 4, 1}, uint32(0)))
	wr(t, data, msg(byte('y'), uint32(2), rect{0, 0, 4, 1}, []byte{0x00, 0x40, 0x80, 0xFF}))
	wr(t, data, msg(byte('b'), uint32(3), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 4, 1}, rect{0, 0, 4, 1}, uint32(DBlack)))
	wr(t, data, msg(byte('d'), uint32(3), uint32(1), uint32(2),
		rect{0, 0, 4, 1}, pt{0, 0}, pt{0, 0}))
	wr(t, data, msg(byte('r'), uint32(3), rect{0, 0, 4, 1}))
	buf := make([]byte, 12)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	for i, want := range []byte{0x00, 0x40, 0x80, 0xFF} {
		if buf[i*3] != want || buf[i*3+1] != want || buf[i*3+2] != want {
			t.Fatalf("pixel %d = %x, want grey %02x", i, buf[i*3:i*3+3], want)
		}
	}
}

// TestCompositeA8Mask: an a8 mask image masks by its alpha channel with the
// same result as the equivalent k8 grey mask (draw(2)).
func TestCompositeA8Mask(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	allocOpaque(t, data, 1, ChanRGB24, DWhite)
	wr(t, data, msg(byte('b'), uint32(2), uint32(0), byte(0), uint32(ChanALPHA8), byte(0),
		rect{0, 0, 4, 1}, rect{0, 0, 4, 1}, uint32(0)))
	wr(t, data, msg(byte('y'), uint32(2), rect{0, 0, 4, 1}, []byte{0x00, 0x40, 0x80, 0xFF}))
	wr(t, data, msg(byte('b'), uint32(3), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 4, 1}, rect{0, 0, 4, 1}, uint32(DBlack)))
	wr(t, data, msg(byte('d'), uint32(3), uint32(1), uint32(2),
		rect{0, 0, 4, 1}, pt{0, 0}, pt{0, 0}))
	wr(t, data, msg(byte('r'), uint32(3), rect{0, 0, 4, 1}))
	buf := make([]byte, 12)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	for i, want := range []byte{0x00, 0x40, 0x80, 0xFF} {
		if buf[i*3] != want || buf[i*3+1] != want || buf[i*3+2] != want {
			t.Fatalf("pixel %d = %x, want grey %02x", i, buf[i*3:i*3+3], want)
		}
	}
}

// TestCompositeOpSWithMask: op S writes MUL(ma,src) everywhere in the rect,
// including zero (transparent) where the mask is zero.
func TestCompositeOpSWithMask(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	allocOpaque(t, data, 1, ChanRGB24, DWhite)
	wr(t, data, msg(byte('b'), uint32(2), uint32(0), byte(0), uint32(ChanGREY8), byte(0),
		rect{0, 0, 2, 1}, rect{0, 0, 2, 1}, uint32(0)))
	wr(t, data, msg(byte('y'), uint32(2), rect{0, 0, 2, 1}, []byte{0x00, 0xFF}))
	wr(t, data, msg(byte('b'), uint32(3), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 2, 1}, rect{0, 0, 2, 1}, uint32(0x808080FF))) // grey 128
	wr(t, data, msg(byte('O'), byte(OpS)))
	wr(t, data, msg(byte('d'), uint32(3), uint32(1), uint32(2),
		rect{0, 0, 2, 1}, pt{0, 0}, pt{0, 0}))
	wr(t, data, msg(byte('r'), uint32(3), rect{0, 0, 2, 1}))
	buf := make([]byte, 6)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	if buf[0] != 0 || buf[1] != 0 || buf[2] != 0 {
		t.Fatalf("masked-off pixel = %x, want 00 00 00", buf[0:3])
	}
	if buf[3] != 0xFF || buf[4] != 0xFF || buf[5] != 0xFF {
		t.Fatalf("full-mask pixel = %x, want ff ff ff", buf[3:6])
	}
}

// TestCompositeClearHonorsClipr: Clear zeroes only dstr within dst.clipr
// (set via 'c'), like drawclip.
func TestCompositeClearHonorsClipr(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	wr(t, data, msg(byte('b'), uint32(1), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 4, 4}, rect{0, 0, 4, 4}, uint32(DWhite)))
	wr(t, data, msg(byte('c'), uint32(1), byte(0), rect{1, 1, 3, 3}))
	wr(t, data, msg(byte('O'), byte(OpClear)))
	wr(t, data, msg(byte('d'), uint32(1), uint32(1), uint32(1),
		rect{0, 0, 4, 4}, pt{0, 0}, pt{0, 0}))
	wr(t, data, msg(byte('r'), uint32(1), rect{0, 0, 4, 4}))
	buf := make([]byte, 48)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			i := (y*4 + x) * 3
			want := byte(0xFF)
			if x >= 1 && x < 3 && y >= 1 && y < 3 {
				want = 0
			}
			if buf[i] != want || buf[i+1] != want || buf[i+2] != want {
				t.Fatalf("pixel (%d,%d) = %x, want %02x", x, y, buf[i:i+3], want)
			}
		}
	}
}

// TestCompositeSemiOverSemi: SoverD of premultiplied 0x80808080 over
// 0x40404040 with an opaque mask: fd = 255-MUL(128,255) = 127 and
// out = MUL(255,128)+MUL(127,64) = 128+32 = 160 = 0xA0 per channel.
func TestCompositeSemiOverSemi(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	allocOpaque(t, data, 1, ChanRGBA32, 0x80808080)
	allocOpaque(t, data, 3, ChanGREY1, DWhite)
	wr(t, data, msg(byte('b'), uint32(2), uint32(0), byte(0), uint32(ChanRGBA32), byte(0),
		rect{0, 0, 2, 1}, rect{0, 0, 2, 1}, uint32(0x40404040)))
	wr(t, data, msg(byte('d'), uint32(2), uint32(1), uint32(3),
		rect{0, 0, 2, 1}, pt{0, 0}, pt{0, 0}))
	wr(t, data, msg(byte('r'), uint32(2), rect{0, 0, 2, 1}))
	buf := make([]byte, 8)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if buf[i*4] != 0xA0 || buf[i*4+1] != 0xA0 || buf[i*4+2] != 0xA0 || buf[i*4+3] != 0xA0 {
			t.Fatalf("pixel %d = %x, want a0 a0 a0 a0", i, buf[i*4:i*4+4])
		}
	}
}

// TestCompositeReplWrapAndClip: a 2-pixel replicated source (red, green)
// tiles across the destination while the destination clipr (via 'c') cuts
// the drawn area; the source offset sp=(1,0) shifts the phase.
func TestCompositeReplWrapAndClip(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	wr(t, data, msg(byte('b'), uint32(1), uint32(0), byte(0), uint32(ChanRGB24), byte(1),
		rect{0, 0, 2, 1}, huge, uint32(DBlack)))
	wr(t, data, msg(byte('y'), uint32(1), rect{0, 0, 2, 1}, []byte{0x00, 0x00, 0xFF, 0x00, 0xFF, 0x00}))
	wr(t, data, msg(byte('b'), uint32(2), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 4, 1}, rect{0, 0, 4, 1}, uint32(DBlack)))
	wr(t, data, msg(byte('c'), uint32(2), byte(0), rect{1, 0, 3, 1}))
	allocOpaque(t, data, 3, ChanGREY1, DWhite)
	wr(t, data, msg(byte('d'), uint32(2), uint32(1), uint32(3),
		rect{0, 0, 4, 1}, pt{1, 0}, pt{0, 0}))
	wr(t, data, msg(byte('r'), uint32(2), rect{0, 0, 4, 1}))
	buf := make([]byte, 12)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	want := [][]byte{{0, 0, 0}, {0x00, 0x00, 0xFF}, {0x00, 0xFF, 0x00}, {0, 0, 0}}
	for i, w := range want {
		if string(buf[i*3:i*3+3]) != string(w) {
			t.Fatalf("pixel %d = %x, want %x", i, buf[i*3:i*3+3], w)
		}
	}
}

// TestCompositeNonReplSrcOutside: with a non-replicated source, destination
// pixels whose source sample falls outside src.r are untouched (drawclip
// shrinks the rect).
func TestCompositeNonReplSrcOutside(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	wr(t, data, msg(byte('b'), uint32(1), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 1, 1}, rect{0, 0, 1, 1}, uint32(DRed)))
	wr(t, data, msg(byte('b'), uint32(2), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 3, 1}, rect{0, 0, 3, 1}, uint32(DBlack)))
	allocOpaque(t, data, 3, ChanGREY1, DWhite)
	wr(t, data, msg(byte('d'), uint32(2), uint32(1), uint32(3),
		rect{0, 0, 3, 1}, pt{0, 0}, pt{0, 0}))
	wr(t, data, msg(byte('r'), uint32(2), rect{0, 0, 3, 1}))
	buf := make([]byte, 9)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	if buf[0] != 0 || buf[1] != 0 || buf[2] != 0xFF {
		t.Fatalf("pixel 0 = %x, want red", buf[0:3])
	}
	for i := 1; i < 3; i++ {
		if buf[i*3] != 0 || buf[i*3+1] != 0 || buf[i*3+2] != 0 {
			t.Fatalf("pixel %d = %x, want untouched black", i, buf[i*3:i*3+3])
		}
	}
}

// TestCompositeNTSCMask: a color mask without an alpha channel contributes
// its NTSC grey as the mask alpha (draw(2)); pure red is rgb2k(255,0,0)=76.
func TestCompositeNTSCMask(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	allocOpaque(t, data, 1, ChanRGB24, DWhite)
	// red 1x1 repl mask: bytes blue,green,red
	wr(t, data, msg(byte('b'), uint32(2), uint32(0), byte(0), uint32(ChanRGB24), byte(1),
		rect{0, 0, 1, 1}, huge, uint32(0)))
	wr(t, data, msg(byte('y'), uint32(2), rect{0, 0, 1, 1}, []byte{0x00, 0x00, 0xFF}))
	wr(t, data, msg(byte('b'), uint32(3), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 1, 1}, rect{0, 0, 1, 1}, uint32(DBlack)))
	wr(t, data, msg(byte('d'), uint32(3), uint32(1), uint32(2),
		rect{0, 0, 1, 1}, pt{0, 0}, pt{0, 0}))
	wr(t, data, msg(byte('r'), uint32(3), rect{0, 0, 1, 1}))
	buf := make([]byte, 3)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	if buf[0] != 76 || buf[1] != 76 || buf[2] != 76 {
		t.Fatalf("pixel = %x, want grey 4c (NTSC red)", buf)
	}
}

// TestCompositeSoverDDrawMaskEquivalent checks the sanctioned equivalence
// with image/draw.DrawMask on identical premultiplied RGBA inputs. Both
// implement Porter-Duff source-over; the pinned memdraw rounds at 8-bit
// precision (MUL), image/draw at 16-bit, so per-channel difference is
// tolerance 1. Exact expected values are derived from the memdraw formulas
// and asserted first.
func TestCompositeSoverDDrawMaskEquivalent(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	// src 2x1 non-repl r8g8b8a8: p0=(7f,40,20,7f), p1=(30,60,90,ff) premult
	wr(t, data, msg(byte('b'), uint32(1), uint32(0), byte(0), uint32(ChanRGBA32), byte(0),
		rect{0, 0, 2, 1}, rect{0, 0, 2, 1}, uint32(0)))
	wr(t, data, msg(byte('y'), uint32(1), rect{0, 0, 2, 1},
		[]byte{0x7F, 0x20, 0x40, 0x7F, 0xFF, 0x90, 0x60, 0x30}))
	// k8 mask 2x1: 80, 40
	wr(t, data, msg(byte('b'), uint32(2), uint32(0), byte(0), uint32(ChanGREY8), byte(0),
		rect{0, 0, 2, 1}, rect{0, 0, 2, 1}, uint32(0)))
	wr(t, data, msg(byte('y'), uint32(2), rect{0, 0, 2, 1}, []byte{0x80, 0x40}))
	// dst 2x1 r8g8b8a8 premult (20,30,40,80)
	wr(t, data, msg(byte('b'), uint32(3), uint32(0), byte(0), uint32(ChanRGBA32), byte(0),
		rect{0, 0, 2, 1}, rect{0, 0, 2, 1}, uint32(0x20304080)))
	wr(t, data, msg(byte('d'), uint32(3), uint32(1), uint32(2),
		rect{0, 0, 2, 1}, pt{0, 0}, pt{0, 0}))
	wr(t, data, msg(byte('r'), uint32(3), rect{0, 0, 2, 1}))
	got := make([]byte, 8)
	if _, err := data.Read(got); err != nil {
		t.Fatal(err)
	}
	// memdraw math:
	// p0: fd=255-MUL(127,128)=191; r=MUL(128,127)+MUL(191,32)=64+24=88;
	//     g=32+36=68; b=16+48=64; a=64+96=160
	// p1: fd=191; r=12+24=36; g=24+36=60; b=36+48=84; a=64+96=160
	want := []byte{0xA0, 0x40, 0x44, 0x58, 0xA0, 0x54, 0x3C, 0x24}
	if string(got) != string(want) {
		t.Fatalf("device = %x, want %x (memdraw formulas)", got, want)
	}

	// same draw through image/draw.DrawMask
	simg := image.NewRGBA(image.Rect(0, 0, 2, 1))
	simg.SetRGBA(0, 0, color.RGBA{0x7F, 0x40, 0x20, 0x7F})
	simg.SetRGBA(1, 0, color.RGBA{0x30, 0x60, 0x90, 0xFF})
	mimg := image.NewAlpha(image.Rect(0, 0, 2, 1))
	mimg.SetAlpha(0, 0, color.Alpha{0x80})
	mimg.SetAlpha(1, 0, color.Alpha{0x40})
	dimg := image.NewRGBA(image.Rect(0, 0, 2, 1))
	dimg.SetRGBA(0, 0, color.RGBA{0x20, 0x30, 0x40, 0x80})
	dimg.SetRGBA(1, 0, color.RGBA{0x20, 0x30, 0x40, 0x80})
	draw.DrawMask(dimg, dimg.Bounds(), simg, image.Pt(0, 0), mimg, image.Pt(0, 0), draw.Over)
	for i := 0; i < 2; i++ {
		r16, g16, b16, a16 := dimg.RGBAAt(i, 0).RGBA()
		// device stores bytes a,b,g,r; compare channel-wise at 8-bit precision
		dev := [4]uint32{uint32(got[i*4+3]), uint32(got[i*4+2]), uint32(got[i*4+1]), uint32(got[i*4])}
		ref := [4]uint32{r16 >> 8, g16 >> 8, b16 >> 8, a16 >> 8}
		for c := 0; c < 4; c++ {
			if diff := int(dev[c]) - int(ref[c]); diff < -1 || diff > 1 {
				t.Errorf("pixel %d channel %d: device %d, image/draw %d", i, c, dev[c], ref[c])
			}
		}
	}
}

// TestCompositeSrcIsDst: drawing an image onto itself uses the pre-draw
// source pixels (memdraw buffers same-bitmap draws).
func TestCompositeSrcIsDst(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	// 3x3 dst: corners red, green, blue, white; draw (1,1)-(3,3) from (0,0)
	wr(t, data, msg(byte('b'), uint32(1), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 3, 3}, rect{0, 0, 3, 3}, uint32(DBlack)))
	wr(t, data, msg(byte('y'), uint32(1), rect{0, 0, 1, 1}, []byte{0x00, 0x00, 0xFF}))
	wr(t, data, msg(byte('y'), uint32(1), rect{1, 0, 2, 1}, []byte{0x00, 0xFF, 0x00}))
	wr(t, data, msg(byte('y'), uint32(1), rect{0, 1, 1, 2}, []byte{0xFF, 0x00, 0x00}))
	allocOpaque(t, data, 2, ChanGREY1, DWhite)
	wr(t, data, msg(byte('d'), uint32(1), uint32(1), uint32(2),
		rect{1, 1, 3, 3}, pt{0, 0}, pt{0, 0}))
	wr(t, data, msg(byte('r'), uint32(1), rect{0, 0, 3, 3}))
	buf := make([]byte, 27)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	// expected: (1,1)<-(0,0) red; (2,1)<-(1,0) green; (1,2)<-(0,1) blue;
	// (2,2)<-(1,1) ORIGINAL black (a naive in-place loop would see the
	// already-written red at (1,1))
	want := map[[2]int][]byte{
		{1, 1}: {0x00, 0x00, 0xFF}, {2, 1}: {0x00, 0xFF, 0x00},
		{1, 2}: {0xFF, 0x00, 0x00}, {2, 2}: {0x00, 0x00, 0x00},
		{0, 0}: {0x00, 0x00, 0xFF}, {1, 0}: {0x00, 0xFF, 0x00}, {0, 1}: {0xFF, 0x00, 0x00},
	}
	for p, w := range want {
		i := (p[1]*3 + p[0]) * 3
		if string(buf[i:i+3]) != string(w) {
			t.Fatalf("pixel %v = %x, want %x", p, buf[i:i+3], w)
		}
	}
}

// stringTestFont builds the wire sequence of the pinned default-font path
// (getdefont's 'b'+'y' for subfont bits, buildfont's 'b' cache image and
// 'i', loadchar's 'l' per glyph): a k8 font with two 1x1 glyphs whose mask
// greys are grey0 and grey1, both advancing one pixel.
func stringTestFont(t *testing.T, data fs.File, cacheID, bitsID uint32, grey0, grey1 byte) {
	t.Helper()
	// subfont bits: 'b' + 'y' (getdefont)
	wr(t, data, msg(byte('b'), bitsID, uint32(0), byte(0), uint32(ChanGREY8), byte(0),
		rect{0, 0, 2, 1}, rect{0, 0, 2, 1}, uint32(0)))
	wr(t, data, msg(byte('y'), bitsID, rect{0, 0, 2, 1}, []byte{grey0, grey1}))
	// cache image + font init ('b' + 'i', buildfont/fontresize)
	wr(t, data, msg(byte('b'), cacheID, uint32(0), byte(0), uint32(ChanGREY8), byte(0),
		rect{0, 0, 2, 1}, rect{0, 0, 2, 1}, uint32(0)))
	wr(t, data, msg(byte('i'), cacheID, uint32(2), byte(1)))
	// load both glyphs ('l', loadchar)
	wr(t, data, msg(byte('l'), cacheID, bitsID, uint16(0), rect{0, 0, 1, 1}, pt{0, 0}, byte(0), byte(1)))
	wr(t, data, msg(byte('l'), cacheID, bitsID, uint16(1), rect{1, 0, 2, 1}, pt{1, 0}, byte(0), byte(1)))
}

// TestStringK8Font: white text through a k8 font; glyph 0 has full opacity
// (mask 0xff) and glyph 1 half (0x80): SoverD onto black gives 0xff and
// MUL(128,255) = 0x80.
func TestStringK8Font(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	stringTestFont(t, data, 4, 5, 0xFF, 0x80)
	allocOpaque(t, data, 7, ChanGREY1, DWhite)
	wr(t, data, msg(byte('b'), uint32(6), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 2, 1}, rect{0, 0, 2, 1}, uint32(DBlack)))
	wr(t, data, msg(byte('s'), uint32(6), uint32(7), uint32(4),
		pt{0, 1}, rect{0, 0, 2, 1}, pt{0, 0}, uint16(2), []byte{0, 0, 1, 0}))
	wr(t, data, msg(byte('r'), uint32(6), rect{0, 0, 2, 1}))
	buf := make([]byte, 6)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	if buf[0] != 0xFF || buf[1] != 0xFF || buf[2] != 0xFF {
		t.Fatalf("glyph 0 = %x, want ff ff ff", buf[0:3])
	}
	if buf[3] != 0x80 || buf[4] != 0x80 || buf[5] != 0x80 {
		t.Fatalf("glyph 1 = %x, want 80 80 80", buf[3:6])
	}
}

// TestStringK1FontPlacement: a k1 glyph with left=-1 and the pen at the
// baseline (p.y=1, ascent=1) paints its bits starting one pixel left of
// and one row above the pen.
func TestStringK1FontPlacement(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	// subfont bits 2x2 k1: rows 10, 11 (bits within a byte are big-endian,
	// leftmost pixel is the high bit)
	wr(t, data, msg(byte('b'), uint32(5), uint32(0), byte(0), uint32(ChanGREY1), byte(0),
		rect{0, 0, 2, 2}, rect{0, 0, 2, 2}, uint32(0)))
	wr(t, data, msg(byte('y'), uint32(5), rect{0, 0, 2, 2}, []byte{0x80, 0xC0}))
	wr(t, data, msg(byte('b'), uint32(4), uint32(0), byte(0), uint32(ChanGREY1), byte(0),
		rect{0, 0, 2, 2}, rect{0, 0, 2, 2}, uint32(0)))
	wr(t, data, msg(byte('i'), uint32(4), uint32(1), byte(1)))
	wr(t, data, msg(byte('l'), uint32(4), uint32(5), uint16(0), rect{0, 0, 2, 2}, pt{0, 0}, byte(0xFF), byte(3)))
	allocOpaque(t, data, 7, ChanGREY1, DBlack)
	wr(t, data, msg(byte('b'), uint32(6), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 4, 2}, rect{0, 0, 4, 2}, uint32(DWhite)))
	wr(t, data, msg(byte('s'), uint32(6), uint32(7), uint32(4),
		pt{1, 1}, rect{0, 0, 4, 2}, pt{0, 0}, uint16(1), []byte{0, 0}))
	wr(t, data, msg(byte('r'), uint32(6), rect{0, 0, 4, 2}))
	buf := make([]byte, 24)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	// glyph box (0,0)-(2,2); mask bits 1,0 / 1,1 over white dst with black src
	want := []byte{
		0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
		0, 0, 0, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
	}
	if string(buf) != string(want) {
		t.Fatalf("pixels = %x, want %x", buf, want)
	}
}

// TestStringClipRect confines glyphs to the message clipr (which replaces
// the destination clipr for the duration of the 's').
func TestStringClipRect(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	wr(t, data, msg(byte('b'), uint32(5), uint32(0), byte(0), uint32(ChanGREY1), byte(0),
		rect{0, 0, 2, 2}, rect{0, 0, 2, 2}, uint32(0)))
	wr(t, data, msg(byte('y'), uint32(5), rect{0, 0, 2, 2}, []byte{0xC0, 0xC0}))
	wr(t, data, msg(byte('b'), uint32(4), uint32(0), byte(0), uint32(ChanGREY1), byte(0),
		rect{0, 0, 2, 2}, rect{0, 0, 2, 2}, uint32(0)))
	wr(t, data, msg(byte('i'), uint32(4), uint32(1), byte(1)))
	wr(t, data, msg(byte('l'), uint32(4), uint32(5), uint16(0), rect{0, 0, 2, 2}, pt{0, 0}, byte(0), byte(2)))
	allocOpaque(t, data, 7, ChanGREY1, DBlack)
	wr(t, data, msg(byte('b'), uint32(6), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 2, 2}, rect{0, 0, 2, 2}, uint32(DWhite)))
	// clip to the right column only
	wr(t, data, msg(byte('s'), uint32(6), uint32(7), uint32(4),
		pt{0, 1}, rect{1, 0, 2, 2}, pt{0, 0}, uint16(1), []byte{0, 0}))
	wr(t, data, msg(byte('r'), uint32(6), rect{0, 0, 2, 2}))
	buf := make([]byte, 12)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0xFF, 0xFF, 0xFF, 0, 0, 0,
		0xFF, 0xFF, 0xFF, 0, 0, 0,
	}
	if string(buf) != string(want) {
		t.Fatalf("pixels = %x, want %x", buf, want)
	}
}

// TestStringBgPaint: 'x' paints the background under the full string extent
// first (op SoverD, opaque), then the glyphs; glyph 1 composites white at
// half opacity over the blue background: r=g=MUL(128,255)=0x80,
// b=MUL(128,255)+MUL(127,255)=0xff.
func TestStringBgPaint(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	stringTestFont(t, data, 4, 5, 0xFF, 0x80)
	allocOpaque(t, data, 7, ChanGREY1, DWhite)
	allocOpaque(t, data, 8, ChanRGB24, 0x0000FFFF) // blue
	wr(t, data, msg(byte('b'), uint32(6), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 2, 1}, rect{0, 0, 2, 1}, uint32(DBlack)))
	wr(t, data, msg(byte('x'), uint32(6), uint32(7), uint32(4),
		pt{0, 1}, rect{0, 0, 2, 1}, pt{0, 0}, uint16(2), uint32(8), pt{0, 0}, []byte{0, 0, 1, 0}))
	wr(t, data, msg(byte('r'), uint32(6), rect{0, 0, 2, 1}))
	buf := make([]byte, 6)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	if buf[0] != 0xFF || buf[1] != 0xFF || buf[2] != 0xFF {
		t.Fatalf("glyph 0 = %x, want ff ff ff", buf[0:3])
	}
	if buf[3] != 0xFF || buf[4] != 0x80 || buf[5] != 0x80 {
		t.Fatalf("glyph 1 = %x, want b=ff g=80 r=80", buf[3:6])
	}
}

// TestStringErrors covers devdraw's diagnostics for the font ops.
func TestStringErrors(t *testing.T) {
	dev := New()
	ctl, data, _ := openClient(t, dev)
	defer ctl.Close()
	defer data.Close()

	stringTestFont(t, data, 4, 5, 0xFF, 0xFF)
	allocOpaque(t, data, 7, ChanGREY1, DWhite)
	wr(t, data, msg(byte('b'), uint32(6), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 2, 1}, rect{0, 0, 2, 1}, uint32(DBlack)))

	// not a font
	if _, err := fs.Write(data, msg(byte('s'), uint32(6), uint32(7), uint32(7),
		pt{0, 1}, rect{0, 0, 2, 1}, pt{0, 0}, uint16(1), []byte{0, 0})); err == nil {
		t.Fatal("string from non-font accepted")
	}
	// index out of range on 's': glyph 0 is drawn, then the error
	if _, err := fs.Write(data, msg(byte('s'), uint32(6), uint32(7), uint32(4),
		pt{0, 1}, rect{0, 0, 2, 1}, pt{0, 0}, uint16(2), []byte{0, 0, 2, 0})); err == nil {
		t.Fatal("out-of-range cache index accepted")
	}
	wr(t, data, msg(byte('r'), uint32(6), rect{0, 0, 2, 1}))
	buf := make([]byte, 6)
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	if buf[0] != 0xFF || buf[1] != 0xFF || buf[2] != 0xFF {
		t.Fatalf("glyph before bad index = %x, want ff ff ff", buf[0:3])
	}
	// index out of range on 'x': nothing is drawn (bg validated first)
	wr(t, data, msg(byte('f'), uint32(6)))
	wr(t, data, msg(byte('b'), uint32(6), uint32(0), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 2, 1}, rect{0, 0, 2, 1}, uint32(DBlack)))
	allocOpaque(t, data, 8, ChanRGB24, 0x0000FFFF)
	if _, err := fs.Write(data, msg(byte('x'), uint32(6), uint32(7), uint32(4),
		pt{0, 1}, rect{0, 0, 2, 1}, pt{0, 0}, uint16(2), uint32(8), pt{0, 0}, []byte{0, 0, 2, 0})); err == nil {
		t.Fatal("out-of-range cache index accepted on x")
	}
	wr(t, data, msg(byte('r'), uint32(6), rect{0, 0, 2, 1}))
	if _, err := data.Read(buf); err != nil {
		t.Fatal(err)
	}
	for i, b := range buf {
		if b != 0 {
			t.Fatalf("pixel %d = %x after failed x, want untouched black", i, b)
		}
	}
	// display image cannot be a font
	if _, err := fs.Write(data, msg(byte('i'), uint32(0), uint32(1), byte(1))); err == nil {
		t.Fatal("display as font accepted")
	}
	// font size bounds
	if _, err := fs.Write(data, msg(byte('i'), uint32(6), uint32(0), byte(1))); err == nil {
		t.Fatal("zero-char font accepted")
	}
	if _, err := fs.Write(data, msg(byte('i'), uint32(6), uint32(4097), byte(1))); err == nil {
		t.Fatal("4097-char font accepted")
	}
	// 'l' with bad index
	if _, err := fs.Write(data, msg(byte('l'), uint32(4), uint32(5), uint16(2),
		rect{0, 0, 1, 1}, pt{0, 0}, byte(0), byte(1))); err == nil {
		t.Fatal("'l' with out-of-range index accepted")
	}
	// 'i' on a window image
	wr(t, data, msg(byte('A'), uint32(1), uint32(0), uint32(7), byte(0)))
	wr(t, data, msg(byte('b'), uint32(9), uint32(1), byte(0), uint32(ChanRGB24), byte(0),
		rect{0, 0, 4, 4}, rect{0, 0, 4, 4}, uint32(DBlack)))
	if _, err := fs.Write(data, msg(byte('i'), uint32(9), uint32(1), byte(1))); err == nil {
		t.Fatal("window as font accepted")
	}
}
