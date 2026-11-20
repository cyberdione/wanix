// Package draw implements a bounded slice of the Plan 9 draw(3) device
// protocol as a Wanix kernel filesystem device.
//
// Byte layouts and semantics are taken from the pinned 4th-edition sources
// (mirror github.com/0intro/plan9 commit 0aab9506bfc2acb5203a411b917cea62cf3eeb91):
//   - sys/src/9/port/devdraw.c (opcode dispatch, ctl format, IOUNIT)
//   - sys/include/draw.h (BPLONG/BPSHORT little-endian, channel packing,
//     Porter-Duff op values, color constants)
//   - draw(3) 4th-edition manual
//
// Protocol generation: this device serves the 4th-edition generation only:
// ctl reads are always 144 bytes (12 fields x 12 chars, including the repl
// field that draw(3) prose omits) and the 'v' flush message is 1 byte. The
// newer-generation 4-byte screen id suffix on 'v' (_isnewdisplay) is not
// served.
//
// Supported data-file opcodes: A b c d f F i l N n O r s v x y.
// All other opcodes are rejected with an explicit error.
//
// Explicit limitations of this slice:
//   - Channel formats are limited to GREY1 (k1), GREY8 (k8), RGB24 (r8g8b8),
//     RGBA32 (r8g8b8a8) and ALPHA8 (a8). Channel descriptor values follow
//     the pinned strtochan (r8g8b8 == 0x081828, r8g8b8a8 == 0x08182848,
//     a8 == 0x48); alpha-bearing pixels are stored premultiplied per
//     allocimage(2) and memdraw.
//   - 'd' implements the Porter-Duff ops Clear, S and SoverD with real
//     alpha compositing (premultiplied channels, memdraw rounding) and
//     per-pixel masks (grey or alpha masks); other Porter-Duff ops are
//     rejected.
//   - 'b' with a screen id allocates a plain image (no layer/window
//     compositing); refresh methods are recorded but produce no notifications.
//   - refresh file reads return EOF when empty instead of blocking.
//   - Strings ('s'/'x') draw from a font cache image initialized by 'i'
//     and loaded glyph-by-glyph by 'l'; this is the whole 4th-edition
//     default-font path (libdraw getdefont allocates the subfont bits with
//     'b'+'y', buildfont allocates the cache with 'b' and 'i', loadchar
//     sends 'l' per glyph), so no font file service exists in the device.
//   - Screens ('A'/'F') are bookkeeping only; there is no window manager.
//   - No shapes ('e' 'E' 'L' 'p' 'P'), compressed loads ('Y'), origin
//     ('o'), top/bottom ('t'), public screen attach ('S'), or debug
//     toggle ('D').
package draw

import (
	"context"
	"fmt"
	"image"
	"strconv"
	"strings"
	"sync"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
)

// IOUNIT is the kernel draw device's data-file iounit (devdraw.c).
const IOUNIT = 64 * 1024

// Refresh methods from draw(3). Recorded but no notifications are generated.
const (
	Refbackup = 0
	Refnone   = 1
	Refmesg   = 2
)

// Draw op values (draw.h Porter-Duff enum).
const (
	OpClear  = 0
	OpS      = 10 // SinD|SoutD
	OpSoverD = 11 // SinD|SoutD|DoutS
)

var (
	ErrBadCommand   = fmt.Errorf("bad draw command")
	ErrShortDraw    = fmt.Errorf("short draw message")
	ErrImageExists  = fmt.Errorf("image id in use")
	ErrNoImage      = fmt.Errorf("unknown id for draw image")
	ErrNoName       = fmt.Errorf("image name not found")
	ErrNamed        = fmt.Errorf("image already named")
	ErrWrongName    = fmt.Errorf("wrong name for image")
	ErrScreenExists = fmt.Errorf("screen id in use")
	ErrNoClient     = fmt.Errorf("unknown draw client")
	ErrNotFont      = fmt.Errorf("image not a font")
	ErrIndex        = fmt.Errorf("character index out of range")
	ErrSaturated    = fmt.Errorf("draw: resource limit reached")
)

// Device is a draw(3) filesystem device. Its root contains a "new" file and
// a numbered directory per client; a leading "draw/" path prefix is also
// accepted so that binding the device after /dev yields /dev/draw/new, as
// with Plan 9's bind of #i onto /dev.
type Device struct {
	mu      sync.Mutex
	clients map[uint32]*client
	names   map[string]*Image
	cmap    [256][3]byte
	screen  *Image
	nextID  uint32

	// Limits (defaults applied on first use).
	MaxClients      int // default 64
	MaxImages       int // per client, default 1024
	MaxPixels       int // per image, default 1<<24
	MaxMessageBytes int // single data-message cap, default 1<<20
	ScreenRect      image.Rectangle
	ScreenChan      uint32
}

// New returns a Device with a 1024x768 r8g8b8 display image (id 0).
func New() *Device {
	return &Device{}
}

func (d *Device) maxClients() int {
	if d.MaxClients <= 0 {
		return 64
	}
	return d.MaxClients
}
func (d *Device) maxImages() int {
	if d.MaxImages <= 0 {
		return 1024
	}
	return d.MaxImages
}
func (d *Device) maxPixels() int {
	if d.MaxPixels <= 0 {
		return 1 << 24
	}
	return d.MaxPixels
}
func (d *Device) maxMessage() int {
	if d.MaxMessageBytes <= 0 {
		return 1 << 20
	}
	return d.MaxMessageBytes
}
func (d *Device) screenRect() image.Rectangle {
	if d.ScreenRect.Empty() {
		return image.Rect(0, 0, 1024, 768)
	}
	return d.ScreenRect
}
func (d *Device) screenChan() uint32 {
	if d.ScreenChan == 0 {
		return ChanRGB24
	}
	return d.ScreenChan
}

func (d *Device) initLocked() {
	if d.clients != nil {
		return
	}
	d.clients = make(map[uint32]*client)
	d.names = make(map[string]*Image)
	// Default color map per devdraw.c drawcmap().
	i := 0
	for r := 0; r != 4; r++ {
		for v := 0; v != 4; v, i = v+1, i+16 {
			j := v - r
			for g := 0; g != 4; g++ {
				for b := 0; b != 4; b, j = b+1, j+1 {
					den := r
					if g > den {
						den = g
					}
					if b > den {
						den = b
					}
					var cr, cg, cb int
					if den == 0 {
						cr, cg, cb = v*17, v*17, v*17
					} else {
						num := 17 * (4*den + v)
						cr = r * num / den
						cg = g * num / den
						cb = b * num / den
					}
					d.cmap[i+(j&15)] = [3]byte{byte(cr), byte(cg), byte(cb)}
				}
			}
		}
	}
	d.screen = newImage(0, d.screenChan(), false, d.screenRect(), d.screenRect(), DWhite)
}

func (d *Device) Open(name string) (fs.File, error) {
	return d.OpenContext(context.Background(), name)
}

func (d *Device) Stat(name string) (fs.FileInfo, error) {
	return d.StatContext(context.Background(), name)
}

func (d *Device) StatContext(ctx context.Context, name string) (fs.FileInfo, error) {
	r, err := d.resolve(name)
	if err != nil {
		return nil, err
	}
	return r.stat(), nil
}

// resolved is a side-effect-free view of one device path: the kind of file
// it names and, for per-client paths, the existing client that owns it.
// Resolution never creates clients and never takes references, so any
// number of resolves for one open leave device state untouched — the
// requirement the kernel namespace router imposes by statting a path
// before opening it (fs/vfs NS.OpenContext). Only OpenContext converts a
// resolved view into an open file, performing the mutation (newClient for
// "new", client.retain for <n>/<file>) exactly once per actual open.
type resolved struct {
	kind   resKind
	client *client
	id     string
}

type resKind int

const (
	resRoot resKind = iota
	resNew
	resClientDir
	resCtl
	resData
	resColormap
	resRefresh
)

// resolve strips an optional leading "draw/" prefix and looks up the path
// without mutating device or client state.
func (d *Device) resolve(name string) (*resolved, error) {
	name = strings.TrimPrefix(name, "draw/")
	if name == "." {
		return &resolved{kind: resRoot}, nil
	}
	if name == "new" {
		return &resolved{kind: resNew}, nil
	}
	idStr, file, ok := strings.Cut(name, "/")
	if !ok {
		return nil, fs.ErrNotExist
	}
	id, err := strconv.ParseUint(idStr, 10, 32)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	c := d.lookupClient(uint32(id))
	if c == nil {
		return nil, fs.ErrNotExist
	}
	r := &resolved{id: idStr, client: c}
	switch file {
	case ".":
		r.kind = resClientDir
	case "ctl":
		r.kind = resCtl
	case "data":
		r.kind = resData
	case "colormap":
		r.kind = resColormap
	case "refresh":
		r.kind = resRefresh
	default:
		return nil, fs.ErrNotExist
	}
	return r, nil
}

// stat is the FileInfo for a resolved path. It names static metadata only;
// nothing about the device or client state is touched or revealed.
func (r *resolved) stat() fs.FileInfo {
	switch r.kind {
	case resRoot:
		return fskit.Entry(".", fs.ModeDir|0555)
	case resNew:
		return fskit.Entry("new", 0666)
	case resClientDir:
		return fskit.Entry(r.id, fs.ModeDir|0555)
	case resCtl:
		return fskit.Entry("ctl", 0666)
	case resData:
		return fskit.Entry("data", 0666)
	case resColormap:
		return fskit.Entry("colormap", 0666)
	case resRefresh:
		return fskit.Entry("refresh", 0444)
	}
	return nil
}

func (d *Device) OpenContext(ctx context.Context, name string) (fs.File, error) {
	r, err := d.resolve(name)
	if err != nil {
		return nil, err
	}
	switch r.kind {
	case resRoot:
		return fskit.DirFile(fskit.Entry(".", fs.ModeDir|0555),
			fskit.Entry("draw", fs.ModeDir|0555),
			fskit.Entry("new", 0666)), nil
	case resNew:
		c, err := d.newClient()
		if err != nil {
			return nil, err
		}
		return &ctlFile{client: c}, nil
	case resClientDir:
		return fskit.DirFile(fskit.Entry(r.id, fs.ModeDir|0555),
			fskit.Entry("ctl", 0666), fskit.Entry("data", 0666),
			fskit.Entry("colormap", 0666), fskit.Entry("refresh", 0444)), nil
	case resCtl:
		r.client.retain()
		return &ctlFile{client: r.client}, nil
	case resData:
		r.client.retain()
		return &dataFile{client: r.client}, nil
	case resColormap:
		r.client.retain()
		return &colormapFile{dev: d, client: r.client}, nil
	case resRefresh:
		r.client.retain()
		return &refreshFile{client: r.client}, nil
	}
	return nil, fs.ErrNotExist
}

func (d *Device) newClient() (*client, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.initLocked()
	if len(d.clients) >= d.maxClients() {
		return nil, ErrSaturated
	}
	d.nextID++
	for d.clients[d.nextID] != nil {
		d.nextID++
	}
	c := &client{
		dev:     d,
		id:      d.nextID,
		images:  map[uint32]*Image{0: d.screen},
		screens: make(map[uint32]*screen),
		op:      OpSoverD,
		refs:    1,
		infoSet: true, // infoid 0 = display image, readable once
	}
	d.clients[c.id] = c
	return c, nil
}

func (d *Device) lookupClient(id uint32) *client {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.clients[id]
}

func (d *Device) removeClient(id uint32) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.clients[id]
	if c == nil {
		return
	}
	// Drop global names owned by this client's images.
	for name, img := range d.names {
		for _, ci := range c.images {
			if ci == img {
				delete(d.names, name)
			}
		}
	}
	delete(d.clients, id)
}

type screen struct {
	id      uint32
	imageid uint32
	fillid  uint32
	public  bool
}

type client struct {
	dev *Device
	id  uint32

	mu      sync.Mutex
	images  map[uint32]*Image
	screens map[uint32]*screen
	infoid  int32 // -1 until set; ctl reads consume it
	infoSet bool
	op      int
	msgbuf  []byte
	readbuf []byte
	refs    int
	closed  bool
	refresh [][]byte
}

func (c *client) retain() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refs++
}

func (c *client) release() {
	c.mu.Lock()
	refs := c.refs - 1
	c.refs = refs
	c.mu.Unlock()
	if refs <= 0 {
		c.dev.removeClient(c.id)
	}
}
