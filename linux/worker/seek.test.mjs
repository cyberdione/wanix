import { test } from 'node:test';
import assert from 'node:assert/strict';
globalThis.self = {};
const { runLinux } = await import('./worker.js');

// Stub module loading only: calls below enter the production syscall dispatcher.
async function fixture(options, check) {
    const compile = WebAssembly.compile, instantiate = WebAssembly.instantiate;
    const memory = new WebAssembly.Memory({ initial: 1 });
    let size = 100, nextHandle = 10;
    const calls = [];
    WebAssembly.compile = async () => ({});
    WebAssembly.instantiate = async (_, imports) => ({ exports: { memory, _start() {
        const syscall = imports['ferrule.linux.syscall'].syscall;
        const bytes = new Uint8Array(memory.buffer), view = new DataView(memory.buffer);
        check({ syscall, bytes, view, calls, setSize: n => { size = n; } });
    } } });
    try {
        await runLinux({ data: {
            bin: Uint8Array.of(0,97,115,109,1,0,0,0,0), args: [], env: [], tid: 'seek', ...options,
        } }, { call(method, arg) {
            calls.push([method, arg]);
            switch (method) {
            case 'path_open': return nextHandle++;
            case 'path_open_existing': return {ok: true, fd: nextHandle++};
            case 'path_touch': case 'fd_close': case 'exit': return true;
            case 'path_size': return size;
            case 'fd_read': return Uint8Array.of(0x42);
            default: throw new Error(`unexpected RPC ${method}`);
            }
        } });
    } finally { WebAssembly.compile = compile; WebAssembly.instantiate = instantiate; }
}

function open(syscall, bytes) {
    bytes.set(new TextEncoder().encode('out.txt\0'), 128);
    const fd = syscall(56, -100, 128, 0, 0);
    assert.ok(fd >= 3);
    return fd;
}

// Independent raw hi/lo words and result bytes, never the implementation's decoder.
test('_llseek5arg hi/lo order, sign, exact safe backend boundary and CUR/END', async () => {
    await fixture({}, ({syscall: s, bytes: b, view: v, setSize}) => {
        const fd = open(s, b);
        const seek = (hi, lo, whence, expected, errno = 0) => {
            b.fill(0xa5, 255, 265);
            assert.equal(s(62, fd, hi, lo, 256, whence), errno);
            if (errno) assert.deepEqual([...b.subarray(255, 265)], Array(10).fill(0xa5));
            else {
                assert.equal(v.getBigInt64(256, true), expected);
                assert.equal(b[255], 0xa5); assert.equal(b[264], 0xa5);
            }
        };
        seek(0, -1, 0, 4294967295n);
        assert.deepEqual([...b.subarray(256, 264)], [255,255,255,255,0,0,0,0]);
        seek(1, 7, 0, 4294967303n);
        seek(-1, -1, 1, 4294967302n);
        seek(-1, -1, 0, null, -22);
        seek(0, 0, 1, 4294967302n);
        seek(-1, -1, 2, 99n);
        seek(-1, -101, 2, null, -22);
        seek(0, 0, 1, 99n);
        seek(0x1fffff, -1, 0, 9007199254740991n);
        seek(0x200000, 0, 0, null, -75);
        seek(0, 0, 1, 9007199254740991n);
        seek(0, 1, 1, null, -75);
        seek(0, 8, 0, 8n);
        for (const w of [3, 4, 5, -1]) seek(0, 0, w, null, -22);
        seek(0, 0, 1, 8n);
        setSize(-5); seek(0, 0, 2, null, -5);
        seek(0, 0, 1, 8n);
    });
});

test('seek precedes result copy: EFAULT moves offset; seek errors preserve canaries', async () => {
    await fixture({}, ({syscall: s, bytes: b, view: v, calls, setSize}) => {
        const fd = open(s, b), end = b.length;
        b.fill(0xcc, end - 8);
        assert.equal(s(62, fd, 0, 10, end - 4, 0), -14);
        assert.deepEqual([...b.subarray(end - 8)], Array(8).fill(0xcc));
        assert.equal(s(62, fd, 0, 0, 256, 1), 0);
        assert.equal(v.getBigInt64(256, true), 10n);
        assert.equal(s(63, fd, 512, 1), 1);
        assert.equal(calls.findLast(([m]) => m === 'fd_read')[1].at, 10);
        for (const ptr of [end - 4, -1]) {
            assert.equal(s(62, fd, -1, -1, ptr, 0), -22);
            assert.equal(s(62, fd, 0, 0, ptr, 3), -22);
            setSize(-5);
            assert.equal(s(62, fd, 0, 0, ptr, 2), -5);
        }
        assert.equal(s(62, fd, 0, 0, 256, 1), 0);
        assert.equal(v.getBigInt64(256, true), 11n);
        // Exact memory-end destination succeeds, and no neighboring byte changes.
        b[end - 9] = 0xa5;
        assert.equal(s(62, fd, 0, 23, end - 8, 0), 0);
        assert.equal(v.getBigInt64(end - 8, true), 23n);
        assert.equal(b[end - 9], 0xa5);
        assert.equal(s(62, fd, 0, 24, -1, 0), -14);
        assert.equal(s(62, fd, 0, 0, 256, 1), 0);
        assert.equal(v.getBigInt64(256, true), 24n);
    });
});

test('configured streams/text are ESPIPE, absent descriptors EBADF; whence ordering', async () => {
    for (const options of [{stdin: 'in', stdout: 'out', stderr: 'err'}, {stdinText: ''}, {}]) {
        await fixture(options, ({syscall: s, bytes: b}) => {
            b.fill(0xa5, 256, 264);
            for (const fd of [0, 1, 2, 99]) {
                const valid = fd === 0 ? options.stdin || options.stdinText !== undefined
                    : fd === 1 ? options.stdout : fd === 2 ? options.stderr : false;
                for (const w of [0, 1, 2, 3, 4]) assert.equal(s(62, fd, 0, 0, -1, w), valid ? -29 : -9);
                for (const w of [-1, 5]) assert.equal(s(62, fd, 0, 0, -1, w), valid ? -22 : -9);
            }
            assert.deepEqual([...b.subarray(256, 264)], Array(8).fill(0xa5));
        });
    }
});
