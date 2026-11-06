import { test } from 'node:test';
import assert from 'node:assert/strict';
globalThis.self = {};
const { runLinux } = await import('./worker.js');

test('actual memory syscall dispatch: bounded RW-only nonreclaiming arena', async () => {
    const compile = WebAssembly.compile, instantiate = WebAssembly.instantiate;
    const memory = new WebAssembly.Memory({initial: 1, maximum: 5});
    WebAssembly.compile = async () => ({});
    WebAssembly.instantiate = async (_, imports) => ({exports: {memory, _start() {
        const call = (nr, ...args) => imports['ferrule.linux.syscall'].syscall(nr, ...args);
        const mmap = (len, prot = 3, flags = 0x22, fd = -1, offset = 0, addr = 0) =>
            call(222, addr, len, prot, flags, fd, offset);
        const initial = memory.buffer.byteLength;
        assert.equal(initial, 3 * 65536); // module + unchanged two startup pages
        const reject = (fn, errno) => {
            const before = new Uint8Array(memory.buffer).slice();
            assert.equal(fn(), errno);
            assert.deepEqual(new Uint8Array(memory.buffer), before);
        };
        for (const request of [0, 1, initial, -1]) reject(() => call(214, request), -38);
        reject(() => mmap(0), -22);
        reject(() => mmap(1, 3, 0x20), -22);
        for (const flags of [0x21, 0x23, 0x32, 0x100022, 0x80000022, 2])
            reject(() => mmap(1, 3, flags), -38);
        for (const prot of [0, 1, 2, 4, 7, 8, -1]) reject(() => mmap(1, prot), -38);
        reject(() => mmap(1, 3, 0x22, 0), -38);
        reject(() => mmap(1, 3, 0x22, -1, 1), -22);
        reject(() => mmap(1, 3, 0x22, -1, 4096), -38);
        for (const len of [-1, 0x80000000, 16 * 1024 * 1024]) reject(() => mmap(len), -12);
        for (const [nr, args, errno] of [
            [215, [1, 4096], -22], [215, [0, 0], -22],
            [215, [0xfffff000, 4097], -22], [215, [0, 4096], -38],
            [226, [1, 0, 3], -22], [226, [0, 0, 8], -22],
            [226, [0xfffff000, 4097, 3], -12],
            [226, [0, 0, 3], -38], [226, [0, 4096, 0], -38],
        ]) reject(() => call(nr, ...args), errno);
        const first = mmap(1);
        assert.equal(first, initial); // rejected calls did not consume arena state
        assert.equal(memory.buffer.byteLength, initial + 65536);
        assert.deepEqual(new Uint8Array(memory.buffer, first, 4096), new Uint8Array(4096));
        new Uint8Array(memory.buffer, first, 4096).fill(0xa5);
        // Dirty unused physical capacity; allocation must explicitly zero it.
        new Uint8Array(memory.buffer, first + 4096, 8192).fill(0xcc);
        const second = mmap(4097, 3, 0x22, -1, 0, 12345);
        assert.equal(second, first + 4096); // Linux 4 KiB extents, not 64 KiB
        assert.deepEqual(new Uint8Array(memory.buffer, second, 8192), new Uint8Array(8192));
        assert.ok(new Uint8Array(memory.buffer, first, 4096).every(x => x === 0xa5));
        for (const nr of [215, 226]) reject(() => call(nr, first, 4096, 3), -38);
        // Real declared-maximum failure below the diagnostic 16 MiB budget.
        reject(() => mmap(3 * 65536), -12);
        assert.equal(mmap(4096), second + 8192); // failure did not consume state
        const thirdEnd = second + 12288;
        assert.equal(mmap(5 * 65536 - thirdEnd), thirdEnd);
        assert.equal(memory.buffer.byteLength, 5 * 65536);
        reject(() => mmap(1), -12);
    }}});
    try {
        await runLinux({data: {bin: Uint8Array.of(0,97,115,109,1,0,0,0,0), args: [], env: []}},
            {call(method) { assert.equal(method, 'exit'); }});
    } finally { WebAssembly.compile = compile; WebAssembly.instantiate = instantiate; }
});
