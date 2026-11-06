import { test } from 'node:test';
import assert from 'node:assert/strict';
import { FileDescription } from './file-description.js';
function fixture() {
    const bytes = new Uint8Array(64); let size = 0;
    const handle = {
        read(out, {at}) { const n = Math.max(0, Math.min(out.length, size-at)); out.set(bytes.subarray(at, at+n)); return n; },
        write(data, {at}) { bytes.set(data, at); size = Math.max(size, at+data.length); return data.length; },
        getSize() { return size; }, close() {},
    };
    return new FileDescription(handle, 2);
}
test('sequential writes, seek, short reads and EOF share one offset', () => {
    const f = fixture();
    assert.equal(f.write(Uint8Array.of(1,2)),2);
    assert.equal(f.write(Uint8Array.of(3)),1);
    assert.equal(f.seek(0,0),0);
    const a = new Uint8Array(2); assert.equal(f.read(a),2); assert.deepEqual([...a],[1,2]);
    const b = new Uint8Array(4); assert.equal(f.read(b),1); assert.equal(b[0],3);
    assert.equal(f.read(b),0);
    assert.equal(f.seek(-2,2),1); assert.equal(f.read(a),2); assert.deepEqual([...a],[2,3]);
});
test('invalid seek leaves offset unchanged; seeking beyond EOF is allowed', () => {
    const f = fixture(); assert.equal(f.seek(4,0),4);
    assert.equal(f.seek(-5,1),-22); assert.equal(f.offset,4);
    assert.equal(f.seek(0,3),-22); assert.equal(f.offset,4);
    assert.equal(f.seek(Number.MAX_SAFE_INTEGER+1,0),-75); assert.equal(f.offset,4);
    assert.equal(f.read(new Uint8Array(1)),0);
});

for (const operation of ['read', 'write']) {
    test(`${operation} rejects unsafe requested end before backend access`, () => {
        let calls = 0;
        const f = new FileDescription({ [operation]() { calls++; return 0; } }, 2);
        assert.equal(f.seek(Number.MAX_SAFE_INTEGER - 1, 0), Number.MAX_SAFE_INTEGER - 1);
        // Even an EOF/short result must not be guessed before I/O.
        const buffer = Uint8Array.of(17, 18);
        assert.equal(f[operation](buffer), -75);
        assert.equal(calls, 0);
        assert.equal(f.offset, Number.MAX_SAFE_INTEGER - 1);
        assert.deepEqual([...buffer], [17, 18]);
        assert.equal(f.seek(1, 1), Number.MAX_SAFE_INTEGER);
        assert.equal(f[operation](new ArrayBuffer(1)), -75);
        assert.equal(calls, 0);
        assert.equal(f.offset, Number.MAX_SAFE_INTEGER);
    });
    test(`${operation} advances only by actual admitted transfer count`, () => {
        const calls = [];
        let result = 1;
        const f = new FileDescription({ [operation](buffer, options) {
            calls.push([buffer.byteLength, options.at]);
            if (operation === 'read' && result > 0) buffer[0] = 42;
            return result;
        } }, 2);
        f.seek(Number.MAX_SAFE_INTEGER - 2, 0);
        const buffer = new Uint8Array(2);
        assert.equal(f[operation](buffer), 1);
        assert.equal(f.offset, Number.MAX_SAFE_INTEGER - 1);
        if (operation === 'read') assert.deepEqual([...buffer], [42, 0]);
        result = 0;
        assert.equal(f[operation](new Uint8Array(1)), 0);
        assert.equal(f.offset, Number.MAX_SAFE_INTEGER - 1);
        result = -5;
        assert.equal(f[operation](new Uint8Array(1)), -5);
        assert.equal(f.offset, Number.MAX_SAFE_INTEGER - 1);
        result = 1;
        assert.equal(f[operation](new Uint8Array(1)), 1);
        assert.equal(f.offset, Number.MAX_SAFE_INTEGER);
        result = 0;
        assert.equal(f[operation](new ArrayBuffer(0)), 0);
        assert.equal(f.offset, Number.MAX_SAFE_INTEGER);
        assert.deepEqual(calls, [[2, Number.MAX_SAFE_INTEGER - 2],
            [1, Number.MAX_SAFE_INTEGER - 1], [1, Number.MAX_SAFE_INTEGER - 1],
            [1, Number.MAX_SAFE_INTEGER - 1], [0, Number.MAX_SAFE_INTEGER]]);
    });
    test(`${operation} rejects malformed backend counts without advancing`, () => {
        let result;
        const f = new FileDescription({ [operation]() { return result; } }, 2);
        f.seek(Number.MAX_SAFE_INTEGER - 1, 0);
        for (result of [2, Number.MAX_SAFE_INTEGER + 1, NaN, Infinity, 0.5, undefined, null]) {
            assert.equal(f[operation](new Uint8Array(1)), -5);
            assert.equal(f.offset, Number.MAX_SAFE_INTEGER - 1);
        }
    });
}

for (const [mode, operation] of [[0, 'write'], [1, 'read'], [3, 'read'], [3, 'write']]) {
    test(`mode ${mode} denies ${operation} including zero length`, () => {
        let calls = 0;
        const f = new FileDescription({[operation]() { calls++; return 0; }}, mode);
        for (const size of [0, 1]) assert.equal(f[operation](new Uint8Array(size)), -9);
        assert.equal(calls, 0); assert.equal(f.offset, 0);
    });
}
