import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import vm from 'node:vm';
import test from 'node:test';

const root = process.env.CBOR_INPUT_ROOT;
assert.ok(root, 'CBOR_INPUT_ROOT must identify the reviewed original dependency tree');
const patches = JSON.parse(fs.readFileSync(new URL('./patches.json', import.meta.url)));
function context() {
 let calls = 0;
 const exports = {};
 const ctx = vm.createContext({ exports, module: { exports }, TextDecoder, TextEncoder, console,
  Function: function () { calls++; throw Error('dynamic Function forbidden'); } });
 return { ctx, exports, calls: () => calls };
}
function checks(api) {
 const structures = [['x'], ['__proto__'], ['wire']];
 const decoder = new api.Decoder({ structures });
 for (let i = 0; i < 8; i++) {
  const value = decoder.decode(Uint8Array.from([0xd9, 0xe0, 0, 0x81, i]));
  assert.equal(value.x, i);
 }
 const safe = decoder.decode(Uint8Array.from([0xd9, 0xe0, 1, 0x81, 7]));
 assert.equal(safe.__proto_, 7);
 assert.equal(Object.hasOwn(safe, '__proto__'), false);
 const mapped = new api.Decoder({ structures: [['wire']], keyMap: { local: 'wire' }, useRecords: true });
 for (let i = 0; i < 8; i++) assert.equal(mapped.decode(Uint8Array.from([0xd9,0xe0,0,0x81,i])).local, i);
 assert.equal(api.decode(Uint8Array.from([0x82, 1, 2])).join(','), '1,2');
 assert.throws(() => api.decode(Uint8Array.from([0x82, 1])));
 api.addExtension({ tag: 1000, decode: value => value + 1 });
 assert.equal(api.decode(Uint8Array.from([0xd9, 3, 0xe8, 4])), 5);
}
for (const patch of patches) test(`pinned no-eval ${patch.path}`, () => {
 const input = fs.readFileSync(path.join(root, patch.path), 'utf8');
 assert.equal(crypto.createHash('sha256').update(input).digest('hex'), patch.sha256);
 assert.equal(input.split(patch.old).length, 2);
 let source = input.replace(patch.old, patch.new);
 assert.doesNotMatch(source, /\bnew\s+Function\s*\(/);
 const c = context();
 if (patch.path.includes('/esm/')) {
  source = source.replace(/export (let|const|class|function) /g, '$1 ');
  source += '\nglobalThis.api = { Decoder, decode, addExtension };';
 } else source += '\nglobalThis.api = exports;';
 vm.runInContext(source, c.ctx);
 checks(c.ctx.api);
 assert.equal(c.calls(), 0);
});
test('supported cbor-x index-no-eval imports without Function and preserves records', () => {
 const c = context();
 vm.runInContext(fs.readFileSync(path.join(root, 'cbor-x/dist/index-no-eval.cjs'), 'utf8'), c.ctx);
 checks(c.exports);
 assert.equal(c.calls(), 0);
});
