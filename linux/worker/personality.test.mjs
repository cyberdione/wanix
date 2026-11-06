import { test } from 'node:test';
import assert from 'node:assert/strict';
globalThis.self = {};
const { createLinuxPersonality } = await import('./worker.js');

function fixture(text = 'abc') {
    const bytes = new Uint8Array(512).fill(0xcc), accesses = [], calls = [];
    const check = (at, size) => {
        assert.ok(Number.isInteger(at) && at >= 0 && size >= 0 && at + size <= bytes.length);
        accesses.push([at, size]);
    };
    const memory = {
        get byteLength() { return bytes.length; },
        read(at, size) { check(at, size); return bytes.slice(at, at + size); },
        write(at, data) { check(at, data.length); bytes.set(data, at); },
    };
    const personality = createLinuxPersonality({data: {stdinText: text}}, {
        call(method, params) { calls.push([method, params]); },
    }, memory);
    return {bytes, accesses, calls, ...personality};
}

test('checked-memory-only dispatch preserves partial reads and bounds without instantiation', () => {
    const p = fixture();
    assert.equal(p.syscall(63, 0, 510, 3), -14);
    assert.deepEqual(p.accesses, []);
    assert.equal(p.syscall(63, 0, 10, 2), 2);
    assert.deepEqual([...p.bytes.slice(9, 13)], [0xcc, 97, 98, 0xcc]);
    assert.equal(p.syscall(63, 0, 20, 8), 1);
    assert.equal(p.syscall(63, 0, 20, 8), 0);
    assert.equal(p.syscall(222, 0, 4096, 3, 0x22, -1, 0), -38);
    assert.equal(p.syscall(166, 0), 0o22);
    assert.equal(fixture().syscall(166, 0), 0o22);
    assert.throws(() => p.syscall(93, 7), err => err.kind === 'exit');
    p.finish();
    assert.deepEqual(p.calls, [['exit', {code: 7}]]);
});

test('vectors use checked scalar copies, not a backing buffer', () => {
    const p = fixture('xyz');
    const view = new DataView(p.bytes.buffer);
    view.setUint32(32, 100, true); view.setUint32(36, 2, true);
    view.setUint32(40, 200, true); view.setUint32(44, 2, true);
    assert.equal(p.syscall(65, 0, 32, 2), 3);
    assert.deepEqual([...p.bytes.slice(100, 102)], [120, 121]);
    assert.equal(p.bytes[200], 122);
    assert.equal(p.bytes[201], 0xcc);
});
