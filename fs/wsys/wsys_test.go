package wsys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/signal"
	"tractor.dev/wanix/fs/vfs"
)

// alloc returns a device with one allocated window and its id.
func alloc(t *testing.T) (*Device, *window, string) {
	t.Helper()
	d := New()
	f, err := d.Open("new")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(string(b))
	w := d.lookup(mustAtoi(t, id))
	if w == nil {
		t.Fatalf("window %s not registered", id)
	}
	return d, w, id
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("bad id %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func openWin(t *testing.T, d *Device, id, name string) fs.File {
	t.Helper()
	f, err := d.Open(id + "/" + name)
	if err != nil {
		t.Fatalf("open %s/%s: %v", id, name, err)
	}
	return f
}

func TestAllocationAndListing(t *testing.T) {
	d := New()
	var ids []string
	for range 2 {
		f, err := d.Open("new")
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, strings.TrimSpace(string(b)))
	}
	if ids[0] == ids[1] || ids[0] != "1" || ids[1] != "2" {
		t.Fatalf("ids = %v", ids)
	}
	dir, err := d.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	dir.Close()
	names := []string{}
	for _, id := range d.windowIDs() {
		names = append(names, fmt.Sprint(id))
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "1,2" {
		t.Fatalf("windowIDs = %v", names)
	}
}

func TestLabelReadWriteAndNote(t *testing.T) {
	_, w, id := alloc(t)
	lf := openWin(t, w.dev, id, "label")
	if _, err := fs.Write(lf, []byte("hello-window\n")); err != nil {
		t.Fatal(err)
	}
	// a fresh read sees the current label from offset 0
	rf := openWin(t, w.dev, id, "label")
	got, err := io.ReadAll(rf)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello-window\n" {
		t.Fatalf("label read = %q", got)
	}
	// the host sees the change on the notes queue
	nf := openWin(t, w.dev, id, "srv/notes")
	note := make([]byte, 64)
	n, err := nf.Read(note)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(note[:n])) != "label hello-window" {
		t.Fatalf("note = %q", note[:n])
	}
}

func TestMouseRecordFormat(t *testing.T) {
	rec := mouseRecord('m', 10, 20, 1, 1234567)
	if len(rec) != MouseRecordLen || MouseRecordLen != 49 {
		t.Fatalf("record len %d", len(rec))
	}
	want := "m         10          20           1     1234567 "
	if string(rec) != want {
		t.Fatalf("record = %q want %q", rec, want)
	}
}

func TestMouseSingleReaderSlot(t *testing.T) {
	d, _, id := alloc(t)
	m1 := openWin(t, d, id, "mouse")
	defer m1.Close()
	if _, err := d.Open(id + "/mouse"); !errors.Is(err, ErrInUse) {
		t.Fatalf("second mouse open = %v", err)
	}
	// closing the descriptor frees the slot
	m1.Close()
	m2 := openWin(t, d, id, "mouse")
	defer m2.Close()
}

func TestMouseBlockingDelivery(t *testing.T) {
	d, _, id := alloc(t)
	mf := openWin(t, d, id, "mouse")
	defer mf.Close()
	got := make(chan []byte, 1)
	go func() {
		b := make([]byte, 49)
		n, err := mf.Read(b)
		if err != nil {
			got <- []byte("err:" + err.Error())
			return
		}
		got <- b[:n]
	}()
	select {
	case <-got:
		t.Fatal("mouse read did not block")
	case <-time.After(10 * time.Millisecond):
	}
	sf := openWin(t, d, id, "srv/mouse")
	if _, err := fs.Write(sf, []byte("0 100 200\n")); err != nil {
		t.Fatal(err)
	}
	sf.Close()
	select {
	case b := <-got:
		if b[0] != 'm' {
			t.Fatalf("record = %c, want m", b[0])
		}
		if !strings.Contains(string(b), fmt.Sprintf("%11d %11d", 100, 200)) {
			t.Fatalf("record = %q", b)
		}
	case <-time.After(time.Second):
		t.Fatal("mouse read stayed blocked")
	}
}

func TestMouseQueueSaturation(t *testing.T) {
	d := New()
	f, err := d.Open("new")
	if err != nil {
		t.Fatal(err)
	}
	idBytes, _ := io.ReadAll(f)
	f.Close()
	id := strings.TrimSpace(string(idBytes))
	d.MaxMouseEvents = 2
	for i := range 3 {
		sf := openWin(t, d, id, "srv/mouse")
		_, err := fs.Write(sf, []byte(fmt.Sprintf("%d %d %d\n", i, 100+i, 200+i)))
		sf.Close()
		if i == 2 && !errors.Is(err, signal.ErrSaturated) {
			t.Fatalf("third publish err = %v, want saturation", err)
		}
		if i < 2 && err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
}

func TestMouseResizeCoalescing(t *testing.T) {
	d, w, id := alloc(t)
	gf := openWin(t, d, id, "srv/geom")
	if _, err := fs.Write(gf, []byte("0 0 560 380\n")); err != nil {
		t.Fatal(err)
	}

	// two geometry commits with no reader coalesce to one 'r'
	if _, err := fs.Write(gf, []byte("0 0 660 430\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Write(gf, []byte("0 0 760 480\n")); err != nil {
		t.Fatal(err)
	}

	mf := openWin(t, d, id, "mouse")
	defer mf.Close()
	b := make([]byte, 49)
	n, err := mf.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	if b[0] != 'r' {
		t.Fatalf("first record = %q, want resize", b[:n])
	}
	// no queued event was consumed and the flag is cleared: the next read
	// blocks
	got := make(chan []byte, 1)
	go func() {
		b2 := make([]byte, 49)
		n2, err2 := mf.Read(b2)
		if err2 != nil {
			got <- []byte("err:" + err2.Error())
			return
		}
		got <- b2[:n2]
	}()
	select {
	case <-got:
		t.Fatal("second read did not block after resize was delivered")
	case <-time.After(20 * time.Millisecond):
	}
	// an event delivered while a resize is pending rebrands the record.
	// The black-box pair (srv/geom write, then srv/mouse write) lets the
	// parked reader wake between the two commits, delivering the resize
	// with the pre-event mouse state — legal rio behavior that made this
	// test flaky. Perform the same two commits atomically under the window
	// lock (srvMouseFile.Write's exact mutation order) so the reader sees
	// both; the asserted outcome is unchanged.
	w.mu.Lock()
	w.setRectLocked(image.Rect(0, 0, 760, 480))
	w.mouse = mouseState{buttons: 1, x: 300, y: 220}
	if err := w.publishMouseLocked(1, 300, 220); err != nil {
		w.mu.Unlock()
		t.Fatal(err)
	}
	w.mu.Unlock()
	select {
	case b3 := <-got:
		if b3[0] != 'r' {
			t.Fatalf("event under pending resize = %c, want r", b3[0])
		}
		if !strings.Contains(string(b3), fmt.Sprintf("%11d %11d", 300, 220)) {
			t.Fatalf("resize record carries the event position: %q", b3)
		}
	case <-time.After(time.Second):
		t.Fatal("read stayed blocked after publish")
	}
}

func TestWctlWriteGrammar(t *testing.T) {
	d, w, id := alloc(t)
	gf := openWin(t, d, id, "srv/geom")
	if _, err := fs.Write(gf, []byte("10 20 570 400\n")); err != nil {
		t.Fatal(err)
	}
	wf := openWin(t, d, id, "wctl")
	defer wf.Close()

	cases := []struct {
		cmd  string
		want string
	}{
		{"resize -dx +100 -dy +50\n", "resize 10 20 670 450"},
		{"resize -dx 100\n", "resize 10 20 110 400"},
		{"move -minx 100 -miny 200\n", "move 100 200 660 580"},
		{"resize -r 0 0 800 600\n", "resize 0 0 800 600"},
		{"current\n", "current"},
		{"top\n", "top"},
	}
	for _, c := range cases {
		if _, err := fs.Write(wf, []byte(c.cmd)); err != nil {
			t.Fatalf("write %q: %v", c.cmd, err)
		}
	}
	bad := []struct {
		cmd string
		err error
	}{
		// new and delete are real verbs now (ADR 0007 K1): their grammar
		// and effects live in the dedicated tests below
		{"frobnicate", ErrBadCommand},
		{"resize -dx", ErrBadParam},
		{"resize -zz 5", ErrBadParam},
		{"current extra", ErrExtraText},
	}
	for _, c := range bad {
		_, err := fs.Write(wf, []byte(c.cmd))
		if !errors.Is(err, c.err) {
			t.Fatalf("write %q err = %v, want %v", c.cmd, err, c.err)
		}
	}

	// the host drains the resolved commands in order
	hf := openWin(t, d, id, "srv/wctl")
	var lines []string
	buf := make([]byte, 4096)
	n, err := hf.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	lines = strings.FieldsFunc(string(buf[:n]), func(r rune) bool { return r == '\n' })
	want := []string{"resize 10 20 670 450", "resize 10 20 110 400", "move 100 200 660 580", "resize 0 0 800 600", "current", "top"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("wctl queue = %v want %v", lines, want)
	}
	_ = w
}

func TestWctlReadArmedThenBlocksUntilGeometryChange(t *testing.T) {
	d, _, id := alloc(t)
	gf := openWin(t, d, id, "srv/geom")
	if _, err := fs.Write(gf, []byte("0 0 560 380\n")); err != nil {
		t.Fatal(err)
	}

	wf := openWin(t, d, id, "wctl")
	defer wf.Close()

	// too short
	if _, err := wf.Read(make([]byte, 10)); !errors.Is(err, ErrTooShort) {
		t.Fatalf("short read = %v", err)
	}

	// armed at open: first read returns the current geometry immediately
	b := make([]byte, 128)
	n, err := wf.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	line := string(b[:n])
	want := fmt.Sprintf("%11d %11d %11d %11d %s %s ", 0, 0, 560, 380, "notcurrent", "visible")
	if line != want {
		t.Fatalf("first geometry = %q want %q", line, want)
	}

	// the next read blocks until a geometry commit
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
		t.Fatal("second wctl read did not block")
	case <-time.After(10 * time.Millisecond):
	}
	if _, err := fs.Write(gf, []byte("10 20 670 450\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case line2 := <-got:
		want2 := fmt.Sprintf("%11d %11d %11d %11d %s %s ", 10, 20, 670, 450, "notcurrent", "visible")
		if line2 != want2 {
			t.Fatalf("second geometry = %q want %q", line2, want2)
		}
	case <-time.After(time.Second):
		t.Fatal("wctl read stayed blocked")
	}

	// single reader
	if _, err := d.Open(id + "/wctl"); !errors.Is(err, ErrInUse) {
		t.Fatalf("second wctl open = %v", err)
	}
}

func TestWctlCurrentWord(t *testing.T) {
	d, _, id := alloc(t)
	cf := openWin(t, d, id, "srv/ctl")
	if _, err := fs.Write(cf, []byte("current on")); err != nil {
		t.Fatal(err)
	}
	wf := openWin(t, d, id, "wctl")
	defer wf.Close()
	b := make([]byte, 128)
	n, err := wf.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(b[:n]), " current visible ") {
		t.Fatalf("geometry line = %q", b[:n])
	}
}

func TestConsRouting(t *testing.T) {
	d, _, id := alloc(t)
	// host publishes console input; the task's cons read consumes it
	inf := openWin(t, d, id, "srv/consin")
	if _, err := fs.Write(inf, []byte("type me\n")); err != nil {
		t.Fatal(err)
	}
	inf.Close()

	cf := openWin(t, d, id, "cons")
	defer cf.Close()
	b := make([]byte, 32)
	n, err := cf.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:n]) != "type me\n" {
		t.Fatalf("cons read = %q", b[:n])
	}

	// a second open multiplexes over the same console (rio cons)
	cf2 := openWin(t, d, id, "cons")
	defer cf2.Close()
	if _, err := fs.Write(cf2, []byte("out\n")); err != nil {
		t.Fatal(err)
	}
	outf := openWin(t, d, id, "srv/consout")
	ob := make([]byte, 32)
	n, err = outf.Read(ob)
	if err != nil {
		t.Fatal(err)
	}
	if string(ob[:n]) != "out\n" {
		t.Fatalf("consout = %q", ob[:n])
	}
}

func TestConsCtlModesResetOnClose(t *testing.T) {
	d, _, id := alloc(t)
	cf := openWin(t, d, id, "consctl")
	if _, err := fs.Write(cf, []byte("rawon")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Write(cf, []byte("holdon")); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Write(cf, []byte("frob")); !errors.Is(err, ErrBadCommand) {
		t.Fatalf("bad consctl = %v", err)
	}
	if _, err := cf.Read(make([]byte, 1)); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("consctl read = %v", err)
	}
	st := stateJSON(t, d, id)
	if st["raw"] != true || st["hold"] != true {
		t.Fatalf("state = %v", st)
	}
	cf.Close()
	st = stateJSON(t, d, id)
	if st["raw"] != false || st["hold"] != false {
		t.Fatalf("state after close = %v", st)
	}
}

func stateJSON(t *testing.T, d *Device, id string) map[string]any {
	t.Helper()
	f := openWin(t, d, id, "srv/state")
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("state json %q: %v", b, err)
	}
	return m
}

func TestStateReport(t *testing.T) {
	d, _, id := alloc(t)
	st := stateJSON(t, d, id)
	if st["version"] != Version {
		t.Fatalf("version = %v", st["version"])
	}
	if st["label"] != "<unnamed>" {
		t.Fatalf("default label = %v", st["label"])
	}
}

func TestCloseTeardown(t *testing.T) {
	d, _, id := alloc(t)
	cf := openWin(t, d, id, "srv/ctl")
	if _, err := fs.Write(cf, []byte("close")); err != nil {
		t.Fatal(err)
	}
	// the id is released and reads fail closed
	if _, err := d.Stat(id + "/label"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat after close = %v", err)
	}
}

func TestStatIsSideEffectFree(t *testing.T) {
	d, _, id := alloc(t)
	// the namespace router stats before opening: repeated stats must not
	// consume the single-reader slots
	for range 5 {
		if _, err := d.Stat(id + "/mouse"); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Stat(id + "/wctl"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Open(id + "/mouse"); err != nil {
		t.Fatalf("mouse open after stats: %v", err)
	}
}

func TestNamespaceBinds(t *testing.T) {
	d, _, id := alloc(t)
	ctx := context.Background()
	ns := vfs.New(ctx)
	// registration, exactly wasm.go's sysbindings
	if err := ns.Bind(d, ".", "#wsys"); err != nil {
		t.Fatal(err)
	}
	// driver-style whole-device bind at a mount point
	if err := ns.Bind(ns, "#wsys", "mnt/wsys"); err != nil {
		t.Fatal(err)
	}
	f, err := ns.Open("mnt/wsys/" + id + "/label")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Write(f, []byte("bound\n")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	f2, err := ns.Open("mnt/wsys/" + id + "/label")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(f2)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "bound\n" {
		t.Fatalf("label through bind = %q", b)
	}

	// single-window bind (the stricter grant shape, the #wminput pattern)
	if err := ns.Bind(ns, "#wsys/"+id, "dev"); err != nil {
		t.Fatal(err)
	}
	f3, err := ns.Open("dev/label")
	if err != nil {
		t.Fatal(err)
	}
	b3, err := io.ReadAll(f3)
	if err != nil {
		t.Fatal(err)
	}
	if string(b3) != "bound\n" {
		t.Fatalf("label through window bind = %q", b3)
	}
}
