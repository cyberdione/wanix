package wanix

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
	"tractor.dev/wanix/fs/signal"
)

type exitFile struct{ closes int }

func (f *exitFile) Close() error               { f.closes++; return nil }
func (f *exitFile) Read([]byte) (int, error)   { return 0, fs.ErrClosed }
func (f *exitFile) Stat() (fs.FileInfo, error) { return fskit.Entry("x", 0600), nil }
func TestTaskExitReleasesOwnedResources(t *testing.T) {
	tasks := NewTaskFS()
	task, err := tasks.Alloc("auto", nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &exitFile{}
	task.OpenFD(f, "x")
	task.OpenFD(f, "x")
	var called atomic.Int32
	task.AddCleanup(func() { called.Add(1) })
	task.Exit("130")
	task.Exit("0")
	task.AddCleanup(func() { called.Add(1) })
	if f.closes != 1 || called.Load() != 2 || task.exit != "130" || task.Context().Err() != context.Canceled {
		t.Fatal("exit did not clean exactly once")
	}
	if task.Start() != fs.ErrClosed {
		t.Fatal("exited task restarted")
	}
}
func TestTaskExitUnblocksSignalRead(t *testing.T) {
	tasks := NewTaskFS()
	task, _ := tasks.Alloc("auto", nil)
	s, hub := signal.New()
	f, err := s.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	task.OpenFD(f, "winch")
	done := make(chan error, 1)
	go func() { _, err := f.Read(make([]byte, 20)); done <- err }()
	for hub.SubscriberCount() == 0 {
		runtime.Gosched()
	}
	task.Exit("130")
	if err := <-done; err != fs.ErrClosed {
		t.Fatalf("read: %v", err)
	}
}
