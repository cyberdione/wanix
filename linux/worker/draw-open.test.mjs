import { test } from 'node:test';
import assert from 'node:assert/strict';

globalThis.self = {};
const { runLinux } = await import('./worker.js');

// /dev/draw device-file opens through the personality openat: the driver
// binds #i after dev (plan 9 bind -a #i /dev) and this allowlist admits
// exactly the device files that binding serves. Device opens carry
// server-side semantics — opening "new" allocates a draw client — so unlike
// scratch opens there is no path_touch creation; read/write use the same
// positioned fd_read/fd_write RPC scratch files use (FileDescription).

function makeCaller() {
    const calls = [];
    let nextFd = 100;
    return {
        calls,
        call(method, arg) {
            calls.push([method, arg]);
            if (method === 'path_open') return nextFd++;
            if (method === 'fd_read') return new Uint8Array(arg.count);
            if (method === 'fd_write') return arg.data.length;
            return true;
        },
    };
}

async function withLinuxSyscall(caller, fn) {
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
            bin: Uint8Array.of(0, 97, 115, 109, 1, 0, 0, 0, 0), args: [], env: [], tid: 'test',
        } }, caller);
    } finally {
        WebAssembly.compile = compile;
        WebAssembly.instantiate = instantiate;
    }
}

const putPath = (memory, ptr, path) =>
    new Uint8Array(memory.buffer).set(new TextEncoder().encode(path + '\0'), ptr);

test('openat admits the draw device files and maps them into the task namespace', async () => {
    const caller = makeCaller();
    await withLinuxSyscall(caller, (syscall, memory) => {
        for (const path of [
            'dev/draw/new', '/dev/draw/new', 'dev/draw/1/ctl',
            'dev/draw/1/data', 'dev/draw/1/refresh', 'dev/draw/1/colormap',
        ]) {
            putPath(memory, 128, path);
            const fd = syscall(56, -100, 128, 2, 0);
            assert.ok(fd >= 3, `${path} openat fd ${fd}`);
        }
    });
    const opens = caller.calls.filter(([m]) => m === 'path_open').map(([, a]) => a.path);
    assert.deepEqual(opens, [
        'dev/draw/new', 'dev/draw/new', 'dev/draw/1/ctl',
        'dev/draw/1/data', 'dev/draw/1/refresh', 'dev/draw/1/colormap',
    ]);
    // device opens never create scratch entries
    assert.equal(caller.calls.filter(([m]) => m === 'path_touch').length, 0);
});

test('openat rejects every other dev path and malformed draw paths', async () => {
    const caller = makeCaller();
    await withLinuxSyscall(caller, (syscall, memory) => {
        for (const path of [
            'dev', 'dev/draw', 'dev/draw/', 'dev/draw/new/x', 'dev/draw/ctl',
            'dev/draw/1/nope', 'dev/draw/x/data', 'dev/draw/1/data/extra',
            'dev/draw/-1/data', 'dev/console', 'dev/draw/new/../new',
            'dev/../dev/draw/new', 'etc/passwd',
        ]) {
            putPath(memory, 128, path);
            assert.equal(syscall(56, -100, 128, 2, 0), -13, `${path} must be denied`);
        }
    });
    // nothing but the final exit crossed the boundary
    assert.deepEqual(caller.calls, [['exit', { code: 0 }]]);
});

test('a draw fd reads and writes positioned through the shared description', async () => {
    const caller = makeCaller();
    await withLinuxSyscall(caller, (syscall, memory) => {
        putPath(memory, 128, 'dev/draw/1/data');
        const fd = syscall(56, -100, 128, 2, 0);
        const mem = new Uint8Array(memory.buffer);
        mem.set([9, 8, 7], 200);
        assert.equal(syscall(64, fd, 200, 3), 3);          // write at 0
        assert.equal(syscall(64, fd, 200, 2), 2);          // write at 3
        assert.equal(syscall(63, fd, 300, 1), 1);          // read at 5
        const seq = caller.calls.filter(([m]) => m === 'fd_write' || m === 'fd_read');
        assert.deepEqual(seq.map(([m, a]) => [m, a.at]), [['fd_write', 0], ['fd_write', 3], ['fd_read', 5]]);
    });
});

test('statx reports draw files as character devices without a size round-trip', async () => {
    const caller = makeCaller();
    await withLinuxSyscall(caller, (syscall, memory) => {
        putPath(memory, 128, 'dev/draw/new');
        // the minimal shim writes the caller's 4th arg as its buffer
        assert.equal(syscall(291, -100, 128, 0, 512, 0), 0);
        const dv = new DataView(memory.buffer);
        assert.equal(dv.getUint32(512 + 24, true), 8630); // S_IFCHR|0666
        assert.equal(dv.getBigUint64(512 + 32, true), 0n);
        assert.equal(caller.calls.some(([m, a]) => m === 'path_size' && String(a.path).includes('draw')), false);
    });
});

test('an open failure from the device surfaces as EACCES, not a crash', async () => {
    const caller = {
        calls: [],
        call(method, arg) {
            this.calls.push([method, arg]);
            if (method === 'path_open') throw new Error('unknown id for draw client');
            return true;
        },
    };
    await withLinuxSyscall(caller, (syscall, memory) => {
        putPath(memory, 128, 'dev/draw/9/data');
        assert.equal(syscall(56, -100, 128, 2, 0), -13);
    });
});
