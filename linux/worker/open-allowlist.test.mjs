import { test } from 'node:test';
import assert from 'node:assert/strict';
globalThis.self = {};
const { runLinux, messageHandler } = await import('./worker.js');

test('openat actual dispatch admits flat aliases and denies descendants without RPC or memory writes', async () => {
    const compile = WebAssembly.compile, instantiate = WebAssembly.instantiate;
    const memory = new WebAssembly.Memory({initial: 1}), calls = [];
    WebAssembly.compile = async () => ({});
    WebAssembly.instantiate = async (_, imports) => ({exports: {memory, _start() {
        const bytes = new Uint8Array(memory.buffer);
        const open = path => {
            bytes.fill(0xa5, 127, 512);
            bytes.set(new TextEncoder().encode(path + '\0'), 128);
            const before = bytes.slice(), n = calls.length;
            const result = imports['ferrule.linux.syscall'].syscall(56, -100, 128, 0, 0);
            assert.deepEqual(bytes, before);
            return {result, calls: calls.slice(n)};
        };
        for (const path of ['tmpfile_x/secret.txt', 'tmp/tmpfile_x/secret.txt', '/tmp/tmpfile_x/secret.txt',
            'tmpfile_x/./secret.txt', '../out.txt', 'tmpfile_x/../out.txt', '.', './out.txt',
            '//out.txt', '//tmp/tmpfile_x', 'tmp//tmpfile_x', 'tmpfile_x//secret.txt', 'out.txt/']) {
            assert.deepEqual(open(path), {result: -13, calls: []}, path);
        }
        for (const path of ['out.txt', '/out.txt', 'tmpfile_x', '/tmpfile_x', 'tmp/tmpfile_x', '/tmp/tmpfile_x']) {
            const got = open(path);
            assert.ok(got.result >= 3, path);
            const target = '#task/test/w/' + (path.endsWith('out.txt') ? 'out.txt' : 'tmpfile_x');
            assert.deepEqual(got.calls, [['path_open_existing', {path: target}]]);
        }
        bytes[bytes.length - 1] = 111;
        const n = calls.length;
        assert.equal(imports['ferrule.linux.syscall'].syscall(56, -100, bytes.length - 1, 0, 0), -14);
        assert.equal(calls.length, n);
    }}});
    try {
        await runLinux({data: {bin: Uint8Array.of(0,97,115,109,1,0,0,0,0), tid: 'test', args: [], env: []}},
            {call(method, arg) { calls.push([method, arg]); return method === 'path_open_existing' ? {ok: true, fd: 10} : true; }});
    } finally { WebAssembly.compile = compile; WebAssembly.instantiate = instantiate; }
});

test('broker-only touch/open enforce task-local names and reject existing leaf links', async () => {
    const calls = [], replies = [];
    let stat = {Mode: 0x8000, IsDir: false};
    const fs = {
        async lstatResult(path) {calls.push(['lstat', path]); if (stat instanceof Error) throw stat; return {OK:true, Value:stat};},
        async openResult(path) {calls.push(['open', path]); return {OK:true, Value:10};},
        async writeFile(path) {calls.push(['writeFile', path]);},
    };
    const handler = messageHandler(fs, {respond(value) {replies.push(value);}}, 'test');
    for (const method of ['path_open', 'path_touch']) {
        for (const path of ['#task/other/w/out.txt', '#task/test/exit', '#task/test/w/secret.txt',
            '#task/test/w/tmpfile_x/secret.txt', '#task/test/w/../out.txt', '#task/test/w//out.txt']) {
            calls.length = 0;
            await handler({data: {method, path}});
            assert.match(replies.at(-1).__wanix_error, /permission denied/);
            assert.deepEqual(calls, []);
        }
        stat = {Mode: 0xa000, IsDir: false}; calls.length = 0;
        await handler({data: {method, path: '#task/test/w/out.txt'}});
        assert.match(replies.at(-1).__wanix_error, /permission denied/);
        assert.deepEqual(calls, [['lstat', '#task/test/w/out.txt']]);
        stat = {Mode: 0x8000, IsDir: false};
        for (const name of ['out.txt', 'tmpfile_x']) {
            await handler({data: {method, path: '#task/test/w/' + name}});
            assert.ok(!replies.at(-1)?.__wanix_error);
        }
    }
    stat = Object.assign(new Error('no such file'), {code: 'ENOENT'});
    await handler({data: {method: 'path_touch', path: '#task/test/w/tmpfile_new'}});
    assert.equal(replies.at(-1), true);
    calls.length = 0;
    for (const fd of [0,1,2]) await handler({data: {method: 'path_open', path: `#task/test/fd/${fd}`}});
    assert.deepEqual(calls.map(c => c[0]), ['open', 'open', 'open']);
});
