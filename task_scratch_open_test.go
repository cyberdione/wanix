package wanix

import (
	"errors"
	"sync"
	"syscall"
	"testing"
	"tractor.dev/wanix/fs"
)

func TestScratchOpenOwnerRoute(t *testing.T) {
	r := scratchRoot(t)
	c := routedTask(t, r, "linux-scratch")
	other := routedTask(t, r, "linux-scratch")
	base := "#task/" + c.ID() + "/w/"
	if err := fs.WriteFile(r.NS(), base+"out.txt", []byte("own"), 0600); err != nil {
		t.Fatal(err)
	}
	foreign := "#task/" + other.ID() + "/w/"
	if err := fs.WriteFile(r.NS(), foreign+"out.txt", []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	// Numeric aliases shadow live resources. Textual session ID alone is insufficient.
	r.fsys.mu.Lock()
	r.fsys.aliases[c.ID()] = other
	r.fsys.mu.Unlock()
	h, aliasErr := c.OpenScratchFile(base+"out.txt", "read", false)
	if aliasErr == nil {
		b := make([]byte, 3)
		_, _ = h.Read(b)
		h.Close()
		if string(b) != "own" {
			t.Fatalf("numeric alias reached foreign data %q", b)
		}
	} else if !errors.Is(aliasErr, syscall.EACCES) {
		t.Fatal(aliasErr)
	}
	r.fsys.mu.Lock()
	delete(r.fsys.aliases, c.ID())
	r.fsys.mu.Unlock()
	legacy := routedTask(t, r, "linux")
	if _, err := legacy.OpenScratchFile("#task/"+legacy.ID()+"/w/tmpfile_missing", "read", false); !errors.Is(err, syscall.ENOSYS) {
		t.Fatalf("legacy: %v", err)
	}
	// Bind swaps may select either snapshot, but only an owned pointer can open.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if err := c.NS().Bind(other.w, ".", base[:len(base)-1], fs.BindReplace); err != nil {
				t.Error(err)
				return
			}
			if err := c.NS().Bind(c.w, ".", base[:len(base)-1], fs.BindReplace); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	for i := 0; i < 100; i++ {
		h, err := c.OpenScratchFile(base+"out.txt", "readwrite", true)
		if err == nil {
			h.Close()
		} else if !errors.Is(err, syscall.EACCES) {
			t.Error(err)
		}
	}
	wg.Wait()
	b, err := fs.ReadFile(r.NS(), foreign+"out.txt")
	if err != nil || string(b) != "foreign" {
		t.Fatalf("foreign changed %q %v", b, err)
	}
}

func TestScratchOpenExitAdmission(t *testing.T) {
	for i := 0; i < 100; i++ {
		r := scratchRoot(t)
		c := routedTask(t, r, "linux-scratch")
		name := "#task/" + c.ID() + "/w/out.txt"
		if err := fs.WriteFile(c.w, "out.txt", []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		parent, err := c.OpenScratchFile(name, "readwrite", false)
		if err != nil {
			t.Fatal(err)
		}
		parentFD := r.OpenFD(parent, name)
		start, done := make(chan struct{}), make(chan struct{})
		go func() { <-start; c.Exit("7"); close(done) }()
		close(start)
		fd, err := c.OpenScratchFD(name, "write", true)
		<-done
		b, readErr := fs.ReadFile(c.w, "out.txt")
		if readErr != nil {
			t.Fatal(readErr)
		}
		if err == nil {
			if len(b) != 0 {
				t.Fatalf("successful truncate: %q", b)
			}
			if _, _, e := c.FD(fd); !errors.Is(e, fs.ErrClosed) {
				t.Fatalf("FD survives: %v", e)
			}
		} else if !errors.Is(err, fs.ErrClosed) || string(b) != "keep" {
			t.Fatalf("rejected destructive open: %q %v", b, err)
		}
		for _, trunc := range []bool{false, true} {
			if _, e := c.OpenScratchFile(name, "write", trunc); !errors.Is(e, fs.ErrClosed) {
				t.Fatalf("file: %v", e)
			}
			if _, e := c.OpenScratchFD(name, "write", trunc); !errors.Is(e, fs.ErrClosed) {
				t.Fatalf("fd: %v", e)
			}
		}
		after, e := fs.ReadFile(c.w, "out.txt")
		if e != nil || string(after) != string(b) {
			t.Fatalf("post-exit changed: %q %v", after, e)
		}
		h, _, e := r.FD(parentFD)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = h.(interface{ Write([]byte) (int, error) }).Write([]byte("live")); e != nil {
			t.Fatal(e)
		}
		r.CloseFD(parentFD)
		b, e = fs.ReadFile(r.NS(), "#task/"+c.ID()+"/exit")
		if e != nil || string(b) != "7\n" {
			t.Fatalf("status: %q %v", b, e)
		}
	}
}
func TestScratchOpenAdmissionBeforeExitCleanup(t *testing.T) {
	r := scratchRoot(t)
	c := routedTask(t, r, "linux-scratch")
	if err := fs.WriteFile(c.w, "out.txt", []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	name := "#task/" + c.ID() + "/w/out.txt"
	fd, err := c.OpenScratchFD(name, "write", true)
	if err != nil {
		t.Fatal(err)
	}
	h, _, err := c.FD(fd)
	if err != nil {
		t.Fatal(err)
	}
	c.AddCleanup(func() {
		if _, e := c.OpenScratchFD(name, "write", true); !errors.Is(e, fs.ErrClosed) {
			t.Errorf("cleanup: %v", e)
		}
	})
	c.Exit("0")
	if _, err := h.Stat(); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("owned handle: %v", err)
	}
}
