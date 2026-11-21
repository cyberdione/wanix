import { test } from 'node:test';
import assert from 'node:assert/strict';

globalThis.self = {};
const { runLinux } = await import('./worker.js');

// K4: the visibility contract for a saturated wsys cons queue. The device
// is the arbiter of acceptance; when it rejects a cons write (the
// ErrSaturated shape, or any device error such as a slot teardown), the
// rejection must be visible to the task as -EIO: the handler answers with
// the error, CallBuffer raises it on the sync side, and the syscall
// boundary maps it to EIO. Nothing on this path may fabricate a success
// count — an unqueued success is the silent write loss K4 exists to rule
// out. With the kernel-side fix the saturation error stops firing under
// mere load (cons writes block until the host drains); it remains the
// contract for real device errors (window teardown).

function saturatedCaller() {
    const calls = [];
    return {
        calls,
        call(method, arg) {
            calls.push([method, arg]);
            switch (method) {
            case 'path_open':
                return 100 + calls.filter(([m]) => m === 'path_open').length;
            case 'fd_write':
                throw new Error('signal: queue saturated');
            case 'path_size':
                return 0;
            case 'fd_close':
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

test('a rejected cons write surfaces as -EIO at the syscall, never as a fabricated success', async () => {
    const caller = saturatedCaller();
    await withLinuxSyscall(caller, (syscall, memory) => {
        putPath(memory, 128, 'mnt/wsys/1/cons');
        const cons = syscall(56, -100, 128, 2, 0);
        assert.equal(cons, 3); // the task-side fd; the device fd rides the RPC

        // the device rejects the write: the task sees EIO, the exact
        // payload was offered to the device exactly once (positioned,
        // like every openat'd file in this personality)
        putPath(memory, 256, 'x');
        const payload = new Uint8Array(memory.buffer, 256, 11);
        payload.set(new TextEncoder().encode('hello tail\n'));
        assert.equal(syscall(64, cons, 256, 11), -5);
        const writes = caller.calls.filter(([m]) => m === 'fd_write');
        assert.equal(writes.length, 1);
        assert.deepEqual(Array.from(writes[0][1].data), Array.from(new TextEncoder().encode('hello tail\n')));
        assert.equal(writes[0][1].at, 0);

        // the boundary survives: the task may retry, the worker is alive
        assert.equal(syscall(64, cons, 256, 11), -5);
        assert.equal(caller.calls.filter(([m]) => m === 'fd_write').length, 2);

        // gathered writes take the same boundary
        const iov = new DataView(memory.buffer);
        iov.setUint32(512, 256, true); iov.setUint32(516, 11, true);
        iov.setUint32(520, 300, true); iov.setUint32(524, 5, true);
        assert.equal(syscall(66, cons, 512, 2), -5);

        // a succeeding write reports exactly the device's count
        caller.call = (() => {
            const base = saturatedCaller().call;
            return function (method, arg) {
                if (method === 'fd_write') return arg.data.length;
                return base.call(this, method, arg);
            };
        })();
        assert.equal(syscall(64, cons, 256, 11), 11);
    });
});
