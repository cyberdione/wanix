package wsys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/signal"
	"tractor.dev/wanix/fs/vfs"
)

// ---- wctl slot verbs (ADR 0007 K1) ----

// wctlWrite writes one command to id's wctl file and releases the
// single-reader slot.
func wctlWrite(t *testing.T, d *Device, id, cmd string) {
	t.Helper()
	wf := openWin(t, d, id, "wctl")
	if _, err := fs.Write(wf, []byte(cmd)); err != nil {
		wf.Close()
		t.Fatalf("wctl write %q on %s: %v", cmd, id, err)
	}
	if err := wf.Close(); err != nil {
		t.Fatalf("wctl close on %s: %v", id, err)
	}
}

// drainQueue reads one buffered frame from a queue-backed file.
func drainQueue(t *testing.T, f fs.File) string {
	t.Helper()
	b := make([]byte, 4096)
	n, err := f.Read(b)
	if err != nil {
		t.Fatalf("queue read: %v", err)
	}
	return string(b[:n])
}

func TestWctlNewAllocatesSlot(t *testing.T) {
	d, _, id := alloc(t)
	wctlWrite(t, d, id, "new -r 0 0 400 300\n")

	// the slot exists with the recorded rect, visible in the listing
	st := stateJSON(t, d, "2")
	if st["id"].(float64) != 2 {
		t.Fatalf("new slot id = %v", st["id"])
	}
	if got := fmt.Sprint(st["rect"]); got != "[0 0 400 300]" {
		t.Fatalf("new slot rect = %v", got)
	}

	// the handshake: the new slot announces itself on its own srv/wctl
	nf := openWin(t, d, "2", "srv/wctl")
	if line := drainQueue(t, nf); line != "new 2 0 0 400 300\n" {
		t.Fatalf("new announcement = %q", line)
	}
	// and through the device-level enumeration
	list := listingJSON(t, d)
	if len(list.Windows) != 2 || list.Windows[1].ID != 2 || list.Windows[1].Label != "<unnamed>" ||
		list.Windows[1].Rect != [4]int{0, 0, 400, 300} || !list.Windows[1].Visible || list.Windows[1].PID != 0 {
		t.Fatalf("enumeration = %+v", list.Windows)
	}
}

func TestWctlNewDefaultRectCascade(t *testing.T) {
	d, _, id := alloc(t)
	// rio newrect(): 600x400 at 32+16*i, cycling ten positions
	wctlWrite(t, d, id, "new\n")
	wctlWrite(t, d, id, "new\n")
	list := listingJSON(t, d)
	if list.Windows[1].Rect != [4]int{32, 32, 632, 432} {
		t.Fatalf("first cascade rect = %v", list.Windows[1].Rect)
	}
	if list.Windows[2].Rect != [4]int{48, 48, 648, 448} {
		t.Fatalf("second cascade rect = %v", list.Windows[2].Rect)
	}
}

func TestWctlNewRecordsPidHideScrollAndCommand(t *testing.T) {
	d, _, id := alloc(t)
	wctlWrite(t, d, id, "new -pid 42 -hide -noscroll rio-edit main.c\n")
	st := stateJSON(t, d, "2")
	if st["pid"].(float64) != 42 {
		t.Fatalf("pid = %v", st["pid"])
	}
	if st["visible"] != false {
		t.Fatalf("visible = %v, want hidden", st["visible"])
	}
	if st["scrolling"] != false {
		t.Fatalf("scrolling = %v", st["scrolling"])
	}
	if st["cmd"] != "rio-edit main.c" {
		t.Fatalf("cmd = %v", st["cmd"])
	}
	// -scroll keeps the scrolling default
	wctlWrite(t, d, id, "new -scroll\n")
	if st = stateJSON(t, d, "3"); st["scrolling"] != true {
		t.Fatalf("scrolling = %v", st["scrolling"])
	}
}

func TestWctlNewExhaustionFailsLikeRioEwalloc(t *testing.T) {
	d := New()
	d.MaxWindows = 1
	f, err := d.Open("new")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	id := strings.TrimSpace(string(b))
	wf := openWin(t, d, id, "wctl")
	defer wf.Close()
	if _, err := fs.Write(wf, []byte("new\n")); !errors.Is(err, ErrAllocFailed) {
		t.Fatalf("new on full device = %v, want ErrAllocFailed", err)
	}
}

func TestWctlDeleteFreesSlot(t *testing.T) {
	d, _, id := alloc(t)
	wctlWrite(t, d, id, "new -r 0 0 100 100\n")
	// a foreign delete (-id) tears the target down: reads fail closed and
	// the id is released
	wctlWrite(t, d, id, "delete -id 2\n")
	if _, err := d.Stat("2/label"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat after delete = %v", err)
	}
	// deleting an unknown id is rio's "no such window id"
	wf := openWin(t, d, id, "wctl")
	if _, err := fs.Write(wf, []byte("delete -id 99\n")); !errors.Is(err, ErrNoSuchWindow) {
		wf.Close()
		t.Fatalf("delete unknown = %v", err)
	}
	wf.Close()
	// deleting the writing window itself frees it
	wctlWrite(t, d, id, "delete\n")
	if _, err := d.Stat(id + "/label"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat after self delete = %v", err)
	}
	// the enumeration drops the slots
	list := listingJSON(t, d)
	if len(list.Windows) != 0 {
		t.Fatalf("enumeration after deletes = %+v", list.Windows)
	}
}

func TestWctlHideUnhide(t *testing.T) {
	d, _, id := alloc(t)
	gf := openWin(t, d, id, "srv/geom")
	if _, err := fs.Write(gf, []byte("0 0 560 380\n")); err != nil {
		t.Fatal(err)
	}

	// error strings and state flips on one held wctl handle
	wf := openWin(t, d, id, "wctl")
	// unhide of a visible window is rio's "window not hidden"
	if _, err := fs.Write(wf, []byte("unhide\n")); !errors.Is(err, ErrNotHidden) {
		wf.Close()
		t.Fatalf("unhide of visible = %v", err)
	}
	// hide flips the state; the host sees the note like a label write
	if _, err := fs.Write(wf, []byte("hide\n")); err != nil {
		wf.Close()
		t.Fatal(err)
	}
	if st := stateJSON(t, d, id); st["visible"] != false {
		wf.Close()
		t.Fatalf("visible = %v", st["visible"])
	}
	// hide of a hidden window is rio's "window already hidden"
	if _, err := fs.Write(wf, []byte("hide\n")); !errors.Is(err, ErrAlreadyHidden) {
		wf.Close()
		t.Fatalf("double hide = %v", err)
	}
	// unhide restores
	if _, err := fs.Write(wf, []byte("unhide\n")); err != nil {
		wf.Close()
		t.Fatal(err)
	}
	if st := stateJSON(t, d, id); st["visible"] != true {
		wf.Close()
		t.Fatalf("visible after unhide = %v", st["visible"])
	}
	wf.Close()
	nf := openWin(t, d, id, "srv/notes")
	note := drainQueue(t, nf)
	nf.Close()
	if note != "hide\nunhide\n" {
		t.Fatalf("notes = %q", note)
	}

	// a blocked wctl reader wakes on the visibility change: the geometry
	// line's last word flips (rio wind.c WWread). The hide goes through the
	// same single-reader handle, as a rio wctl fd would.
	wf = openWin(t, d, id, "wctl")
	defer wf.Close()
	b := make([]byte, 128)
	if _, err := wf.Read(b); err != nil { // armed at open
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		b2 := make([]byte, 128)
		n2, err2 := wf.Read(b2)
		if err2 != nil {
			got <- "err:" + err2.Error()
			return
		}
		got <- string(b2[:n2])
	}()
	select {
	case <-got:
		t.Fatal("wctl read did not block before hide")
	case <-time.After(10 * time.Millisecond):
	}
	if _, err := fs.Write(wf, []byte("hide\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-got:
		if !strings.HasSuffix(line, "notcurrent hidden ") {
			t.Fatalf("hidden line = %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("wctl read stayed blocked after hide")
	}
}

func TestWctlScrollNoscroll(t *testing.T) {
	d, _, id := alloc(t)
	if st := stateJSON(t, d, id); st["scrolling"] != true {
		t.Fatalf("default scrolling = %v", st["scrolling"])
	}
	wctlWrite(t, d, id, "noscroll\n")
	if st := stateJSON(t, d, id); st["scrolling"] != false {
		t.Fatalf("scrolling after noscroll = %v", st["scrolling"])
	}
	wctlWrite(t, d, id, "scroll\n")
	if st := stateJSON(t, d, id); st["scrolling"] != true {
		t.Fatalf("scrolling after scroll = %v", st["scrolling"])
	}
	nf := openWin(t, d, id, "srv/notes")
	if note := drainQueue(t, nf); note != "noscroll\nscroll\n" {
		t.Fatalf("notes = %q", note)
	}
}

func TestWctlSetPid(t *testing.T) {
	d, _, id := alloc(t)
	wctlWrite(t, d, id, "new\n")
	// set on another slot via -id
	wctlWrite(t, d, id, "set -id 2 -pid 7\n")
	if st := stateJSON(t, d, "2"); st["pid"].(float64) != 7 {
		t.Fatalf("pid = %v", st["pid"])
	}
	// set on the writing window
	wctlWrite(t, d, id, "set -pid 9\n")
	if st := stateJSON(t, d, id); st["pid"].(float64) != 9 {
		t.Fatalf("pid = %v", st["pid"])
	}
	// rio ignores pid <= 0 and still succeeds
	wctlWrite(t, d, id, "set -pid 0\n")
	if st := stateJSON(t, d, id); st["pid"].(float64) != 9 {
		t.Fatalf("pid after -pid 0 = %v", st["pid"])
	}
	nf := openWin(t, d, id, "srv/notes")
	if note := drainQueue(t, nf); note != "pid 9\n" {
		t.Fatalf("notes = %q", note)
	}
}

func TestWctlTargetedGeometryQueuesOnTarget(t *testing.T) {
	d, _, id := alloc(t)
	// the writing window's rect is the arithmetic base (rio's parse
	// ordering), so commit one first, exactly like the frozen grammar test
	gf := openWin(t, d, id, "srv/geom")
	if _, err := fs.Write(gf, []byte("10 20 570 400\n")); err != nil {
		t.Fatal(err)
	}
	wctlWrite(t, d, id, "new -r 0 0 100 100\n")
	tf := openWin(t, d, "2", "srv/wctl")
	defer tf.Close()
	// the slot handshake announcement arrives first
	if line := drainQueue(t, tf); line != "new 2 0 0 100 100\n" {
		t.Fatalf("new announcement = %q", line)
	}
	// the resolved line lands on the target's srv/wctl queue
	wctlWrite(t, d, id, "resize -id 2 -dx +50\n")
	if line := drainQueue(t, tf); line != "resize 10 20 620 400\n" {
		t.Fatalf("targeted resize line = %q", line)
	}
	// the device records intent only: the target's rect is unchanged
	// until the display server commits via srv/geom
	if st := stateJSON(t, d, "2"); fmt.Sprint(st["rect"]) != "[0 0 100 100]" {
		t.Fatalf("target rect = %v", st["rect"])
	}
	// targeted move re-bases on the target's size (rio w->screenr)
	wctlWrite(t, d, id, "move -id 2 -minx 200 -miny 100\n")
	if line := drainQueue(t, tf); line != "move 200 100 300 200\n" {
		t.Fatalf("targeted move line = %q", line)
	}
	// current/top/bottom stay parameter-less (the frozen grammar): focus on
	// another slot goes through that slot's own wctl file
	tf.Close()
	wctlWrite(t, d, "2", "current\n")
	tf2 := openWin(t, d, "2", "srv/wctl")
	defer tf2.Close()
	if line := drainQueue(t, tf2); line != "current\n" {
		t.Fatalf("slot current line = %q", line)
	}
	if _, err := fs.Write(openWin(t, d, id, "wctl"), []byte("resize -id 99 -dx 1\n")); !errors.Is(err, ErrNoSuchWindow) {
		t.Fatalf("resize unknown id = %v", err)
	}
}

// ---- srv/state enumeration ----

func listingJSON(t *testing.T, d *Device) deviceState {
	t.Helper()
	f, err := d.Open("srv/state")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	var list deviceState
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("listing json %q: %v", b, err)
	}
	if list.Version != Version {
		t.Fatalf("listing version = %q", list.Version)
	}
	return list
}

func TestEnumerationStableOrderAndSnapshot(t *testing.T) {
	d, _, id := alloc(t)
	wctlWrite(t, d, id, "new -pid 5\n")      // 2
	wctlWrite(t, d, "2", "new -r 1 2 3 4\n") // 3

	list := listingJSON(t, d)
	if len(list.Windows) != 3 {
		t.Fatalf("windows = %+v", list.Windows)
	}
	if list.Windows[0].ID != 1 || list.Windows[1].ID != 2 || list.Windows[2].ID != 3 {
		t.Fatalf("order = %+v", list.Windows)
	}
	if list.Windows[1].PID != 5 || !list.Windows[1].Visible {
		t.Fatalf("slot 2 = %+v", list.Windows[1])
	}
	if list.Windows[2].Rect != [4]int{1, 2, 3, 4} {
		t.Fatalf("slot 3 rect = %+v", list.Windows[2].Rect)
	}

	// one stable snapshot per open: state changes between the reads of a
	// single handle do not tear the listing
	f, err := d.Open("srv/state")
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, 48)
	n, err := f.Read(head)
	if err != nil {
		f.Close()
		t.Fatal(err)
	}
	wctlWrite(t, d, id, "hide -id 3\n")
	rest, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	var snap deviceState
	if err := json.Unmarshal(append(head[:n], rest...), &snap); err != nil {
		t.Fatalf("snapshot json: %v", err)
	}
	if !snap.Windows[2].Visible {
		t.Fatalf("snapshot torn mid-open: %+v", snap.Windows[2])
	}
	// the next open sees the change
	list = listingJSON(t, d)
	if list.Windows[2].Visible {
		t.Fatalf("slot 3 still visible in fresh listing: %+v", list.Windows[2])
	}
}

func TestEnumerationIdsNeverReuse(t *testing.T) {
	d, _, id := alloc(t)
	wctlWrite(t, d, id, "new\n")
	wctlWrite(t, d, id, "delete -id 2\n")
	wctlWrite(t, d, id, "new\n")
	list := listingJSON(t, d)
	if len(list.Windows) != 2 || list.Windows[1].ID != 3 {
		t.Fatalf("ids = %+v", list.Windows)
	}
}

// ---- task-side grants ----

// bindDevice mounts d in a fresh namespace the way the driver does.
func bindDevice(t *testing.T, d *Device, src, dst string) *vfs.NS {
	t.Helper()
	ns := vfs.New(context.Background())
	if err := ns.Bind(d, ".", "#wsys"); err != nil {
		t.Fatal(err)
	}
	if err := ns.Bind(ns, src, dst); err != nil {
		t.Fatalf("bind %s: %v", src, err)
	}
	return ns
}

func mustDeny(t *testing.T, ns *vfs.NS, path string) {
	t.Helper()
	// stat keeps the grant's permission error (ResolveTo reaches the grant);
	// NS.OpenContext swallows the open error into not-exist, so a
	// default-grant task simply does not see the path. Both must fail.
	if _, err := ns.Stat(path); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("stat %s = %v, want permission denied", path, err)
	}
	if _, err := ns.Open(path); err == nil {
		t.Fatalf("open %s succeeded, want a denial", path)
	}
}

func TestDefaultGrantDeniesSrvPlane(t *testing.T) {
	d, _, id := alloc(t)
	wctlWrite(t, d, id, "new\n")

	ns := bindDevice(t, d, "#wsys", "mnt/wsys")
	// window files stay reachable
	f, err := ns.Open("mnt/wsys/" + id + "/label")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	// at the grant layer itself both stat and open deny with permission
	gr, err := d.BindAllocFS("#wsys")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{id + "/srv/consin", id + "/srv/ctl", "srv/state"} {
		if _, err := gr.(grant).OpenContext(context.Background(), p); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("grant open %s = %v, want permission denied", p, err)
		}
		if _, err := gr.(grant).StatContext(context.Background(), p); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("grant stat %s = %v, want permission denied", p, err)
		}
	}
	// the whole per-window srv plane and the device enumeration are denied
	for _, p := range []string{
		"mnt/wsys/" + id + "/srv",
		"mnt/wsys/" + id + "/srv/consin",
		"mnt/wsys/" + id + "/srv/consout",
		"mnt/wsys/" + id + "/srv/mouse",
		"mnt/wsys/" + id + "/srv/geom",
		"mnt/wsys/" + id + "/srv/wctl",
		"mnt/wsys/" + id + "/srv/notes",
		"mnt/wsys/" + id + "/srv/state",
		"mnt/wsys/" + id + "/srv/ctl",
		"mnt/wsys/srv",
		"mnt/wsys/srv/state",
	} {
		mustDeny(t, ns, p)
	}

	// single-window binds keep N10 semantics: the device's Route hands
	// them straight to the raw window (BindAllocFS never runs for that
	// shape), so their srv visibility stays gated by the worker's openat
	// allowlist — the device-level denial here covers the driver's actual
	// mount, the whole-device default bind
	ns2 := bindDevice(t, d, "#wsys/"+id, "dev")
	if _, err := ns2.Open("dev/label"); err != nil {
		t.Fatal(err)
	}
}

func TestWMGrantAdmitsSrvPlaneAndRoutesInput(t *testing.T) {
	d, _, id := alloc(t)
	wctlWrite(t, d, id, "new\n")

	ns := bindDevice(t, d, "#wsys/wm", "mnt/wsys")
	// the WM grant admits the enumeration
	f, err := ns.Open("mnt/wsys/srv/state")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	var list deviceState
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("wm enumeration %q: %v", b, err)
	}
	if len(list.Windows) != 2 {
		t.Fatalf("wm enumeration = %+v", list.Windows)
	}

	// input routing: through the grant, the WM writes cons input and mouse
	// events into another slot's srv files and the slot's window files
	// deliver them exactly as merged (blocking reads, byte-exact records)
	cmf := openWin(t, d, "2", "cons")
	got := make(chan []byte, 1)
	go func() {
		b2 := make([]byte, 32)
		n2, err2 := cmf.Read(b2)
		if err2 != nil {
			got <- []byte("err:" + err2.Error())
			return
		}
		got <- b2[:n2]
	}()
	cinf, err := ns.Open("mnt/wsys/2/srv/consin")
	if err != nil {
		t.Fatalf("wm consin open: %v", err)
	}
	if _, err := fs.Write(cinf, []byte("from the wm\n")); err != nil {
		t.Fatal(err)
	}
	cinf.Close()
	select {
	case b3 := <-got:
		if string(b3) != "from the wm\n" {
			t.Fatalf("cons read = %q", b3)
		}
	case <-time.After(time.Second):
		t.Fatal("cons read stayed blocked")
	}
	cmf.Close()

	mf := openWin(t, d, "2", "mouse")
	defer mf.Close()
	mrec := make(chan []byte, 1)
	go func() {
		b2 := make([]byte, 49)
		n2, err2 := mf.Read(b2)
		if err2 != nil {
			mrec <- []byte("err:" + err2.Error())
			return
		}
		mrec <- b2[:n2]
	}()
	mf2, err := ns.Open("mnt/wsys/2/srv/mouse")
	if err != nil {
		t.Fatalf("wm mouse open: %v", err)
	}
	if _, err := fs.Write(mf2, []byte("1 100 200\n")); err != nil {
		t.Fatal(err)
	}
	mf2.Close()
	select {
	case rec := <-mrec:
		if len(rec) != MouseRecordLen || rec[0] != 'm' {
			t.Fatalf("mouse record = %q", rec)
		}
		if !strings.Contains(string(rec), fmt.Sprintf("%11d %11d", 100, 200)) {
			t.Fatalf("mouse record = %q", rec)
		}
	case <-time.After(time.Second):
		t.Fatal("mouse read stayed blocked")
	}

	// the wm bind keeps the window files reachable too
	if _, err := ns.Open("mnt/wsys/" + id + "/label"); err != nil {
		t.Fatal(err)
	}
	// and still denies everything outside the srv plane and window files
	if _, err := ns.Open("mnt/wsys/2/winname"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("winname = %v", err)
	}
	if _, err := ns.Open("mnt/wsys/wm"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("wm path = %v", err)
	}
}

func TestBindAllocShapes(t *testing.T) {
	d, _, id := alloc(t)
	// the object bind (".", the runtime's registration) is the raw device:
	// the display server keeps full srv access through the root
	raw, err := d.BindAllocFS(".")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := raw.(*Device); !ok {
		t.Fatalf(". bind = %T", raw)
	}
	g, err := d.BindAllocFS("#wsys")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := g.(grant); !ok {
		t.Fatalf("#wsys bind = %T", g)
	}
	g, err = d.BindAllocFS("#wsys/wm")
	if err != nil {
		t.Fatal(err)
	}
	if gr, ok := g.(grant); !ok || !gr.wm {
		t.Fatalf("#wsys/wm bind = %T", g)
	}
	g, err = d.BindAllocFS("#wsys/" + id)
	if err != nil {
		t.Fatal(err)
	}
	// single-window binds resolve straight to the raw window (N10)
	if _, ok := g.(*window); !ok {
		t.Fatalf("#wsys/<id> bind = %T", g)
	}
	if _, err := d.BindAllocFS("#wsys/wm/x"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("#wsys/wm/x = %v", err)
	}
	// saturation of the host allocator is unchanged (allocation happens at
	// read, the "new" file's ReadFunc)
	d.MaxWindows = 1
	nf, err := d.Open("new")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(nf)
	nf.Close()
	if !errors.Is(err, signal.ErrSaturated) {
		t.Fatalf("host new on full device = %v", err)
	}
}
