package memfs

import (
	"errors"
	"io"
	"testing"
	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
)

func TestUnlinkOpenLifetime(t *testing.T) {
	m := From(fskit.MapFS{"out.txt": fskit.RawNode([]byte("old"))})
	f, err := m.Open("out.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Remove(m, "out.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open("out.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("open after unlink: %v", err)
	}
	b := make([]byte, 3)
	if _, err := io.ReadFull(f, b); err != nil || string(b) != "old" {
		t.Fatalf("read retained handle: %q %v", b, err)
	}
	if _, err := f.(io.Seeker).Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.(io.Writer).Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.(io.Seeker).Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(f, b); err != nil || string(b) != "new" {
		t.Fatalf("read written retained handle: %q %v", b, err)
	}
	if err := fs.Sync(f); !errors.Is(err, fs.ErrNotSupported) {
		t.Fatalf("sync: %v", err)
	}
	replacement, err := m.Create("out.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.(io.Writer).Write([]byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(m, "out.txt")
	if err != nil || string(data) != "replacement" {
		t.Fatalf("recreation isolation: %q %v", data, err)
	}
	if _, err := f.Read(b); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("read closed: %v", err)
	}
}
