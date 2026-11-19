import { test } from 'node:test';
import assert from 'node:assert/strict';
globalThis.self = {};
const { CallBuffer } = await import('./lib.js');
const { runLinux, messageHandler } = await import('./worker.js');

test('unlinkat validates arguments and removes admitted names through RPC', async () => {
    const compile = WebAssembly.compile, instantiate = WebAssembly.instantiate;
    const memory = new WebAssembly.Memory({ initial: 1 });
    const calls = [];
    const caller = new CallBuffer(new SharedArrayBuffer(16384));
    const postMessage = globalThis.postMessage;
    globalThis.postMessage = ({method, path}) => {
        if (method === 'exit') return caller.respond(true);
        assert.equal(method, 'path_remove'); calls.push(path);
        const errno = path.endsWith('missing') ? 2 : path.endsWith('dir') ? 21 : path.endsWith('link') ? 38 : 0;
        caller.respond(errno ? {ok: false, errno, message: 'backend rejection'} : {ok: true});
    };
    WebAssembly.compile = async () => ({});
    WebAssembly.instantiate = async (_, imports) => ({ exports: { memory, _start() {
        const syscall = imports['ferrule.linux.syscall'].syscall;
        const unlink = (path, fd = -100, flags = 0) => {
            new Uint8Array(memory.buffer).set(new TextEncoder().encode(path + '\0'), 128);
            return syscall(35, fd, 128, flags);
        };
        for (const path of ['out.txt', 'tmpfile_test', 'tmp/tmpfile_test', '/out.txt']) assert.equal(unlink(path), 0);
        assert.equal(unlink('/out.txt', 99), 0); // absolute paths ignore dirfd
        assert.equal(unlink('out.txt', 99), -9);
        assert.equal(unlink('out.txt', -100, 0x200), -38);
        for (const flags of [1, -1, 0x201]) assert.equal(unlink('out.txt', -100, flags), -22);
        for (const path of ['../outside', 'tmpfile_x/../out.txt', 'elsewhere', 'tmpfile_x//z', 'tmpfile_x/.']) assert.equal(unlink(path), -13);
        assert.equal(unlink(''), -2);
        assert.equal(syscall(35, -100, -1, 0), -14);
        assert.equal(syscall(87, 128, 0, 0), -38);
        assert.equal(unlink('tmpfile_missing'), -2);
        assert.equal(unlink('tmpfile_dir'), -21);
        assert.equal(unlink('tmpfile_link'), -38);
        assert.equal(calls.length, 8);
    } } });
    try {
        await runLinux({ data: { bin: Uint8Array.of(0,97,115,109,1,0,0,0,0), args: [], env: [], tid: 'test' } }, caller);
    } finally {
        WebAssembly.compile = compile; WebAssembly.instantiate = instantiate;
        globalThis.postMessage = postMessage;
    }
});

test('outer bridge uses lstat and Remove, rejects directories and symlinks without mutation', async () => {
    const removed = [], responses = [];
    const prefix = '#task/test/w/';
    const handler = messageHandler({
        async lstat(path) {
            path = path.slice(prefix.length).replace(/^tmpfile_/, '');
            if (path === 'missing') throw new Error('open missing: file does not exist');
            if (path === 'denied') throw new Error('permission denied');
            return {Mode: path === 'dir' ? 0x4000 : path === 'link' ? 0xa000 : 0x8000, IsDir: path === 'dir'};
        },
        async remove(path) { removed.push(path); },
    }, {respond(value) { responses.push(value); }}, 'test');
    for (const name of ['regular', 'dir', 'link', 'missing', 'denied']) await handler({data: {method: 'path_remove', path: prefix + 'tmpfile_' + name}});
    assert.deepEqual(removed, [prefix + 'tmpfile_regular']);
    assert.deepEqual(responses[0], {ok: true});
    assert.deepEqual(responses.slice(1).map(r => r.errno), [21, 38, 2, 13]);
});
