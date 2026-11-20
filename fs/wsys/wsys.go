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
//	#wsys/<id>/cons           rio cons: read console input (blocking),
//	                          write console output. Multi-open (rio multiplexes).
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
//	                          (current|top|bottom|resize|move with rio's
//	                          -r/-minx/-miny/-maxx/-maxy/-dx/-dy parameters,
//	                          resolved against the device's current rect);
//	                          read is a single-reader blocking geometry line
//	                          "%11d %11d %11d %11d current|notcurrent visible "
//	                          that returns once per geometry change (armed at
//	                          open, exactly rio's wctlready); needs >= 4*12 bytes
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
// Explicit limitations of this slice (do not claim otherwise):
//   - The window id is allocated by the host WM; the device has no "new"
//     wctl command (rio's new creates a window and runs a program — window
//     creation stays with the desktop surface machinery).
//   - wctl verbs new, set, delete, hide, unhide, scroll, noscroll and the
//     -pid/-id/-cd parameters are not in the subset and fail the write.
//   - Geometry is trusted from srv/geom writes; the WM applies its own
//     clamps and writes the final rect back (the session owns sizing).
//   - cons has no line editing, hold queueing or raw-mode semantics beyond
//     recording the consctl state; raw/hold are state for the WM.
//   - mouse writes (buttonmap) are not served; there is no cursor, snarf,
//     scrollr, kbdin, winname, text or image access.
//   - The linux driver binds the whole device into every linux task's
//     namespace (the #draw pattern); per-task window visibility therefore
//     follows namespace visibility, not a per-window grant. The device
//     supports per-window binds ("#wsys/<id>") for a stricter grant later.
package wsys

import (
	"context"
	"fmt"
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
// desktop adapter must carry the same tag.
const Version = "wsys/0.1 (rio-4e subset)"

// MouseRecordLen is the 4th-edition mouse record length: one status char
// plus four %11d fields (rio xfid.c, devmouse.c).
const MouseRecordLen = 1 + 4*12

// MinWctlRead is rio's Etooshort bound for wctl reads.
const MinWctlRead = 4 * 12

var (
	ErrInUse       = fmt.Errorf("wsys: file already open")
	ErrBadCommand  = fmt.Errorf("wsys: unrecognized wctl command")
	ErrBadParam    = fmt.Errorf("wsys: missing or bad wctl parameter")
	ErrExtraText   = fmt.Errorf("wsys: extraneous text in wctl message")
	ErrBadRect     = fmt.Errorf("wsys: bad rectangle in wctl request")
	ErrNotInSubset = fmt.Errorf("wsys: wctl command not in subset")
	ErrTooShort    = fmt.Errorf("wsys: wctl read too short")
)

// Device is the window-service filesystem device. Windows live under
// numeric ids; the root additionally carries "new" for the host WM.
type Device struct {
	start time.Time // msec epoch for mouse records: ms since device start

	mu             sync.Mutex
	windows        map[int]*window
	nextID         int
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
func (d *Device) splitID(name string) (int, string, bool) {
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
	id, rest, ok := d.splitID(name)
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
		entries := []fs.DirEntry{fskit.Entry("new", 0444)}
		for _, id := range d.windowIDs() {
			entries = append(entries, fskit.Entry(strconv.Itoa(id), fs.ModeDir|0555))
		}
		return fskit.DirFile(fskit.Entry(".", fs.ModeDir|0555), entries...), nil
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
	id, rest, ok := d.splitID(name)
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
	if d.windows == nil {
		d.windows = make(map[int]*window)
	}
	if len(d.windows) >= d.maxWindows() {
		return nil, signal.ErrSaturated
	}
	d.nextID++
	w := &window{
		dev:      d,
		id:       d.nextID,
		label:    "<unnamed>", // rio's default label (wind.c)
		consInQ:  signal.NewQueue(d.maxQueueBytes()),
		consOutQ: signal.NewQueue(d.maxQueueBytes()),
		wctlQ:    signal.NewQueue(d.maxQueueBytes()),
		notesQ:   signal.NewQueue(d.maxQueueBytes()),
	}
	w.cond = sync.NewCond(&w.mu)
	d.windows[w.id] = w
	return w, nil
}

func (d *Device) removeWindow(id int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.windows, id)
}

// Route implements fs.RouteFS so a single window can be bound directly
// (bind '#wsys/3' dst), the same shape as the #wminput queue allocator.
func (d *Device) Route(ctx context.Context, name string) (fs.FS, string, error) {
	if id, rest, ok := d.splitID(name); ok {
		if w := d.lookup(id); w != nil {
			return w, rest, nil
		}
	}
	return d, name, nil
}

// BindAllocFS supports binding the whole device ("#wsys", the driver's
// mount) or a single window directory ("#wsys/<id>", the stricter grant).
func (d *Device) BindAllocFS(name string) (fs.FS, error) {
	switch name {
	case ".", "#wsys", "wsys":
		return d, nil
	}
	if id, _, ok := d.splitID(strings.TrimPrefix(name, "#wsys/")); ok {
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
