# headless — native Wanix transport contract

`wanix headless` exposes a Wanix root task over **two independent native
duplex transports** so a non-browser host (Fersh) can drive Wanix without a
DOM. It adds no new filesystem or task protocol: it wires Wanix's existing
namespace/task/9P primitives to native streams, reusing the exact seams the
browser path uses.

## Why two transports

The browser reaches Wanix through two separate `MessageChannel`s:

- `_openPort` → the task/API port: `api.Responder` over a `duplex/mux` session
  (CBOR-framed `talk` RPC).
- `_open9P` → a raw 9P stream: `p9.NewServer(p9kit.Attacher(task.NS()))` over a
  full-duplex reader/writer pair.

`headless` mirrors that split. They cannot share one mux session:
`rpc.Server.Respond` accepts every channel on a session and decodes it as a
`talk` RPC call, so a raw 9P channel on the same session is rejected. Two
streams is the minimal faithful port of the browser design.

## What's exposed

| Transport | Address flag | Backed by | Framing |
|---|---|---|---|
| task/API port | `--api <addr>` | `api.Responder(sess, root)` | `duplex/mux` + `talk`/CBOR |
| raw 9P stream | `--9p <addr>` | `p9kit.Attacher(root.NS())` | raw 9P2000.L over a byte stream |

Address schemes (both flags):

- `stdio` — the process stdin/stdout. **At most one** of the two flags may be
  `stdio` (there is only one stdin/stdout pair).
- `unix://<path>` — Unix domain socket listener.
- `tcp://<host:port>` — TCP listener.

Logs always go to **stderr**, leaving stdin/stdout free for a `stdio`
transport.

## Defaults

`wanix headless` defaults to `--api stdio --9p unix://${XDG_RUNTIME_DIR:-/tmp}/wanix-9p-<uid>.sock`.
So a host that spawns `wanix headless` as a subprocess gets the API port over
the child's stdio and 9P over a Unix socket — both transports live, no extra
wiring.

## How a native host connects

### Spawned-subprocess (API over stdio, 9P over socket)

```
host spawns:  wanix headless --api stdio --9p unix:///tmp/wanix-9p.sock
API :  child stdin/stdout is a duplex/mux session; dial talk/CBOR on it.
9P  :  connect a 9P2000.L client to /tmp/wanix-9p.sock (raw, no mux).
```

### Two sockets (no stdio coupling)

```
host spawns:  wanix headless --api unix:///tmp/wanix-api.sock --9p unix:///tmp/wanix-9p.sock
API :  dial /tmp/wanix-api.sock → mux.New(conn) → talk peer.
9P  :  connect a 9P client to /tmp/wanix-9p.sock.
```

### Embedding (reusable Go API, no command)

A host that already owns a duplex connection can serve the primitives directly:

```go
// API over an existing mux session (one per accepted connection)
headless.ServeAPI(sess, root)

// 9P over any io.ReadWriteCloser (one per accepted connection)
headless.Serve9P(ctx, rwc, root)

// Or run both listeners declaratively:
headless.New(headless.Options{APIAddr: "unix:///tmp/api", NinePAddr: "tcp://:0"}).Run(ctx)
```

## Lifecycle / cancellation

- `Server.Run(ctx)` blocks until `ctx` is cancelled or both transports stop.
- On cancellation, listeners close and in-flight connections are closed so
  9P/RPC sessions do not outlive the host.
- The command traps SIGINT/SIGTERM, cancels the context, and exits 0 on clean
  shutdown / 1 on error.

## Namespace

`headless.Bootstrap()` builds a minimal native root namespace: the built-in
`#wanix` and `#task` trees plus `#pipe`, `#signal`, `#ramfs` allocators, and
the host-process exec driver registered as `exec`. This mirrors the browser
bootstrap (`web/wasm.go`), omitting only browser-only bindings (`#web`, `#dom`,
`#js`, …). A host binds further trees over the API/`#task/*/ctl` as needed.

## Product boundary (per epic)

Wanix owns its namespace, task model, GoJS behavior and 9P serving. Fersh
supplies the native sockets/process/stdio. `headless` does not define a new
filesystem/task protocol and puts no VFS behavior into Fersh — it only exposes
the two existing seams over native streams. The browser path is unchanged.
