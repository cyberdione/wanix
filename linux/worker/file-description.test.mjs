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
    return new FileDescription(handle);
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
