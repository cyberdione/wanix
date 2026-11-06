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
 {size:0,pointer:0xffffffff,expected:-14,producer:0}, // EFAULT precedes any producer call
 {size:0,pointer:"last",expected:0},
 {size:0,pointer:"over",expected:-14,producer:0},
 {throws:true,pointer:1024},
];
try {
 for (const c of cases) {
  const memory = new WebAssembly.Memory({initial:1,maximum:256});
  const calls=[];
  const backendError=new Error("synthetic backend failure");
  const caller={call(method,args){calls.push([method,args]);if(method==="path_stat"){if(c.throws)throw backendError;return {ok:true,stat:{Mode:0x8000,IsDir:false,Size:c.size}};}if(method==="exit")return;throw Error("unexpected backend call");}};
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
   // Rebased onto main's record: 0x201 header, file type at 28, safe size at 40.
   assert.equal(view.getUint32(pointer,true),0x201);
   assert.equal(view.getUint16(pointer+28,true),0x8000);
   assert.equal(view.getBigUint64(pointer+40,true),BigInt(c.size));
   assert.equal(view.getBigUint64(pointer+32,true),0n);
   if(pointer===1024){assert.equal(bytes[1023],0xaa);assert.equal(bytes[1280],0xaa);}
   assert.equal(bytes[0x7ff],before[0x7ff]); // Mask is not output pointer.
  }}});
  await runLinux({data:{tid:"diagnostic",bin:new Uint8Array([0,97,115,109,1,0,0,0,0]),args:[],env:[]}},caller);
  assert.equal(calls.filter(([method])=>method==="path_stat").length,c.producer===undefined?1:c.producer);
 }
 // FD routing uses actual openat population of the real handler's openFiles map.
 for (const backendFD of [0, 0xffffffff, 0x100000000, 0n]) {
  const memory=new WebAssembly.Memory({initial:1,maximum:256});
  const calls=[];
  const caller={call(method,args){
   calls.push([method,args]);
   if(method==="path_touch")return true;
   if(method==="path_open_existing")return {ok:true,fd:backendFD};
   if(method==="fd_stat_size")return 9;
   if(method==="path_stat")return {ok:true,stat:{Mode:0x8000,IsDir:false,Size:11}};
   if(method==="fd_close" || method==="exit")return true;
   throw Error("unexpected FD test call");
  }};
  // Main's typed openat rejects backend fds outside the safe-integer envelope.
  const envelopeOk=typeof backendFD==="number" && Number.isSafeInteger(backendFD) && backendFD>=0;
  WebAssembly.compile=async()=>({diagnostic:true});
  WebAssembly.instantiate=async(_module,imports)=>({exports:{memory,_start(){
   const bytes=new Uint8Array(memory.buffer);
   bytes.set(new TextEncoder().encode("out.txt\0"),64);bytes[80]=0;
   const syscall=imports["ferrule.linux.syscall"].syscall;
   const guest=syscall(56,-100,64,0,0,0,0);
   assert.equal(guest,envelopeOk?3:-5);
   if(!envelopeOk)return;
   assert.notEqual(guest,backendFD);
   // Routing admits only validBackendFD values; wider numbers fail as -5.
   const routed=typeof backendFD==="number" && backendFD<=0xffffffff;
   for(const pathname of [0,80]) {
    const count=calls.length;
    assert.equal(syscall(291,guest,pathname,0x1000,0x7ff,1024,0),routed?0:-5);
    const delta=calls.slice(count);
    assert.equal(delta.filter(([method])=>method==="path_stat").length,0);
    assert.equal(delta.filter(([method])=>method==="fd_stat_size").length,routed?1:0);
    if(routed){assert.equal(delta[0][1].fd,backendFD);assert.equal(new DataView(memory.buffer).getBigUint64(1064,true),9n);}
   }
   // Nonempty AT_EMPTY_PATH keeps main's unsupported -38; plain path stat flows
   // from AT_FDCWD. A relative path with an open dirfd stays -20 (main's policy).
   assert.equal(syscall(291,guest,64,0x1000,0x7ff,1024,0),-38);
   assert.equal(syscall(291,guest,64,0,0x7ff,1024,0),-20);
   assert.equal(syscall(291,-100,64,0,0x7ff,1024,0),0);
   assert.equal(calls.at(-1)[0],"path_stat");
   assert.equal(syscall(291,-100,0,0x1000,0x7ff,1024,0),-95);
   assert.equal(syscall(291,0,0,0x1000,0x7ff,1024,0),-95);
   assert.equal(syscall(57,guest,0,0,0,0,0),0);
   const count=calls.length;
   assert.equal(syscall(291,guest,0,0x1000,0x7ff,1024,0),-9);
   assert.equal(calls.length,count);
  }}});
  await runLinux({data:{tid:"diagnostic",stdinText:"",bin:new Uint8Array([0,97,115,109,1,0,0,0,0]),args:[],env:[]}},caller);
  assert.equal(calls.filter(([method])=>method==="fd_close").length,envelopeOk?1:0);
 }
 console.log("actual statx dispatch diagnostic cases PASS; fake Wasm startup, producer/flags/path not acceptance");
} finally {
 WebAssembly.compile=originalCompile;
 WebAssembly.instantiate=originalInstantiate;
 delete globalThis.self;
}
