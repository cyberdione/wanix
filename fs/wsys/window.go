package wsys

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
	"tractor.dev/wanix/fs/signal"
)

// window is one per-window service directory. Geometry, the label and the
// consctl state live here; the host-fed queues are bounded byte streams
// with atomic frames (fs/signal Queue: busy on overflow, drain-then-EOF
// close), while the task-facing cons output queue backpressures its writer
// instead (Queue.WriteContext: block until drained, fail on teardown).
type window struct {
	dev *Device
	id  int

	mu        sync.Mutex
	cond      *sync.Cond // wakes mouse and wctl readers
	label     string
	rect      image.Rectangle
	current   bool
	hidden    bool   // rio's hidden list membership (wctl hide/unhide)
	pid       int    // attached task id (wctl -pid / set)
	scrolling bool   // wctl scroll/noscroll (on for new windows, rio default)
	cmd       string // requested command text recorded from wctl new
	resized   bool   // pending coalesced resize (rio w->resized)
	mouse     mouseState
	mouseQ    [][]byte // queued 49-byte records
	mouseOpen bool     // single reader (rio returns Einuse)
	wctlOpen  bool     // single reader (rio returns Einuse)
	raw       bool
	hold      bool
	wctlSeq   int // geometry generation; wctl reads block until it moves

	consInQ  *signal.Queue // host writes (srv/consin) -> task cons reads
	consOutQ *signal.Queue // task cons writes -> host reads (srv/consout)
	wctlQ    *signal.Queue // resolved wctl command lines -> host reads
	notesQ   *signal.Queue // label/consctl change notes -> host reads

	closed bool
}

type mouseState struct {
	buttons int
	x, y    int
}

// ---- resolve: side-effect free (stat never takes a slot or allocates) ----

const (
	resDir = iota
	resCons
	resConsctl
	resMouse
	resLabel
	resWctl
	resSrvDir
	resSrvConsIn
	resSrvConsOut
	resSrvMouse
	resSrvGeom
	resSrvWctl
	resSrvNotes
	resSrvState
	resSrvCtl
)

func kindStat(kind int) fs.FileInfo {
	switch kind {
	case resDir:
		return fskit.Entry(".", fs.ModeDir|0555)
	case resCons:
		return fskit.Entry("cons", 0666)
	case resConsctl:
		return fskit.Entry("consctl", 0222)
	case resMouse:
		return fskit.Entry("mouse", 0444)
	case resLabel:
		return fskit.Entry("label", 0666)
	case resWctl:
		return fskit.Entry("wctl", 0666)
	case resSrvDir:
		return fskit.Entry("srv", fs.ModeDir|0555)
	case resSrvConsIn:
		return fskit.Entry("consin", 0222)
	case resSrvConsOut:
		return fskit.Entry("consout", 0444)
	case resSrvMouse:
		return fskit.Entry("mouse", 0222)
	case resSrvGeom:
		return fskit.Entry("geom", 0666)
	case resSrvWctl:
		return fskit.Entry("wctl", 0444)
	case resSrvNotes:
		return fskit.Entry("notes", 0444)
	case resSrvState:
		return fskit.Entry("state", 0444)
	case resSrvCtl:
		return fskit.Entry("ctl", 0222)
	}
	return nil
}

func (w *window) resolve(name string) (int, error) {
	switch name {
	case ".", "":
		return resDir, nil
	case "cons":
		return resCons, nil
	case "consctl":
		return resConsctl, nil
	case "mouse":
		return resMouse, nil
	case "label":
		return resLabel, nil
	case "wctl":
		return resWctl, nil
	case "srv":
		return resSrvDir, nil
	case "srv/consin":
		return resSrvConsIn, nil
	case "srv/consout":
		return resSrvConsOut, nil
	case "srv/mouse":
		return resSrvMouse, nil
	case "srv/geom":
		return resSrvGeom, nil
	case "srv/wctl":
		return resSrvWctl, nil
	case "srv/notes":
		return resSrvNotes, nil
	case "srv/state":
		return resSrvState, nil
	case "srv/ctl":
		return resSrvCtl, nil
	}
	return -1, fs.ErrNotExist
}

func (w *window) Stat(name string) (fs.FileInfo, error) {
	return w.StatContext(context.Background(), name)
}

func (w *window) StatContext(_ context.Context, name string) (fs.FileInfo, error) {
	kind, err := w.resolve(name)
	if err != nil {
		return nil, err
	}
	return kindStat(kind), nil
}

func (w *window) Open(name string) (fs.File, error) {
	return w.OpenContext(context.Background(), name)
}

func (w *window) OpenContext(ctx context.Context, name string) (fs.File, error) {
	kind, err := w.resolve(name)
	if err != nil {
		return nil, err
	}
	switch kind {
	case resDir:
		return fskit.DirFile(fskit.Entry(".", fs.ModeDir|0555),
			fskit.Entry("cons", 0666), fskit.Entry("consctl", 0222),
			fskit.Entry("mouse", 0444), fskit.Entry("label", 0666),
			fskit.Entry("wctl", 0666), fskit.Entry("srv", fs.ModeDir|0555)), nil
	case resCons:
		return &consFile{w: w, ctx: ctx}, nil
	case resConsctl:
		return &consctlFile{w: w}, nil
	case resMouse:
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.closed {
			return nil, fs.ErrNotExist
		}
		if w.mouseOpen {
			return nil, ErrInUse
		}
		w.mouseOpen = true
		return &mouseFile{w: w, ctx: ctx}, nil
	case resLabel:
		return &labelFile{w: w}, nil
	case resWctl:
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.closed {
			return nil, fs.ErrNotExist
		}
		if w.wctlOpen {
			return nil, ErrInUse
		}
		w.wctlOpen = true
		// armed at open: the first read returns immediately (rio wctlready)
		return &wctlFile{w: w, ctx: ctx, lastSeq: w.wctlSeq - 1}, nil
	case resSrvDir:
		return fskit.DirFile(fskit.Entry("srv", fs.ModeDir|0555),
			fskit.Entry("consin", 0222), fskit.Entry("consout", 0444),
			fskit.Entry("mouse", 0222), fskit.Entry("geom", 0666),
			fskit.Entry("wctl", 0444), fskit.Entry("notes", 0444),
			fskit.Entry("state", 0444), fskit.Entry("ctl", 0222)), nil
	case resSrvConsIn:
		return &queueWriteFile{q: w.consInQ, name: "consin"}, nil
	case resSrvConsOut:
		return &queueReadFile{q: w.consOutQ, name: "consout", ctx: ctx}, nil
	case resSrvMouse:
		return &srvMouseFile{w: w}, nil
	case resSrvGeom:
		return &srvGeomFile{w: w}, nil
	case resSrvWctl:
		return &queueReadFile{q: w.wctlQ, name: "wctl", ctx: ctx}, nil
	case resSrvNotes:
		return &queueReadFile{q: w.notesQ, name: "notes", ctx: ctx}, nil
	case resSrvState:
		return &srvStateFile{w: w}, nil
	case resSrvCtl:
		return &srvCtlFile{w: w}, nil
	}
	return nil, fs.ErrNotExist
}

var (
	_ fs.FS            = (*window)(nil)
	_ fs.OpenContextFS = (*window)(nil)
	_ fs.StatContextFS = (*window)(nil)
)

// ---- state helpers ----

func (w *window) closeLocked() error {
	if w.closed {
		return nil
	}
	w.closed = true
	w.cond.Broadcast()
	err := w.consInQ.Close()
	if err2 := w.consOutQ.Close(); err2 != nil && err == nil {
		err = err2
	}
	if err2 := w.wctlQ.Close(); err2 != nil && err == nil {
		err = err2
	}
	if err2 := w.notesQ.Close(); err2 != nil && err == nil {
		err = err2
	}
	return err
}

// setRectLocked commits a geometry change: wakes blocked wctl readers and
// arms the coalesced mouse resize.
func (w *window) setRectLocked(r image.Rectangle) {
	w.rect = r
	w.wctlSeq++
	w.resized = true
	w.cond.Broadcast()
}

// publishMouseLocked enqueues one 49-byte 'm' record. A full queue rejects
// the publish; the producer must surface busy (wmqueue semantics).
func (w *window) publishMouseLocked(buttons, x, y int) error {
	if len(w.mouseQ) >= w.dev.maxMouseEvents() {
		return signal.ErrSaturated
	}
	w.mouseQ = append(w.mouseQ, mouseRecord('m', x, y, buttons, w.msecNow()))
	w.cond.Broadcast()
	return nil
}

// msecNow is the record timestamp: milliseconds since device start, the
// plan 9 msec convention (a compact since-boot counter, not wall time).
func (w *window) msecNow() int64 { return time.Since(w.dev.start).Milliseconds() }

// mouseRecord formats the 4th-edition record: "%c%11d %11d %11d %11d ".
func mouseRecord(c byte, x, y, buttons int, msec int64) []byte {
	b := fmt.Sprintf("%c%11d %11d %11d %11d ", c, x, y, buttons, msec)
	if len(b) != MouseRecordLen {
		panic("wsys: mouse record length " + strconv.Itoa(len(b)))
	}
	return []byte(b)
}

// ---- task-side files ----

// consFile is rio's /dev/cons: reads block for console input published by
// the host (srv/consin), writes produce console output drained by the host
// (srv/consout). Multiple opens multiplex over the same pair (rio's cons
// serves alternate data to alternate readers; here readers compete for
// frames and writes interleave — one window, one console).
type consFile struct {
	w   *window
	ctx context.Context
}

func (f *consFile) Read(p []byte) (int, error) {
	return f.ReadContext(f.ctx, p)
}

func (f *consFile) ReadContext(ctx context.Context, p []byte) (int, error) {
	return f.w.consInQ.ReadContext(ctx, p)
}

func (f *consFile) Write(p []byte) (int, error) {
	// cons output is a stream the host drains through srv/consout: the
	// write blocks until every byte is queued (plan9 pipe write semantics,
	// WriteContext) instead of rejecting whole writes once the 32 KiB
	// window fills — a task's echo must never lose its tail to a slow host
	// (the S1 recipe). The bound is the device lifetime: window teardown
	// closes the queue and fails the parked write with a real error.
	return f.w.consOutQ.WriteContext(context.Background(), p)
}

func (f *consFile) Close() error               { return nil }
func (f *consFile) Stat() (fs.FileInfo, error) { return fskit.Entry("cons", 0666), nil }

// consctlFile records rio's raw/hold console modes; closing the descriptor
// resets both (rio xfidclose). Reads are not served (rio rejects them).
type consctlFile struct {
	w *window
}

func (f *consctlFile) Write(p []byte) (int, error) {
	cmd := strings.TrimSpace(string(p))
	w := f.w
	var note string
	w.mu.Lock()
	switch cmd {
	case "rawon":
		w.raw = true
		note = "rawon"
	case "rawoff":
		w.raw = false
		note = "rawoff"
	case "holdon":
		w.hold = true
		note = "holdon"
	case "holdoff":
		w.hold = false
		note = "holdoff"
	default:
		w.mu.Unlock()
		return 0, ErrBadCommand
	}
	w.mu.Unlock()
	_, _ = w.notesQ.Write([]byte(note + "\n"))
	return len(p), nil
}

func (f *consctlFile) Close() error {
	w := f.w
	w.mu.Lock()
	w.raw = false
	w.hold = false
	w.mu.Unlock()
	return nil
}

func (f *consctlFile) Read([]byte) (int, error)   { return 0, fs.ErrPermission }
func (f *consctlFile) Stat() (fs.FileInfo, error) { return fskit.Entry("consctl", 0222), nil }

// mouseFile serves rio's blocking single-reader mouse reads. A pending
// resize is delivered as an 'r' record built from the last mouse state and
// never consumes a queued event; queued events deliver as 'm' records (a
// pending resize rebrands the record's first char, rio's ordering). Queued
// records stay readable after srv/ctl close (drain-then-closed).
type mouseFile struct {
	w   *window
	ctx context.Context
}

func (f *mouseFile) Read(p []byte) (int, error) { return f.ReadContext(f.ctx, p) }

func (f *mouseFile) ReadContext(ctx context.Context, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w := f.w
	stop := context.AfterFunc(ctx, func() { w.mu.Lock(); w.cond.Broadcast(); w.mu.Unlock() })
	defer stop()
	w.mu.Lock()
	defer w.mu.Unlock()
	for {
		if w.resized {
			// deliver the resize notification without consuming a record
			w.resized = false
			rec := mouseRecord('r', w.mouse.x, w.mouse.y, w.mouse.buttons, w.msecNow())
			return copy(p, rec), nil
		}
		if len(w.mouseQ) > 0 {
			rec := w.mouseQ[0]
			w.mouseQ = w.mouseQ[1:]
			if w.resized {
				w.resized = false
				rec[0] = 'r'
			}
			return copy(p, rec), nil
		}
		if w.closed {
			return 0, fs.ErrClosed
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		w.cond.Wait()
	}
}

func (f *mouseFile) Close() error {
	w := f.w
	w.mu.Lock()
	defer w.mu.Unlock()
	w.mouseOpen = false
	w.cond.Broadcast()
	return nil
}

func (f *mouseFile) Stat() (fs.FileInfo, error) { return fskit.Entry("mouse", 0444), nil }

// labelFile serves rio's label: the whole write replaces the label verbatim
// and is noted to the host; reads return the current bytes sequentially.
type labelFile struct {
	w      *window
	offset int
}

func (f *labelFile) Read(p []byte) (int, error) {
	f.w.mu.Lock()
	label := f.w.label
	closed := f.w.closed
	f.w.mu.Unlock()
	if closed {
		return 0, fs.ErrClosed
	}
	if f.offset >= len(label) {
		return 0, io.EOF
	}
	n := copy(p, label[f.offset:])
	f.offset += n
	return n, nil
}

func (f *labelFile) Write(p []byte) (int, error) {
	w := f.w
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return 0, fs.ErrClosed
	}
	w.label = string(p)
	w.mu.Unlock()
	_, _ = w.notesQ.Write([]byte("label " + strings.TrimRight(string(p), "\n") + "\n"))
	return len(p), nil
}

func (f *labelFile) Close() error               { return nil }
func (f *labelFile) Stat() (fs.FileInfo, error) { return fskit.Entry("label", 0666), nil }

// wctlFile is rio's wctl. Writes parse one command (wctl.c grammar subset)
// and hand the resolved command to the host queue; reads block until the
// geometry generation moves past this descriptor's last observation (the
// first read after open is armed, rio's wctlready at open).
type wctlFile struct {
	w       *window
	ctx     context.Context
	lastSeq int
}

func (f *wctlFile) Write(p []byte) (int, error) {
	req, err := f.w.dev.parseWctl(string(p), f.w.rectNow())
	if err != nil {
		return 0, err
	}
	if err := f.w.dev.applyWctl(f.w, req); err != nil {
		return 0, err
	}
	return len(p), nil
}

// formatWctlRect is the resolved geometry line shape ("resize x1 y1 x2 y2").
func formatWctlRect(cmd string, r image.Rectangle) string {
	return fmt.Sprintf("%s %d %d %d %d\n", cmd, r.Min.X, r.Min.Y, r.Max.X, r.Max.Y)
}

func (w *window) rectNow() image.Rectangle {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rect
}

func (f *wctlFile) Read(p []byte) (int, error) { return f.ReadContext(f.ctx, p) }

func (f *wctlFile) ReadContext(ctx context.Context, p []byte) (int, error) {
	if len(p) < MinWctlRead {
		return 0, ErrTooShort
	}
	w := f.w
	stop := context.AfterFunc(ctx, func() { w.mu.Lock(); w.cond.Broadcast(); w.mu.Unlock() })
	defer stop()
	w.mu.Lock()
	defer w.mu.Unlock()
	for f.lastSeq == w.wctlSeq {
		if w.closed {
			return 0, fs.ErrClosed
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		w.cond.Wait()
	}
	f.lastSeq = w.wctlSeq
	r := w.rect
	cur := "notcurrent"
	if w.current {
		cur = "current"
	}
	vis := "visible"
	if w.hidden {
		vis = "hidden"
	}
	line := fmt.Sprintf("%11d %11d %11d %11d %s %s ", r.Min.X, r.Min.Y, r.Max.X, r.Max.Y, cur, vis)
	return copy(p, line), nil
}

func (f *wctlFile) Close() error {
	w := f.w
	w.mu.Lock()
	defer w.mu.Unlock()
	w.wctlOpen = false
	w.cond.Broadcast()
	return nil
}

func (f *wctlFile) Stat() (fs.FileInfo, error) { return fskit.Entry("wctl", 0666), nil }

// ---- host service plane ----

type queueWriteFile struct {
	q    *signal.Queue
	name string
}

func (f *queueWriteFile) Write(p []byte) (int, error) { return f.q.Write(p) }
func (f *queueWriteFile) Read([]byte) (int, error)    { return 0, fs.ErrPermission }
func (f *queueWriteFile) Close() error                { return nil }
func (f *queueWriteFile) Stat() (fs.FileInfo, error)  { return fskit.Entry(f.name, 0222), nil }

type queueReadFile struct {
	q    *signal.Queue
	name string
	ctx  context.Context
}

func (f *queueReadFile) Read(p []byte) (int, error) {
	return f.ReadContext(f.ctx, p)
}

func (f *queueReadFile) ReadContext(ctx context.Context, p []byte) (int, error) {
	return f.q.ReadContext(ctx, p)
}
func (f *queueReadFile) Write([]byte) (int, error)  { return 0, fs.ErrPermission }
func (f *queueReadFile) Close() error               { return nil }
func (f *queueReadFile) Stat() (fs.FileInfo, error) { return fskit.Entry(f.name, 0444), nil }

type srvMouseFile struct {
	w *window
}

func (f *srvMouseFile) Write(p []byte) (int, error) {
	fields := strings.Fields(string(p))
	if len(fields) != 3 {
		return 0, ErrBadParam
	}
	var n [3]int
	for i, s := range fields {
		v, err := strconv.Atoi(s)
		if err != nil {
			return 0, ErrBadParam
		}
		n[i] = v
	}
	w := f.w
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, fs.ErrClosed
	}
	w.mouse = mouseState{buttons: n[0], x: n[1], y: n[2]}
	if err := w.publishMouseLocked(n[0], n[1], n[2]); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (f *srvMouseFile) Read([]byte) (int, error)   { return 0, fs.ErrPermission }
func (f *srvMouseFile) Close() error               { return nil }
func (f *srvMouseFile) Stat() (fs.FileInfo, error) { return fskit.Entry("mouse", 0222), nil }

type srvGeomFile struct {
	w      *window
	offset int
}

func (f *srvGeomFile) Read(p []byte) (int, error) {
	line := f.w.geomLine()
	if f.offset >= len(line) {
		return 0, io.EOF
	}
	n := copy(p, line[f.offset:])
	f.offset += n
	return n, nil
}

func (w *window) geomLine() []byte {
	w.mu.Lock()
	r := w.rect
	w.mu.Unlock()
	return []byte(fmt.Sprintf("%d %d %d %d\n", r.Min.X, r.Min.Y, r.Max.X, r.Max.Y))
}

func (f *srvGeomFile) Write(p []byte) (int, error) {
	fields := strings.Fields(string(p))
	if len(fields) != 4 {
		return 0, ErrBadParam
	}
	var v [4]int
	for i, s := range fields {
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, ErrBadParam
		}
		v[i] = n
	}
	if v[0] >= v[2] || v[1] >= v[3] {
		return 0, ErrBadRect
	}
	w := f.w
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, fs.ErrClosed
	}
	w.setRectLocked(image.Rect(v[0], v[1], v[2], v[3]))
	return len(p), nil
}

func (f *srvGeomFile) Close() error               { return nil }
func (f *srvGeomFile) Stat() (fs.FileInfo, error) { return fskit.Entry("geom", 0666), nil }

type srvStateFile struct {
	w      *window
	offset int
	snap   []byte
}

func (f *srvStateFile) Read(p []byte) (int, error) {
	if f.snap == nil {
		w := f.w
		w.mu.Lock()
		st := map[string]any{
			"version":       Version,
			"id":            w.id,
			"label":         w.label,
			"rect":          []int{w.rect.Min.X, w.rect.Min.Y, w.rect.Max.X, w.rect.Max.Y},
			"visible":       !w.hidden,
			"pid":           w.pid,
			"scrolling":     w.scrolling,
			"cmd":           w.cmd,
			"raw":           w.raw,
			"hold":          w.hold,
			"current":       w.current,
			"mouseOpen":     w.mouseOpen,
			"wctlOpen":      w.wctlOpen,
			"resizePending": w.resized,
			"queuedMouse":   len(w.mouseQ),
		}
		w.mu.Unlock()
		b, err := json.Marshal(st)
		if err != nil {
			return 0, err
		}
		f.snap = append(b, '\n')
	}
	if f.offset >= len(f.snap) {
		return 0, io.EOF
	}
	n := copy(p, f.snap[f.offset:])
	f.offset += n
	return n, nil
}

func (f *srvStateFile) Write([]byte) (int, error)  { return 0, fs.ErrPermission }
func (f *srvStateFile) Close() error               { return nil }
func (f *srvStateFile) Stat() (fs.FileInfo, error) { return fskit.Entry("state", 0444), nil }

type srvCtlFile struct {
	w *window
}

func (f *srvCtlFile) Write(p []byte) (int, error) {
	cmd := strings.TrimSpace(string(p))
	w := f.w
	switch cmd {
	case "close":
		w.mu.Lock()
		w.closeLocked()
		w.mu.Unlock()
		w.dev.removeWindow(w.id)
		return len(p), nil
	case "current on", "current off":
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.closed {
			return 0, fs.ErrClosed
		}
		w.current = cmd == "current on"
		w.wctlSeq++
		w.cond.Broadcast()
		return len(p), nil
	}
	return 0, ErrBadCommand
}

func (f *srvCtlFile) Read([]byte) (int, error)   { return 0, fs.ErrPermission }
func (f *srvCtlFile) Close() error               { return nil }
func (f *srvCtlFile) Stat() (fs.FileInfo, error) { return fskit.Entry("ctl", 0222), nil }

// ---- wctl command parsing (rio wctl.c grammar, N10 tokenization) ----

// wctlRequest is one parsed wctl command.
type wctlRequest struct {
	cmd    string
	rect   image.Rectangle // resolved geometry for new/resize; move origin
	id     int             // -id target (0: the writing window)
	pid    int             // -pid
	hide   bool            // -hide (new)
	scroll int             // -scroll +1, -noscroll -1, unset 0 (new)
	arg    string          // trailing text (new): the requested command
}

// parseWctl parses one wctl command with rio's parameter arithmetic,
// tokenized exactly as N10 tokenized it (whitespace fields, exact param
// match — rio's word() matches prefixes, N10's grammar does not).
// Geometry verbs resolve against cur, the writing window's rect, even when
// -id retargets afterwards (rio's parsewctl ordering); new resolves against
// rio's newrect() cascade instead. The first word that is not a parameter
// ends the scan: for new the remainder is the requested command text, for
// every other verb it is a bad parameter (N10's grammar, which keeps rio's
// "missing or bad wctl parameter" for that shape).
func (d *Device) parseWctl(s string, cur image.Rectangle) (wctlRequest, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return wctlRequest{}, ErrBadCommand
	}
	req := wctlRequest{cmd: fields[0]}
	switch req.cmd {
	case "current", "top", "bottom":
		if len(fields) != 1 {
			return wctlRequest{}, ErrExtraText
		}
		return req, nil
	case "new":
		req.rect = d.newRect() // rio: newrect() replaces the base before params
	case "resize", "move", "set", "delete", "hide", "unhide", "scroll", "noscroll":
		req.rect = cur
	default:
		return wctlRequest{}, ErrBadCommand
	}
	i := 1
scan:
	for i < len(fields) {
		p := fields[i]
		switch p {
		case "-r":
			var v [4]int
			for j := range v {
				i++
				if i >= len(fields) {
					return wctlRequest{}, ErrBadParam
				}
				n, err := rioStrtol(fields[i])
				if err != nil {
					return wctlRequest{}, ErrBadParam
				}
				v[j] = n
			}
			req.rect = image.Rect(v[0], v[1], v[2], v[3])
		case "-minx", "-miny", "-maxx", "-maxy", "-dx", "-dy":
			i++
			if i >= len(fields) {
				return wctlRequest{}, ErrBadParam
			}
			sv := fields[i]
			sign := 0
			if strings.HasPrefix(sv, "-") {
				sign = -1
				sv = sv[1:]
			} else if strings.HasPrefix(sv, "+") {
				sign = +1
				sv = sv[1:]
			}
			n, err := rioStrtol(sv)
			if err != nil {
				return wctlRequest{}, ErrBadParam
			}
			// set mirrors wctl.c: set(sign, neg, abs, pos)
			set := func(neg, abs, pos int) int {
				switch sign {
				case -1:
					return neg
				case +1:
					return pos
				}
				return abs
			}
			switch p {
			case "-minx":
				req.rect.Min.X = set(req.rect.Min.X-n, n, req.rect.Min.X+n)
			case "-miny":
				req.rect.Min.Y = set(req.rect.Min.Y-n, n, req.rect.Min.Y+n)
			case "-maxx":
				req.rect.Max.X = set(req.rect.Max.X-n, n, req.rect.Max.X+n)
			case "-maxy":
				req.rect.Max.Y = set(req.rect.Max.Y-n, n, req.rect.Max.Y+n)
			case "-dx":
				// Deltax: neg max-n, abs min+n, pos max+n
				req.rect.Max.X = set(req.rect.Max.X-n, req.rect.Min.X+n, req.rect.Max.X+n)
			case "-dy":
				req.rect.Max.Y = set(req.rect.Max.Y-n, req.rect.Min.Y+n, req.rect.Max.Y+n)
			}
		case "-pid":
			i++
			if i >= len(fields) {
				return wctlRequest{}, ErrBadParam
			}
			// rio eats a leading sign for -pid and ignores it (no set())
			n, err := rioStrtol(fields[i])
			if err != nil {
				return wctlRequest{}, ErrBadParam
			}
			req.pid = n
		case "-id":
			i++
			if i >= len(fields) {
				return wctlRequest{}, ErrBadParam
			}
			n, err := rioStrtol(fields[i])
			if err != nil {
				return wctlRequest{}, ErrBadParam
			}
			req.id = n
		case "-hide":
			req.hide = true
		case "-scroll":
			req.scroll = +1
		case "-noscroll":
			req.scroll = -1
		case "-cd":
			i++
			if i >= len(fields) {
				return wctlRequest{}, ErrBadParam
			}
			// the value is consumed and ignored: the device does not exec
		default:
			if req.cmd == "new" {
				// rio: trailing text after new is the command to run; the
				// device records it (whitespace-normalized by the field
				// split) for the display server's launch wiring
				req.arg = strings.Join(fields[i:], " ")
				break scan
			}
			return wctlRequest{}, ErrBadParam
		}
		i++
	}
	// rio checks the resolved rect for new and resize (Ebadwr); move is
	// re-based on the -id target's size by the caller and checked there
	if req.cmd == "new" || req.cmd == "resize" {
		if req.rect.Min.X >= req.rect.Max.X || req.rect.Min.Y >= req.rect.Max.Y {
			return wctlRequest{}, ErrBadRect
		}
	}
	return req, nil
}

// applyWctl performs a parsed command. Geometry verbs queue the resolved
// line on the target slot's srv/wctl queue (the display server commits
// geometry via srv/geom writes; the device never resizes a slot itself).
// The slot verbs act on device state and note the change like label writes.
func (d *Device) applyWctl(w *window, req wctlRequest) error {
	target, err := d.resolveTarget(w, req.id)
	if err != nil {
		return err
	}
	switch req.cmd {
	case "new":
		return d.newWindowFromWctl(req)
	case "resize":
		return writeWctlLine(target, formatWctlRect("resize", req.rect))
	case "move":
		// rio Move keeps the target's size and takes the new origin
		dx, dy := target.rectSize()
		rect := image.Rect(req.rect.Min.X, req.rect.Min.Y, req.rect.Min.X+dx, req.rect.Min.Y+dy)
		if rect.Min.X >= rect.Max.X || rect.Min.Y >= rect.Max.Y {
			return ErrBadRect
		}
		return writeWctlLine(target, formatWctlRect("move", rect))
	case "current", "top", "bottom":
		return writeWctlLine(target, req.cmd+"\n")
	case "set":
		// rio wsetpid: pid <= 0 is ignored and still succeeds
		if req.pid > 0 {
			target.setPID(req.pid)
		}
		return nil
	case "delete":
		return d.deleteWindow(target)
	case "hide":
		if err := target.setHidden(true); err != nil {
			return err
		}
		target.note("hide")
		return nil
	case "unhide":
		if err := target.setHidden(false); err != nil {
			return err
		}
		target.note("unhide")
		return nil
	case "scroll":
		target.setScrolling(true)
		target.note("scroll")
		return nil
	case "noscroll":
		target.setScrolling(false)
		target.note("noscroll")
		return nil
	}
	return ErrBadCommand
}

// resolveTarget returns the window a command acts on: the writing window
// unless -id names another slot (rio's retarget after parsewctl).
func (d *Device) resolveTarget(w *window, id int) (*window, error) {
	if id == 0 {
		return w, nil
	}
	t := d.lookup(id)
	if t == nil {
		return nil, ErrNoSuchWindow
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, ErrWindowDeleted
	}
	return t, nil
}

// newWindowFromWctl is wctl new (rio's wctlnew minus the exec): it
// allocates the slot with the resolved rect and recorded flags, then
// announces the new slot on its own srv/wctl queue. The display server
// discovers the slot through the srv/state enumeration; launch wiring and
// the -pid attach stay outside the device.
func (d *Device) newWindowFromWctl(req wctlRequest) error {
	d.mu.Lock()
	w, err := d.newWindowLocked()
	d.mu.Unlock()
	if err != nil {
		// rio's Ewalloc: the allocation failed (device slot cap)
		return ErrAllocFailed
	}
	w.mu.Lock()
	w.rect = req.rect
	w.pid = req.pid
	w.hidden = req.hide
	w.scrolling = req.scroll >= 0
	w.cmd = req.arg
	w.mu.Unlock()
	// the slot handshake: a fresh queue always accepts the announcement
	_, _ = w.wctlQ.Write([]byte(formatWctlRect(fmt.Sprintf("new %d", w.id), req.rect)))
	return nil
}

// deleteWindow is wctl delete: the srv/ctl close teardown, synchronous, for
// the writing window or any -id target (rio has no per-window ownership).
func (d *Device) deleteWindow(w *window) error {
	w.mu.Lock()
	err := w.closeLocked()
	w.mu.Unlock()
	if err != nil {
		return ErrIO
	}
	d.removeWindow(w.id)
	return nil
}

func writeWctlLine(w *window, line string) error {
	_, err := w.wctlQ.Write([]byte(line))
	return err
}

// ---- slot state changes (noted to the host like label writes) ----

func (w *window) note(line string) { _, _ = w.notesQ.Write([]byte(line + "\n")) }

func (w *window) setPID(pid int) {
	w.mu.Lock()
	w.pid = pid
	w.mu.Unlock()
	w.note(fmt.Sprintf("pid %d", pid))
}

func (w *window) setHidden(h bool) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if h == w.hidden {
		if h {
			return ErrAlreadyHidden
		}
		return ErrNotHidden
	}
	w.hidden = h
	// the wctl geometry line reports visible|hidden: wake its readers
	w.wctlSeq++
	w.cond.Broadcast()
	return nil
}

func (w *window) setScrolling(on bool) {
	w.mu.Lock()
	w.scrolling = on
	w.mu.Unlock()
}

func (w *window) rectSize() (int, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rect.Dx(), w.rect.Dy()
}

// rioStrtol accepts plan 9's optional square brackets around rectangles.
func rioStrtol(s string) (int, error) {
	s = strings.Trim(s, "[]")
	return strconv.Atoi(s)
}
