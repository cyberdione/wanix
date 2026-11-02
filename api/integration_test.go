package api

import (
	"bytes"
	"context"
	"io"
	iofs "io/fs"
	"testing"

	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"

	"tractor.dev/toolkit-go/duplex/codec"
	"tractor.dev/toolkit-go/duplex/mux"
	"tractor.dev/toolkit-go/duplex/talk"
	"tractor.dev/wanix"
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
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		Responder(a, root)
	}()
	client := talk.NewPeer(b, codec.CBORCodec{})
	t.Cleanup(func() {
		_ = client.Close()
		<-serverDone
		_ = a.Close()
	})
	return client
}

type inertTaskDriver struct{}

func (inertTaskDriver) Check(*wanix.Task) bool  { return false }
func (inertTaskDriver) Start(*wanix.Task) error { return nil }

func TestTaskSelfReadThroughResponder(t *testing.T) {
	root, err := wanix.NewRoot()
	if err != nil {
		t.Fatal(err)
	}
	root.Register("test", inertTaskDriver{})
	childID, err := fs.ReadFile(root.NS(), "#task/new/test")
	if err != nil {
		t.Fatal(err)
	}
	child, err := root.Lookup(string(bytes.TrimSpace(childID)))
	if err != nil {
		t.Fatal(err)
	}
	a, b := mux.Pair()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		Responder(a, child)
	}()
	client := talk.NewPeer(b, codec.CBORCodec{})
	t.Cleanup(func() {
		_ = client.Close()
		<-serverDone
		_ = a.Close()
	})
	var id []byte
	_, err = client.Call(context.Background(), "ReadFile", []any{"#task/self/id"}, &id)
	if err != nil {
		t.Fatalf("child ReadFile(#task/self/id): %v", err)
	}
	if string(id) != child.ID()+"\n" {
		t.Fatalf("child id = %q, want %q", id, child.ID()+"\n")
	}
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

type stderrTestFS struct {
	data bytes.Buffer
}

func (f *stderrTestFS) Open(name string) (iofs.File, error) {
	if name != "." {
		return nil, fs.ErrNotExist
	}
	return &stderrTestFile{owner: f}, nil
}

type stderrTestFile struct{ owner *stderrTestFS }

func (f *stderrTestFile) Read([]byte) (int, error)    { return 0, io.EOF }
func (f *stderrTestFile) Write(p []byte) (int, error) { return f.owner.data.Write(p) }
func (f *stderrTestFile) Close() error                { return nil }
func (f *stderrTestFile) Stat() (fs.FileInfo, error)  { return fskit.Entry("stderr", 0644), nil }

func TestWriteAtRoutesTaskStderrFDToBoundStream(t *testing.T) {
	root, err := wanix.NewRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := root.NS().Bind(memfs.New(), ".", "."); err != nil {
		t.Fatal(err)
	}
	stderr := &stderrTestFS{}
	if err := root.NS().Bind(stderr, ".", "#task/1/fd/2"); err != nil {
		t.Fatal(err)
	}

	a, b := mux.Pair()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		Responder(a, root)
	}()
	client := talk.NewPeer(b, codec.CBORCodec{})
	t.Cleanup(func() {
		_ = client.Close()
		<-serverDone
		_ = a.Close()
	})
	ctx := context.Background()
	for _, part := range []struct {
		data   string
		offset uint64
	}{{"first", 0}, {"second", 5}} {
		_, err := client.Call(ctx, "WriteAt", []any{uint64(2), []byte(part.data), part.offset})
		if err != nil {
			t.Fatalf("WriteAt(%q): %v", part.data, err)
		}
	}
	if got := stderr.data.String(); got != "firstsecond" {
		t.Fatalf("stderr bytes = %q, want sequential stream data", got)
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
