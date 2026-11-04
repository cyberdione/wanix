package api

import (
	"bytes"
	"context"
	"io"
	"testing"

	"tractor.dev/toolkit-go/duplex/codec"
	"tractor.dev/toolkit-go/duplex/mux"
	"tractor.dev/toolkit-go/duplex/talk"
	"tractor.dev/wanix"
	"tractor.dev/wanix/fs/fskit"
	"tractor.dev/wanix/fs/memfs"
)

// startTestServer wires a Responder on one end of an in-memory duplex session,
// with a root task whose namespace is a fresh memfs, and returns a client peer.
func startTestServer(t *testing.T) *talk.Peer {
	t.Helper()
	root, err := wanix.NewRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := root.NS().Bind(memfs.New(), ".", "."); err != nil {
		t.Fatal(err)
	}
	a, b := mux.Pair()
	t.Cleanup(func() { a.Close(); b.Close() })
	go Responder(a, root)
	return talk.NewPeer(b, codec.CBORCodec{})
}

func startTestServerWithTask(t *testing.T) (*talk.Peer, *wanix.Task) {
	t.Helper()
	root, err := wanix.NewRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := root.NS().Bind(memfs.New(), ".", "."); err != nil {
		t.Fatal(err)
	}
	a, b := mux.Pair()
	t.Cleanup(func() { a.Close(); b.Close() })
	go Responder(a, root)
	return talk.NewPeer(b, codec.CBORCodec{}), root
}

func TestReadReturnsBytesAlongsideEOF(t *testing.T) {
	client, task := startTestServerWithTask(t)
	fd := task.OpenFD(fskit.NewStreamFile(eofReader{Reader: bytes.NewReader([]byte("data"))}, nil, nil, fskit.Entry("stream", 0600)), "stream")
	var got []byte
	if _, err := client.Call(context.Background(), "Read", []any{uint64(fd), uint64(8)}, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("read = %q, want data", got)
	}
}

type eofReader struct{ io.Reader }

func (r eofReader) Read(p []byte) (int, error) {
	n, _ := r.Reader.Read(p)
	return n, io.EOF
}

func TestGuardRecoversPanic(t *testing.T) {
	client := startTestServer(t)
	ctx := context.Background()

	// Chmod panics on a bad mode type; the guard must recover it and return
	// an error instead of crashing the kernel.
	if _, err := client.Call(ctx, "Chmod", []any{"hello.txt", "bad"}); err == nil {
		t.Fatal("expected error from guarded panic")
	}

	// The kernel must still serve subsequent calls.
	if _, err := client.Call(ctx, "WriteFile", []any{"after.txt", []byte("ok")}); err != nil {
		t.Fatalf("server died after a guarded panic: %v", err)
	}
}

func TestChtimesIntegerTimestamps(t *testing.T) {
	client := startTestServer(t)
	ctx := context.Background()

	if _, err := client.Call(ctx, "WriteFile", []any{"hello.txt", []byte("hi")}); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	// Integer timestamps: CBOR encodes whole-number seconds as integers, which
	// is what used to panic the kernel (arg was asserted as float64).
	if _, err := client.Call(ctx, "Chtimes", []any{"hello.txt", int64(1700000000), uint64(1700000001)}); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}
