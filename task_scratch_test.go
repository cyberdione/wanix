package wanix

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/memfs"
	"tractor.dev/wanix/fs/scratchfs"
)

type scratchTestDriver struct{ fail bool }

func (d scratchTestDriver) Check(*Task) bool { return true }
func (d scratchTestDriver) Start(*Task) error {
	if d.fail {
		return fs.ErrInvalid
	}
	return nil
}
func scratchRoot(t *testing.T) *Task {
	t.Helper()
	r, e := NewRoot()
	if e != nil {
		t.Fatal(e)
	}
	for _, k := range []string{"linux", "gojs", "wasi"} {
		r.Register(k, scratchTestDriver{})
	}
	t.Cleanup(func() { r.Exit("0") })
	return r
}
func routedTask(t *testing.T, r *Task, selector string) *Task {
	t.Helper()
	b, e := fs.ReadFile(r.NS(), "#task/new/"+selector)
	if e != nil {
		t.Fatal(e)
	}
	c, e := r.Lookup(strings.TrimSpace(string(b)))
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func TestLinuxScratchSelection(t *testing.T) {
	r := scratchRoot(t)
	if _, ok := r.w.(*memfs.FS); !ok {
		t.Fatalf("root backend %T", r.w)
	}
	c := routedTask(t, r, "linux-scratch")
	if c.kind != "linux" || c.parent != r {
		t.Fatal("profile changed kind/parent")
	}
	if _, ok := c.w.(*scratchfs.FS); !ok {
		t.Fatalf("backend %T", c.w)
	}
	entries, e := fs.ReadDir(r.NS(), "#task/new")
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, entry := range entries {
		if entry.Name() == linuxScratchSelector {
			found = true
		}
	}
	if !found {
		t.Fatal("missing discovery")
	}
	for _, kind := range []string{"linux", "auto", "gojs", "wasi"} {
		d := routedTask(t, r, kind)
		if _, ok := d.w.(*memfs.FS); !ok {
			t.Fatalf("%s backend %T", kind, d.w)
		}
		if e := fs.WriteFile(d.w, "file", []byte("data"), 0600); e != nil {
			t.Fatal(e)
		}
		if e := fs.Symlink(d.w, "file", "link"); e != nil {
			t.Fatal(e)
		}
		if s, e := fs.Readlink(d.w, "link"); e != nil || s != "file" {
			t.Fatalf("readlink %s %v", s, e)
		}
		if e := fs.Chown(d.w, "file", 123, 456); e != nil {
			t.Fatal(e)
		}
	}
	child, e := r.fsys.Alloc("linux", c)
	if e != nil {
		t.Fatal(e)
	}
	if _, ok := child.w.(*memfs.FS); !ok {
		t.Fatal("inherited policy")
	}
}
func TestLinuxScratchRejectedAllocations(t *testing.T) {
	r := scratchRoot(t)
	before := len(r.Tasks())
	for _, s := range []string{"linux-scratch/extra", "linux-scratchx", "unknown"} {
		if _, e := fs.ReadFile(r.NS(), "#task/new/"+s); e == nil {
			t.Fatalf("accepted %s", s)
		}
	}
	for _, k := range []string{"auto", "gojs", "wasi", "missing"} {
		if _, e := r.fsys.AllocWithOptions(k, r, TaskAllocOptions{Scratch: LinuxRegularScratch}); e == nil {
			t.Fatal("accepted incompatible kind")
		}
	}
	if _, e := r.fsys.AllocWithOptions("linux", r, TaskAllocOptions{Scratch: 42}); e == nil {
		t.Fatal("accepted unknown policy")
	}
	if _, e := r.fsys.Alloc(linuxScratchSelector, r); e == nil {
		t.Fatal("selector is not a kind")
	}
	if len(r.Tasks()) != before {
		t.Fatal("failed allocation published task")
	}
	bare, e := NewRoot()
	if e != nil {
		t.Fatal(e)
	}
	defer bare.Exit("0")
	if _, e := fs.ReadFile(bare.NS(), "#task/new/linux-scratch"); e == nil {
		t.Fatal("missing driver accepted")
	}
	if len(bare.Tasks()) != 1 {
		t.Fatal("missing driver leaked task")
	}
	for _, register := range []func(){func() { r.Register(linuxScratchSelector, scratchTestDriver{}) }, func() { r.fsys.Register(linuxScratchSelector, scratchTestDriver{}) }} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("reserved driver accepted")
				}
			}()
			register()
		}()
	}
}
func TestLinuxScratchParentHandleLifetime(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			r := scratchRoot(t)
			r.Register("linux", scratchTestDriver{fail: fail})
			c := routedTask(t, r, "linux-scratch")
			backend := c.w
			base := "#task/" + c.ID() + "/w"
			if e := fs.WriteFile(r.NS(), base+"/seed", []byte("abcdef"), 0600); e != nil {
				t.Fatal(e)
			}
			h, e := fs.OpenFile(r.NS(), base+"/seed", os.O_RDWR, 0)
			if e != nil {
				t.Fatal(e)
			}
			defer h.Close()
			r.fsys.mu.Lock()
			r.fsys.aliases["scratch-alias"] = c
			r.fsys.mu.Unlock()
			if e := r.NS().Bind(r.NS(), base, "scratch-bound"); e != nil {
				t.Fatal(e)
			}
			self, e := c.fsys.OpenContext(c.Context(), "self/w/seed")
			if e != nil {
				t.Fatal(e)
			}
			selfData, e := io.ReadAll(self)
			self.Close()
			if e != nil || string(selfData) != "abcdef" {
				t.Fatalf("contextual self %q %v", selfData, e)
			}
			// Launchers explicitly bind contextual self after allocation.
			if e := c.NS().Bind(c, ".", "#task/self", fs.BindReplace); e != nil {
				t.Fatal(e)
			}
			for _, route := range []struct {
				task *Task
				path string
			}{{r, base + "/seed"}, {r, "#task/scratch-alias/w/seed"}, {c, "#task/self/w/seed"}, {r, "scratch-bound/seed"}} {
				b, e := fs.ReadFile(route.task.NS(), route.path)
				if e != nil || string(b) != "abcdef" {
					t.Fatalf("route %s %q %v", route.path, b, e)
				}
			}
			c.cmd = "changed"
			e = c.Start()
			if (e != nil) != fail {
				t.Fatalf("start %v", e)
			}
			second, e := fs.OpenFile(r.NS(), base+"/seed", os.O_RDWR, 0)
			if e != nil {
				t.Fatal(e)
			}
			if _, e := second.(io.WriterAt).WriteAt([]byte("XY"), 2); e != nil {
				t.Fatal(e)
			}
			second.Close()
			b := make([]byte, 6)
			if _, e := h.(io.ReaderAt).ReadAt(b, 0); e != nil || string(b) != "abXYef" {
				t.Fatalf("atomic visibility %q %v", b, e)
			}
			owned, e := fs.OpenFile(c.w, "seed", os.O_RDWR, 0)
			if e != nil {
				t.Fatal(e)
			}
			fd := c.OpenFD(owned, "seed")
			c.Exit("0")
			c.Exit("1")
			if _, _, e := c.FD(fd); !errors.Is(e, fs.ErrClosed) {
				t.Fatalf("task fd %v", e)
			}
			if _, e := h.(io.WriterAt).WriteAt([]byte("Z"), 0); e != nil {
				t.Fatalf("parent handle revoked %v", e)
			}
			if c.w != backend {
				t.Fatal("backend replaced")
			}
			b, e = fs.ReadFile(r.NS(), base+"/seed")
			if e != nil || string(b) != "ZbXYef" {
				t.Fatalf("parent inspection %q %v", b, e)
			}
		})
	}
}
func TestLinuxScratchConcurrentAllocation(t *testing.T) {
	r := scratchRoot(t)
	var wg sync.WaitGroup
	ids := make(chan string, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, e := fs.ReadFile(r.NS(), "#task/new/linux-scratch")
			if e != nil {
				t.Error(e)
				return
			}
			ids <- string(b)
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Error("duplicate id")
		}
		seen[id] = true
	}
	if len(seen) != 32 {
		t.Fatal(len(seen))
	}
}

// auto callbacks must be able to reenter registry and routed task operations.
type reentrantScratchDriver struct {
	err              error
	checked, started bool
}

func (d *reentrantScratchDriver) Check(t *Task) bool {
	d.checked = true
	t.Register("callback-registered", scratchTestDriver{})
	_, d.err = fs.Stat(t.NS(), "#task/"+t.ID()+"/w")
	return true
}
func (d *reentrantScratchDriver) Start(t *Task) error {
	d.started = true
	if d.err != nil {
		return d.err
	}
	_, err := t.fsys.Alloc("auto", t)
	if err != nil {
		return err
	}
	_, err = t.Lookup(t.ID())
	return err
}
func TestAutoDriverCallbackRouteReentry(t *testing.T) {
	r, err := NewRoot()
	if err != nil {
		t.Fatal(err)
	}
	driver := &reentrantScratchDriver{}
	r.Register("linux", driver)
	done := make(chan error, 1)
	go func() { done <- r.Start() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		defer r.Exit("0")
		if !driver.checked || !driver.started || r.kind != "linux" {
			t.Fatal("auto callbacks did not run")
		}
		if _, ok := r.w.(*memfs.FS); !ok {
			t.Fatalf("auto backend changed: %T", r.w)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("auto callbacks deadlocked while reentering TaskFS")
	}
}
