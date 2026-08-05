//go:build !js && !wasm

package headless_test

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tractor.dev/toolkit-go/duplex/codec"
	"tractor.dev/toolkit-go/duplex/mux"
	"tractor.dev/toolkit-go/duplex/talk"
	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/memfs"
	"tractor.dev/wanix/fs/p9kit"
	"tractor.dev/wanix/headless"
)

// statOK asserts a path resolves in the namespace (bind present and reachable).
// Device presentation varies (#signal is a file, #pipe a dir), so we only
// require the bind to resolve — which is how the namespace is consumed.
func statOK(t *testing.T, ns fs.FS, p string) {
	t.Helper()
	if _, err := fs.Stat(ns, p); err != nil {
		t.Errorf("namespace: Stat %q: %v", p, err)
	}
}

// TestRunRegistersNativeGoJSDriver verifies that a caller-supplied root gets
// the native GoJS driver too. This protects the production composition where
// the launcher prepares its namespace before starting the listeners.
func TestRunRegistersNativeGoJSDriver(t *testing.T) {
	root, err := headless.Bootstrap()
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "api.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := headless.New(headless.Options{
		Root:       root,
		APIAddr:    "unix://" + sock,
		GoJSRunner: "/unused/test-runner.mjs",
	})
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("API socket was not created: %v", err)
	}
	resource, err := fs.OpenContext(context.Background(), root.NS(), "#task/new/gojs")
	if err != nil {
		t.Fatalf("gojs task kind was not registered: %v", err)
	}
	defer resource.Close()
	if _, err := io.ReadAll(resource); err != nil {
		t.Fatalf("allocate gojs task: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// TestBootstrap boots the native namespace and checks the documented device
// binds are reachable. Asserted via Stat (walk), which is how the namespace is
// actually consumed; the root ReadDir synthesis intentionally hides "#"-devices.
func TestBootstrap(t *testing.T) {
	root, err := headless.Bootstrap()
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	for _, p := range []string{"#task", "#wanix", "#pipe", "#signal", "#ramfs"} {
		statOK(t, root.NS(), p)
	}
}

// TestServe9P drives the raw 9P stream end-to-end: a 9P client writes a file
// through the headless-served namespace and reads it back, proving the stream
// is bound to the live task namespace.
func TestServe9P(t *testing.T) {
	root, err := headless.Bootstrap()
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if err := root.NS().Bind(memfs.New(), ".", "scratch"); err != nil {
		t.Fatalf("bind scratch: %v", err)
	}

	srvConn, clientConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = headless.Serve9P(ctx, srvConn, root) }()

	fsys, err := p9kit.ClientFS(clientConn, "")
	if err != nil {
		t.Fatalf("ClientFS: %v", err)
	}
	if err := fs.WriteFile(fsys, "scratch/hello.txt", []byte("hello headless"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := fs.OpenContext(context.Background(), fsys, "scratch/hello.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(f)
	f.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "hello headless" {
		t.Fatalf("9P round-trip = %q, want %q", got, "hello headless")
	}
}

// apiStatReply mirrors the api.Stat handler's response struct.
type apiStatReply struct {
	Size    int64
	Mode    uint32
	IsDir   bool
	ModTime time.Time
}

// TestServeAPI drives the task/API port end-to-end: a talk client calls Stat
// over the headless-served mux session and reaches the live namespace.
func TestServeAPI(t *testing.T) {
	root, err := headless.Bootstrap()
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	srvConn, clientConn := net.Pipe()
	go headless.ServeAPI(mux.New(srvConn), root)

	peer := talk.NewPeer(mux.New(clientConn), codec.CBORCodec{})
	defer peer.Close()

	var reply apiStatReply
	if _, err := peer.Call(context.Background(), "Stat", []string{"#task"}, &reply); err != nil {
		t.Fatalf("Call Stat: %v", err)
	}
	if !reply.IsDir {
		t.Errorf("Stat #task over API: IsDir=false, want true")
	}
}

// TestRunGuards checks Server.Run rejects invalid address combinations.
func TestRunGuards(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := headless.New(headless.Options{}).Run(ctx); err == nil {
		t.Error("Run with no addresses should error")
	}
	if err := headless.New(headless.Options{APIAddr: "stdio", NinePAddr: "stdio"}).Run(ctx); err == nil {
		t.Error("Run with both stdio should error")
	}
}

// TestServe9PCancel verifies cancellation closes the stream promptly rather
// than blocking on a peer that never disconnects.
func TestServe9PCancel(t *testing.T) {
	root, err := headless.Bootstrap()
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	srvConn, clientConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- headless.Serve9P(ctx, srvConn, root) }()

	cancel()
	_ = clientConn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve9P did not return after cancellation")
	}
}
