package wanix

import (
	"errors"
	"syscall"
	"testing"
	"tractor.dev/wanix/fs"
)

func TestBoundedOpenFDAdmission(t *testing.T) {
	r := scratchRoot(t)
	c := routedTask(t, r, "linux-scratch")
	name := "#task/" + c.ID() + "/w/out.txt"
	if e := fs.WriteFile(c.w, "out.txt", []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	c.mu.Lock()
	idx, count := c.fdIdx, len(c.fds)
	c.mu.Unlock()
	fd, e := c.OpenScratchCreateBoundedFD(name, "write", true, false, false, 0, 3)
	if fd != -1 || !errors.Is(e, syscall.EOVERFLOW) {
		t.Fatalf("%d %v", fd, e)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if idx != c.fdIdx || count != len(c.fds) {
		t.Fatal("published rejected fd")
	}
}
