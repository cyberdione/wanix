import test from 'node:test';
import assert from 'node:assert/strict';
import {webcrypto} from 'node:crypto';
import {createCapture2Runner} from './capture2-runner.mjs';
globalThis.crypto ??= webcrypto;
globalThis.self = {};
const {createLinuxPersonality} = await import('./worker.js');
test('checked destination rejection does not consume stdin and trusted exit is injected',()=>{
 const fault={}; let denied=true; const data=new Uint8Array(64);
 const memory={byteLength:64,check(){if(denied)throw fault;},read:(a,n)=>data.slice(a,a+n),write:(a,b)=>data.set(b,a)};
 const p=createLinuxPersonality({data:{stdinText:'abc'}},{call(){}},memory,{identity:23,exit(code){throw code;}});
 assert.throws(()=>p.syscall(63,0,0,2),e=>e===fault);
 denied=false; assert.equal(p.syscall(63,0,0,2),2); assert.deepEqual([...data.slice(0,2)],[97,98]);
 assert.equal(p.syscall(96),23); assert.throws(()=>p.syscall(93,7),e=>e===7);
 p.cleanup();p.cleanup();
});
test('runner routes host-only authority, awaits run, classifies only private exit',async()=>{
 let cfg, cleaned=0, memoryCalls=0; const calls=[];
 const authority={byteLength:524288,syscall(){memoryCalls++;return 91;},copyin(){return new Uint8Array();},copyout(){},check(){}};
 const run=createCapture2Runner({extractWasm:b=>b,isCopyFault:()=>false,
 createLinuxPersonality:(_e,_c,m,o)=>({cleanup(){cleaned++;},syscall(n,...a){assert.equal(m.byteLength,524288);return n===93?o.exit(a[0]):o.identity;}}),
 createPairedLoader:c=>{cfg=c;return{async load(){return{async run(){
 const call=c.hostImports['ferrule.linux.syscall'].syscall;
 assert.equal(call(authority,222,0,0,0,0,0,0),91);assert.equal(call(authority,96),23);
 assert.equal(c.exitCode({kind:'exit'}),undefined);
 try{call(authority,93,7);}catch(e){assert.equal(c.exitCode(e),7);}
 await c.cleanup();return{kind:'exit',code:7};
 }}}}}});
 const result=await run({data:{tid:23,args:['hello'],env:[],bin:new Uint8Array([1])}},{call:(...a)=>calls.push(a)});
 assert.equal(result.kind,'exit');assert.equal(cleaned,1);assert.equal(memoryCalls,1);assert.deepEqual(calls,[['exit',{code:7}]]);
});
test('admission rejection never invokes personality or reports success',async()=>{
 const run=createCapture2Runner({extractWasm:b=>b,isCopyFault:()=>false,createLinuxPersonality(){assert.fail();},createPairedLoader:()=>({load(){throw Error('reject');}})});
 await assert.rejects(run({data:{tid:1,args:['h'],env:[],bin:new Uint8Array([1])}},{call(){assert.fail();}}),/reject/);
});
