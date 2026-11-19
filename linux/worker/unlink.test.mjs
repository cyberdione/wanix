import { test } from 'node:test';
import assert from 'node:assert/strict';

globalThis.self = {};
const { runLinux } = await import('./worker.js');

test('unlinkat is explicitly unsupported without mutating names or open storage', async () => {
    const compile = WebAssembly.compile, instantiate = WebAssembly.instantiate;
    const memory = new WebAssembly.Memory({ initial: 1 });
    const calls = [];
    const files = new Map([['out.txt', Uint8Array.of(65, 66)]]);
    const openStorage = files.get('out.txt');
    WebAssembly.compile = async () => ({});
    WebAssembly.instantiate = async (_, imports) => ({ exports: {
        memory,
        _start() {
            const syscall = imports['ferrule.linux.syscall'].syscall;
            const bytes = new Uint8Array(memory.buffer);
            for (const path of ['out.txt', 'tmpfile_test', 'tmp/tmpfile_test', '/out.txt', '../outside', '']) {
                bytes.set(new TextEncoder().encode(path + '\0'), 128);
                for (const dirfd of [-100, 3, -1]) {
                    for (const flags of [0, 0x200, 1, -1]) {
                        assert.equal(syscall(35, dirfd, 128, flags), -38);
                    }
                }
            }
            // An unsupported syscall does not inspect even an invalid pointer.
            assert.equal(syscall(35, -100, -1, 0), -38);
            // 87 is timerfd_gettime on this ABI, NOT x86-64 unlink.
            assert.equal(syscall(87, 128, 0, 0), -38);
            assert.deepEqual(calls, []);
            assert.equal(files.get('out.txt'), openStorage);
            assert.deepEqual([...openStorage], [65, 66]);
        },
    } });
    try {
        await runLinux({ data: {
            bin: Uint8Array.of(0,97,115,109,1,0,0,0,0), args: [], env: [], tid: 'test',
        } }, { call(method, arg) {
            calls.push(method);
            assert.equal(method, 'exit');
            assert.equal(arg.code, 0);
        } });
        assert.deepEqual(calls, ['exit']);
    } finally {
        WebAssembly.compile = compile;
        WebAssembly.instantiate = instantiate;
    }
});
