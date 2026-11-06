import vm from 'node:vm';import {readFileSync} from 'node:fs';
import test from 'node:test';import assert from 'node:assert/strict';
globalThis.self={};const {memoryControlInput,controlSelection}=await import('./worker.js');
test('finite controls produce exact DTO and preserve outer string identity',()=>{
 for(const scenario of ['rw','none','ro','cross-none','host-copy','rw-store-readback','none-load','mprotect-hole-prefix']){
 const data={tid:'2',diagnosticProfile:'memory-controls-v1',memoryControlScenario:scenario,bin:'not-authority'};
 assert.deepEqual(memoryControlInput(data),{tid:2,mode:'memory-controls-v1',fixture:'scalar-i32-v1',scenario});assert.equal(data.tid,'2');}
});
test('invalid combinations and noncanonical task IDs fail closed',()=>{
 for(const data of [{diagnosticProfile:'memory-controls-v1',memoryControlScenario:'other'}, {memoryControlScenario:'rw'}, {diagnosticProfile:'capture-2-authored-layout-v1',memoryControlScenario:'rw'}]) assert.throws(()=>controlSelection(data));
 for(const tid of [2,'0','02','2147483648'])assert.throws(()=>memoryControlInput({tid,diagnosticProfile:'memory-controls-v1',memoryControlScenario:'rw'}));
 assert.equal(controlSelection({}),null);assert.equal(controlSelection({diagnosticProfile:'capture-2-authored-layout-v1'}),null);
});

test('actual outer controls snapshot finite names and never read task bin', async()=>{
 const source=readFileSync(new URL('./worker.js',import.meta.url),'utf8');
 const sent=[];let reads=0;let original;
 class WanixHandle { async readText(path){if(original)original.memoryControlScenario='invalid-after-snapshot';return path.endsWith('/env')?'':path.endsWith('/cmd')?'hello.wasm':'';} async readFile(){reads++;return new Uint8Array([1]);} }
 class CallBuffer {constructor(buffer){this.buffer=buffer;}}
 class Worker {postMessage(message){sent.push(message);}}
 const context=vm.createContext({self:{},console:{log(){},error(){}},SharedArrayBuffer,Uint8Array,Worker});
 const module=new vm.SourceTextModule(source,{context});
 await module.link(specifier=>{
  const names=specifier==='./lib.js'?['WanixHandle','CallBuffer','FileHandle']:['FileDescription'];
  return new vm.SyntheticModule(names,function(){for(const name of names)this.setExport(name,({WanixHandle,CallBuffer,FileHandle:class{},FileDescription:class{}})[name]);},{context});
 });
 await module.evaluate();
 for(const scenario of ['rw','none','ro','cross-none','host-copy','rw-store-readback','none-load','mprotect-hole-prefix']){
 const profile='memory-controls-v1'; original={tid:'2',port:{},url:'test-only-worker',diagnosticProfile:profile,memoryControlScenario:scenario};
  await context.self.onmessage({data:{worker:original}});
  const message=sent.at(-1);
  assert.equal(message.diagnosticProfile,profile);
  assert.equal(message.tid,"2");assert.equal(message.bin,null);assert.equal(message.memoryControlScenario,scenario);
  assert.equal(message.buffer.byteLength,16384);
 }
 assert.equal(sent.length,8);assert.equal(reads,0);
 original=null; await context.self.onmessage({data:{worker:{tid:"2",diagnosticProfile:"memory-controls-v1",memoryControlScenario:"invalid",port:{}}}});assert.equal(sent.length,8);assert.equal(reads,0);
});

