// Diagnostic Node harness: fake Wasm startup, actual imported syscall closure.
import assert from "node:assert/strict";
globalThis.self = {};
const originalCompile = WebAssembly.compile;
const originalInstantiate = WebAssembly.instantiate;
const {runLinux} = await import("./worker.js"); // First import failure must STOP.
const cases = [
 {size:37, pointer:1024, expected:0},
 {size:0, pointer:1024, expected:0},
 {size:Number.MAX_SAFE_INTEGER, pointer:1024, expected:0},
 ...[null,undefined,-1,1.5,{__wanix_error:"backend"}].map(size=>({size,pointer:1024,expected:-5})),
 {size:0,pointer:0xffffffff,expected:-14},
 {size:0,pointer:"last",expected:0},
 {size:0,pointer:"over",expected:-14},
 {throws:true,pointer:1024},
];
try {
 for (const c of cases) {
  const memory = new WebAssembly.Memory({initial:1,maximum:256});
  const calls=[];
  const backendError=new Error("synthetic backend failure");
  const caller={call(method,args){calls.push([method,args]);if(method==="path_size"){if(c.throws)throw backendError;return c.size;}if(method==="exit")return;throw Error("unexpected backend call");}};
  WebAssembly.compile=async bytes=>({diagnostic:true});
  WebAssembly.instantiate=async (_module,imports)=>({exports:{memory,_start(){
   const bytes=new Uint8Array(memory.buffer);
   bytes.set(new TextEncoder().encode("out.txt\0"),64);
   const pointer=c.pointer==="last"?bytes.length-256:c.pointer==="over"?bytes.length-255:c.pointer;
   if(pointer===1024)bytes.fill(0xaa,1023,1281);
   const before=bytes.slice();
   if(c.throws){assert.throws(()=>imports["ferrule.linux.syscall"].syscall(291,-100,64,0,0x7ff,pointer,0),error=>error===backendError);assert.deepEqual(bytes,before);return;}
   const result=imports["ferrule.linux.syscall"].syscall(291,-100,64,0,0x7ff,pointer,0);
   assert.equal(result,c.expected);
   if(result!==0){assert.deepEqual(bytes,before);return;}
   const view=new DataView(memory.buffer);
   assert.equal(view.getUint32(pointer,true),0x200);
   assert.equal(view.getBigUint64(pointer+40,true),BigInt(c.size));
   assert.equal(view.getUint16(pointer+28,true),0);
   assert.equal(view.getBigUint64(pointer+32,true),0n);
   if(pointer===1024){assert.equal(bytes[1023],0xaa);assert.equal(bytes[1280],0xaa);}
   assert.equal(bytes[0x7ff],before[0x7ff]); // Mask is not output pointer.
  }}});
  await runLinux({data:{tid:"diagnostic",bin:new Uint8Array([0,97,115,109,1,0,0,0,0]),args:[],env:[]}},caller);
  assert.equal(calls.filter(([method])=>method==="path_size").length,1);
 }
 console.log("actual statx dispatch diagnostic cases PASS; fake Wasm startup, producer/flags/path not acceptance");
} finally {
 WebAssembly.compile=originalCompile;
 WebAssembly.instantiate=originalInstantiate;
 delete globalThis.self;
}
