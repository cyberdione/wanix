import { test } from 'node:test';
import assert from 'node:assert/strict';
globalThis.self = {};
const { runLinux, messageHandler } = await import('./worker.js');
const { CallBuffer } = await import('./lib.js');

async function fixture(backend, check, real = false) {
    const compile = WebAssembly.compile, instantiate = WebAssembly.instantiate;
    const post = globalThis.postMessage;
    const memory = new WebAssembly.Memory({ initial: 1 });
    const call = new CallBuffer(new SharedArrayBuffer(16384));
    globalThis.postMessage = ({method, ...params}) => {
        try { call.respond(backend(method, params)); }
        catch (err) { call.respond({__wanix_error: err.message}); }
    };
    const caller = real ? call : { maxData: call.maxData, call: backend };
    WebAssembly.compile = async () => ({});
    WebAssembly.instantiate = async (_, imports) => ({ exports: { memory,
        _start() { check(imports['ferrule.linux.syscall'].syscall, new Uint8Array(memory.buffer), call); },
    } });
    try {
        await runLinux({data: {bin: Uint8Array.of(0,97,115,109,1,0,0,0,0),
            args: [], env: [], stdin: 'in', stdout: 'out'}}, caller);
    } finally {
        WebAssembly.compile = compile; WebAssembly.instantiate = instantiate;
        globalThis.postMessage = post;
    }
}

test('large reads use real CallBuffer codec and return a legal short read', async () => {
    let reads = 0;
    await fixture((method, args) => {
        if (method === 'path_open') return 10;
        if (method === 'fd_read') { reads++; return new Uint8Array(args.count).fill(42); }
        if (method === 'exit') { assert.equal(args.code, 0); return true; }
        assert.fail(method);
    }, (syscall, bytes, call) => {
        for (const count of [16384, 32768]) {
            bytes.fill(99, 20000, 52768);
            assert.equal(syscall(63, 0, 20000, count), call.maxData);
            assert.equal(bytes[20000 + call.maxData - 1], 42);
            assert.equal(bytes[20000 + call.maxData], 99);
        }
        const v = new DataView(bytes.buffer);
        v.setUint32(128, 20000, true); v.setUint32(132, 32768, true);
        v.setUint32(136, 55000, true); v.setUint32(140, 1, true);
        assert.equal(syscall(65, 0, 128, 2), call.maxData);
        assert.equal(reads, 3);
    }, true);
});

test('writev rejects invalid count before backend or guest mutation', async () => {
    let writes = 0;
    await fixture(method => {
        if (method === 'fd_write') { writes++; return 0; }
        return 0;
    }, (syscall, bytes) => {
        bytes.fill(99, 128, 9000);
        const before = bytes.slice();
        for (const count of [-1, -2147483648, 1025])
            assert.equal(syscall(66, 1, -1, count), -22);
        assert.equal(writes, 0);
        assert.deepEqual(bytes, before);
        assert.equal(syscall(66, 1, 128, 0), 0);
        bytes.fill(0, 128, 128 + 1024 * 8);
        assert.equal(syscall(66, 1, 128, 1024), 0);
        assert.equal(writes, 2);
    });
});

test('invalid write counts and thrown I/O failures are explicit errno', async () => {
    let response;
    await fixture(method => {
        if (method === 'fd_write' || method === 'fd_read') {
            if (response instanceof Error) throw response;
            return response;
        }
        return 0;
    }, (syscall) => {
        for (response of [undefined, null, NaN, 1.5, 5, -4096]) {
            assert.equal(syscall(64, 1, 32, 4), -5);
            assert.equal(syscall(66, 1, 128, 0), -5);
        }
        response = Object.assign(new Error('closed'), {code: 'EBADF'});
        assert.equal(syscall(64, 1, 32, 4), -9);
        assert.equal(syscall(63, 0, 32, 4), -9);
        response = new Error('transport failed');
        assert.equal(syscall(63, 0, 32, 4), -5);
        for (response of [undefined, {}, new Uint8Array(5)])
            assert.equal(syscall(63, 0, 32, 4), -5);
    });
});

test('async backend errors cross actual response codec as typed I/O errors', async () => {
    const call = new CallBuffer(new SharedArrayBuffer(16384));
    await messageHandler({readAt() { throw Object.assign(new Error('closed'), {code:'EBADF'}); }}, call, 1)
        ({data: {method:'fd_read', fd:0, count:4, at:0}});
    const post = globalThis.postMessage;
    globalThis.postMessage = () => { Atomics.store(call.ctrl, 0, 1); };
    try { assert.deepEqual(call.call('fd_read', {}), {ioError:9}); }
    finally { globalThis.postMessage = post; }
});

test('readv keeps completed bytes and cursor when a later response is malformed (real codec)', async () => {
    const reads = [];
    await fixture((method, args) => {
        if (method === 'path_open') return 10;
        if (method === 'path_open_existing') return {ok: true, fd: 11};
        if (method === 'fd_read') {
            reads.push(args.at);
            return reads.length === 2 ? {} : Uint8Array.of(41, 42);
        }
        if (method === 'exit') return true;
        assert.fail(method);
    }, (syscall, bytes) => {
        bytes.set(new TextEncoder().encode('out.txt\0'), 32);
        const fd = syscall(56, -100, 32, 0, 0);
        assert.equal(fd, 3);
        const view = new DataView(bytes.buffer);
        view.setUint32(128, 256, true); view.setUint32(132, 2, true);
        view.setUint32(136, 272, true); view.setUint32(140, 2, true);
        bytes.fill(99, 255, 276);
        assert.equal(syscall(65, fd, 128, 2), 2);
        assert.deepEqual(bytes.slice(255, 260), Uint8Array.of(99, 41, 42, 99, 99));
        assert.deepEqual(bytes.slice(271, 276), new Uint8Array(5).fill(99));
        assert.deepEqual(reads, [0, 2]);
        assert.equal(syscall(63, fd, 272, 2), 2);
        assert.deepEqual(reads, [0, 2, 2]);
        assert.deepEqual(bytes.slice(271, 276), Uint8Array.of(99, 41, 42, 99, 99));
    }, true);
});
