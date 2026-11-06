package scratchfs

import (
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

func mustOpen(t *testing.T, f *FS, n string, flags int) *File {
	t.Helper()
	h, e := f.OpenHandle(n, flags, 0600)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { h.Close() })
	return h
}
func bytesAt(t *testing.T, h *File) string {
	t.Helper()
	b := make([]byte, 100)
	n, e := h.ReadAt(b, 0)
	if e != nil && !errors.Is(e, io.EOF) {
		t.Fatal(e)
	}
	return string(b[:n])
}
func TestExclusive(t *testing.T) {
	for round := 0; round < 20; round++ {
		f := New()
		start := make(chan struct{})
		var wg sync.WaitGroup
		var wins atomic.Int32
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				h, e := f.OpenHandle("file", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
				if e == nil {
					wins.Add(1)
					h.Write([]byte("winner"))
					h.Close()
				} else if !errors.Is(e, fs.ErrExist) {
					t.Errorf("loser: %v", e)
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("wins=%d", wins.Load())
		}
		if got := bytesAt(t, mustOpen(t, f, "file", os.O_RDONLY)); got != "winner" {
			t.Fatal(got)
		}
	}
}
func TestLiveLegacyAndCreate(t *testing.T) {
	f := New()
	old, e := f.Create("f")
	if e != nil {
		t.Fatal(e)
	}
	a := old.(*File)
	a.Write([]byte("dirty"))
	legacy, e := f.Open("f")
	if e != nil {
		t.Fatal(e)
	}
	b := legacy.(*File)
	id, _ := a.Stat()
	c := mustOpen(t, f, "f", os.O_CREATE|os.O_RDWR)
	ci, _ := c.Stat()
	if ci.Sys() != id.Sys() || ci.Mode() != 0600 && ci.Mode() != 0644 || bytesAt(t, c) != "dirty" {
		t.Fatal("CREATE changed inode")
	}
	tr := mustOpen(t, f, "f", os.O_TRUNC|os.O_RDWR)
	for _, h := range []*File{a, b, c, tr} {
		if bytesAt(t, h) != "" {
			t.Fatal("stale truncate")
		}
	}
	a.Close()
	b.Close()
	if bytesAt(t, c) != "" {
		t.Fatal("close restored bytes")
	}
	c.WriteAt([]byte("a"), 0)
	tr.WriteAt([]byte("b"), 1)
	tr.Close()
	if bytesAt(t, c) != "ab" {
		t.Fatal("lost writes")
	}
	again, e := f.Create("f")
	if e != nil {
		t.Fatal(e)
	}
	fi, _ := again.Stat()
	if fi.Sys() != id.Sys() || bytesAt(t, c) != "" {
		t.Fatal("Create replaced inode")
	}
	again.Close()
	f.Truncate("f", 3)
	if bytesAt(t, c) != "\x00\x00\x00" {
		t.Fatal("pathname truncate not live")
	}
}
func TestNamespaceLifetime(t *testing.T) {
	f := New()
	if e := f.Mkdir("dir", 0750); e != nil {
		t.Fatal(e)
	}
	a := mustOpen(t, f, "dir/a", os.O_CREATE|os.O_RDWR)
	a.Write([]byte("old"))
	id, _ := a.Stat()
	if e := f.Remove("dir/a"); e != nil {
		t.Fatal(e)
	}
	b := mustOpen(t, f, "dir/a", os.O_CREATE|os.O_RDWR)
	b.Write([]byte("new"))
	a.Truncate(1)
	a.WriteAt([]byte("X"), 0)
	a.Close()
	if bytesAt(t, b) != "new" {
		t.Fatal("unlinked handle affected replacement")
	}
	bi, _ := b.Stat()
	if bi.Sys() == id.Sys() {
		t.Fatal("identity reused")
	}
	dst := mustOpen(t, f, "dir/dst", os.O_CREATE|os.O_RDWR)
	dst.Write([]byte("dst"))
	if e := f.Rename("dir/a", "dir/dst"); e != nil {
		t.Fatal(e)
	}
	if bytesAt(t, dst) != "dst" || bytesAt(t, mustOpen(t, f, "dir/dst", 0)) != "new" {
		t.Fatal("rename identity")
	}
	if e := f.Rename("dir", "renamed"); e != nil {
		t.Fatal(e)
	}
	entries, e := fs.ReadDir(f, "renamed")
	if e != nil || len(entries) != 1 || entries[0].Name() != "dst" {
		t.Fatalf("entries %v %v", entries, e)
	}
	if e := f.Rename("renamed", "renamed/sub"); e == nil {
		t.Fatal("cycle allowed")
	}
	if e := f.Remove("renamed"); e == nil {
		t.Fatal("removed nonempty dir")
	}
	other := New()
	if _, e := other.Stat("renamed"); !errors.Is(e, fs.ErrNotExist) {
		t.Fatal("isolation")
	}
}
func TestFailureAndDirections(t *testing.T) {
	f := New()
	w := mustOpen(t, f, "f", os.O_CREATE|os.O_WRONLY)
	w.Write([]byte("abc"))
	before, _ := f.Stat("f")
	for _, flags := range []int{3, os.O_RDONLY | os.O_TRUNC, os.O_RDONLY | os.O_APPEND, 1 << 29} {
		if _, e := f.OpenHandle("f", flags, 0777); e == nil {
			t.Fatal("bad flags accepted")
		}
	}
	if _, e := f.OpenFileGranted("f", os.O_RDWR|os.O_TRUNC, 0, true, false); !errors.Is(e, fs.ErrPermission) {
		t.Fatal(e)
	}
	if _, e := f.OpenFileGranted("missing", os.O_CREATE, 0600, true, false); !errors.Is(e, fs.ErrPermission) {
		t.Fatal(e)
	}
	after, _ := f.Stat("f")
	if before != after {
		t.Fatal("failure mutated metadata")
	}
	for _, fn := range []func() error{func() error { _, e := w.Read(nil); return e }, func() error { _, e := w.ReadAt(nil, 0); return e }} {
		if !errors.Is(fn(), fs.ErrPermission) {
			t.Fatal("read direction")
		}
	}
	r := mustOpen(t, f, "f", 0)
	for _, fn := range []func() error{func() error { _, e := r.Write(nil); return e }, func() error { _, e := r.WriteAt(nil, 0); return e }, func() error { return r.Truncate(0) }} {
		if !errors.Is(fn(), fs.ErrPermission) {
			t.Fatal("write direction")
		}
	}
	if _, e := f.OpenHandle("missing/child", os.O_CREATE|os.O_RDWR, 0600); e == nil {
		t.Fatal("missing parent")
	}
	for _, n := range []string{"../f", "/f", "a/../f", "./f", ""} {
		if _, e := f.OpenHandle(n, os.O_CREATE|os.O_RDWR, 0600); e == nil {
			t.Fatal("invalid path")
		}
	}
	if e := f.Symlink("f", "link"); e == nil {
		t.Fatal("symlink success")
	}
	if _, e := f.Readlink("f"); e == nil {
		t.Fatal("readlink success")
	}
	w.Close()
	if _, e := w.Write(nil); !errors.Is(e, fs.ErrClosed) {
		t.Fatal(e)
	}
	if _, e := w.Stat(); !errors.Is(e, fs.ErrClosed) {
		t.Fatal(e)
	}
	if _, e := w.Seek(0, 0); !errors.Is(e, fs.ErrClosed) {
		t.Fatal(e)
	}
	if e := w.Truncate(0); !errors.Is(e, fs.ErrClosed) {
		t.Fatal(e)
	}
	if e := w.Sync(); !errors.Is(e, fs.ErrClosed) {
		t.Fatal(e)
	}
}
func TestOffsetsSparseAppend(t *testing.T) {
	f := New()
	a := mustOpen(t, f, "f", os.O_CREATE|os.O_RDWR)
	b := mustOpen(t, f, "f", os.O_RDWR)
	a.Write([]byte("ab"))
	buf := make([]byte, 1)
	b.Read(buf)
	if string(buf) != "a" {
		t.Fatal("shared offsets")
	}
	a.WriteAt([]byte("z"), 4)
	off, _ := a.Seek(0, io.SeekCurrent)
	if off != 2 || bytesAt(t, b) != "ab\x00\x00z" {
		t.Fatal("positioned IO")
	}
	for _, off := range []int64{-1, math.MaxInt64} {
		if _, e := a.WriteAt([]byte("xx"), off); e == nil {
			t.Fatal("overflow accepted")
		}
	}
	a.Seek(1, io.SeekStart)
	if _, e := a.Seek(math.MaxInt64, io.SeekCurrent); e == nil {
		t.Fatal("seek overflow")
	}
	off, _ = a.Seek(0, io.SeekCurrent)
	if off != 1 {
		t.Fatal("failed seek moved offset")
	}
	if e := a.Sync(); e == nil {
		t.Fatal("unsupported sync")
	}
	a.Truncate(0)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, e := f.OpenHandle("f", os.O_WRONLY|os.O_APPEND, 0)
			if e != nil {
				t.Error(e)
				return
			}
			defer h.Close()
			for j := 0; j < 20; j++ {
				if _, e := h.Write([]byte("xy")); e != nil {
					t.Error(e)
				}
			}
		}()
	}
	wg.Wait()
	fi, _ := a.Stat()
	if fi.Size() != 640 {
		t.Fatal(fi.Size())
	}
	got := make([]byte, 640)
	a.ReadAt(got, 0)
	for i := 0; i < len(got); i += 2 {
		if string(got[i:i+2]) != "xy" {
			t.Fatal("non atomic append")
		}
	}
}
func TestRenameOpenRace(t *testing.T) {
	for i := 0; i < 40; i++ {
		f := New()
		a := mustOpen(t, f, "a", os.O_CREATE|os.O_RDWR)
		a.Write([]byte("a"))
		b := mustOpen(t, f, "b", os.O_CREATE|os.O_RDWR)
		b.Write([]byte("b"))
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; f.Rename("a", "b") }()
		go func() {
			defer wg.Done()
			<-start
			h, e := f.OpenHandle("b", os.O_RDWR|os.O_TRUNC, 0)
			if e != nil {
				t.Error(e)
				return
			}
			h.Write([]byte("x"))
			h.Close()
		}()
		close(start)
		wg.Wait()
		av := bytesAt(t, a)
		bv := bytesAt(t, b)
		if !(av == "x" && bv == "b" || av == "a" && bv == "x") {
			t.Fatalf("not selected inode: %q %q", av, bv)
		}
	}
}

func TestRetainedDirectoryStat(t *testing.T) {
	f := New()
	if e := f.Mkdir("old", 0755); e != nil {
		t.Fatal(e)
	}
	mustOpen(t, f, "old/child", os.O_CREATE|os.O_RDWR)
	handle, e := f.Open("old")
	if e != nil {
		t.Fatal(e)
	}
	defer handle.Close()
	original, e := handle.Stat()
	if e != nil {
		t.Fatal(e)
	}
	check := func(size int64) {
		t.Helper()
		got, e := handle.Stat()
		if e != nil {
			t.Fatal(e)
		}
		if got.Sys() != original.Sys() || got.Size() != size {
			t.Fatalf("retained dir stat: identity=%v size=%d; want %v %d", got.Sys(), got.Size(), original.Sys(), size)
		}
	}
	check(3)
	if e := f.Rename("old", "moved"); e != nil {
		t.Fatal(e)
	}
	check(3)
	if e := f.Mkdir("old", 0755); e != nil {
		t.Fatal(e)
	}
	for _, n := range []string{"old/a", "old/b"} {
		mustOpen(t, f, n, os.O_CREATE|os.O_RDWR)
	}
	check(3)
	mustOpen(t, f, "moved/second", os.O_CREATE|os.O_RDWR)
	check(4)
	for _, n := range []string{"moved/child", "moved/second", "moved"} {
		if e := f.Remove(n); e != nil {
			t.Fatal(e)
		}
	}
	check(2)
	if e := f.Mkdir("moved", 0755); e != nil {
		t.Fatal(e)
	}
	mustOpen(t, f, "moved/replacement", os.O_CREATE|os.O_RDWR)
	check(2)
}
