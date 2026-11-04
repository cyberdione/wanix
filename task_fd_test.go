package wanix

import (
	"errors"
	"testing"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
)

func bindTaskStdio(t *testing.T, task *Task, name string) {
	t.Helper()
	source := fskit.MapFS{".": fskit.RawNode([]byte(name), 0600)}
	if err := task.NS().Bind(source, ".", "#task/1/fd/0"); err != nil {
		t.Fatalf("bind fd 0: %v", err)
	}
}

func TestTaskFDCachesStdioFile(t *testing.T) {
	task, err := NewRoot()
	if err != nil {
		t.Fatal(err)
	}
	bindTaskStdio(t, task, "first")

	first, path, err := task.FD(0)
	if err != nil {
		t.Fatalf("first fd lookup: %v", err)
	}
	if want := "#task/1/fd/0"; path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	bindTaskStdio(t, task, "second")

	second, _, err := task.FD(0)
	if err != nil {
		t.Fatalf("second fd lookup: %v", err)
	}
	if first != second {
		t.Fatal("stdio fd was reopened instead of using its cached file")
	}
}

func TestTaskFDDoesNotReopenClosedStdio(t *testing.T) {
	task, err := NewRoot()
	if err != nil {
		t.Fatal(err)
	}
	bindTaskStdio(t, task, "stdin")
	if _, _, err := task.FD(0); err != nil {
		t.Fatalf("open fd 0: %v", err)
	}
	if err := task.CloseFD(0); err != nil {
		t.Fatalf("close fd 0: %v", err)
	}
	if _, _, err := task.FD(0); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("reopened explicitly closed fd: %v", err)
	}
}
