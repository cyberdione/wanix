package draw

import (
	"context"
	"encoding/binary"
	"fmt"
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
		rect{0, 0, 1, 1}, rect{0, 0, 1, 1}, uint32(DWhite)))
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
	if _, err := fs.Write(data, msg(byte('s'))); err == nil {
		t.Fatal("unsupported opcode s accepted")
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
		rect{0, 0, 1, 1}, rect{0, 0, 1, 1}, uint32(DWhite)))
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
