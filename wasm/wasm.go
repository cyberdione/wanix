//go:build js && wasm

package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"path"
	"strconv"
	"strings"
	"syscall/js"
	"time"

	"github.com/hugelgupf/p9/p9"
	"tractor.dev/toolkit-go/duplex/mux"
	"tractor.dev/wanix"
	"tractor.dev/wanix/api"
	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/cowfs"
	"tractor.dev/wanix/fs/httpfs"
	"tractor.dev/wanix/fs/memfs"
	"tractor.dev/wanix/fs/net/tcp"
	"tractor.dev/wanix/fs/p9kit"
	"tractor.dev/wanix/fs/pipe"
	"tractor.dev/wanix/fs/signal"
	"tractor.dev/wanix/fs/tarfs"
	"tractor.dev/wanix/misc"
	"tractor.dev/wanix/misc/allocfs"
	"tractor.dev/wanix/misc/jsutil"
	"tractor.dev/wanix/term"
	"tractor.dev/wanix/vm"
	"tractor.dev/wanix/web"
	"tractor.dev/wanix/web/idbfs"
	"tractor.dev/wanix/web/jsfs"
	"tractor.dev/wanix/web/sys"
	"tractor.dev/wanix/web/worker"
)

func main() {
	log.SetFlags(log.Lshortfile)

	root, err := wanix.NewRoot()
	if err != nil {
		log.Fatal(err)
	}

	tcpServices := make(map[string]fs.FS)
	networkObservers := make(map[string]*networkObserver)
	networkGrantGeneration := 0
	sysbindings := []struct {
		dst  string
		fsys fs.FS
	}{
		{"#term", term.New(root)},
		{"#web", web.New(root)},
		{"#vm", vm.New(root)},
		{"#pipe", &pipe.Allocator{}},
		{"#signal", &signal.Allocator{}},
		{"#wminput", &signal.QueueAllocator{MaxBytes: 32 * 1024, MaxQueues: 64}},
		// {"#ramfs", &memfs.Allocator{}},
		{"#js", jsfs.NewFS(js.Global())},
	}
	for _, b := range sysbindings {
		if err := root.NS().Bind(b.fsys, ".", b.dst); err != nil {
			log.Fatal(err)
		}
	}

	ramfs := allocfs.New(func(ctx context.Context, id string, opts map[string]string) (fs.FS, error) {
		return memfs.New(), nil
	})
	if err := root.NS().Bind(ramfs, ".", "#ramfs"); err != nil {
		log.Fatal(err)
	}

	httpfs := allocfs.New(func(ctx context.Context, id string, opts map[string]string) (fs.FS, error) {
		u, err := url.Parse(opts["url"])
		if err != nil {
			return nil, err
		}
		if u.Scheme == "" {
			return nil, fmt.Errorf("url is required")
		}
		if t, ok := opts["token"]; ok {
			q := u.Query()
			q.Set("token", t)
			u.RawQuery = q.Encode()
		}
		fmt.Println("url", u.String())
		hfs := httpfs.New(u.String(), nil)
		if _, err := hfs.Stat("."); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			if err := fs.Mkdir(hfs, ".", 0755); err != nil && !errors.Is(err, fs.ErrExist) {
				if _, statErr := hfs.Stat("."); statErr != nil {
					return nil, err
				}
			}
		}
		_, err = hfs.ReadDir(".")
		if err != nil {
			return nil, err
		}
		return hfs, nil
	})
	if err := root.NS().Bind(httpfs, ".", "#httpfs"); err != nil {
		log.Fatal(err)
	}

	idbfs := allocfs.New(func(ctx context.Context, id string, opts map[string]string) (fs.FS, error) {
		n, ok := opts["name"]
		if !ok {
			return nil, fmt.Errorf("name is required")
		}
		return idbfs.New(n), nil
	})
	if err := root.NS().Bind(idbfs, ".", "#idbfs"); err != nil {
		log.Fatal(err)
	}

	cwfs := allocfs.New(func(ctx context.Context, id string, opts map[string]string) (fs.FS, error) {
		originfs, _, ok := fs.Origin(ctx)
		if !ok {
			return nil, fmt.Errorf("no origin in context")
		}
		b, ok := opts["base"]
		if !ok {
			return nil, fmt.Errorf("base is required")
		}
		bfsys, err := fs.Sub(originfs, b)
		if err != nil {
			return nil, err
		}

		o, ok := opts["overlay"]
		if !ok {
			return nil, fmt.Errorf("overlay is required")
		}
		ofsys, err := fs.Sub(originfs, o)
		if err != nil {
			return nil, err
		}

		return &cowfs.FS{Base: bfsys, Overlay: ofsys}, nil
	})
	if err := root.NS().Bind(cwfs, ".", "#cowfs"); err != nil {
		log.Fatal(err)
	}

	el := sys.Element()
	el.Set("_openPort", js.FuncOf(func(this js.Value, args []js.Value) any {
		ch := js.Global().Get("MessageChannel").New()
		port := el.Call("_portWrap", ch.Get("port1"))
		wr := &jsutil.Writer{Value: port}
		rd := &jsutil.Reader{Value: port}
		sess, err := mux.DialIO(wr, rd)
		if err != nil {
			log.Fatal(err)
		}

		go func(sess mux.Session, args []js.Value) {
			task := root
			if len(args) > 0 {
				t, err := root.Lookup(args[0].String())
				if err == nil {
					task = t
				}
			}
			api.Responder(sess, task)
		}(sess, args)

		return ch.Get("port2")
	}))

	el.Set("_grantNetwork", js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) < 1 || len(args) > 2 || args[0].Type() != js.TypeString {
			return false
		}
		taskID := args[0].String()
		task, err := root.Lookup(taskID)
		if err != nil || task == root {
			return false
		}
		if _, exists := tcpServices[taskID]; exists {
			return false
		}
		callback := js.Undefined()
		if len(args) == 2 {
			if args[1].Type() != js.TypeObject || args[1].IsNull() {
				return false
			}
			callback = args[1].Get("onConnection")
			if !callback.IsUndefined() && callback.Type() != js.TypeFunction {
				return false
			}
		}
		networkGrantGeneration++
		observer := newNetworkObserver(taskID, networkGrantGeneration, callback)
		var tcpService fs.FS
		if pageSvc := js.Global().Get("__ferruleosTcpService"); pageTCPCapable(pageSvc) {
			// Page-owned service: all provider I/O happens in the page's pump
			// loops; the kernel only composes the namespace (jsfs + data
			// adapter). Prefer the factory so each grant gets an isolated
			// conn-id space, like the tinygo per-grant service.
			factory := pageSvc.Get("create")
			if factory.Type() != js.TypeFunction {
				observer.invalidate()
				observer.receive.Release()
				return false
			}
			made := factory.Invoke(map[string]any{"onConnection": observer.receive})
			if !pageTCPCapable(made) {
				observer.invalidate()
				return false
			}
			pageSvc = made
			tcpService = newPageTCPService(pageSvc)
		} else {
			if callback.Type() == js.TypeFunction {
				observer.invalidate()
				observer.receive.Release()
				return false
			}
			tcpService = tcp.New()
		}
		if err := grantNetwork(root, tcpService, taskID); err != nil {
			observer.invalidate()
			if closer, ok := tcpService.(interface{ CloseAll() }); ok {
				closer.CloseAll()
			}
			return false
		}
		tcpServices[taskID] = tcpService
		networkObservers[taskID] = observer
		task.AddCleanup(func() {
			// A later grant must never be revoked by an older grant's cleanup.
			if networkObservers[taskID] == observer {
				el.Call("_revokeNetwork", taskID)
			}
		})
		return true
	}))

	el.Set("_revokeNetwork", js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) != 1 || args[0].Type() != js.TypeString {
			return false
		}
		taskID := args[0].String()
		tcpService, ok := tcpServices[taskID]
		if !ok {
			return false
		}
		if err := revokeNetwork(root, tcpService, taskID); err != nil {
			if _, lookupErr := root.Lookup(taskID); lookupErr == nil {
				return false
			}
		}
		if observer := networkObservers[taskID]; observer != nil {
			observer.invalidate()
			if page, ok := tcpService.(*pageTCPService); ok {
				if detach := page.svc.Get("clearObserver"); detach.Type() == js.TypeFunction {
					detach.Invoke()
					observer.receive.Release()
				}
			} else {
				observer.receive.Release()
			}
			delete(networkObservers, taskID)
		}
		if closer, ok := tcpService.(interface{ CloseAll() }); ok {
			closer.CloseAll()
		}
		delete(tcpServices, taskID)
		return true
	}))

	el.Set("_networkConnections", js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) != 1 || args[0].Type() != js.TypeString {
			return []any{}
		}
		if observer := networkObservers[args[0].String()]; observer != nil {
			return observer.snapshot()
		}
		return []any{}
	}))

	el.Set("_open9P", js.FuncOf(func(this js.Value, args []js.Value) any {
		ch := NewP9Channel()

		go func() {
			task := root
			if len(args) > 0 {
				t, err := root.Lookup(args[0].String())
				if err == nil {
					task = t
				}
			}
			var o []p9.ServerOpt
			// o = append(o, p9.WithServerLogger(ulog.Log))
			srv := p9.NewServer(p9kit.Attacher(task.NS(), p9kit.WithMemAttrStore()), o...)
			if err := srv.Handle(ch.Reader(), ch.Writer()); err != nil {
				log.Fatal(err)
			}
		}()

		return ch.Port()
	}))

	el.Set("_updateTerminals", js.FuncOf(func(this js.Value, args []js.Value) any {
		// termEl := args[0]
		for _, task := range root.Tasks() {
			w := worker.FromTask(task)
			if w.IsUndefined() {
				continue
			}
			w.Call("postMessage", map[string]any{
				"screen": "foobar",
			})
		}
		return js.Undefined()
	}))

	el.Set("_setupNamespace", js.FuncOf(func(this js.Value, args []js.Value) any {
		taskID := args[0].String()
		baseFS := args[1].String()
		bindings := jsutil.ToSlice(args[2])
		task, err := root.Lookup(taskID)
		if err != nil {
			log.Fatal(err)
		}
		var resolve, reject js.Value
		promise := js.Global().Get("Promise").New(js.FuncOf(func(this js.Value, args []js.Value) any {
			resolve = args[0]
			reject = args[1]
			return js.Undefined()
		}))
		go func() {
			defer func() {
				if r := recover(); r != nil {
					reject.Invoke(js.ValueOf(r))
				}
			}()
			if taskID != "1" { // leave root namespace alone
				_ = baseFS
				// if err := task.NS().UnbindAll(); err != nil {
				// 	log.Println("error unbinding namespace", err)
				// 	return
				// }
				// if baseFS != "" && baseFS != "<null>" { // todo: better way to handle <null> upstream?
				// 	if err := task.NS().Bind(root.NS(), baseFS, "."); err != nil {
				// 		log.Println("error binding base filesystem", err, "baseFS", baseFS, "taskID", taskID)
				// 		return
				// 	}
				// }
			}

			// these bindings are set up in the bind custom element js
			for _, binding := range bindings {
				dst := binding.Get("dst").String()
				typ := binding.Get("type").String()
				var src string
				if !binding.Get("src").IsNull() {
					src = binding.Get("src").String()
				}
				// union := binding.Get("union").String()

				optsObj := binding.Get("opts")
				var opts []fs.BindOption
				if optsObj.Truthy() && optsObj.InstanceOf(js.Global().Get("Object")) {
					keys := js.Global().Get("Object").Call("keys", optsObj)
					for i := 0; i < keys.Length(); i++ {
						k := strings.TrimPrefix(keys.Index(i).String(), "opt")
						if len(k) > 0 {
							k = strings.ToLower(k[:1]) + k[1:]
						}
						v := optsObj.Get(keys.Index(i).String()).String()
						opts = append(opts, fs.BindOption(fmt.Sprintf("%s=%s", k, v)))

					}
				}

				var perm fs.FileMode
				if !binding.Get("perm").IsUndefined() {
					// Convert mode string (like "644" or "0644") to int
					modeStr := binding.Get("perm").String()
					m, err := strconv.ParseInt(modeStr, 8, 32)
					if err != nil {
						log.Fatalf("invalid file permission %q: %v", modeStr, err)
					}
					perm = fs.FileMode(m)
				}

				switch {
				case typ == "archive":
					v, err := jsutil.AwaitErr(binding.Get("data"))
					if err != nil {
						log.Println("error fetching archive", err)
						return
					}
					archiveFS, err := tarfs.From(tar.NewReader(jsutil.NewReadableStream(v)))
					if err != nil {
						log.Println("error creating archive filesystem", err)
						return
					}
					rwfs := memfs.New()
					// t := time.Now()
					if err := fs.CopyFS(archiveFS, ".", rwfs, "."); err != nil {
						log.Println("error copying archive to memory filesystem", err)
						return
					}
					// log.Println("copied archive to memory filesystem in", time.Since(t))
					if err := task.NS().Bind(rwfs, ".", dst); err != nil {
						log.Println("error binding archive", err)
						return
					}
				case typ == "fetch" || (typ == "file" && src != ""):
					v, err := jsutil.AwaitErr(binding.Get("data"))
					if err != nil {
						log.Println("error fetching", err)
						return
					}
					buf, err := io.ReadAll(jsutil.NewReadableStream(v))
					if err != nil {
						log.Println("error reading fetch", err)
						return
					}
					filefs := memfs.New()
					if err := fs.WriteFile(filefs, path.Base(dst), buf, perm); err != nil {
						log.Println("error writing fetch", err)
						return
					}
					if err := task.NS().Bind(filefs, path.Base(dst), dst); err != nil {
						log.Println("error binding fetch", err)
						return
					}
				case typ == "file" && src == "":
					v, err := jsutil.AwaitErr(binding.Get("data"))
					if err != nil {
						log.Println("error fetching", err)
						return
					}
					buf, err := io.ReadAll(jsutil.NewReadableStream(v))
					if err != nil {
						log.Println("error reading fetch", err)
						return
					}
					if err := fs.WriteFile(task.NS(), dst, buf, perm); err != nil {
						log.Fatalf("error writing file %s: %v", dst, err)
					}
					// TODO: FIX this, why do we have to chmod here? we set mode in writefile!
					if err := fs.Chmod(task.NS(), dst, perm); err != nil {
						log.Println("error chmodding fetch", err)
						return
					}
				case typ == "ns":
					// jsutil.Log("binding ns", src, dst, task.ID())
					if err := task.Bind(src, dst, opts...); err != nil {
						log.Fatal(err)
					}
				case typ == "import":
					t := time.Now()
					v, err := jsutil.AwaitErr(binding.Get("import"))
					if err != nil {
						log.Println("error importing", err)
						return
					}
					conn := misc.NewFakeConn(NewP9PortReadWriter(v))
					fsys, err := p9kit.ClientFS(conn, "")
					if err != nil {
						log.Println("error creating client for import", err)
						return
					}
					if err := task.NS().Bind(fsys, ".", dst); err != nil {
						log.Println("error binding import", err)
						return
					}
					log.Println("imported in", time.Since(t))
				default:
					reject.Invoke(fmt.Errorf("unknown binding type %q", typ))
				}
			}
			resolve.Invoke(js.Undefined())
		}()
		return promise
	}))

	sys.Element().Call("_wasmReady")

	select {}
}
