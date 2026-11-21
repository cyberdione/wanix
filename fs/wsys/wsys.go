// Package wsys implements the per-window rio(4) file-service subset as a
// Wanix kernel filesystem device.
//
// Byte layouts and semantics are taken from the pinned 4th-edition sources
// (mirror github.com/0intro/plan9 commit 0aab9506bfc2acb5203a411b917cea62cf3eeb91):
//   - sys/src/cmd/rio/xfid.c  (mouse record, label read, wctl open/close/read)
//   - sys/src/cmd/rio/wctl.c  (wctl command grammar, rect arithmetic)
//   - sys/src/cmd/rio/wind.c  (wctl geometry line, resize flag coalescing)
//   - sys/src/9/port/devmouse.c (49-byte mouse record format)
//
// and the rio(4) manual. Rio places windows but does not mediate draw
// access; this device carries only the window files, never /dev/draw.
//
// The device hosts one service directory per window under numeric ids:
//
//	#wsys/new                 read: allocate a window, returns "<id>\n" (host side)
//	#wsys/srv/state           read: whole-device window listing for the WM
//	                          (one stable JSON snapshot per open, ascending id)
//	#wsys/wm                  bind source of the WM grant: the whole device
//	                          plus the srv plane, for the one task whose
//	                          launch env carries WSYS_WM=1 (the driver binds
//	                          "#wsys/wm" instead of "#wsys" for it)
//	#wsys/<id>/cons           rio cons: read console input (blocking),
//	                          write console output (blocking backpressure:
//	                          writes queue as the host drains srv/consout
//	                          and fail on window teardown; bytes are never
//	                          dropped silently). Multi-open (rio multiplexes).
//	#wsys/<id>/consctl        rio consctl: write rawon|rawoff|holdon|holdoff;
//	                          closing the fd resets raw and hold (rio semantics)
//	#wsys/<id>/mouse          rio mouse: blocking single-reader reads of
//	                          49-byte records "%c%11d %11d %11d %11d " with
//	                          x y buttons msec; the record char is 'm', or 'r'
//	                          while a resize is pending (rio's w->resized flag:
//	                          multiple resizes coalesce to one 'r'; a pending
//	                          resize unblocks a read even with no queued event)
//	#wsys/<id>/label          rio label: read/write the label verbatim
//	#wsys/<id>/wctl           rio wctl: write one command per write
//	                          (current|top|bottom|resize|move|new|delete|
//	                          hide|unhide|scroll|noscroll|set with rio's
//	                          -r/-minx/-miny/-maxx/-maxy/-dx/-dy parameters
//	                          plus -pid/-id/-hide/-scroll/-noscroll,
//	                          resolved against the writing window's rect with
//	                          rio's arithmetic and error strings); read is a
//	                          single-reader blocking geometry line
//	                          "%11d %11d %11d %11d current|notcurrent "
//	                          "visible|hidden " that returns once per
//	                          geometry or visibility change (armed at open,
//	                          exactly rio's wctlready); needs >= 4*12 bytes
//	#wsys/<id>/srv/           host service plane, opened by the desktop WM
//	                          through the root namespace; the task-side
//	                          openat allowlist never admits it:
//	     srv/consin          write: publish console input bytes (task cons reads)
//	     srv/consout         read: drain console output the task wrote
//	     srv/mouse           write: "buttons x y\n" -> one queued mouse record
//	                          (msec is stamped by the device); a full queue
//	                          rejects the write (ErrSaturated) — the producer
//	                          must surface busy
//	     srv/geom            read "minx miny maxx maxy\n"; write the same to
//	                          commit a geometry change (flags the coalesced
//	                          resize, wakes wctl readers)
//	     srv/wctl            read: one resolved command line per read, in
//	                          order ("resize 10 20 670 450", "current", ...)
//	     srv/notes           read: host-relevant state changes, one line each:
//	                          "label <label>", "rawon", "rawoff", "holdon",
//	                          "holdoff"
//	     srv/state           read: one-line JSON status (incl. the version tag)
//	     srv/ctl             write: "close" (teardown: queues drain then EOF,
//	                          mouse/wctl reads fail closed, the window id is
//	                          released), "current on|off" (the word reported
//	                          by wctl reads)
//
// Window management (ADR 0007, the K1 slice): the wctl slot verbs are real.
// The device records intent and state; the display server acts through the
// srv/ plane:
//
//	wctl new        allocates a slot (rio's newrect() cascade default;
//	                -pid/-hide/-scroll/-noscroll recorded; trailing text is
//	                the requested command, recorded, never exec'd) and
//	                announces "new <id> <rect>" on the new slot's srv/wctl
//	wctl delete     frees the slot (the srv/ctl close teardown)
//	wctl hide/unhide, scroll/noscroll, set -pid
//	                slot state changes, noted to the host like label writes
//	wctl -id n      targets another slot (rio's post-parse retarget; the
//	                rect arithmetic stays based on the writing window)
//
// Task-side grants (bind + openat allowlist only, no new syscalls): the
// default whole-device and single-window binds expose exactly the window
// files and deny srv/ at the device; the "#wsys/wm" bind admits srv/ — the
// enumeration plus per-window host operations — for the one task the driver
// binds it into (its launch env carries WSYS_WM=1). The worker's openat
// allowlist mirrors the same grant. Default per-task grants are unchanged.
//
// Explicit limitations of this slice (do not claim otherwise):
//   - The device does not exec: wctl new records the requested command
//     text; running it in the slot stays with the display server's launch
//     wiring, and -cd is parsed and ignored.
//   - Geometry is trusted from srv/geom writes; the WM applies its own
//     clamps and writes the final rect back (the session owns sizing).
//   - cons has no line editing, hold queueing or raw-mode semantics beyond
//     recording the consctl state; raw/hold are state for the WM.
//   - mouse writes (buttonmap) are not served; there is no cursor, snarf,
//     scrollr, kbdin, winname, text or image access.
//   - The linux driver binds the whole device into every linux task's
//     namespace (the #draw pattern); per-task window visibility follows
//     namespace visibility. The bind source is "#wsys" (default grant) or
//     "#wsys/wm" (WM grant, keyed on the task env); per-window binds
//     ("#wsys/<id>") stay supported under the default grant.
package wsys

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
	"tractor.dev/wanix/fs/signal"
)

// Version tags this device's contract; wire-visible changes bump it and the
// desktop adapter must carry the same tag. 0.2 adds the wctl slot verbs
// (new/delete/hide/unhide/scroll/noscroll/set), the srv/state enumeration,
// the WM grant bind ("#wsys/wm") and the pid/visible/scrolling/cmd state
// fields (ADR 0007 K1).
const Version = "wsys/0.2 (rio-4e subset)"

// MouseRecordLen is the 4th-edition mouse record length: one status char
// plus four %11d fields (rio xfid.c, devmouse.c).
const MouseRecordLen = 1 + 4*12

// MinWctlRead is rio's Etooshort bound for wctl reads.
const MinWctlRead = 4 * 12

var (
	ErrInUse      = fmt.Errorf("wsys: file already open")
	ErrBadCommand = fmt.Errorf("wsys: unrecognized wctl command")
	ErrBadParam   = fmt.Errorf("wsys: missing or bad wctl parameter")
	ErrExtraText  = fmt.Errorf("wsys: extraneous text in wctl message")
	ErrBadRect    = fmt.Errorf("wsys: bad rectangle in wctl request")
	ErrTooShort   = fmt.Errorf("wsys: wctl read too short")

	// rio wctl.c error strings for the slot verbs (writewctl).
	ErrNoSuchWindow  = fmt.Errorf("wsys: no such window id")
	ErrWindowDeleted = fmt.Errorf("wsys: window deleted")
	ErrAlreadyHidden = fmt.Errorf("wsys: window already hidden")
	ErrNotHidden     = fmt.Errorf("wsys: window not hidden")
	ErrAllocFailed   = fmt.Errorf("wsys: window allocation failed in wctl request")
	ErrIO            = fmt.Errorf("wsys: I/O error on window close")
)

// Device is the window-service filesystem device. Windows live under
// numeric ids; the root additionally carries "new" for the host WM and
// "srv/state" (the WM enumeration).
type Device struct {
	start time.Time // msec epoch for mouse records: ms since device start

	mu             sync.Mutex
	windows        map[int]*window
	nextID         int
	cascade        int // wctl new's default placement cycle (rio newrect)
	MaxWindows     int // default 64
	MaxQueueBytes  int // per host-consumed queue, default 32 KiB
	MaxMouseEvents int // queued mouse records per window, default 64
}

// New returns an empty Device.
func New() *Device { return &Device{start: time.Now()} }

func (d *Device) maxWindows() int {
	if d.MaxWindows <= 0 {
		return 64
	}
	return d.MaxWindows
}
func (d *Device) maxQueueBytes() int {
	if d.MaxQueueBytes <= 0 {
		return 32 << 10
	}
	return d.MaxQueueBytes
}
func (d *Device) maxMouseEvents() int {
	if d.MaxMouseEvents <= 0 {
		return 64
	}
	return d.MaxMouseEvents
}

func (d *Device) Stat(name string) (fs.FileInfo, error) {
	return d.StatContext(context.Background(), name)
}

func (d *Device) Open(name string) (fs.File, error) {
	return d.OpenContext(context.Background(), name)
}

// splitID returns the window id and the remaining path for a device-local
// name, with or without the optional "wsys/" prefix (the device also serves
// as "#wsys" bound at a mount point, so both shapes arrive). It performs no
// allocation and takes no slots: StatContext may call it any number of
// times before an open (the namespace router stats before opening, fs/vfs
// NS.OpenContext); only OpenContext mutates.
func splitID(name string) (int, string, bool) {
	name = strings.TrimPrefix(name, "wsys/")
	if name == "." || name == "" || name == "new" {
		return 0, "", false
	}
	idStr, rest, _ := strings.Cut(name, "/")
	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 || strconv.Itoa(id) != idStr {
		return 0, "", false
	}
	if rest == "" {
		rest = "."
	}
	return id, rest, true
}

func (d *Device) lookup(id int) *window {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.windows[id]
}

func (d *Device) StatContext(ctx context.Context, name string) (fs.FileInfo, error) {
	name = strings.TrimPrefix(name, "wsys/")
	if name == "." || name == "" {
		return fskit.Entry(".", fs.ModeDir|0555), nil
	}
	if name == "new" {
		return fskit.Entry("new", 0444), nil
	}
	if name == "srv" {
		return fskit.Entry("srv", fs.ModeDir|0555), nil
	}
	if name == "srv/state" {
		return fskit.Entry("state", 0444), nil
	}
	id, rest, ok := splitID(name)
	if !ok {
		return nil, fs.ErrNotExist
	}
	w := d.lookup(id)
	if w == nil {
		return nil, fs.ErrNotExist
	}
	return w.StatContext(ctx, rest)
}

func (d *Device) OpenContext(ctx context.Context, name string) (fs.File, error) {
	name = strings.TrimPrefix(name, "wsys/")
	if name == "." || name == "" {
		entries := []fs.DirEntry{fskit.Entry("new", 0444), fskit.Entry("srv", fs.ModeDir|0555)}
		for _, id := range d.windowIDs() {
			entries = append(entries, fskit.Entry(strconv.Itoa(id), fs.ModeDir|0555))
		}
		return fskit.DirFile(fskit.Entry(".", fs.ModeDir|0555), entries...), nil
	}
	if name == "srv" {
		return fskit.DirFile(fskit.Entry("srv", fs.ModeDir|0555), fskit.Entry("state", 0444)), nil
	}
	if name == "srv/state" {
		return &srvListingFile{d: d}, nil
	}
	if name == "new" {
		return &fskit.FuncFile{Node: fskit.Entry("new", 0444), ReadFunc: func(n *fskit.Node) error {
			w, err := d.newWindow()
			if err != nil {
				return err
			}
			fskit.SetData(n, []byte(strconv.Itoa(w.id)+"\n"))
			return nil
		}}, nil
	}
	id, rest, ok := splitID(name)
	if !ok {
		return nil, fs.ErrNotExist
	}
	w := d.lookup(id)
	if w == nil {
		return nil, fs.ErrNotExist
	}
	return w.OpenContext(ctx, rest)
}

func (d *Device) windowIDs() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	ids := make([]int, 0, len(d.windows))
	for id := range d.windows {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func (d *Device) newWindow() (*window, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.newWindowLocked()
}

func (d *Device) newWindowLocked() (*window, error) {
	if d.windows == nil {
		d.windows = make(map[int]*window)
	}
	if len(d.windows) >= d.maxWindows() {
		return nil, signal.ErrSaturated
	}
	d.nextID++
	w := &window{
		dev:       d,
		id:        d.nextID,
		label:     "<unnamed>", // rio's default label (wind.c)
		scrolling: true,        // rio's scrolling default for new windows
		consInQ:   signal.NewQueue(d.maxQueueBytes()),
		consOutQ:  signal.NewQueue(d.maxQueueBytes()),
		wctlQ:     signal.NewQueue(d.maxQueueBytes()),
		notesQ:    signal.NewQueue(d.maxQueueBytes()),
	}
	w.cond = sync.NewCond(&w.mu)
	d.windows[w.id] = w
	return w, nil
}

// newRect is rio's newrect(): the default placement for wctl new, cycling
// through ten cascade offsets. Rio caps the size against the screen; the
// device has no screen rect, so rio's caps (600x400) are the size.
func (d *Device) newRect() image.Rectangle {
	d.mu.Lock()
	defer d.mu.Unlock()
	i := d.cascade
	d.cascade = (d.cascade + 1) % 10
	minx := 32 + 16*i
	miny := 32 + 16*i
	return image.Rect(minx, miny, minx+600, miny+400)
}

// stateSnapshot renders the srv/state enumeration: live slots in ascending
// id order, one stable JSON snapshot per open (ids are never reused).
func (d *Device) stateSnapshot() []byte {
	d.mu.Lock()
	ids := make([]int, 0, len(d.windows))
	for id := range d.windows {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	list := make([]listedWindow, 0, len(ids))
	for _, id := range ids {
		w := d.windows[id]
		w.mu.Lock()
		list = append(list, listedWindow{
			ID:        w.id,
			Label:     w.label,
			Rect:      [4]int{w.rect.Min.X, w.rect.Min.Y, w.rect.Max.X, w.rect.Max.Y},
			Visible:   !w.hidden,
			PID:       w.pid,
			Current:   w.current,
			Scrolling: w.scrolling,
			Cmd:       w.cmd,
		})
		w.mu.Unlock()
	}
	d.mu.Unlock()
	b, err := json.Marshal(deviceState{Version: Version, Windows: list})
	if err != nil {
		return nil
	}
	return append(b, '\n')
}

// listedWindow is one slot in the srv/state enumeration: the stable
// listing fields (id, label, rect, visible, pid) plus the state the host
// needs to service the slot.
type listedWindow struct {
	ID        int    `json:"id"`
	Label     string `json:"label"`
	Rect      [4]int `json:"rect"`
	Visible   bool   `json:"visible"`
	PID       int    `json:"pid"`
	Current   bool   `json:"current"`
	Scrolling bool   `json:"scrolling"`
	Cmd       string `json:"cmd,omitempty"`
}

type deviceState struct {
	Version string         `json:"version"`
	Windows []listedWindow `json:"windows"`
}

// srvListingFile serves #wsys/srv/state: the WM's window enumeration. One
// stable JSON snapshot per open: live slots in ascending id order; ids are
// never reused, and a later open reflects allocation and teardown.
type srvListingFile struct {
	d      *Device
	offset int
	snap   []byte
}

func (f *srvListingFile) Read(p []byte) (int, error) {
	if f.snap == nil {
		f.snap = f.d.stateSnapshot()
	}
	if f.offset >= len(f.snap) {
		return 0, io.EOF
	}
	n := copy(p, f.snap[f.offset:])
	f.offset += n
	return n, nil
}

func (f *srvListingFile) Write([]byte) (int, error)  { return 0, fs.ErrPermission }
func (f *srvListingFile) Close() error               { return nil }
func (f *srvListingFile) Stat() (fs.FileInfo, error) { return fskit.Entry("state", 0444), nil }

func (d *Device) removeWindow(id int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.windows, id)
}

// Route implements fs.RouteFS so a single window can be bound directly
// (bind '#wsys/3' dst), the same shape as the #wminput queue allocator.
// "wm" resolves to the device root so the WM bind ("#wsys/wm") lands on
// "."; BindAllocFS turns that bind into the granted device.
func (d *Device) Route(ctx context.Context, name string) (fs.FS, string, error) {
	if name == "wm" {
		return d, ".", nil
	}
	if id, rest, ok := splitID(name); ok {
		if w := d.lookup(id); w != nil {
			return w, rest, nil
		}
	}
	return d, name, nil
}

// BindAllocFS supports binding the whole device ("#wsys", the driver's
// mount, default grant: window files only, srv denied at the device) or the
// WM grant ("#wsys/wm": adds the srv plane for that one task). Single-window
// binds ("#wsys/<id>") never reach here for a whole shape — the device's
// Route hands them straight to the raw window, exactly as merged in N10;
// their srv visibility stays gated by the worker's openat allowlist, since
// the same window object serves the display server through the root.
func (d *Device) BindAllocFS(name string) (fs.FS, error) {
	if name == "." {
		// the object bind (registration, e.g. the runtime's #wsys entry):
		// the raw device, srv plane ungranted — the display server's view
		return d, nil
	}
	switch name {
	case "#wsys", "wsys":
		return grant{fsys: d}, nil
	case "#wsys/wm", "wsys/wm", "wm":
		return grant{fsys: d, wm: true}, nil
	}
	if id, _, ok := splitID(strings.TrimPrefix(name, "#wsys/")); ok {
		if w := d.lookup(id); w != nil {
			return w, nil
		}
	}
	return nil, fs.ErrNotExist
}

var (
	_ fs.FS            = (*Device)(nil)
	_ fs.OpenContextFS = (*Device)(nil)
	_ fs.StatContextFS = (*Device)(nil)
	_ fs.RouteFS       = (*Device)(nil)
)
