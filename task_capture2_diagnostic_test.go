package wanix

import (
	"testing"
	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/scratchfs"
)

func TestCapture2DiagnosticSelection(t *testing.T) {
	r := scratchRoot(t)
	c := routedTask(t, r, linuxCapture2Selector)
	if c.kind != "linux" || c.parent != r || c.DiagnosticProfile() != Capture2DiagnosticProfile {
		t.Fatal("incorrect diagnostic allocation")
	}
	if _, ok := c.w.(*scratchfs.FS); !ok {
		t.Fatalf("backend %T", c.w)
	}
	for _, selector := range []string{"linux", "linux-scratch"} {
		if routedTask(t, r, selector).DiagnosticProfile() != "" {
			t.Fatal("changed ordinary launch")
		}
	}
	child, err := r.fsys.Alloc("linux", c)
	if err != nil || child.DiagnosticProfile() != "" {
		t.Fatal("profile inherited", err)
	}
	entries, err := fs.ReadDir(r.NS(), "#task/new")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Name() == linuxCapture2Selector {
			found = true
		}
	}
	if !found {
		t.Fatal("missing selector discovery")
	}
}

func TestCapture2DiagnosticRejectsBeforePublication(t *testing.T) {
	r := scratchRoot(t)
	before := len(r.Tasks())
	for _, tc := range []struct {
		kind string
		opts TaskAllocOptions
	}{
		{"linux", TaskAllocOptions{Scratch: LinuxRegularScratch, DiagnosticProfile: "unknown"}},
		{"linux", TaskAllocOptions{DiagnosticProfile: Capture2DiagnosticProfile}},
		{"wasi", TaskAllocOptions{Scratch: LinuxRegularScratch, DiagnosticProfile: Capture2DiagnosticProfile}},
	} {
		if _, err := r.fsys.AllocWithOptions(tc.kind, r, tc.opts); err == nil {
			t.Fatal("accepted invalid profile")
		}
	}
	if len(r.Tasks()) != before {
		t.Fatal("failed allocation published")
	}
	defer func() {
		if recover() == nil {
			t.Error("reserved driver accepted")
		}
	}()
	r.Register(linuxCapture2Selector, scratchTestDriver{})
}
