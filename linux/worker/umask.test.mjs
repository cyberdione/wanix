import { test } from 'node:test';
import assert from 'node:assert/strict';
globalThis.self = {};
const { runLinux } = await import('./worker.js');

test('umask actual dispatch exchanges signed i32 bits and resets every parentless launch', async () => {
    const compile = WebAssembly.compile, instantiate = WebAssembly.instantiate;
    const dispatches = [];
    let start;
    WebAssembly.compile = async () => ({});
    WebAssembly.instantiate = async (_, imports) => {
        const syscall = imports['ferrule.linux.syscall'].syscall;
        dispatches.push(syscall);
        const memory = new WebAssembly.Memory({initial: 1});
        return {exports: {memory, _start() { start(syscall, memory); }}};
    };
    const event = {data: {bin: Uint8Array.of(0,97,115,109,1,0,0,0,0),
        tid: 'umask', args: [], env: []}};
    const calls = [];
    const caller = {call(method, args) {
        assert.equal(method, 'exit', 'umask/open rejection must not issue backend RPC');
        calls.push(args);
    }};
    try {
        start = (s, memory) => {
            // This explicitly selected Linux-bootstrap profile is not inheritance.
            assert.equal(s(166, 0), 0o022);
            assert.equal(s(166, -1), 0);
            assert.equal(s(166, -2147483648), 0o777);
            assert.equal(s(166, 0x400001a5), 0);
            assert.equal(s(166, -2147483648 | 0o123), 0o645);
            // Trailing pointer-looking values are ignored; memory is untouched.
            const bytes = new Uint8Array(memory.buffer);
            const before = bytes.slice();
            assert.equal(s(166, 0, -1, -2147483648, -1, -1, -1), 0o123);
            assert.deepEqual(bytes, before);
            assert.equal(s(166, 0o077), 0);
            bytes.set(new TextEncoder().encode('out.txt\0'), 128);
            for (const flags of [128, 1024, 2|1024]) {
                assert.equal(s(56, -100, 128, flags, 0o666), -38);
            }
        };
        await runLinux(event, caller);
        // Reusing the same event/caller is still a fresh execution. Keep the old
        // closure live to prove independent state, not just a final reset.
        start = s => {
            assert.equal(s(166, 0o002), 0o022);
            assert.equal(dispatches[0](166, 0o777), 0o077);
            assert.equal(s(166, 0), 0o002);
        };
        await runLinux(event, caller);
        start = s => assert.equal(s(166, 0), 0o022);
        await runLinux(event, caller);
        assert.deepEqual(calls, [{code: 0}, {code: 0}, {code: 0}]);
    } finally {
        WebAssembly.compile = compile;
        WebAssembly.instantiate = instantiate;
    }
});
