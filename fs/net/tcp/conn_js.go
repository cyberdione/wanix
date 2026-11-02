//go:build js && wasm

package tcp

import (
	"context"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"
	"syscall/js"

	"tractor.dev/toolkit-go/engine/cli"
	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/fskit"
	"tractor.dev/wanix/misc"
	"tractor.dev/wanix/misc/jsutil"
)

type connState string

const (
	stateIdle       connState = "idle"
	stateConnecting connState = "connecting"
	stateConnected  connState = "connected"
	stateClosed     connState = "closed"
	stateError      connState = "error"
)

// Conn adapts the session host's JavaScript network stream to a Plan 9 data
// file. The provider is looked up on every dial so it may be installed after
// the wasm kernel has booted.
type Conn struct {
	id  string
	svc *Service

	mu         sync.RWMutex
	state      connState
	stream     js.Value
	lastErr    error
	generation uint64
	attempt    uint64
}

func newConn(id string, svc *Service) *Conn {
	return &Conn{id: id, svc: svc, state: stateIdle, stream: js.Undefined()}
}

func (c *Conn) shutdown() {
	c.mu.Lock()
	stream := c.stream
	c.stream = js.Undefined()
	c.state = stateClosed
	c.mu.Unlock()
	closeStreamAsync(stream)
}

func (c *Conn) Open(name string) (fs.File, error) {
	return c.OpenContext(context.Background(), name)
}

func (c *Conn) OpenContext(ctx context.Context, name string) (fs.File, error) {
	return fs.OpenContext(ctx, c.rootFS(), name)
}

func (c *Conn) rootFS() fskit.MapFS {
	return fskit.MapFS{
		"ctl": misc.ControlFile(&cli.Command{
			Usage: "ctl",
			Run: func(ctx *cli.Context, args []string) {
				if len(args) == 0 {
					return
				}
				var err error
				switch args[0] {
				case "dial":
					if len(args) != 2 {
						err = fmt.Errorf("usage: dial <target>")
					} else {
						err = c.startDial(args[1])
					}
				case "hangup", "cancel":
					if len(args) != 1 {
						err = fmt.Errorf("usage: %s", args[0])
					} else {
						err = c.hangup()
					}
				case "dispose":
					if len(args) != 2 {
						err = fmt.Errorf("usage: dispose <generation>")
					} else {
						err = c.dispose(args[1])
					}
				case "bind", "announce", "listen":
					err = fmt.Errorf("%s: %w", args[0], fs.ErrNotSupported)
				default:
					err = fmt.Errorf("%s: %w", args[0], fs.ErrNotSupported)
				}
				if err != nil {
					panic(err)
				}
			},
		}),
		"status": misc.FieldFile(func() (string, error) { return c.status(), nil }),
		"generation": misc.FieldFile(func() (string, error) {
			c.mu.RLock()
			defer c.mu.RUnlock()
			return strconv.FormatUint(c.generation, 10), nil
		}),
		"data": fskit.OpenFunc(func(ctx context.Context, name string) (fs.File, error) {
			c.mu.RLock()
			gen := c.generation
			stream := c.stream
			state := c.state
			c.mu.RUnlock()
			if name != "." {
				parsed, err := strconv.ParseUint(name, 10, 64)
				if err != nil || strconv.FormatUint(parsed, 10) != name {
					return nil, fs.ErrNotExist
				}
				if parsed != gen {
					return nil, fs.ErrPermission
				}
				gen = parsed
			}
			if state != stateConnected || !stream.Truthy() {
				return nil, fs.ErrPermission
			}
			return &dataFile{conn: c, generation: gen, stream: stream}, nil
		}),
	}
}

func (c *Conn) Route(ctx context.Context, name string) (fs.FS, string, error) {
	return c.rootFS().Route(ctx, name)
}

func (c *Conn) armDial() (js.Value, uint64, uint64, error) {
	provider, err := netProvider()
	if err != nil {
		return js.Undefined(), 0, 0, err
	}
	c.mu.Lock()
	if c.state == stateConnected || c.state == stateConnecting || c.state == stateClosed {
		c.mu.Unlock()
		return js.Undefined(), 0, 0, fmt.Errorf("dial: %w", fs.ErrPermission)
	}
	oldStream := c.stream
	c.stream = js.Undefined()
	c.generation++
	c.attempt++
	attempt := c.attempt
	gen := c.generation
	c.state = stateConnecting
	c.lastErr = nil
	c.mu.Unlock()
	closeStreamAsync(oldStream)
	return provider, gen, attempt, nil
}

func (c *Conn) startDial(target string) error {
	provider, gen, attempt, err := c.armDial()
	if err != nil {
		return err
	}
	go c.finishDial(provider, gen, attempt, target)
	return nil
}

func (c *Conn) dial(ctx context.Context, target string) error {
	provider, gen, attempt, err := c.armDial()
	if err != nil {
		return err
	}
	return c.finishDial(provider, gen, attempt, target)
}

func (c *Conn) finishDial(provider js.Value, gen, attempt uint64, target string) error {
	log.Printf("KDBG2 finishDial: enter id=%s gen=%d attempt=%d target=%s", c.id, gen, attempt, target) // KDBG2 (temporary)
	defer func() { log.Printf("KDBG2 finishDial: exit id=%s gen=%d attempt=%d", c.id, gen, attempt) }() // KDBG2 (temporary)
	stream, err := jsutil.AwaitErr(provider.Call("openStream", target))
	log.Printf("KDBG2 finishDial: openStream returned err=%v truthy=%v", err, stream.Truthy()) // KDBG2 (temporary)
	if err != nil {
		c.mu.Lock()
		if c.attempt == attempt && c.state == stateConnecting {
			c.state = stateError
			c.lastErr = err
		}
		c.mu.Unlock()
		return err
	}
	if stream.IsNull() || stream.IsUndefined() || stream.Get("read").Type() != js.TypeFunction || stream.Get("write").Type() != js.TypeFunction || stream.Get("close").Type() != js.TypeFunction {
		err = fmt.Errorf("openStream: invalid stream")
		c.mu.Lock()
		if c.attempt == attempt {
			c.state = stateError
			c.lastErr = err
		}
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	if c.attempt != attempt || c.state != stateConnecting {
		c.mu.Unlock()
		closeStreamAsync(stream)
		return fmt.Errorf("dial: %w", fs.ErrPermission)
	}
	c.stream = stream
	c.state = stateConnected
	c.lastErr = nil
	c.mu.Unlock()
	log.Printf("KDBG2 finishDial: CONNECTED id=%s gen=%d", c.id, gen) // KDBG2 (temporary)
	return nil
}

// netProvider resolves globalThis.__ferruleos.net.openStream dynamically.
func netProvider() (js.Value, error) {
	log.Printf("KDBG2 netProvider: enter") // KDBG2 (temporary live-pass instrumentation)
	root := js.Global().Get("__ferruleos")
	if root.IsNull() || root.IsUndefined() {
		log.Printf("KDBG2 netProvider: no __ferruleos global") // KDBG2 (temporary)
		return js.Undefined(), fmt.Errorf("network provider unavailable")
	}
	net := root.Get("net")
	if net.IsNull() || net.IsUndefined() || net.Get("openStream").Type() != js.TypeFunction {
		log.Printf("KDBG2 netProvider: __ferruleos.net missing openStream") // KDBG2 (temporary)
		return js.Undefined(), fmt.Errorf("network provider unavailable")
	}
	log.Printf("KDBG2 netProvider: ok") // KDBG2 (temporary)
	return net, nil
}

func (c *Conn) Read(p []byte) (int, error) {
	c.mu.RLock()
	stream, gen, state := c.stream, c.generation, c.state
	c.mu.RUnlock()
	return readStream(stream, gen, c, p, state)
}

func readStream(stream js.Value, gen uint64, c *Conn, p []byte, state connState) (int, error) {
	log.Printf("KDBG2 readStream: enter id=%s gen=%d len=%d state=%s", c.id, gen, len(p), state) // KDBG2 (temporary)
	if state != stateConnected || stream.IsUndefined() || stream.IsNull() {
		log.Printf("KDBG2 readStream: not connected (state=%s) -> err", state) // KDBG2 (temporary)
		if state == stateIdle || state == stateConnecting {
			return 0, fs.ErrPermission
		}
		return 0, io.EOF
	}
	v, err := jsutil.AwaitErr(stream.Call("read", len(p)))
	log.Printf("KDBG2 readStream: provider read(%d) -> err=%v null=%v", len(p), err, v.IsNull() || v.IsUndefined()) // KDBG2 (temporary)
	if err != nil {
		c.setErrorGeneration(gen, err)
		log.Printf("KDBG2 readStream: provider error -> 0, %v", err) // KDBG2 (temporary)
		return 0, err
	}
	if v.IsNull() || v.IsUndefined() {
		log.Printf("KDBG2 readStream: provider null -> EOF") // KDBG2 (temporary)
		return 0, io.EOF
	}
	length := v.Get("byteLength")
	if length.Type() != js.TypeNumber {
		log.Printf("KDBG2 readStream: invalid data (no byteLength)") // KDBG2 (temporary)
		return 0, fmt.Errorf("network stream returned invalid data")
	}
	n := length.Int()
	if n < 0 || n > len(p) {
		log.Printf("KDBG2 readStream: TOO MUCH DATA n=%d len=%d", n, len(p)) // KDBG2 (temporary)
		return 0, fmt.Errorf("network stream returned too much data")
	}
	if n > 0 {
		js.CopyBytesToGo(p[:n], v)
	}
	c.mu.RLock()
	current := c.generation == gen && c.state == stateConnected
	c.mu.RUnlock()
	if !current {
		log.Printf("KDBG2 readStream: generation stale after read -> ErrPermission") // KDBG2 (temporary)
		return 0, fs.ErrPermission
	}
	log.Printf("KDBG2 readStream: -> n=%d err=nil", n) // KDBG2 (temporary)
	return n, nil
}

func (c *Conn) Write(p []byte) (int, error) {
	c.mu.RLock()
	stream, gen, connected := c.stream, c.generation, c.state == stateConnected
	c.mu.RUnlock()
	return writeStream(stream, gen, c, p, connected)
}

func writeStream(stream js.Value, gen uint64, c *Conn, p []byte, connected bool) (int, error) {
	log.Printf("KDBG2 writeStream: enter id=%s gen=%d len=%d connected=%v", c.id, gen, len(p), connected) // KDBG2 (temporary)
	if !connected || stream.IsUndefined() || stream.IsNull() {
		log.Printf("KDBG2 writeStream: not connected -> ErrPermission") // KDBG2 (temporary)
		return 0, fs.ErrPermission
	}
	buf := js.Global().Get("Uint8Array").New(len(p))
	js.CopyBytesToJS(buf, p)
	// Lane H: a kernel-initiated stream.write through syscall/js is accepted by
	// the provider (count returned, Arti logs the write) but the bytes never
	// reach the wire, while page-driven writes on the same streams deliver.
	// When the page registers __ferruleosWrite, perform the write from that pure
	// JS frame and await its promise instead.
	var promise js.Value
	if seam := js.Global().Get("__ferruleosWrite"); seam.Type() == js.TypeFunction {
		log.Printf("KDBG2 writeStream: __ferruleosWrite seam len=%d", len(p)) // KDBG2 (temporary)
		promise = seam.Invoke(stream, buf)
	} else {
		promise = stream.Call("write", buf)
	}
	v, err := jsutil.AwaitErr(promise)
	log.Printf("KDBG2 writeStream: provider write(%d) -> err=%v v=%v", len(p), err, v) // KDBG2 (temporary)
	if err != nil {
		c.setErrorGeneration(gen, err)
		log.Printf("KDBG2 writeStream: provider error -> 0, %v", err) // KDBG2 (temporary)
		return 0, err
	}
	if v.Type() != js.TypeNumber {
		log.Printf("KDBG2 writeStream: invalid write count type=%v", v.Type()) // KDBG2 (temporary)
		return 0, fmt.Errorf("invalid network write count")
	}
	n := v.Int()
	if n < 0 || n > len(p) {
		log.Printf("KDBG2 writeStream: invalid write count n=%d len=%d", n, len(p)) // KDBG2 (temporary)
		return 0, fmt.Errorf("invalid network write count")
	}
	c.mu.RLock()
	current := c.generation == gen && c.state == stateConnected
	c.mu.RUnlock()
	if !current {
		log.Printf("KDBG2 writeStream: generation stale after write -> ErrPermission") // KDBG2 (temporary)
		return 0, fs.ErrPermission
	}
	log.Printf("KDBG2 writeStream: -> n=%d err=nil", n) // KDBG2 (temporary)
	return n, nil
}

func (c *Conn) Close() error { return c.hangup() }

// hangup invalidates the current attempt immediately. The provider API does not
// accept a cancellation signal, so a pending open is closed when it eventually resolves.
func closeStreamAsync(stream js.Value) {
	if stream.IsUndefined() || stream.IsNull() {
		return
	}
	go func() { _, _ = jsutil.AwaitErr(stream.Call("close")) }()
}

func (c *Conn) hangup() error {
	c.mu.Lock()
	stream := c.stream
	c.stream = js.Undefined()
	c.attempt++
	if c.state != stateClosed {
		c.state = stateIdle
	}
	c.lastErr = nil
	c.mu.Unlock()
	log.Printf("KDBG2 hangup: id=%s gen=%d", c.id, c.generation) // KDBG2 (temporary)
	closeStreamAsync(stream)
	return nil
}

func (c *Conn) dispose(token string) error {
	gen, err := strconv.ParseUint(token, 10, 64)
	if err != nil || strconv.FormatUint(gen, 10) != token {
		return fmt.Errorf("dispose: invalid generation")
	}
	c.mu.Lock()
	current := c.generation
	if gen < current {
		c.mu.Unlock()
		return nil
	}
	if gen > current {
		c.mu.Unlock()
		return fmt.Errorf("dispose: future generation")
	}
	stream := c.stream
	c.stream = js.Undefined()
	c.state = stateClosed
	c.attempt++
	c.mu.Unlock()
	c.svc.remove(c.id)
	closeStreamAsync(stream)
	log.Printf("KDBG2 dispose: id=%s gen=%d -> closed+removed", c.id, gen) // KDBG2 (temporary)
	return nil
}

// dataFile pins every operation to one dial generation. Closing a VFS file handle
// never destroys the allocation; SSH releases it with conditional dispose.
type dataFile struct {
	conn       *Conn
	generation uint64
	stream     js.Value
	dead       bool
	mu         sync.Mutex
}

func (f *dataFile) Stat() (fs.FileInfo, error) { return fskit.Entry("data", 0644), nil }
func (f *dataFile) Close() error               { f.mu.Lock(); f.dead = true; f.mu.Unlock(); return nil }
func (f *dataFile) Read(p []byte) (int, error) {
	log.Printf("KDBG2 dataFile.Read: enter len=%d gen=%d", len(p), f.generation) // KDBG2 (temporary)
	n, err := func() (int, error) {
		f.mu.Lock()
		dead := f.dead
		f.mu.Unlock()
		if dead {
			return 0, fs.ErrPermission
		}
		f.conn.mu.RLock()
		current := f.conn.generation == f.generation && f.conn.state == stateConnected
		f.conn.mu.RUnlock()
		if !current {
			log.Printf("KDBG2 dataFile.Read: stale/dead gen=%d conn.gen=%d state=%s", f.generation, f.conn.generation, f.conn.state) // KDBG2 (temporary)
			return 0, fs.ErrPermission
		}
		return readStream(f.stream, f.generation, f.conn, p, stateConnected)
	}()
	log.Printf("KDBG2 dataFile.Read: exit n=%d err=%v", n, err) // KDBG2 (temporary)
	return n, err
}
func (f *dataFile) Write(p []byte) (int, error) {
	log.Printf("KDBG2 dataFile.Write: enter len=%d gen=%d", len(p), f.generation) // KDBG2 (temporary)
	n, err := func() (int, error) {
		f.mu.Lock()
		dead := f.dead
		f.mu.Unlock()
		if dead {
			return 0, fs.ErrPermission
		}
		f.conn.mu.RLock()
		current := f.conn.generation == f.generation && f.conn.state == stateConnected
		f.conn.mu.RUnlock()
		if !current {
			log.Printf("KDBG2 dataFile.Write: stale/dead gen=%d conn.gen=%d state=%s", f.generation, f.conn.generation, f.conn.state) // KDBG2 (temporary)
			return 0, fs.ErrPermission
		}
		return writeStream(f.stream, f.generation, f.conn, p, true)
	}()
	log.Printf("KDBG2 dataFile.Write: exit n=%d err=%v", n, err) // KDBG2 (temporary)
	return n, err
}

func (c *Conn) setError(err error) {
	c.mu.Lock()
	c.lastErr = err
	if c.state == stateConnected {
		c.state = stateError
	}
	c.mu.Unlock()
}

func (c *Conn) setErrorGeneration(gen uint64, err error) {
	c.mu.Lock()
	if c.generation == gen && c.state == stateConnected {
		c.state = stateError
		c.lastErr = err
		log.Printf("KDBG2 setErrorGeneration: id=%s gen=%d -> stateError err=%v", c.id, gen, err) // KDBG2 (temporary)
	}
	c.mu.Unlock()
}

func (c *Conn) status() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	status := string(c.state)
	if status == "" {
		status = string(stateIdle)
	}
	if c.lastErr != nil {
		status += " err=" + strings.ReplaceAll(c.lastErr.Error(), "\n", " ")
	}
	return status
}

var _ io.ReadWriteCloser = (*Conn)(nil)
