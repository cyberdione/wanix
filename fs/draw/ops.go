package draw

import (
	"fmt"
)

// execMessages parses and executes complete messages from c.msgbuf, leaving
// any trailing partial message buffered for the next write. Message layouts
// follow the devdraw.c drawmesg cases; multibyte integers are little-endian
// (BPLONG/BPSHORT in draw.h).
func (c *client) execMessages() error {
	for len(c.msgbuf) > 0 {
		m, err := c.messageLen(c.msgbuf)
		if err != nil {
			return err
		}
		if m < 0 {
			if len(c.msgbuf) > c.dev.maxMessage() {
				return fmt.Errorf("draw: message too large")
			}
			return nil // incomplete; wait for more
		}
		msg := c.msgbuf[:m]
		if err := c.exec(msg); err != nil {
			return fmt.Errorf("draw %q: %w", string(msg[0]), err)
		}
		c.msgbuf = c.msgbuf[m:]
	}
	return nil
}

// messageLen returns the total length of the first message in buf, or -1 if
// more bytes are needed. Unknown opcodes are an immediate error.
func (c *client) messageLen(buf []byte) (int, error) {
	op := buf[0]
	switch op {
	// fixed-length messages
	case 'A':
		return need(buf, 1+4+4+4+1)
	case 'b':
		return need(buf, 1+4+4+1+4+1+4*4+4*4+4)
	case 'c':
		return need(buf, 1+4+1+4*4)
	case 'd':
		return need(buf, 1+4+4+4+4*4+2*4+2*4)
	case 'f', 'F':
		return need(buf, 1+4)
	case 'O':
		return need(buf, 1+1)
	case 'r':
		return need(buf, 1+4+4*4)
	case 'v':
		return 1, nil
	// variable-length messages
	case 'n':
		if len(buf) < 6 {
			return -1, nil
		}
		if j := int(buf[5]); j == 0 {
			return 0, ErrShortDraw
		} else {
			return need(buf, 6+j)
		}
	case 'N':
		if len(buf) < 7 {
			return -1, nil
		}
		if j := int(buf[6]); j == 0 {
			return 0, ErrShortDraw
		} else {
			return need(buf, 7+j)
		}
	case 'y':
		if len(buf) < 21 {
			return -1, nil
		}
		img := c.images[bgLong(buf[1:])]
		if img == nil {
			return 0, ErrNoImage
		}
		r := bgRect(buf[5:])
		if !r.In(img.r) {
			return 0, fmt.Errorf("writeimage outside image")
		}
		return need(buf, 21+bytesPerLine(r, img.depth)*r.Dy())
	}
	return 0, fmt.Errorf("%w: %#x", ErrBadCommand, op)
}

func need(buf []byte, m int) (int, error) {
	if len(buf) < m {
		return -1, nil
	}
	return m, nil
}

// exec executes one complete message. Caller holds c.mu.
func (c *client) exec(msg []byte) error {
	switch msg[0] {
	case 'A':
		// allocate screen: id[4] imageid[4] fillid[4] public[1]
		id := bgLong(msg[1:])
		if id == 0 {
			return fmt.Errorf("bad screen id")
		}
		if c.screens[id] != nil {
			return ErrScreenExists
		}
		if c.images[bgLong(msg[5:])] == nil || c.images[bgLong(msg[9:])] == nil {
			return ErrNoImage
		}
		c.screens[id] = &screen{id: id, imageid: bgLong(msg[5:]), fillid: bgLong(msg[9:]), public: msg[13] != 0}
		return nil

	case 'b':
		// allocate image: id[4] screenid[4] refresh[1] chan[4] repl[1] r[16] clipr[16] color[4]
		id := bgLong(msg[1:])
		scrnid := bgLong(msg[5:])
		refresh := msg[9]
		chanFmt := bgLong(msg[10:])
		repl := msg[14]
		r := bgRect(msg[15:])
		clipr := bgRect(msg[31:])
		color := bgLong(msg[47:])
		if c.images[id] != nil {
			return ErrImageExists
		}
		if repl != 0 && repl != 1 {
			return fmt.Errorf("bad repl flag")
		}
		if refresh > Refmesg {
			return fmt.Errorf("unknown refresh method")
		}
		if r.Dx() < 0 || r.Dy() < 0 || r.Dx() > 1<<15 || r.Dy() > 1<<15 ||
			r.Dx()*r.Dy() > c.dev.maxPixels() {
			return ErrSaturated
		}
		if len(c.images) >= c.dev.maxImages() {
			return ErrSaturated
		}
		if _, err := chanDesc(chanFmt); err != nil {
			return err
		}
		if scrnid != 0 {
			// Window image on a screen: this slice has no layer compositor,
			// so it is allocated as a plain image with the kernel's
			// compatibility constraints (devdraw.c case 'b').
			scr := c.screens[scrnid]
			if scr == nil {
				return fmt.Errorf("unknown screen id")
			}
			simg := c.images[scr.imageid]
			if repl == 1 || chanFmt != simg.chanFmt {
				return fmt.Errorf("image parameters incompatible with screen")
			}
		}
		if clipr.Empty() {
			clipr = r
		}
		if repl == 0 {
			clipr = clipr.Intersect(r)
		}
		img := newImage(id, chanFmt, repl == 1, r, clipr, color)
		img.screen = scrnid
		img.refresh = refresh
		c.images[id] = img
		return nil

	case 'c':
		// set repl and clip: id[4] repl[1] clipr[16]
		img := c.images[bgLong(msg[1:])]
		if img == nil {
			return ErrNoImage
		}
		if img.name != "" {
			return fmt.Errorf("cannot change repl/clipr of shared image")
		}
		if msg[5] != 0 {
			img.repl = true
		}
		img.clipr = bgRect(msg[6:])
		return nil

	case 'd':
		// draw: dst[4] src[4] mask[4] dstr[16] srcp[8] maskp[8]
		dst := c.images[bgLong(msg[1:])]
		src := c.images[bgLong(msg[5:])]
		mask := c.images[bgLong(msg[9:])]
		if dst == nil || src == nil || mask == nil {
			return ErrNoImage
		}
		switch c.op {
		case OpClear, OpS, OpSoverD:
		default:
			return fmt.Errorf("unsupported draw op %d", c.op)
		}
		drawBlit(dst, src, bgRect(msg[13:]), bgPoint(msg[29:]), c.op)
		return nil

	case 'f':
		// free image: id[4]
		id := bgLong(msg[1:])
		img := c.images[id]
		if img == nil {
			return ErrNoImage
		}
		if img.name != "" {
			c.dev.mu.Lock()
			delete(c.dev.names, img.name)
			c.dev.mu.Unlock()
		}
		delete(c.images, id)
		return nil

	case 'F':
		// free screen: id[4]
		id := bgLong(msg[1:])
		if c.screens[id] == nil {
			return fmt.Errorf("unknown screen id")
		}
		delete(c.screens, id)
		return nil

	case 'N':
		// name an image: id[4] in[1] j[1] name[j]
		in := msg[5] != 0
		j := int(msg[6])
		name := string(msg[7 : 7+j])
		img := c.images[bgLong(msg[1:])]
		if img == nil {
			return ErrNoImage
		}
		c.dev.mu.Lock()
		defer c.dev.mu.Unlock()
		if in {
			if img.name != "" {
				return ErrNamed
			}
			if c.dev.names[name] != nil {
				return ErrNamed
			}
			img.name = name
			c.dev.names[name] = img
			return nil
		}
		if c.dev.names[name] != img {
			return ErrWrongName
		}
		img.name = ""
		delete(c.dev.names, name)
		return nil

	case 'n':
		// attach named image: id[4] j[1] name[j]
		j := int(msg[5])
		name := string(msg[6 : 6+j])
		id := bgLong(msg[1:])
		if c.images[id] != nil {
			return ErrImageExists
		}
		c.dev.mu.Lock()
		img := c.dev.names[name]
		c.dev.mu.Unlock()
		if img == nil {
			return ErrNoName
		}
		c.images[id] = img
		c.infoid = int32(id)
		c.infoSet = true
		return nil

	case 'O':
		// set compositing operator: op[1]
		c.op = int(msg[1])
		return nil

	case 'r':
		// read pixels: id[4] r[16]; next data-file reads return the pixels
		img := c.images[bgLong(msg[1:])]
		if img == nil {
			return ErrNoImage
		}
		data, err := img.unload(bgRect(msg[5:]))
		if err != nil {
			return err
		}
		c.readbuf = data
		return nil

	case 'v':
		// flush: all operations are applied eagerly; nothing to do
		return nil

	case 'y':
		// load pixels: id[4] r[16] data[x]
		img := c.images[bgLong(msg[1:])]
		if img == nil {
			return ErrNoImage
		}
		_, err := img.load(bgRect(msg[5:]), msg[21:])
		return err
	}
	return ErrBadCommand
}
