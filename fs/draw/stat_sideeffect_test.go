package draw

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/vfs"
)

// The kernel namespace router resolves every open as Stat followed by Open
// (fs/vfs NS.OpenContext stats each candidate binding before opening it),
// so a device whose resolve mutates state pays for the stat: fs/draw's
// pre-fix resolve created a client for "new" and retained one reference
// per <n>/<file> on every resolve. One kernel-side open of /dev/draw/new
// therefore leaked a whole client (the first open of a fresh kernel
// reported "connection 2", not 1) and opens of client files leaked one
// reference each, so the last close never freed the client. These tests
// pin the fixed contract: resolution is side-effect-free; stat never
// creates clients or takes references; open does exactly once.

func connID(t *testing.T, f fs.File) uint32 {
	t.Helper()
	info := make([]byte, 144)
	n, err := f.Read(info)
	if err != nil {
		t.Fatalf("read ctl: %v", err)
	}
	if n != 144 {
		t.Fatalf("ctl read = %d bytes, want 144", n)
	}
	fields := strings.Fields(string(info[:n]))
	id, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil {
		t.Fatalf("parse conn id %q: %v", fields[0], err)
	}
	return uint32(id)
}

// Two consecutive resolves for one open (the router's Stat+Open pass) must
// not allocate an extra client, take extra references, or leave the client
// un-freeable: no client exists until the real open, refs reach zero on
// the last close, and the client slot then disappears.
func TestStatOpenDoubleResolveNoSideEffects(t *testing.T) {
	dev := New()

	// Stat pass alone: no client may be created.
	for i := 0; i < 2; i++ {
		fi, err := dev.Stat("new")
		if err != nil {
			t.Fatalf("stat new: %v", err)
		}
		if fi.IsDir() {
			t.Fatal("new stat is a dir")
		}
	}
	if _, err := dev.Stat("1/ctl"); err == nil {
		t.Fatal("stat of client 1 ctl succeeded with no client 1")
	}

	// Open pass: the first client ever is connection 1.
	ctl, err := dev.Open("new")
	if err != nil {
		t.Fatalf("open new: %v", err)
	}
	if id := connID(t, ctl); id != 1 {
		t.Fatalf("first client = %d, want 1 (stat leaked a client)", id)
	}

	// The router stats the file paths around every open; stats must not
	// take references, so the client must stay freeable by this one close.
	for _, name := range []string{"1/.", "1/ctl", "1/data", "1/colormap", "1/refresh"} {
		fi, err := dev.Stat(name)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		wantDir := name == "1/."
		if fi.IsDir() != wantDir {
			t.Fatalf("stat %s dir = %v, want %v", name, fi.IsDir(), wantDir)
		}
	}
	if err := ctl.Close(); err != nil {
		t.Fatalf("close ctl: %v", err)
	}
	if _, err := dev.Stat("1/ctl"); err == nil {
		t.Fatal("client 1 still addressable after its only fd closed (stat retained a ref)")
	}
	if _, err := dev.Open("1/ctl"); err == nil {
		t.Fatal("client 1 still openable after its only fd closed")
	}

	// No phantom ids: the next connection is 2, not 3.
	ctl2, err := dev.Open("new")
	if err != nil {
		t.Fatalf("open new again: %v", err)
	}
	defer ctl2.Close()
	if id := connID(t, ctl2); id != 2 {
		t.Fatalf("second client = %d, want 2", id)
	}
}

// Refcount across multiple open files of one client, with the router's
// stat pass in between every open: each open takes exactly one reference,
// each close drops exactly one, and the client is freed by the last close
// (the pre-fix stat retain made the count never reach zero).
func TestStatOpenDoubleResolveRefcountFrees(t *testing.T) {
	dev := New()
	ctl, err := dev.Open("new")
	if err != nil {
		t.Fatalf("open new: %v", err)
	}
	id := connID(t, ctl)
	files := map[string]fs.File{}

	// The ctl fd from "new" holds the client's only reference; the stat
	// pass around these opens must not add any.
	for _, name := range []string{"ctl", "data"} {
		if _, err := dev.Stat(devPath(id, name)); err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		f, err := dev.Open(devPath(id, name))
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		files[name] = f
	}
	if err := files["ctl"].Close(); err != nil {
		t.Fatalf("close ctl: %v", err)
	}
	if _, err := dev.Stat(devPath(id, "refresh")); err != nil {
		t.Fatalf("client gone while fds open: %v", err)
	}

	// Closing the "new" ctl leaves the data fd holding the client.
	if err := ctl.Close(); err != nil {
		t.Fatalf("close new ctl: %v", err)
	}
	if _, err := dev.Stat(devPath(id, "ctl")); err != nil {
		t.Fatalf("client gone while data fd open: %v", err)
	}

	// The last close frees the client; its files stop resolving.
	if err := files["data"].Close(); err != nil {
		t.Fatalf("close data: %v", err)
	}
	if _, err := dev.Stat(devPath(id, "ctl")); err == nil {
		t.Fatal("client still addressable after last fd closed")
	}
	if _, err := dev.Open(devPath(id, "data")); err == nil {
		t.Fatal("client still openable after last fd closed")
	}
}

func devPath(id uint32, file string) string {
	return fmt.Sprintf("%d/%s", id, file)
}

// The live path from the conformance run: #i bound after dev, every open
// through the namespace. Before the fix the first /dev/draw/new of a fresh
// kernel reported "connection 2"; it must be connection 1, and closing it
// must make the client unreachable through the namespace too.
func TestNamespaceRouterStatOpenSingleClient(t *testing.T) {
	dev := New()
	ns := vfs.New(context.Background())
	if err := ns.Bind(dev, ".", "#i"); err != nil {
		t.Fatalf("bind #i: %v", err)
	}
	if err := ns.Bind(ns, "#i", "dev"); err != nil {
		t.Fatalf("bind dev: %v", err)
	}

	f, err := ns.Open("dev/draw/new")
	if err != nil {
		t.Fatalf("open dev/draw/new: %v", err)
	}
	if id := connID(t, f); id != 1 {
		t.Fatalf("first namespace client = %d, want 1 (router stat leaked a client)", id)
	}
	if _, err := ns.Stat("dev/draw/new"); err != nil {
		t.Fatalf("stat dev/draw/new: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := ns.Stat("dev/draw/1/ctl"); err == nil {
		t.Fatal("client 1 still addressable through namespace after last close")
	}
	if _, err := ns.Open("dev/draw/1/ctl"); err == nil {
		t.Fatal("client 1 still openable through namespace after last close")
	}
}
