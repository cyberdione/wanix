import { test } from 'node:test';
import assert from 'node:assert/strict';

globalThis.self = {};
const { runLinux } = await import('./worker.js');

// K2: closing a window while a task is parked in a blocking wsys read (or
// writing the closed window's files) must fail that one call (EIO), not
// kill the personality sync worker. The device contract is fail-closed:
// the slot teardown (srv/ctl close, or the K1 wctl delete) fails parked
// mouse/wctl readers with a closed error and drains queue reads to EOF,
// so the parked fd_read RPC resolves with an error response and later
// writes are rejected outright. CallBuffer.call turns an error response
// into a throw on the sync worker thread; the caller below replays
// exactly that shape — reads deliver until the slot closes, then each
// parked read is answered with the device error, writes on the closed
// slot's files are rejected, and files outside the closed slot keep
// serving.

function slotCaller(closedSlotFds, liveFd) {
    const calls = [];
    const reads = new Map(); // fd -> deliveries so far
    const state = { slotGone: false }; // set when the failed parked read is seen
    return {
        calls,
        state,
        call(method, arg) {
            calls.push([method, arg]);
            switch (method) {
            case 'path_open':
                return 99 + calls.filter(([m]) => m === 'path_open').length;
            case 'fd_read': {
                const n = reads.get(arg.fd) ?? 0;
                reads.set(arg.fd, n + 1);
                if (closedSlotFds.has(arg.fd)) {
                    // one queued record delivers, then the slot closes
                    // while the next read is parked: the device fails it
                    if (n === 0) return new Uint8Array(Math.min(arg.count, 49));
                    state.slotGone = true;
                    throw new Error('wsys: no such window id');
                }
                if (arg.fd === liveFd) {
                    // another window's file keeps serving, then EOF
                    return n < 2 ? new Uint8Array(arg.count) : new Uint8Array(0);
                }
                throw new Error(`unexpected fd_read fd ${arg.fd}`);
            }
            case 'fd_write':
                // writes succeed while the slot lives; once the teardown is
                // observed, the closed slot's files reject writes outright
                if (state.slotGone && closedSlotFds.has(arg.fd)) {
                    throw new Error('wsys: no such window id');
                }
                return arg.data.length;
            case 'path_size':
                // seek's SEEK_END round-trip on the closed slot's file
                if (state.slotGone) throw new Error('wsys: no such window id');
                return 0;
            case 'fd_close':
                if (closedSlotFds.has(arg.fd)) throw new Error('wsys: no such window id');
                return true;
            case 'exit':
                return true;
            default:
                throw new Error(`unexpected method ${method}`);
            }
        },
    };
}

async function withLinuxSyscall(caller, fn, env = []) {
    const compile = WebAssembly.compile, instantiate = WebAssembly.instantiate;
    const memory = new WebAssembly.Memory({ initial: 1 });
    WebAssembly.compile = async () => ({});
    WebAssembly.instantiate = async (_, imports) => ({
        exports: {
            memory,
            _start() { fn(imports['ferrule.linux.syscall'].syscall, memory); },
        },
    });
    try {
        await runLinux({ data: {
            bin: Uint8Array.of(0, 97, 115, 109, 1, 0, 0, 0, 0), args: [], env, tid: 'test',
        } }, caller);
    } finally {
        WebAssembly.compile = compile;
        WebAssembly.instantiate = instantiate;
    }
}

const putPath = (memory, ptr, path) =>
    new Uint8Array(memory.buffer).set(new TextEncoder().encode(path + '\0'), ptr);

test('a slot closed while a window read is parked fails that read with EIO and the worker survives', async () => {
    const caller = slotCaller(new Set([100]), 101);
    await withLinuxSyscall(caller, (syscall, memory) => {
        putPath(memory, 128, 'mnt/wsys/1/mouse');
        const mouse = syscall(56, -100, 128, 2, 0);
        assert.equal(mouse, 3); // the task-side fd; the device fd (100) rides the RPC
        // parked read delivers the queued record
        assert.equal(syscall(63, mouse, 200, 128), 49);
        // the slot closes while the next read is parked: the read reports
        // the failure (-EIO) instead of taking the sync worker down
        assert.equal(syscall(63, mouse, 200, 128), -5);
        // the worker is alive: other open files keep working (another
        // window's cons still serves frames, then EOF)
        putPath(memory, 128, 'mnt/wsys/2/cons');
        const cons = syscall(56, -100, 128, 2, 0);
        assert.equal(cons, 4);
        assert.equal(syscall(63, cons, 400, 16), 16);
        assert.equal(syscall(63, cons, 400, 16), 16);
        assert.equal(syscall(63, cons, 400, 16), 0);
        // the task observes closure and exits cleanly
        syscall(93, 0, 0, 0, 0, 0, 0);
    });
    assert.deepEqual(caller.calls[caller.calls.length - 1], ['exit', { code: 0 }]);
});

test('every parked reader on the closed slot fails with EIO, including readv', async () => {
    // window 1 closes with two readers parked: mouse (100) and one of its
    // two cons readers (101); the other cons reader sits on window 2 (102)
    const caller = slotCaller(new Set([100, 101]), 102);
    await withLinuxSyscall(caller, (syscall, memory) => {
        putPath(memory, 128, 'mnt/wsys/1/mouse');
        const mouse = syscall(56, -100, 128, 2, 0);
        assert.equal(mouse, 3);
        assert.equal(syscall(63, mouse, 200, 128), 49);
        putPath(memory, 128, 'mnt/wsys/1/cons');
        const consClosed = syscall(56, -100, 128, 2, 0);
        assert.equal(consClosed, 4);
        assert.equal(syscall(63, consClosed, 240, 16), 16);
        putPath(memory, 128, 'mnt/wsys/2/cons');
        const consLive = syscall(56, -100, 128, 2, 0);
        assert.equal(consLive, 5);
        assert.equal(syscall(63, consLive, 400, 16), 16);
        // the slot closes: each parked reader sees its own per-read error
        assert.equal(syscall(63, mouse, 200, 128), -5);
        assert.equal(syscall(63, consClosed, 240, 16), -5);
        // readv shares the error path: the failing descriptor reports EIO
        const view = new DataView(memory.buffer);
        view.setUint32(500, 200, true); view.setUint32(504, 8, true);
        view.setUint32(508, 300, true); view.setUint32(512, 8, true);
        assert.equal(syscall(65, mouse, 500, 2), -5);
        // the file outside the closed slot keeps serving
        assert.equal(syscall(63, consLive, 400, 16), 16);
        syscall(93, 0, 0, 0, 0, 0, 0);
    });
    assert.deepEqual(caller.calls[caller.calls.length - 1], ['exit', { code: 0 }]);
});

test('task-exit teardown completes when the closed slot refuses fd_close', async () => {
    const caller = slotCaller(new Set([100]), 101);
    await withLinuxSyscall(caller, (syscall, memory) => {
        putPath(memory, 128, 'mnt/wsys/1/mouse');
        const mouse = syscall(56, -100, 128, 2, 0);
        assert.equal(syscall(63, mouse, 200, 128), 49);
        assert.equal(syscall(63, mouse, 200, 128), -5);
        putPath(memory, 128, 'mnt/wsys/2/cons');
        const cons = syscall(56, -100, 128, 2, 0);
        assert.equal(syscall(63, cons, 400, 16), 16);
        // exit with both descriptors open: the dead slot's close fails,
        // teardown must still finish and report the task's own exit code
        syscall(93, 0, 0, 0, 0, 0, 0);
    });
    assert.deepEqual(caller.calls[caller.calls.length - 1], ['exit', { code: 0 }]);
    // both descriptors were released through the boundary, in open order
    assert.deepEqual(caller.calls.filter(([m]) => m === 'fd_close').map(([, a]) => a.fd), [100, 101]);
});

test('a write to the closed slot fails with EIO and the worker survives', async () => {
    const caller = slotCaller(new Set([100]), 101);
    await withLinuxSyscall(caller, (syscall, memory) => {
        putPath(memory, 128, 'mnt/wsys/1/cons');
        const cons = syscall(56, -100, 128, 2, 0);
        assert.equal(cons, 3);
        const mem = new Uint8Array(memory.buffer);
        // the task's console output flows while the slot lives
        mem.set(new TextEncoder().encode('hello\n'), 200);
        assert.equal(syscall(64, cons, 200, 6), 6);
        // the parked read observes the teardown (and with it the slot state)
        assert.equal(syscall(63, cons, 400, 16), 16);
        assert.equal(syscall(63, cons, 400, 16), -5);
        // output written after the close reports EIO instead of trapping
        mem.set(new TextEncoder().encode('late\n'), 200);
        assert.equal(syscall(64, cons, 200, 5), -5);
        // another window's file still accepts the task's output
        putPath(memory, 128, 'mnt/wsys/2/cons');
        const live = syscall(56, -100, 128, 2, 0);
        assert.equal(live, 4);
        mem.set(new TextEncoder().encode('ok\n'), 200);
        assert.equal(syscall(64, live, 200, 3), 3);
        syscall(93, 0, 0, 0, 0, 0, 0);
    });
    assert.deepEqual(caller.calls[caller.calls.length - 1], ['exit', { code: 0 }]);
});

test('writev to the closed slot fails with EIO; the live file scatters normally', async () => {
    const caller = slotCaller(new Set([100]), 101);
    await withLinuxSyscall(caller, (syscall, memory) => {
        putPath(memory, 128, 'mnt/wsys/1/mouse');
        const mouse = syscall(56, -100, 128, 2, 0);
        assert.equal(syscall(63, mouse, 200, 128), 49);
        assert.equal(syscall(63, mouse, 200, 128), -5);
        // two iovecs against the closed slot's fd
        const view = new DataView(memory.buffer);
        view.setUint32(500, 200, true); view.setUint32(504, 3, true);
        view.setUint32(508, 210, true); view.setUint32(512, 2, true);
        assert.equal(syscall(66, mouse, 500, 2), -5);
        // the same writev shape succeeds on the live window's cons
        putPath(memory, 128, 'mnt/wsys/2/cons');
        const live = syscall(56, -100, 128, 2, 0);
        assert.equal(live, 4);
        assert.equal(syscall(66, live, 500, 2), 5);
        syscall(93, 0, 0, 0, 0, 0, 0);
    });
    assert.deepEqual(caller.calls[caller.calls.length - 1], ['exit', { code: 0 }]);
});

test('seek and close through the failed slot report per-call errors, not a trap', async () => {
    const caller = slotCaller(new Set([100]), 101);
    await withLinuxSyscall(caller, (syscall, memory) => {
        putPath(memory, 128, 'mnt/wsys/1/mouse');
        const mouse = syscall(56, -100, 128, 2, 0);
        assert.equal(syscall(63, mouse, 200, 128), 49);
        assert.equal(syscall(63, mouse, 200, 128), -5);
        // SEEK_END on the closed slot's file needs a device round-trip
        assert.equal(syscall(62, mouse, 0, 0, 600, 2), -5);
        // close frees the descriptor even when the device reports the error
        assert.equal(syscall(57, mouse, 0, 0, 0, 0), 0);
        assert.equal(syscall(57, mouse, 0, 0, 0, 0), -9); // really gone
        // the surviving file keeps serving and closing cleanly
        putPath(memory, 128, 'mnt/wsys/2/cons');
        const live = syscall(56, -100, 128, 2, 0);
        assert.equal(syscall(63, live, 400, 16), 16);
        syscall(93, 0, 0, 0, 0, 0, 0);
    });
    assert.deepEqual(caller.calls[caller.calls.length - 1], ['exit', { code: 0 }]);
    // only the live descriptor was released through the boundary (the dead
    // slot's was released by the explicit close above)
    assert.deepEqual(caller.calls.filter(([m]) => m === 'fd_close').map(([, a]) => a.fd), [100, 101]);
});
