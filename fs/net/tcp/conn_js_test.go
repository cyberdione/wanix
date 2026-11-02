//go:build js && wasm

package tcp

import (
	"errors"
	"syscall/js"
	"testing"
	"time"

	"tractor.dev/wanix/fs"
)

func TestGenerationPinnedDataRejectsStaleHandle(t *testing.T) {
	c := newConn("1", New())
	c.generation = 2
	c.state = stateConnected
	f := &dataFile{conn: c, generation: 1}
	if _, err := f.Read(make([]byte, 1)); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("stale read error = %v, want ErrPermission", err)
	}
	if _, err := f.Write([]byte{1}); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("stale write error = %v, want ErrPermission", err)
	}
}

func TestDisposeGenerationRejectsFutureAndIgnoresStale(t *testing.T) {
	c := newConn("1", New())
	c.generation = 2
	if err := c.dispose("1"); err != nil {
		t.Fatalf("stale dispose: %v", err)
	}
	if err := c.dispose("3"); err == nil {
		t.Fatal("future generation dispose unexpectedly succeeded")
	}
	if err := c.dispose("02"); err == nil {
		t.Fatal("noncanonical generation dispose unexpectedly succeeded")
	}
}

func TestWriteStreamRoutesThroughPageSeam(t *testing.T) {
	global, object, promise := js.Global(), js.Global().Get("Object"), js.Global().Get("Promise")
	stream := object.New()
	writeFn := js.FuncOf(func(this js.Value, args []js.Value) any { return promise.Call("resolve", args[0].Get("byteLength")) })
	defer writeFn.Release()
	stream.Set("write", writeFn)
	seamCalls, firstByte, gotLen := 0, 0, 0
	seam := js.FuncOf(func(this js.Value, args []js.Value) any {
		seamCalls++
		buf := args[1]
		gotLen = buf.Get("byteLength").Int()
		firstByte = buf.Index(0).Int()
		return args[0].Call("write", buf)
	})
	defer seam.Release()
	global.Set("__ferruleosWrite", seam)
	defer global.Set("__ferruleosWrite", js.Undefined())

	c := newConn("1", New())
	c.state, c.stream, c.generation = stateConnected, stream, 1
	n, err := c.Write([]byte{0x53, 1, 2})
	if err != nil || n != 3 {
		t.Fatalf("write = %d, %v", n, err)
	}
	if seamCalls != 1 {
		t.Fatalf("seam calls = %d, want 1", seamCalls)
	}
	if gotLen != 3 || firstByte != 0x53 {
		t.Fatalf("seam saw len=%d firstByte=%#x, want 3/0x53", gotLen, firstByte)
	}
}

func TestDialAndGenerationPinnedStreamLifecycle(t *testing.T) {
	svc := New()
	id, err := svc.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	global := js.Global()
	promise := global.Get("Promise")
	object := global.Get("Object")
	var closeCount int
	stream := object.New()
	readFn := js.FuncOf(func(this js.Value, args []js.Value) any {
		b := global.Get("Uint8Array").New(1)
		b.SetIndex(0, 65)
		return promise.Call("resolve", b)
	})
	writeFn := js.FuncOf(func(this js.Value, args []js.Value) any { return promise.Call("resolve", args[0].Get("byteLength")) })
	closeFn := js.FuncOf(func(this js.Value, args []js.Value) any { closeCount++; return promise.Call("resolve") })
	defer readFn.Release()
	defer writeFn.Release()
	defer closeFn.Release()
	stream.Set("read", readFn)
	stream.Set("write", writeFn)
	stream.Set("close", closeFn)
	openFn := js.FuncOf(func(this js.Value, args []js.Value) any { return promise.Call("resolve", stream) })
	defer openFn.Release()
	net := object.New()
	net.Set("openStream", openFn)
	root := object.New()
	root.Set("net", net)
	global.Set("__ferruleos", root)
	defer global.Set("__ferruleos", js.Undefined())

	if err := c.dial(nil, "example.onion:22"); err != nil {
		t.Fatalf("dial: %v", err)
	}
	if c.generation != 1 {
		t.Fatalf("generation = %d, want 1", c.generation)
	}
	file, err := c.Open("data/1")
	if err != nil {
		t.Fatalf("open generation data: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close data handle: %v", err)
	}
	if closeCount != 0 {
		t.Fatal("closing data handle closed allocation stream")
	}
	file, err = c.Open("data/1")
	if err != nil {
		t.Fatalf("reopen generation data: %v", err)
	}
	buf := make([]byte, 1)
	n, err := file.Read(buf)
	if err != nil || n != 1 || buf[0] != 65 {
		t.Fatalf("read = %d, %v, %v", n, err, buf)
	}
	if n, err := fs.Write(file, []byte{1, 2}); err != nil || n != 2 {
		t.Fatalf("write = %d, %v", n, err)
	}
	if err := c.hangup(); err != nil {
		t.Fatalf("hangup: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for closeCount != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if closeCount != 1 {
		t.Fatalf("stream closes = %d, want 1", closeCount)
	}
	if _, err := file.Read(buf); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("old generation read = %v, want ErrPermission", err)
	}
	if err := c.dial(nil, "example.onion:22"); err != nil {
		t.Fatalf("redial: %v", err)
	}
	if c.generation != 2 {
		t.Fatalf("redial generation = %d, want 2", c.generation)
	}
	if _, err := c.Open("data/1"); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("stale data path open = %v, want ErrPermission", err)
	}
	if _, err := file.Read(buf); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("retained stale handle read = %v, want ErrPermission", err)
	}
	if err := c.dispose("1"); err != nil {
		t.Fatalf("stale dispose: %v", err)
	}
	if c.state != stateConnected {
		t.Fatal("stale dispose affected live generation")
	}
	if err := c.dispose("2"); err != nil {
		t.Fatalf("dispose live generation: %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for closeCount != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if closeCount != 2 {
		t.Fatalf("stream closes = %d, want 2", closeCount)
	}
	if _, err := svc.Get(id); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("disposed allocation lookup = %v, want ErrNotExist", err)
	}
}

func TestCancelDuringOpenClosesLateStream(t *testing.T) {
	svc := New()
	id, err := svc.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	promise, object := js.Global().Get("Promise"), js.Global().Get("Object")
	var closeCount int
	stream := object.New()
	closeFn := js.FuncOf(func(this js.Value, args []js.Value) any { closeCount++; return promise.Call("resolve") })
	defer closeFn.Release()
	readFn := js.FuncOf(func(this js.Value, args []js.Value) any { return promise.Call("resolve", js.Null()) })
	writeFn := js.FuncOf(func(this js.Value, args []js.Value) any { return promise.Call("resolve", 0) })
	defer readFn.Release()
	defer writeFn.Release()
	stream.Set("read", readFn)
	stream.Set("write", writeFn)
	stream.Set("close", closeFn)
	var resolve js.Value
	executor := js.FuncOf(func(this js.Value, args []js.Value) any { resolve = args[0]; return nil })
	delayed := promise.New(executor)
	executor.Release()
	openFn := js.FuncOf(func(this js.Value, args []js.Value) any { return delayed })
	defer openFn.Release()
	net, root, global := object.New(), object.New(), js.Global()
	net.Set("openStream", openFn)
	root.Set("net", net)
	global.Set("__ferruleos", root)
	defer global.Set("__ferruleos", js.Undefined())
	done := make(chan error, 1)
	go func() { done <- c.startDial("late.onion:22") }()
	for i := 0; i < 100 && c.status() != string(stateConnecting); i++ {
		time.Sleep(time.Millisecond)
	}
	if c.status() != string(stateConnecting) {
		t.Fatalf("dial did not enter connecting: %s", c.status())
	}
	if err := c.hangup(); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	resolve.Invoke(stream)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("start dial: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("dial start did not settle")
	}
	for i := 0; i < 100 && closeCount == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	if closeCount != 1 {
		t.Fatalf("late stream close count = %d, want 1", closeCount)
	}
	if c.status() != string(stateIdle) {
		t.Fatalf("post-cancel status = %q, want idle", c.status())
	}
}

func TestRedialAndLateCloseDoNotWaitForProviderClose(t *testing.T) {
	svc := New()
	id, err := svc.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	global, object := js.Global(), js.Global().Get("Object")
	executor := js.FuncOf(func(js.Value, []js.Value) any { return nil })
	defer executor.Release()
	pending := global.Get("Promise").New(executor)
	closeCalls := 0
	stream := object.New()
	closeFn := js.FuncOf(func(js.Value, []js.Value) any { closeCalls++; return pending })
	defer closeFn.Release()
	stream.Set("close", closeFn)
	stream.Set("read", closeFn)
	stream.Set("write", closeFn)
	openFn := js.FuncOf(func(js.Value, []js.Value) any { return global.Get("Promise").Call("resolve", stream) })
	defer openFn.Release()
	net := object.New()
	net.Set("openStream", openFn)
	root := object.New()
	root.Set("net", net)
	global.Set("__ferruleos", root)
	defer global.Set("__ferruleos", js.Undefined())
	c.state, c.stream = stateError, stream
	if err := c.startDial("test.onion:22"); err != nil {
		t.Fatal(err)
	}
	if err := c.hangup(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for closeCalls != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if closeCalls != 2 {
		t.Fatalf("close calls = %d, want 2", closeCalls)
	}
	if c.status() != string(stateIdle) {
		t.Fatalf("post-cancel status = %s", c.status())
	}
}

func TestRevokeDuringPendingDialClosesLateStream(t *testing.T) {
	svc := New()
	id, err := svc.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	promise, object := js.Global().Get("Promise"), js.Global().Get("Object")
	var resolve js.Value
	executor := js.FuncOf(func(_ js.Value, args []js.Value) any { resolve = args[0]; return nil })
	deferred := promise.New(executor)
	executor.Release()
	openFn := js.FuncOf(func(js.Value, []js.Value) any { return deferred })
	defer openFn.Release()
	stream := object.New()
	closeCalls := 0
	closeFn := js.FuncOf(func(js.Value, []js.Value) any { closeCalls++; return promise.Call("resolve") })
	defer closeFn.Release()
	readFn := js.FuncOf(func(js.Value, []js.Value) any { return promise.Call("resolve", js.Null()) })
	defer readFn.Release()
	writeFn := js.FuncOf(func(js.Value, []js.Value) any { return promise.Call("resolve", 0) })
	defer writeFn.Release()
	stream.Set("close", closeFn)
	stream.Set("read", readFn)
	stream.Set("write", writeFn)
	net := object.New()
	net.Set("openStream", openFn)
	root := object.New()
	root.Set("net", net)
	js.Global().Set("__ferruleos", root)
	defer js.Global().Set("__ferruleos", js.Undefined())
	if err := c.startDial("late.onion:22"); err != nil {
		t.Fatal(err)
	}
	svc.CloseAll()
	if _, err := svc.Get(id); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("revoked allocation remains: %v", err)
	}
	resolve.Invoke(stream)
	deadline := time.Now().Add(time.Second)
	for closeCalls == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if closeCalls != 1 {
		t.Fatalf("late stream close calls = %d, want 1", closeCalls)
	}
	if c.status() != string(stateClosed) {
		t.Fatalf("revoked state = %q", c.status())
	}
}

func TestHangupAndDisposeDoNotWaitForProviderClose(t *testing.T) {
	svc := New()
	id, err := svc.Alloc()
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	executor := js.FuncOf(func(js.Value, []js.Value) any { return nil })
	defer executor.Release()
	pending := js.Global().Get("Promise").New(executor)
	closeCalls := 0
	closeFn := js.FuncOf(func(js.Value, []js.Value) any { closeCalls++; return pending })
	defer closeFn.Release()
	stream := js.Global().Get("Object").New()
	stream.Set("close", closeFn)
	c.state, c.stream, c.generation = stateConnected, stream, 1
	if err := c.hangup(); err != nil {
		t.Fatal(err)
	}
	if c.state != stateIdle || c.stream.Truthy() {
		t.Fatal("hangup did not invalidate stream")
	}
	c.state, c.stream = stateConnected, stream
	if err := c.dispose("1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(id); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dispose did not remove allocation: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for closeCalls != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if closeCalls != 2 {
		t.Fatalf("close calls = %d, want 2", closeCalls)
	}
}

func TestCloseAllDoesNotWaitForProviderClose(t *testing.T) {
	svc := New()
	executor := js.FuncOf(func(js.Value, []js.Value) any { return nil })
	defer executor.Release()
	pending := js.Global().Get("Promise").New(executor)
	closeCalls := 0
	var funcs []js.Func
	defer func() {
		for _, fn := range funcs {
			fn.Release()
		}
	}()
	for _, id := range []string{"1", "2"} {
		c := newConn(id, svc)
		c.state = stateConnected
		stream := js.Global().Get("Object").New()
		closeFn := js.FuncOf(func(js.Value, []js.Value) any { closeCalls++; return pending })
		funcs = append(funcs, closeFn)
		stream.Set("close", closeFn)
		c.stream = stream
		svc.resources[id] = c
	}
	svc.CloseAll()
	for _, id := range []string{"1", "2"} {
		if _, err := svc.Get(id); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("allocation %s remains after revocation: %v", id, err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for closeCalls != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if closeCalls != 2 {
		t.Fatalf("close calls = %d, want 2", closeCalls)
	}
}

func TestCloseAllClosesAndRemovesEveryAllocatedStream(t *testing.T) {
	svc := New()
	promise, object := js.Global().Get("Promise"), js.Global().Get("Object")
	closeCounts := map[string]int{"1": 0, "2": 0}
	for _, id := range []string{"1", "2"} {
		c := newConn(id, svc)
		c.state = stateConnected
		stream := object.New()
		readFn := js.FuncOf(func(js.Value, []js.Value) any { return promise.Call("resolve", js.Null()) })
		writeFn := js.FuncOf(func(_ js.Value, args []js.Value) any { return promise.Call("resolve", args[0].Get("byteLength")) })
		closeFn := js.FuncOf(func(js.Value, []js.Value) any { closeCounts[id]++; return promise.Call("resolve") })
		defer readFn.Release()
		defer writeFn.Release()
		defer closeFn.Release()
		stream.Set("read", readFn)
		stream.Set("write", writeFn)
		stream.Set("close", closeFn)
		c.stream = stream
		svc.resources[id] = c
	}
	svc.CloseAll()
	deadline := time.Now().Add(time.Second)
	for closeCounts["1"] != 1 || closeCounts["2"] != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("close counts = %v", closeCounts)
		}
		time.Sleep(time.Millisecond)
	}
	for _, id := range []string{"1", "2"} {
		if _, err := svc.Get(id); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("allocation %s remains: %v", id, err)
		}
	}
}
