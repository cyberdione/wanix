package pipe

import (
	"context"
	"testing"
)

func TestCancelReaderPreservesSharedPort(t *testing.T) {
	a, b := New(true)
	one := &PortFile{Port: a}
	two := &PortFile{Port: a}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := one.ReadContext(ctx, make([]byte, 1)); done <- err }()
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := b.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if n, err := two.Read(buf); err != nil || n != 1 || buf[0] != 'x' {
		t.Fatalf("shared reader broken: %d %v", n, err)
	}
}

func TestCanceledReaderDoesNotConsumeBufferedData(t *testing.T) {
	a, b := New(true)
	f := &PortFile{Port: a}
	_, _ = b.Write([]byte("x"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := f.ReadContext(ctx, make([]byte, 1)); n != 0 || err != context.Canceled {
		t.Fatalf("cancelled read %d %v", n, err)
	}
	buf := make([]byte, 1)
	if n, err := f.ReadContext(context.Background(), buf); n != 1 || err != nil || buf[0] != 'x' {
		t.Fatalf("data consumed %d %v", n, err)
	}
}
