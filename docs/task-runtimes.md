# Task runtimes: design and status

Date: 2026-11-02 (lane J). Status: implemented (gojs lane live; SMP lane spiked and deferred with evidence).

## Problem

Standard-Go `GOOS=wasip1` tasks cannot run concurrent goroutines that do I/O. Every
WASI syscall is a synchronous import; the wasi worker bridges them through a blocking
CallBuffer (`Atomics.wait` on a single 16 KiB SharedArrayBuffer slot). While one
goroutine sits inside a syscall the entire wasm instance is frozen — reads, writes and
timers alike (`clock_wait` blocks the same way). Proven live: task-ssh completed SSH
KEX and host-key verification, then gossh's concurrent readLoop + auth writer
deadlocked deterministically (the SERVICE_REQUEST write never ran while a read parked).

## Lane map

| Lane | Runtime | Scheduling | Status |
|------|---------|-----------|--------|
| wasi | standard Go / Rust / C `wasi_snapshot_preview1` | none — blocking syscalls on one thread | production (sequential tasks: dialer, files, pager) |
| **gojs** | standard Go `GOOS=js` (wasm_exec ABI) | event-driven — promise-backed syscalls park the calling goroutine, the scheduler runs others | **production (task-ssh)** |
| smp-wasi | `wasi_thread_start` host: N workers sharing one linear memory ("vCPU box") | guest runtime schedules across real wasm threads | **deferred — see spike result** |
| bridge | embedder runtimes (pyodide/emscripten guests) with `wanixfs`-style promise bridge | the guest's own | future |

The kernel auto-routes: `misc/wasmutil.DetectType` sniffs the wasm import section
("gojs" vs "wasi_snapshot_preview1") and `#task/new/auto` picks the driver. Task
launchers should allocate via `#task/new/auto` (ssh's runSSH now does).

## SMP spike result (2026-11-02, falsified for Go)

Go 1.25's wasip1 runtime has no thread support: `runtime/lock_wasip1.go` states
"wasm has no support for threads yet. There is no preemption." and its lock primitives
`throw("notesleep not supported by wasi")`. No `wasi_thread_start` import is emitted
(default or `GOEXPERIMENT=wasithreads`, which is unknown to this toolchain), and the
module exports non-shared memory, so cross-worker attachment is impossible anyway.
Conclusion: the SMP lane stays in the design for C (wasi-sdk `-pthread` via
wasi-libc) and future Go runtimes, but cannot host Go guests today. Evidence:
`/home/birdetta/tmpdir/smpspike` on hypr1 (binary + import scan).

The SMP host design, when it becomes viable (Xen-paravirt shaped, kept for reference):
- one shared linear memory (SharedArrayBuffer) across N workers = vCPUs;
- `wasi_thread_start` harness: new worker, same module + memory, jump to entry;
- multi-slot ring CallBuffer with per-slot cancel words (hypercall-page pattern;
  cancellation is the piece Xen never had and our `stop()` needs);
- guest runtimes schedule themselves; the host never schedules inside a guest.
Browser constraint: cross-worker shared memory needs COOP/COEP (already required).

## gojs lane (implemented)

The kernel has always carried a gojs driver (`gojs/driver.go` + `gojs/worker/worker.js`
with an inlined go1.25 wasm_exec and a promise-backed `globalThis.fs` over the task's
RPC port). What was missing: nothing in-tree used it for real protocol clients, and
task launchers hard-allocated `#task/new/wasi`. Changes:

- task-ssh builds `GOOS=js` (build tags `wasip1 || (js && wasm)`; sources unchanged —
  the os bridge makes `os.*` work over the task namespace), 6.9 MB, imports only "gojs";
- runSSH allocates `#task/new/auto` and writes the `#task/<rid>/args` file the gojs
  worker reads (cmd + args);
- stdio is unchanged: wasm_exec `fs.write(fd)` → task fd N → the bound `#term` device;
- network is unchanged: the granted page-owned `net/tcp` service; the kernel write seam
  applies as before.

Validated live (hypr1, real Tor over Snowflake, ssh.github.com:443, strict ECDSA pin):
full KEX, NEWKEYS both ways, then the previously-impossible phase — SERVICE_REQUEST
(52 B write), server EXT_INFO/SERVICE_ACCEPT (498/98/52 B reads), userauth "none"
attempt (68 B write). The wasi lane deadlocked exactly here; the gojs lane proceeds.
Remaining live flakiness is the Snowflake provider (single-peer churn — see below),
not the task runtime; a 90 s handshake watchdog in task-ssh now fails honestly when
the transport stalls.

## Scheduler-facing task API (house rules)

Plan 9 shaped, file-only, no POSIX signals:
- `#task/<rid>/ctl` — `start`, `stop` (stop = per-slot/per-stream cancel);
- `#task/<rid>/exit`, `env`, `cmd`, `args`, `ns` — lifecycle;
- vCPU count (future, SMP lane): `#task/cpus` / env (`GOMAXPROCS` read natively);
- asynchronous interruption, if ever needed: Plan 9 notes (a `note` file), not signals.

## Known follow-ups

- Snowflake provider: `task-snowflake/main.go` now requests Max:3 peers (arti builds
  preemptive exit circuits for 22/80/443; one WebRTC peer churns under that load).
  Headless-firefox reliability remains poor ("No messages received for 20s" churn);
  real-browser verification needed. The vendored snowflake-wasm fork is dirty
  (client/lib/snowflake.go + nat_policy_test.go) — needs a local fork remote.
- wasm_exec fs bridge: no `readFile`-style helpers are exposed to gojs tasks beyond
  `os.*`; a `wanixfs` global is unnecessary today but would be the pyodide lane's shape.
- CallBuffer: single-slot today; multi-slot + cancel belongs to the SMP lane.
- The fork (pinned x/crypto at ~/tor/onion-clean-review-20261021) is instrumented-free
  and back to its committed state.
