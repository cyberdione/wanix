import assert from "node:assert/strict";
import { exactStatSize, copySizeStatx } from "./statx-core.js";
for (const value of [0,0n,12,12n,BigInt(Number.MAX_SAFE_INTEGER)]) assert.equal(exactStatSize({Size:value}),Number(value));
for (const value of [null,undefined,{}, {Size:-1},{Size:-1n},{Size:1.5},{Size:"0"},{Size:BigInt(Number.MAX_SAFE_INTEGER)+1n}]) assert.throws(()=>exactStatSize(value));
const memory={buffer:new ArrayBuffer(600)};
const bytes=new Uint8Array(memory.buffer);bytes.fill(0xaa);
assert.equal(copySizeStatx(memory,100,37),0);
const view=new DataView(memory.buffer);
assert.equal(view.getUint32(100,true),0x200);
assert.equal(view.getBigUint64(140,true),37n);
assert.equal(bytes[99],0xaa);assert.equal(bytes[356],0xaa);
assert.equal(view.getUint16(128,true),0);assert.equal(view.getBigUint64(132,true),0n);
for (const value of [null,undefined,-1,NaN,{}, {__wanix_error:"backend"}]) {
 const before=bytes.slice();assert.equal(copySizeStatx(memory,100,value),-5);assert.deepEqual(bytes,before);
}
assert.equal(copySizeStatx(memory,344,0),0);
for (const pointer of [345,0xffffffff]) {const before=bytes.slice();assert.equal(copySizeStatx(memory,pointer,0),-14);assert.deepEqual(bytes,before);}
// Source gate pins output argument independently of request mask, no full worker run.
import {readFileSync} from "node:fs";
const source=readFileSync(new URL("./worker.js",import.meta.url),"utf8");
const block=source.slice(source.indexOf("case 291:"),source.indexOf("case 35:"));
assert.match(block,/buf = a\[4\]/);assert.doesNotMatch(block,/0x7ff|42n|4096/);
console.log("finite statx source/helper cases PASS; not flags/path/runtime acceptance");
