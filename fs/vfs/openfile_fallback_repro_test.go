package vfs_test

import (
	"context"
	"io"
	"os"
	"testing"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/memfs"
	"tractor.dev/wanix/fs/vfs"
)

// stubFuncFS mimics pageTCPService: an fs.FS with only Open/OpenContext/
// Stat/StatContext/Route — no Create, no OpenFile. Backed by memfs so Open
// returns a real file for existing names.
type stubFuncFS struct {
	inner *memfs.FS
}

func (f *stubFuncFS) Open(name string) (fs.File, error) {
	return f.inner.Open(name)
}

func (f *stubFuncFS) OpenContext(ctx context.Context, name string) (fs.File, error) {
	return f.inner.OpenContext(ctx, name)
}

func (f *stubFuncFS) Route(ctx context.Context, name string) (fs.FS, string, error) {
	return f, name, nil
}

func newTestNS(t *testing.T) *vfs.NS {
	t.Helper()
	mem := memfs.New()
	if err := mem.Mkdir("1", 0755); err != nil {
		t.Fatal(err)
	}
	fw, err := mem.Create("1/ctl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Write(fw, []byte("")); err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}

	stub := &stubFuncFS{inner: mem}
	ns := vfs.New(t.Context())
	if err := ns.Bind(stub, ".", "net/tcp"); err != nil {
		t.Fatal(err)
	}
	return ns
}

// Repro of the ferrule kernel ENOSYS: a task-side open of a page-service
// function file with O_WRONLY|O_CREATE|O_TRUNC (os.WriteFile-style) fails
// with "resolve: operation not supported" because fs.OpenFile's O_TRUNC
// fallback calls fs.Create unchecked, while fs.WriteFile tolerates it.
func TestOpenFileOTruncateOnFuncfileFS(t *testing.T) {
	ns := newTestNS(t)
	f, err := fs.OpenFile(ns, "net/tcp/1/ctl", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("OpenFile with O_TRUNC failed: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenFilePlainWriteOnlyOnFuncfileFS(t *testing.T) {
	ns := newTestNS(t)
	f, err := fs.OpenFile(ns, "net/tcp/1/ctl", os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("OpenFile O_WRONLY failed: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteFileToleratesMissingCreate(t *testing.T) {
	ns := newTestNS(t)
	if err := fs.WriteFile(ns, "net/tcp/1/ctl", []byte("dial x:22\n"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	f, err := ns.Open("net/tcp/1/ctl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty content after write")
	}
}
