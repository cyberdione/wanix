package wanix

import (
 "errors"
 "testing"
 "testing/fstest"

 "tractor.dev/wanix/fs"
)

func TestTaskExportAdmission(t *testing.T) {
 task := &Task{}
 original := fstest.MapFS{"original": &fstest.MapFile{Data: []byte("original")}}
 replacement := fstest.MapFS{"replacement": &fstest.MapFile{Data: []byte("replacement")}}
 if err := Export(task, original); err != nil { t.Fatal(err) }
 task.Exit("0")
 if err := Export(task, replacement); !errors.Is(err, fs.ErrClosed) { t.Fatalf("late export: %v", err) }
 current, err := task.Export()
 if err != nil { t.Fatal(err) }
 if _, err := current.Open("original"); err != nil { t.Fatalf("accepted export lost: %v", err) }
 if _, err := current.Open("replacement"); !errors.Is(err, fs.ErrNotExist) { t.Fatalf("rejected export visible: %v", err) }
 if _, ok := task.taskMap()["export"]; !ok { t.Fatal("accepted export missing from snapshot") }
}

func TestTaskExportExitLinearization(t *testing.T) {
 for i := 0; i < 32; i++ {
  task := &Task{}
  exported := fstest.MapFS{}
  start := make(chan struct{})
  published := make(chan error, 1)
  exited := make(chan struct{})
  go func() { <-start; published <- Export(task, exported) }()
  go func() { <-start; task.Exit("0"); close(exited) }()
  close(start)
  err := <-published
  <-exited
  current, readErr := task.Export()
  if err == nil {
   if readErr != nil || current == nil { t.Fatalf("accepted export missing: %v", readErr) }
  } else {
   if !errors.Is(err, fs.ErrClosed) { t.Fatalf("unexpected admission error: %v", err) }
   if current != nil || !errors.Is(readErr, fs.ErrNotExist) { t.Fatalf("rejected export published: %v", readErr) }
  }
  if err := Export(task, exported); !errors.Is(err, fs.ErrClosed) { t.Fatalf("post-exit export: %v", err) }
 }
}
