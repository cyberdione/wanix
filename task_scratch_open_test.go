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
