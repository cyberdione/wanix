import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import {readFileSync} from 'node:fs';

test('actual task-worker handler preserves host diagnostic selector in sync postMessage', async()=>{
 const source=readFileSync(new URL('./worker.js',import.meta.url),'utf8');
 const sent=[];
 class WanixHandle { async readText(path){return path.endsWith('/env')?'':path.endsWith('/cmd')?'hello.wasm':'';} async readFile(){return new Uint8Array([1]);} }
 class CallBuffer {constructor(buffer){this.buffer=buffer;}}
 class Worker {postMessage(message){sent.push(message);}}
 const context=vm.createContext({self:{},console:{log(){},error(){}},SharedArrayBuffer,Uint8Array,Worker});
 const module=new vm.SourceTextModule(source,{context});
 await module.link(specifier=>{
  const names=specifier==='./lib.js'?['WanixHandle','CallBuffer','FileHandle']:['FileDescription'];
  return new vm.SyntheticModule(names,function(){for(const name of names)this.setExport(name,({WanixHandle,CallBuffer,FileHandle:class{},FileDescription:class{}})[name]);},{context});
 });
 await module.evaluate();
 for(const profile of ['capture-2-authored-layout-v1',undefined]){
  await context.self.onmessage({data:{worker:{tid:"2",port:{},url:'test-only-worker',diagnosticProfile:profile}}});
  const message=sent.at(-1);
  assert.equal(message.diagnosticProfile,profile);
  assert.equal(message.tid,"2");assert.deepEqual(Array.from(message.bin),[1]);
  assert.equal(message.buffer.byteLength,16384);
 }
 assert.equal(sent.length,2);
});

test('selected inner transport strictly normalizes Go task ID without mutating namespace data',async()=>{
 globalThis.self={};
 const {capture2TaskEvent}=await import('./worker.js');
 const event={data:{tid:'2',diagnosticProfile:'capture-2-authored-layout-v1',args:['hello']}};
 const normalized=capture2TaskEvent(event);
 assert.equal(normalized.data.tid,2);assert.equal(event.data.tid,'2');
 assert.equal(capture2TaskEvent({data:{tid:'2147483647'}}).data.tid,2147483647);
 for(const tid of [2,0,'0','01','+2','-2','2x',' 2','2147483648','999999999999999999999999',''])
  assert.throws(()=>capture2TaskEvent({data:{tid}}),TypeError);
});
