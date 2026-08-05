//go:build !js && !wasm

// Package headless exposes a Wanix root task over native transports so a
// non-browser host (such as Fersh) can drive Wanix without a DOM.
//
// Wanix owns its namespace, task model and 9P serving; headless only wires
// those primitives to native duplex streams. It does not introduce a new
// filesystem or task protocol — it reuses the exact seams the browser path
// uses:
//
//   - The task/API port: api.Responder over a duplex/mux session (CBOR/talk RPC),
//     the same thing the browser reaches through the _openPort MessageChannel.
//   - The raw 9P stream: p9kit.Attacher(root.NS()) over a full-duplex byte stream,
//     the same thing the browser reaches through the _open9P MessageChannel.
//
// A single mux session cannot carry both: rpc.Server.Respond accepts every
// channel on a session and decodes it as a talk Call, so a raw 9P channel on
// the same session would be rejected. The browser therefore uses two separate
// MessageChannels; headless mirrors that with two separate listeners (or two
// separate stdio roles). See CONTRACT.md for the host wiring.
package headless

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/hugelgupf/p9/p9"
	"tractor.dev/toolkit-go/duplex/mux"
	"tractor.dev/wanix"
	"tractor.dev/wanix/api"
	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/memfs"
	"tractor.dev/wanix/fs/p9kit"
	"tractor.dev/wanix/fs/pipe"
	"tractor.dev/wanix/fs/signal"
	"tractor.dev/wanix/misc/allocfs"
	"tractor.dev/wanix/native"
)

// Options configures a headless Server.
type Options struct {
	// APIAddr is the address of the task/API port (duplex mux/talk over CBOR).
	// Schemes: "stdio" (the process stdin/stdout), "unix://path", "tcp://host:port".
	// Empty disables the API port.
	APIAddr string

	// NinePAddr is the address of the raw full-duplex 9P stream bound to the
	// root task namespace. Same schemes as APIAddr. Empty disables 9P.
	NinePAddr string

	// Root is a pre-built root task. If nil, Server.Run boots a minimal native
	// namespace via Bootstrap.
	Root *wanix.Task

	// Logger receives lifecycle messages. If nil, log.Printf is used.
	Logger *log.Logger

	// GoJSRunner is the packaged `wanix-gojs-runner.mjs` path. When set with a
	// socket API address, GoJS Wasm tasks (including rc.wasm) run through Node
	// and the existing API responder rather than a second native VFS.
	GoJSRunner string

	// NodeBinary selects the Node executable used by GoJSRunner. Empty means
	// `node` from PATH.
	NodeBinary string
}

// Server serves a Wanix root task over the two native transports.
type Server struct {
	APIAddr    string
	NinePAddr  string
	Root       *wanix.Task
	Logger     *log.Logger
	GoJSRunner string
	NodeBinary string

	once      sync.Once
	bootErr   error
	closeOnce sync.Once
}

// New returns a Server from Options.
func New(opts Options) *Server {
	return &Server{
		APIAddr:    opts.APIAddr,
		NinePAddr:  opts.NinePAddr,
		Root:       opts.Root,
		Logger:     opts.Logger,
		GoJSRunner: opts.GoJSRunner,
		NodeBinary: opts.NodeBinary,
	}
}

func (s *Server) logger() *log.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return log.New(os.Stderr, "headless: ", log.LstdFlags|log.Lmsgprefix)
}

// root boots the namespace on first use and caches the result.
func (s *Server) root() (*wanix.Task, error) {
	s.once.Do(func() {
		if s.Root != nil {
			return
		}
		s.Root, s.bootErr = Bootstrap()
		if s.bootErr == nil {
			if s.GoJSRunner != "" && !isStdio(s.APIAddr) {
				s.Root.Register("gojs", &native.GoJSDriver{Node: s.NodeBinary, Runner: s.GoJSRunner, APIAddr: s.APIAddr})
			}
			s.logger().Printf("namespace ready; api=%q 9p=%q", s.APIAddr, s.NinePAddr)
		}
	})
	return s.Root, s.bootErr
}

// Run serves the configured transports until ctx is cancelled or both
// transports stop. At least one of APIAddr or NinePAddr must be set, and at
// most one may be "stdio" (there is only one stdin/stdout pair).
func (s *Server) Run(ctx context.Context) error {
	if s.APIAddr == "" && s.NinePAddr == "" {
		return fmt.Errorf("headless: at least one of APIAddr or NinePAddr must be set")
	}
	if isStdio(s.APIAddr) && isStdio(s.NinePAddr) {
		return fmt.Errorf("headless: APIAddr and NinePAddr cannot both be stdio")
	}
	if _, err := s.root(); err != nil {
		return err
	}

	shutdown, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errc := make(chan error, 2)

	serve := func(addr string, fn func(context.Context, *wanix.Task) error) {
		if addr == "" {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(shutdown, s.Root); err != nil {
				errc <- fmt.Errorf("%s: %w", addr, err)
				cancel()
			}
		}()
	}

	serve(s.APIAddr, s.serveAPI)
	serve(s.NinePAddr, s.serve9P)

	wg.Wait()
	close(errc)
	return firstError(errc)
}

// ServeAPI serves the task/API port on an established mux session. It blocks
// until the session closes. This is the reusable per-connection primitive
// behind APIAddr; a host that already owns a mux session can call it directly.
func ServeAPI(sess mux.Session, task *wanix.Task) {
	api.Responder(sess, task)
}

// Serve9P serves a raw full-duplex 9P stream for task's namespace on rwc. It
// blocks until the stream closes (EOF on read) or ctx is cancelled. This is
// the reusable per-connection primitive behind NinePAddr.
func Serve9P(ctx context.Context, rwc io.ReadWriteCloser, task *wanix.Task) error {
	srv := p9.NewServer(p9kit.Attacher(task.NS(), p9kit.WithMemAttrStore()))
	done := make(chan error, 1)
	go func() {
		// p9.Server.Handle wants a ReadCloser + WriteCloser pair; split rwc
		// without double-closing (closing the read side closes the conn).
		done <- srv.Handle(&readSide{rwc}, &writeSide{rwc})
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = rwc.Close()
		return <-done
	}
}

// --- conn splitting for p9.Server.Handle ---

type readSide struct{ io.ReadWriteCloser }

func (r *readSide) Read(p []byte) (int, error) { return r.ReadWriteCloser.Read(p) }
func (r *readSide) Close() error               { return r.ReadWriteCloser.Close() }

type writeSide struct{ io.ReadWriteCloser }

func (w *writeSide) Write(p []byte) (int, error) { return w.ReadWriteCloser.Write(p) }
func (w *writeSide) Close() error                { return nil } // conn closed via readSide

// --- address handling ---

type addrKind int

const (
	kindTCP addrKind = iota
	kindUnix
	kindStdio
)

func isStdio(addr string) bool {
	k, _, _, err := parseAddr(addr)
	return err == nil && k == kindStdio
}

func parseAddr(addr string) (kind addrKind, network, address string, err error) {
	switch {
	case addr == "" || addr == "stdio":
		return kindStdio, "", "", nil
	case strings.HasPrefix(addr, "stdio:"):
		return kindStdio, "", "", nil
	case strings.HasPrefix(addr, "tcp://"):
		return kindTCP, "tcp", strings.TrimPrefix(addr, "tcp://"), nil
	case strings.HasPrefix(addr, "tcp:"):
		return kindTCP, "tcp", strings.TrimPrefix(addr, "tcp:"), nil
	case strings.HasPrefix(addr, "unix://"):
		return kindUnix, "unix", strings.TrimPrefix(addr, "unix://"), nil
	case strings.HasPrefix(addr, "unix:"):
		return kindUnix, "unix", strings.TrimPrefix(addr, "unix:"), nil
	}
	return 0, "", "", fmt.Errorf("headless: unsupported address %q (want stdio, unix://path, or tcp://host:port)", addr)
}

// stdioConn exposes the process stdin/stdout as a single io.ReadWriteCloser.
type stdioConn struct {
	in  *os.File
	out *os.File
}

func (c *stdioConn) Read(p []byte) (int, error)  { return c.in.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.out.Write(p) }
func (c *stdioConn) Close() error {
	// Closing real stdin/stdout would poison the host; closing is driven by
	// EOF on read. This is a no-op so Handle's Close is harmless.
	return nil
}

// acceptLoop wraps a listener and a per-connection handler with graceful
// shutdown tied to ctx. Conns opened after shutdown begins are tracked and
// closed so in-flight 9P/RPC sessions don't outlive the server.
type connServer struct {
	logger  *log.Logger
	ln      net.Listener
	handle  func(context.Context, net.Conn)
	wg      sync.WaitGroup
	conns   map[net.Conn]struct{}
	connsMu sync.Mutex
}

func (cs *connServer) serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = cs.ln.Close()
		cs.closeConns()
	}()
	for {
		c, err := cs.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				cs.wg.Wait()
				return nil
			}
			return err
		}
		cs.trackConn(c, true)
		cs.wg.Add(1)
		go func(c net.Conn) {
			defer cs.wg.Done()
			defer cs.trackConn(c, false)
			defer c.Close()
			cs.handle(ctx, c)
		}(c)
	}
}

func (cs *connServer) trackConn(c net.Conn, add bool) {
	cs.connsMu.Lock()
	defer cs.connsMu.Unlock()
	if cs.conns == nil {
		cs.conns = make(map[net.Conn]struct{})
	}
	if add {
		cs.conns[c] = struct{}{}
	} else {
		delete(cs.conns, c)
	}
}

func (cs *connServer) closeConns() {
	cs.connsMu.Lock()
	defer cs.connsMu.Unlock()
	for c := range cs.conns {
		_ = c.Close()
	}
}

// serveAPI implements the APIAddr transport (stdio or listener).
func (s *Server) serveAPI(ctx context.Context, task *wanix.Task) error {
	kind, network, address, err := parseAddr(s.APIAddr)
	if err != nil {
		return err
	}
	if kind == kindStdio {
		s.logger().Printf("api: serving on stdio")
		c := &stdioConn{in: os.Stdin, out: os.Stdout}
		done := make(chan struct{})
		go func() { ServeAPI(mux.New(c), task); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
		}
		return nil
	}
	ln, err := net.Listen(network, address)
	if err != nil {
		return err
	}
	s.logger().Printf("api: listening on %s://%s", network, ln.Addr())
	cs := &connServer{logger: s.logger(), ln: ln, handle: func(ctx context.Context, c net.Conn) {
		ServeAPI(mux.New(c), task)
	}}
	return cs.serve(ctx)
}

// serve9P implements the NinePAddr transport (stdio or listener).
func (s *Server) serve9P(ctx context.Context, task *wanix.Task) error {
	kind, network, address, err := parseAddr(s.NinePAddr)
	if err != nil {
		return err
	}
	if kind == kindStdio {
		s.logger().Printf("9p: serving on stdio")
		c := &stdioConn{in: os.Stdin, out: os.Stdout}
		return Serve9P(ctx, c, task)
	}
	ln, err := net.Listen(network, address)
	if err != nil {
		return err
	}
	s.logger().Printf("9p: listening on %s://%s", network, ln.Addr())
	cs := &connServer{logger: s.logger(), ln: ln, handle: func(ctx context.Context, c net.Conn) {
		if err := Serve9P(ctx, c, task); err != nil {
			s.logger().Printf("9p: session ended: %v", err)
		}
	}}
	return cs.serve(ctx)
}

// Bootstrap creates a root task with a minimal native namespace: the built-in
// #wanix and #task trees, plus #pipe, #signal and #ramfs allocators, and the
// host-process exec driver registered as "exec". A native host can bind
// further trees over the API/ctl. This deliberately mirrors the browser
// bootstrap in web/wasm, omitting only browser-only bindings.
func Bootstrap() (*wanix.Task, error) {
	root, err := wanix.NewRoot()
	if err != nil {
		return nil, err
	}
	binds := []struct {
		dst  string
		fsys fs.FS
	}{
		{"#pipe", &pipe.Allocator{}},
		{"#signal", &signal.Allocator{}},
	}
	for _, b := range binds {
		if err := root.NS().Bind(b.fsys, ".", b.dst); err != nil {
			return nil, fmt.Errorf("bind %s: %w", b.dst, err)
		}
	}
	ramfs := allocfs.New(func(context.Context, string, map[string]string) (fs.FS, error) {
		return memfs.New(), nil
	})
	if err := root.NS().Bind(ramfs, ".", "#ramfs"); err != nil {
		return nil, fmt.Errorf("bind #ramfs: %w", err)
	}
	root.Register("exec", &native.ExecDriver{})
	return root, nil
}

func firstError(errc <-chan error) error {
	for e := range errc {
		if e != nil {
			return e
		}
	}
	return nil
}
