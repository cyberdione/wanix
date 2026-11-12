package draw

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
)

// ctlFile is the per-client control file. Opening "new" returns the client's
// ctl file directly, per devdraw.c (the fd from opening /dev/draw/new IS the
// ctl file). A read returns exactly 144 bytes: 12 right-justified fields of
// 11 chars each followed by a blank — connection id, image id, channel
// string, repl flag, image rect, clip rect (devdraw.c format
// "%11d %11d %11s %11d %11d %11d %11d %11d %11d %11d %11d %11d "). A 4-byte
// binary write sets the image id reported by the next read. After a read the
// info is consumed; a second read without a new infoid fails, as in the
// kernel (infoid reset to -1).
type ctlFile struct {
	client *client

	mu     sync.Mutex
	closed bool
}

func (f *ctlFile) Stat() (fs.FileInfo, error) { return fskit.Entry("ctl", 0666), nil }

func (f *ctlFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	c := f.client
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.infoSet {
		return 0, ErrNoImage
	}
	img := c.images[uint32(c.infoid)]
	if img == nil {
		return 0, ErrNoImage
	}
	if len(p) < 12*12 {
		return 0, fmt.Errorf("short read")
	}
	repl := 0
	if img.repl {
		repl = 1
	}
	s := fmt.Sprintf("%11d %11d %11s %11d %11d %11d %11d %11d %11d %11d %11d %11d ",
		c.id, c.infoid, img.chanStr, repl,
		img.r.Min.X, img.r.Min.Y, img.r.Max.X, img.r.Max.Y,
		img.clipr.Min.X, img.clipr.Min.Y, img.clipr.Max.X, img.clipr.Max.Y)
	c.infoSet = false
	return copy(p, s), nil
}

func (f *ctlFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	if len(p) != 4 {
		return 0, fmt.Errorf("unknown draw control request")
	}
	c := f.client
	c.mu.Lock()
	defer c.mu.Unlock()
	c.infoid = int32(bgLong(p))
	c.infoSet = true
	return 4, nil
}

func (f *ctlFile) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	f.mu.Unlock()
	f.client.release()
	return nil
}

// dataFile is the per-client message queue. Writes are buffered until a
// complete message is present; malformed or unsupported messages fail the
// write (draw(3) DIAGNOSTICS). Reads return pixel data queued by 'r'.
type dataFile struct {
	client *client

	mu     sync.Mutex
	closed bool
}

func (f *dataFile) Stat() (fs.FileInfo, error) { return fskit.Entry("data", 0666), nil }

func (f *dataFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	c := f.client
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.msgbuf)+len(p) > c.dev.maxMessage() {
		return 0, fmt.Errorf("draw: message too large")
	}
	c.msgbuf = append(c.msgbuf, p...)
	err := c.execMessages()
	if err != nil {
		c.msgbuf = nil
		return 0, err
	}
	return len(p), nil
}

func (f *dataFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	c := f.client
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.readbuf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.readbuf)
	c.readbuf = c.readbuf[n:]
	return n, nil
}

func (f *dataFile) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	f.mu.Unlock()
	f.client.release()
	return nil
}

// refreshFile serves queued 60-byte refresh records (5 ctl-style ints:
// image id + rectangle). This slice never generates refresh notifications;
// reads return EOF when the queue is empty rather than blocking.
type refreshFile struct {
	client *client

	mu     sync.Mutex
	closed bool
	buf    bytes.Buffer
}

func (f *refreshFile) Stat() (fs.FileInfo, error) { return fskit.Entry("refresh", 0444), nil }

func (f *refreshFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	if f.buf.Len() == 0 {
		c := f.client
		c.mu.Lock()
		for _, rec := range c.refresh {
			f.buf.Write(rec)
		}
		c.refresh = nil
		c.mu.Unlock()
	}
	if f.buf.Len() == 0 {
		return 0, io.EOF
	}
	return f.buf.Read(p)
}

func (f *refreshFile) Write(p []byte) (int, error) { return 0, fs.ErrPermission }

func (f *refreshFile) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	f.mu.Unlock()
	f.client.release()
	return nil
}

// colormapFile serves the device color map. Reads render 256 lines of
// "%11d %11d %11d %11d\n" (index, red, green, blue, values replicated to 32
// bits as in devdraw.c). Writes accept lines of the same form.
type colormapFile struct {
	dev    *Device
	client *client

	mu     sync.Mutex
	closed bool
	off    int
	wbuf   []byte
}

func (f *colormapFile) Stat() (fs.FileInfo, error) { return fskit.Entry("colormap", 0666), nil }

func (f *colormapFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	f.dev.mu.Lock()
	var sb strings.Builder
	for i, c := range f.dev.cmap {
		fmt.Fprintf(&sb, "%11d %11d %11d %11d\n", i,
			uint32(c[0])*0x01010101, uint32(c[1])*0x01010101, uint32(c[2])*0x01010101)
	}
	f.dev.mu.Unlock()
	data := sb.String()
	if f.off >= len(data) {
		return 0, io.EOF
	}
	n := copy(p, data[f.off:])
	f.off += n
	return n, nil
}

func (f *colormapFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, fs.ErrClosed
	}
	f.wbuf = append(f.wbuf, p...)
	for {
		i := bytes.IndexByte(f.wbuf, '\n')
		if i < 0 {
			break
		}
		line := string(f.wbuf[:i])
		f.wbuf = f.wbuf[i+1:]
		var idx, r, g, b uint64
		if _, err := fmt.Sscanf(line, "%d %d %d %d", &idx, &r, &g, &b); err != nil {
			return 0, fmt.Errorf("bad colormap entry %q", line)
		}
		if idx > 255 {
			return 0, fmt.Errorf("bad color map index")
		}
		f.dev.mu.Lock()
		f.dev.cmap[idx] = [3]byte{byte(r >> 24), byte(g >> 24), byte(b >> 24)}
		f.dev.mu.Unlock()
	}
	return len(p), nil
}

func (f *colormapFile) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	f.mu.Unlock()
	f.client.release()
	return nil
}
