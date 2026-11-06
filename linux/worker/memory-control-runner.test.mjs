import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import fs from 'node:fs';
const source = fs.readFileSync(new URL('./memory-control-runner.mjs', import.meta.url), 'utf8');
async function setup() {
  const loads=[], capabilities=[];
  const context=vm.createContext({TextEncoder,WebAssembly:{instantiate(){throw Error('actual instantiate forbidden');}}});
  const dependency=new vm.SyntheticModule(['createMemoryControlPairedLoader'],function(){
    this.setExport('createMemoryControlPairedLoader', options=>{
      capabilities.push(options);
      return {async load(selector){loads.push(selector);return {async run(){options.cleanup();return ['none','ro','cross-none','none-load','mprotect-hole-prefix'].includes(selector.scenario)?{kind:'memory-fault',error:{code:'EFAULT'}}:{kind:'return',value:selector.scenario==='rw-store-readback'?99:71};}};}};
    });
  },{context});
  const module=new vm.SourceTextModule(source,{context});
  await module.link(specifier=>{assert.equal(specifier,'./memory-control-runtime/memory-control-paired-loader.mjs');return dependency;});
  await module.evaluate();return {run:module.namespace.runMemoryControl,loads,capabilities};
}
test('eight fixed scenarios use private trusted capabilities and bounded reports',async()=>{
 const {run,loads,capabilities}=await setup();
 for(const scenario of ['rw','none','ro','cross-none','host-copy','rw-store-readback','none-load','mprotect-hole-prefix']){
  const calls=[];const report=await run({tid:7,mode:'memory-controls-v1',fixture:'scalar-i32-v1',scenario},{call:(method,params)=>{assert.notEqual(method,'fd_flush');params.method=method;calls.push([method,params]);return method==='path_open'?9:method==='fd_write'?params.data.length:undefined;}});
  assert.equal(report.tid,7);assert.equal(report.cleanups,1);assert.equal(calls[0][0],'path_open');assert.equal(calls.at(-1)[0],'exit');assert.ok(!('method' in report));
 }
 assert.equal(loads.length,8);assert.equal(capabilities.length,8);
});
test('reject invalid inner identity selectors and injected capabilities before loader creation',async()=>{
 const {run,loads,capabilities}=await setup();const base={tid:1,mode:'memory-controls-v1',fixture:'scalar-i32-v1',scenario:'rw'};
 for(const bad of [{tid:'1'},{tid:0},{tid:2147483648},{scenario:'arbitrary'},{fixture:'guest'},{instantiate:()=>{}},{policy:{}}])
  await assert.rejects(run({...base,...bad},{call(){throw Error('no call');}}));
 assert.equal(loads.length,0);assert.equal(capabilities.length,0);
});

test('partial writes complete and report failures never publish exit0',async()=>{
 for(const failure of [null,0,-1,{ioError:5},'throw','close','write-and-close']){
  const {run}=await setup();let exits=0,writes=0;const chunks=[];
  const caller={call(method,params){params.method=method;
   if(method==='path_open')return 9;
   if(method==='fd_write'){writes++;if(failure==='throw'||failure==='write-and-close')throw Error('write-primary');if(failure!==null&&failure!=='close')return failure;const n=Math.min(7,params.data.length);chunks.push(...params.data.slice(0,n));return n;}
   if(method==='fd_flush')throw Error('flush forbidden for stdout stream');
   if(method==='fd_close'&&['close','write-and-close'].includes(failure))throw Error('close-primary');
   if(method==='exit')exits++;
  }};
  const pending=run({tid:7,mode:'memory-controls-v1',fixture:'scalar-i32-v1',scenario:'rw'},caller);
  if(failure===null){const report=await pending;assert.equal(exits,1);assert.ok(writes>1);assert.equal(JSON.parse(new TextDecoder().decode(Uint8Array.from(chunks))).tid,7);assert.ok(!('method' in report));}
  else{await assert.rejects(pending,failure==='write-and-close'?/write-primary/:undefined);assert.equal(exits,0);}
 }
});
