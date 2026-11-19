package scratchfs

import (
	"os"
	"testing"
	"tractor.dev/wanix/fs"
)

var _ fs.OpenFileFS = (*FS)(nil)

func TestHostHelpers(t *testing.T) {
	f := New()
	if e := fs.MkdirAll(f, "a/b", 0755); e != nil {
		t.Fatal(e)
	}
	if e := fs.WriteFile(f, "a/b/file", []byte("data"), 0640); e != nil {
		t.Fatal(e)
	}
	b, e := fs.ReadFile(f, "a/b/file")
	if e != nil || string(b) != "data" {
		t.Fatalf("%s %v", b, e)
	}
	h, e := fs.OpenFile(f, "a/b/file", os.O_RDWR|os.O_CREATE, 0777)
	if e != nil {
		t.Fatal(e)
	}
	defer h.Close()
	fi, _ := h.Stat()
	if fi.Mode() != 0640 {
		t.Fatal("helper bypassed atomic backend")
	}
}
