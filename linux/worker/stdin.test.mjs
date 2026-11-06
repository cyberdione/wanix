import { test } from 'node:test';
import assert from 'node:assert/strict';

// The worker's browser entrypoint is unused; exercise its actual syscall dispatch.
globalThis.self = {};
const { runLinux } = await import('./worker.js');

async function fixture(text, check) {
    const compile = WebAssembly.compile, instantiate = WebAssembly.instantiate;
    const memory = new WebAssembly.Memory({ initial: 1 });
    WebAssembly.compile = async () => ({});
    WebAssembly.instantiate = async (_, imports) => ({ exports: {
        memory,
        _start() { check(imports['ferrule.linux.syscall'].syscall, new Uint8Array(memory.buffer)); },
    } });
    try {
        await runLinux({ data: {
            bin: Uint8Array.of(0,97,115,109,1,0,0,0,0),
            args: [], env: [], stdinText: text,
        } }, { call(method, arg) { assert.equal(method, 'exit'); assert.equal(arg.code, 0); } });
    } finally {
        WebAssembly.compile = compile;
        WebAssembly.instantiate = instantiate;
    }
}

test('read splits UTF-8 at byte boundaries without overwriting adjacent bytes', async () => {
    const text = 'é€😀Z', expected = new TextEncoder().encode(text);
    await fixture(text, (syscall, bytes) => {
        const actual = [];
        for (const byte of expected) {
            bytes.fill(0xa5, 31, 34);
            assert.equal(syscall(63, 0, 32, 1), 1);
            assert.deepEqual([...bytes.subarray(31, 34)], [0xa5, byte, 0xa5]);
            actual.push(bytes[32]);
        }
        assert.deepEqual(Uint8Array.from(actual), expected);
        assert.equal(new TextDecoder().decode(Uint8Array.from(actual)), text);
        assert.equal(syscall(63, 0, 32, 1), 0);
        assert.equal(syscall(63, 0, 32, 10), 0);
    });
});

test('zero-length reads preserve the byte offset; short reads return bytes and then EOF', async () => {
    await fixture('é', (syscall, bytes) => {
        bytes.fill(0xa5, 32, 36);
        assert.equal(syscall(63, 0, 32, 0), 0);
        assert.equal(bytes[32], 0xa5);
        assert.equal(syscall(63, 0, 32, 1), 1);
        assert.equal(bytes[32], 0xc3);
        assert.equal(syscall(63, 0, 33, 0), 0);
        assert.equal(syscall(63, 0, 33, 3), 1);
        assert.deepEqual([...bytes.subarray(32, 36)], [0xc3, 0xa9, 0xa5, 0xa5]);
        assert.equal(syscall(63, 0, 32, 0), 0);
        assert.equal(syscall(63, 0, 32, 1), 0);
    });
    await fixture('', (syscall) => assert.equal(syscall(63, 0, 32, 1), 0));
});

test('readv shares the read byte offset, skips empty vectors and stops at EOF', async () => {
    await fixture('é€', (syscall, bytes) => {
        const view = new DataView(bytes.buffer);
        const vectors = [[32, 0], [33, 1], [35, 1], [37, 4], [42, 1]];
        vectors.forEach(([base, len], i) => {
            view.setUint32(128 + i * 8, base, true);
            view.setUint32(132 + i * 8, len, true);
        });
        bytes.fill(0xa5, 32, 44);
        assert.equal(syscall(63, 0, 32, 1), 1);
        assert.equal(syscall(65, 0, 128, vectors.length), 4);
        assert.deepEqual([bytes[32], bytes[33], bytes[35], bytes[37], bytes[38]], [0xc3, 0xa9, 0xe2, 0x82, 0xac]);
        for (const i of [34,36,39,40,41,42,43]) assert.equal(bytes[i], 0xa5);
        assert.equal(syscall(65, 0, 128, vectors.length), 0);
        assert.equal(syscall(63, 0, 32, 1), 0);
    });
});
