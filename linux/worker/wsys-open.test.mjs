import { test } from 'node:test';
import assert from 'node:assert/strict';

globalThis.self = {};
const { runLinux } = await import('./worker.js');

// rio(4) per-window files through the personality openat: the driver binds
// #wsys at mnt/wsys and this allowlist admits exactly the window files
// (cons, consctl, mouse, label, wctl) under mnt/wsys/<id>/. The host
// service plane (#wsys/<id>/srv/*) must never be reachable from a task.
// Device opens carry server-side semantics — mouse and wctl are
// single-reader, cons and mouse reads block — and read/write use the same
// positioned fd_read/fd_write RPC scratch files use.

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

test('openat admits the rio window files and maps them into the task namespace', async () => {
    const caller = makeCaller();
    await withLinuxSyscall(caller, (syscall, memory) => {
        for (const path of [
            'mnt/wsys/1/cons', '/mnt/wsys/1/cons', 'mnt/wsys/1/consctl',
            'mnt/wsys/1/mouse', 'mnt/wsys/1/label', 'mnt/wsys/1/wctl',
            'mnt/wsys/42/label',
        ]) {
            putPath(memory, 128, path);
            const fd = syscall(56, -100, 128, 2, 0);
            assert.ok(fd >= 3, `${path} openat fd ${fd}`);
        }
    });
    const opens = caller.calls.filter(([m]) => m === 'path_open').map(([, a]) => a.path);
    assert.deepEqual(opens, [
        'mnt/wsys/1/cons', 'mnt/wsys/1/cons', 'mnt/wsys/1/consctl',
        'mnt/wsys/1/mouse', 'mnt/wsys/1/label', 'mnt/wsys/1/wctl',
        'mnt/wsys/42/label',
    ]);
    // device opens never create scratch entries
    assert.equal(caller.calls.filter(([m]) => m === 'path_touch').length, 0);
});

test('openat rejects the host service plane and every other wsys path', async () => {
    const caller = makeCaller();
    await withLinuxSyscall(caller, (syscall, memory) => {
        for (const path of [
            'mnt/wsys', 'mnt/wsys/1', 'mnt/wsys/1/', 'mnt/wsys/1/srv',
            'mnt/wsys/1/srv/geom', 'mnt/wsys/1/srv/mouse', 'mnt/wsys/1/srv/ctl',
            'mnt/wsys/1/srv/wctl', 'mnt/wsys/1/winname', 'mnt/wsys/x/label',
            'mnt/wsys/1/label/x', 'mnt/wsys/-1/label', 'mnt/other/label',
            'mnt/wsys/1/label/../label', 'dev/wsys/1/label',
        ]) {
            putPath(memory, 128, path);
            assert.equal(syscall(56, -100, 128, 2, 0), -13, `${path} must be denied`);
        }
    });
    // nothing but the final exit crossed the boundary
    assert.deepEqual(caller.calls, [['exit', { code: 0 }]]);
});

test('a window fd reads and writes positioned through the shared description', async () => {
    const caller = makeCaller();
    await withLinuxSyscall(caller, (syscall, memory) => {
        putPath(memory, 128, 'mnt/wsys/1/label');
        const fd = syscall(56, -100, 128, 2, 0);
        const mem = new Uint8Array(memory.buffer);
        mem.set([9, 8, 7], 200);
        assert.equal(syscall(64, fd, 200, 3), 3);          // write at 0
        assert.equal(syscall(63, fd, 300, 2), 2);          // read at 3
        const seq = caller.calls.filter(([m]) => m === 'fd_write' || m === 'fd_read');
        assert.deepEqual(seq.map(([m, a]) => [m, a.at]), [['fd_write', 0], ['fd_read', 3]]);
    });
});

test('statx reports window files as character devices without a size round-trip', async () => {
    const caller = makeCaller();
    await withLinuxSyscall(caller, (syscall, memory) => {
        putPath(memory, 128, 'mnt/wsys/1/mouse');
        assert.equal(syscall(291, -100, 128, 0, 512, 0), 0);
        const dv = new DataView(memory.buffer);
        assert.equal(dv.getUint32(512 + 24, true), 8630); // S_IFCHR|0666
        assert.equal(dv.getBigUint64(512 + 32, true), 0n);
        assert.equal(caller.calls.some(([m, a]) => m === 'path_size' && String(a.path).includes('wsys')), false);
    });
});

test('an open failure from the device surfaces as EACCES, not a crash', async () => {
    const caller = {
        calls: [],
        call(method, arg) {
            this.calls.push([method, arg]);
            if (method === 'path_open') throw new Error('wsys: file already open');
            return true;
        },
    };
    await withLinuxSyscall(caller, (syscall, memory) => {
        putPath(memory, 128, 'mnt/wsys/1/mouse');
        assert.equal(syscall(56, -100, 128, 2, 0), -13);
    });
});

// The WM grant (ADR 0007 K1): a task whose launch env carries WSYS_WM=1
// gets the srv plane through the "#wsys/wm" bind — the device-level
// srv/state enumeration and every window's srv files. Everything outside
// mnt/wsys stays closed, and the default grant still denies srv entirely.

test('the WM grant admits the srv plane and maps it into the task namespace', async () => {
    const caller = makeCaller();
    await withLinuxSyscall(caller, (syscall, memory) => {
        for (const path of [
            'mnt/wsys/srv/state',
            'mnt/wsys/1/srv/consin', 'mnt/wsys/1/srv/consout',
            'mnt/wsys/2/srv/mouse', 'mnt/wsys/2/srv/geom',
            'mnt/wsys/3/srv/wctl', 'mnt/wsys/3/srv/notes',
            'mnt/wsys/3/srv/state', 'mnt/wsys/3/srv/ctl',
            '/mnt/wsys/2/srv/mouse',
        ]) {
            putPath(memory, 128, path);
            const fd = syscall(56, -100, 128, 2, 0);
            assert.ok(fd >= 3, `${path} openat fd ${fd}`);
        }
    }, ['SHELL=/bin/rc', 'WSYS_WM=1']);
    const opens = caller.calls.filter(([m]) => m === 'path_open').map(([, a]) => a.path);
    assert.deepEqual(opens, [
        'mnt/wsys/srv/state',
        'mnt/wsys/1/srv/consin', 'mnt/wsys/1/srv/consout',
        'mnt/wsys/2/srv/mouse', 'mnt/wsys/2/srv/geom',
        'mnt/wsys/3/srv/wctl', 'mnt/wsys/3/srv/notes',
        'mnt/wsys/3/srv/state', 'mnt/wsys/3/srv/ctl',
        'mnt/wsys/2/srv/mouse',
    ]);
    assert.equal(caller.calls.filter(([m]) => m === 'path_touch').length, 0);
});

test('the WM grant widens nothing outside the srv plane', async () => {
    const caller = makeCaller();
    await withLinuxSyscall(caller, (syscall, memory) => {
        for (const path of [
            'mnt/wsys/1/winname', 'mnt/wsys/1/text', 'mnt/wsys/x/label',
            'mnt/wsys/srv', 'mnt/wsys/srv/other', 'mnt/wsys/wm',
            'mnt/wsys/1/srv', 'mnt/wsys/1/srv/wrong', 'mnt/wsys/1/label/x',
            'mnt/wsys/1/label/../label',
        ]) {
            putPath(memory, 128, path);
            assert.equal(syscall(56, -100, 128, 2, 0), -13, `${path} must be denied`);
        }
        // the window files stay reachable under the WM grant
        putPath(memory, 128, 'mnt/wsys/1/cons');
        assert.ok(syscall(56, -100, 128, 2, 0) >= 3);
    }, ['WSYS_WM=1']);
    assert.deepEqual(caller.calls.filter(([m]) => m === 'path_touch').length, 0);
});

test('the default grant denies the device-level srv/state too', async () => {
    const caller = makeCaller();
    await withLinuxSyscall(caller, (syscall, memory) => {
        for (const path of [
            'mnt/wsys/srv/state', 'mnt/wsys/1/srv/consin', 'mnt/wsys/2/srv/mouse',
            'mnt/wsys/1/srv/ctl',
        ]) {
            putPath(memory, 128, path);
            assert.equal(syscall(56, -100, 128, 2, 0), -13, `${path} must be denied`);
        }
    });
    assert.deepEqual(caller.calls, [['exit', { code: 0 }]]);
});

test('the WM grant works only on WSYS_WM=1, not a lookalike', async () => {
    const caller = makeCaller();
    await withLinuxSyscall(caller, (syscall, memory) => {
        for (const path of ['mnt/wsys/srv/state', 'mnt/wsys/1/srv/mouse']) {
            putPath(memory, 128, path);
            assert.equal(syscall(56, -100, 128, 2, 0), -13, `${path} must be denied`);
        }
    }, ['WSYS_WM=2', 'WSYS_WM ', 'WSYS_WM', 'wsys_wm=1']);
    assert.deepEqual(caller.calls, [['exit', { code: 0 }]]);
});

test('statx reports srv files as character devices under the WM grant', async () => {
    const caller = makeCaller();
    await withLinuxSyscall(caller, (syscall, memory) => {
        putPath(memory, 128, 'mnt/wsys/srv/state');
        assert.equal(syscall(291, -100, 128, 0, 512, 0), 0);
        const dv = new DataView(memory.buffer);
        assert.equal(dv.getUint32(512 + 24, true), 8630); // S_IFCHR|0666
        putPath(memory, 128, 'mnt/wsys/2/srv/mouse');
        assert.equal(syscall(291, -100, 128, 0, 512, 0), 0);
        assert.equal(dv.getUint32(512 + 24, true), 8630);
    }, ['WSYS_WM=1']);
});
