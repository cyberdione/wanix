package signal

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
	"tractor.dev/wanix/fs"
)

func TestQueueBlocksThenDeliversAndRejectsOverflow(t *testing.T) {
	q := NewQueue(4)
	got := make(chan string, 1)
	go func() {
		b := make([]byte, 4)
		n, err := q.Read(b)
		if err != nil {
			got <- err.Error()
			return
		}
		got <- string(b[:n])
	}()
	select {
	case <-got:
		t.Fatal("read did not block")
	case <-time.After(10 * time.Millisecond):
	}
	if n, err := q.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatalf("Write = %d,%v", n, err)
	}
	if n, err := q.Write([]byte("zz")); n != 0 || !errors.Is(err, ErrSaturated) {
		t.Fatalf("overflow = %d,%v", n, err)
	}
	select {
	case v := <-got:
		if v != "abc" {
			t.Fatalf("got %q", v)
		}
	case <-time.After(time.Second):
		t.Fatal("read remained blocked")
	}
}

func TestQueueCancellationDoesNotConsumeFutureFrame(t *testing.T) {
	q := NewQueue(8)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.ReadContext(ctx, make([]byte, 8)); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadContext = %v", err)
	}
	if _, err := q.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 8)
	n, err := q.Read(b)
	if err != nil || string(b[:n]) != "ok" {
		t.Fatalf("read %q,%v", b[:n], err)
	}
}

func TestQueueCloseDrainsThenEOFAndWakesReader(t *testing.T) {
	q := NewQueue(8)
	if _, err := q.Write([]byte("last")); err != nil {
		t.Fatal(err)
	}
	_ = q.Close()
	b := make([]byte, 8)
	n, err := q.Read(b)
	if err != nil || string(b[:n]) != "last" {
		t.Fatalf("read %q,%v", b[:n], err)
	}
	if _, err = q.Read(b); err != io.EOF {
		t.Fatalf("EOF = %v", err)
	}
	q2 := NewQueue(8)
	done := make(chan error, 1)
	go func() { _, e := q2.Read(make([]byte, 1)); done <- e }()
	_ = q2.Close()
	select {
	case e := <-done:
		if e != io.EOF {
			t.Fatalf("wake = %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("close failed to wake reader")
	}
}

func TestQueueFSReadWriteAndSingleReader(t *testing.T) {
	q := NewQueueFS(8)
	rf, err := q.OpenContext(context.Background(), "read")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.OpenContext(context.Background(), "read"); err != fs.ErrExist {
		t.Fatalf("second reader: %v", err)
	}
	wf, err := q.OpenContext(context.Background(), "write")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() {
		b := make([]byte, 8)
		n, e := rf.Read(b)
		if e != nil {
			done <- e.Error()
		} else {
			done <- string(b[:n])
		}
	}()
	if _, err = wf.(interface{ Write([]byte) (int, error) }).Write([]byte("event\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got != "event\n" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked read not woken")
	}
	_ = rf.Close()
	if _, err = q.OpenContext(context.Background(), "read"); err != fs.ErrNotExist {
		t.Fatalf("reader close did not close endpoint: %v", err)
	}
}

func TestQueueAllocatorCreatesAndReclaimsIndependentChannels(t *testing.T) {
	a := &QueueAllocator{MaxBytes: 16}
	create := func() string {
		f, err := a.Open("new")
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 40)
		n, err := f.Read(b)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		return string(b[:n-1])
	}
	id1, id2 := create(), create()
	if id1 == id2 {
		t.Fatalf("duplicate ids %q", id1)
	}
	w, err := a.Open(id1 + "/write")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.(interface{ Write([]byte) (int, error) }).Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	r, err := a.Open(id2 + "/read")
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	c, err := a.Open(id1 + "/ctl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.(interface{ Write([]byte) (int, error) }).Write([]byte("close")); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Open(id1 + "/write"); err != fs.ErrNotExist {
		t.Fatalf("closed endpoint retained: %v", err)
	}
	if _, err = a.Open(id2 + "/write"); err != fs.ErrNotExist {
		t.Fatalf("reader close did not reclaim endpoint: %v", err)
	}
}

func TestQueueFSReaderCloseUnblocksAndReclaims(t *testing.T) {
	q := NewQueueFS(8)
	r, err := q.OpenContext(context.Background(), "read")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := r.Read(make([]byte, 1)); done <- err }()
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != fs.ErrClosed {
			t.Fatalf("read after close = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reader close did not wake blocked read")
	}
	if _, err = q.Open("write"); err != fs.ErrNotExist {
		t.Fatalf("closed endpoint reopened: %v", err)
	}
}

func TestQueueAllocatorCapsLiveEndpoints(t *testing.T) {
	a := &QueueAllocator{MaxBytes: 8, MaxQueues: 1}
	newID := func() string {
		f, e := a.Open("new")
		if e != nil {
			t.Fatal(e)
		}
		b := make([]byte, 40)
		n, e := f.Read(b)
		if e != nil {
			t.Fatal(e)
		}
		return string(b[:n-1])
	}
	id := newID()
	f, _ := a.Open(id + "/read")
	g, err := a.Open("new")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = g.Read(make([]byte, 40)); !errors.Is(err, ErrSaturated) {
		t.Fatalf("allocator limit = %v", err)
	}
	_ = f.Close()
	if got := newID(); got == "" || got == id {
		t.Fatalf("endpoint slot was not reclaimed: %q", got)
	}
}

func TestQueueFSContextCancellationClosesEndpoint(t *testing.T) {
	q := NewQueueFS(8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, err := q.OpenContext(ctx, "read")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, e := r.Read(make([]byte, 1)); done <- e }()
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("read cancellation = %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not wake blocked read")
	}
	if _, err = q.Open("write"); err != fs.ErrNotExist {
		t.Fatalf("cancel did not close endpoint: %v", err)
	}
}

func TestQueueFSReaderCloseDoesNotAllowReplacementReader(t *testing.T) {
	q := NewQueueFS(8)
	r, err := q.Open("read")
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = q.Open("read"); err != fs.ErrNotExist {
		t.Fatalf("reader reopened after close: %v", err)
	}
}
