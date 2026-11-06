import { test } from 'node:test';
import assert from 'node:assert/strict';
globalThis.self = {};
const { CallBuffer } = await import('./lib.js');
const { runLinux, messageHandler } = await import('./worker.js');

test('statx restricted ABI, canaries and real CallBuffer error codec', async () => {
    const compile = WebAssembly.compile, instantiate = WebAssembly.instantiate;
    const previous = globalThis.postMessage;
    const memory = new WebAssembly.Memory({initial: 1});
    let bytes, view;
    const caller = new CallBuffer(new SharedArrayBuffer(16384));
    let response = {ok: true, stat: {Mode: 0x81a4, IsDir: false, Size: 2 ** 32 + 17}}, calls = 0;
    globalThis.postMessage = ({method}) => {
        if (method === 'exit') return caller.respond(true);
        assert.equal(method, 'path_stat'); calls++; caller.respond(response);
    };
    WebAssembly.compile = async () => ({});
    WebAssembly.instantiate = async (_, imports) => ({exports: {memory, _start() {
        bytes = new Uint8Array(memory.buffer); view = new DataView(memory.buffer);
        const syscall = imports['ferrule.linux.syscall'].syscall;
        const invoke = (path = 'out.txt', flags = 0, mask = 0x7ff, fd = -100, ptr = 1024, buf = 2048) => {
            bytes.fill(0xa5); bytes.set(new TextEncoder().encode(path + '\0'), 1024);
            const before = bytes.slice();
            const rc = syscall(291, fd, ptr, flags, mask, buf);
            if (rc !== 0) assert.deepEqual(bytes, before);
            return rc;
        };
        assert.equal(invoke(), 0);
        assert.equal(view.getUint32(2048, true), 0x201);
        assert.equal(view.getUint16(2076, true), 0x8000);
        assert.equal(view.getBigUint64(2088, true), 2n ** 32n + 17n);
        const expected = new Uint8Array(256), ev = new DataView(expected.buffer);
        ev.setUint32(0, 0x201, true); ev.setUint16(28, 0x8000, true); ev.setBigUint64(40, 2n ** 32n + 17n, true);
        assert.deepEqual(bytes.slice(2048, 2304), expected);
        assert.equal(bytes[2047], 0xa5); assert.equal(bytes[2304], 0xa5); assert.equal(bytes[0x7ff], 0xa5);
        for (const mask of [0, 0x40000000]) assert.equal(invoke('out.txt', 0, mask), 0);
        for (const flags of [1, 0x8000, 0x6000]) assert.equal(invoke('out.txt', flags), -22);
        for (const flags of [0x1000, 0x2000, 0x4000]) assert.equal(invoke('out.txt', flags), -38);
        assert.equal(invoke('out.txt', 0, 0x80000000), -22);
        assert.equal(invoke(''), -2);
        for (const path of ['tmpfile_x/other', 'tmp/tmpfile_x/other', '../out.txt', 'tmpfile_x/..']) assert.equal(invoke(path), -13);
        assert.equal(invoke('out.txt', 0, 0, 42), -9);
        assert.equal(invoke('/out.txt', 0x900, 0, 42), 0);
        assert.equal(invoke('out.txt', 0, 0, -100, -1), -14);
        assert.equal(invoke('out.txt', 0, 0, -100, bytes.length - 1), -14); // no NUL
        assert.equal(invoke('out.txt', 0, 0, -100, 1024, bytes.length - 100), -14);
        assert.equal(invoke('out.txt', 0, 0, -100, 1024, 0), -14);
        const count = calls;
        for (const errno of [2, 13, 20, 5]) {
            response = {ok: false, errno}; assert.equal(invoke(), -errno);
        }
        assert.equal(calls, count + 4);
        for (const [stat, errno] of [
            [{Mode: 0x4000, IsDir: true, Size: 1}, 38],
            [{Mode: 0xa000, IsDir: false, Size: 1}, 38],
            [{Mode: 0x8000, IsDir: false, Size: -1}, 5],
            [{Mode: 0x8000, IsDir: false, Size: 1.5}, 5],
            [{Mode: 0x8000, IsDir: false, Size: 2 ** 53}, 75],
            [{Mode: 0x8000, IsDir: false, Size: '123'}, 5],
        ]) { response = {ok: true, stat}; assert.equal(invoke(), -errno); }
    }}});
    try { await runLinux({data: {bin: Uint8Array.of(0,97,115,109,1,0,0,0,0), args: [], env: [], tid: 'test'}}, caller); }
    finally { WebAssembly.compile = compile; WebAssembly.instantiate = instantiate; globalThis.postMessage = previous; }
});

test('path_stat calls actual backend stat/lstat and preserves typed failures', async () => {
    const responses = [], calls = [];
    const stat = {Size: 123, Mode: 0x8000, IsDir: false};
    const handler = messageHandler({
        async stat(path) { calls.push(['stat', path]); return stat; },
        async lstat(path) { calls.push(['lstat', path]); throw Error('no such file'); },
    }, {respond(value) {responses.push(value);}}, 'test');
    await handler({data: {method: 'path_stat', path: '#task/test/w/out.txt'}});
    await handler({data: {method: 'path_stat', path: '#task/test/w/tmpfile_x', nofollow: true}});
    await handler({data: {method: 'path_stat', path: '#task/test/w/tmpfile_x/other'}});
    assert.deepEqual(responses, [{ok: true, stat}, {ok: false, errno: 2}, {ok: false, errno: 13}]);
    assert.deepEqual(calls.map(c => c[0]), ['stat', 'lstat']);
});
