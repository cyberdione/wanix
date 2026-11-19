package wanix

import (
	"errors"
	"sync/atomic"
	"testing"

	"tractor.dev/wanix/fs"
)

type cleanupFile struct {
	fs.File
	closes atomic.Int32
}

func (f *cleanupFile) Close() error { f.closes.Add(1); return errors.New("close attempt failed") }
func pendingCleanup(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("cleanup completed early")
	default:
	}
}

func TestTaskCleanupCompletion(t *testing.T) {
	task := &Task{fds: make(map[int]*openFile)}
	file := &cleanupFile{}
	task.fds[1] = &openFile{file: file}
	task.fds[2] = &openFile{file: file}
	entered, release := make(chan struct{}), make(chan struct{})
	task.AddCleanup(func() { close(entered); <-release })
	done := task.cleanupCompletion()
	exitReturned := make(chan struct{})
	go func() { task.Exit("130"); close(exitReturned) }()
	<-entered
	lateEntered, lateRelease, lateReturned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var nested atomic.Int32
	go func() {
		task.AddCleanup(func() { task.AddCleanup(func() { nested.Add(1) }); close(lateEntered); <-lateRelease })
		close(lateReturned)
	}()
	<-lateEntered
	task.Exit("other")
	pendingCleanup(t, done)
	close(release)
	<-exitReturned
	pendingCleanup(t, done)
	if file.closes.Load() != 1 {
		t.Fatal("FD close attempts", file.closes.Load())
	}
	close(lateRelease)
	<-lateReturned
	<-done
	if task.exit != "130" || nested.Load() != 1 {
		t.Fatal("exit/reentrant cleanup changed")
	}
	if task.cleanupCompletion() != done {
		t.Fatal("completion oracle replaced")
	}
	var post int
	task.AddCleanup(func() { post++ })
	if post != 1 {
		t.Fatal("post-seal cleanup not immediate")
	}
	<-done
}

func TestTaskCleanupPanicKeepsCompletionPending(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "original", true: "late"}[late], func(t *testing.T) {
			task := &Task{fds: make(map[int]*openFile)}
			done := task.cleanupCompletion()
			panicCleanup := func() { panic("cleanup panic") }
			recoverCall := func(call func()) {
				t.Helper()
				defer func() {
					if recover() != "cleanup panic" {
						t.Error("panic not propagated")
					}
				}()
				call()
			}
			if !late {
				task.AddCleanup(panicCleanup)
				recoverCall(func() { task.Exit("130") })
			} else {
				entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
				task.AddCleanup(func() { close(entered); <-release })
				go func() { task.Exit("130"); close(returned) }()
				<-entered
				recoverCall(func() { task.AddCleanup(panicCleanup) })
				close(release)
				<-returned
			}
			pendingCleanup(t, done)
		})
	}
}
