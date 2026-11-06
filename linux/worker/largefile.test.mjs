import test from 'node:test';
import assert from 'node:assert/strict';
import {CallBuffer, WanixHandle} from './lib.js';
globalThis.self = {};
const {runLinux, messageHandler} = await import('./worker.js');

test('LARGEFILE actual dispatch and CallBuffer carry bounded policy and typed overflow', async () => {
    const compile=WebAssembly.compile, instantiate=WebAssembly.instantiate, post=globalThis.postMessage;
    const cb=new CallBuffer(new SharedArrayBuffer(1024)), memory=new WebAssembly.Memory({initial:1});
    const calls=[]; let errno;
    globalThis.postMessage=req=>{calls.push(req);cb.respond(errno?{ok:false,errno}:{ok:true,fd:0});};
    WebAssembly.compile=async()=>({});
    WebAssembly.instantiate=async(_,imports)=>({exports:{memory,_start(){
        const s=imports['ferrule.linux.syscall'].syscall;
        new Uint8Array(memory.buffer).set(new TextEncoder().encode('out.txt\0'),128);
        for(const large of [0,0x8000]) for(const flags of [0,1,2,0o100,0o1101,0o1102]) {
            assert.ok(s(56,-100,128,large|flags,0o600)>=3);
            assert.equal(calls.at(-1).maxExistingSize,large?Number.MAX_SAFE_INTEGER:0x7fffffff);
            assert.equal(calls.at(-1).truncate,!!(flags&0o1000));
        }
        errno=75;assert.equal(s(56,-100,128,0x8002,0),-75);
        for(const bit of [0x400,0x10000,0x20000,0x80000]) {
            const n=calls.length; assert.equal(s(56,-100,128,0x8000|bit,0),-38);assert.equal(calls.length,n);
        }
    }}});
    try {await runLinux({data:{bin:Uint8Array.of(0,97,115,109,1,0,0,0,0),tid:'test',args:[],env:[]}},cb);}
    finally {WebAssembly.compile=compile;WebAssembly.instantiate=instantiate;globalThis.postMessage=post;}
});

test('broker forwards exact ceiling through real adapter and response codec, never uses lstat size as gate',async()=>{
    const h=Object.create(WanixHandle.prototype), cb=new CallBuffer(new SharedArrayBuffer(1024));
    let seen, code='EOVERFLOW';
    h.peer={async call(method,args){
        if(method==='LstatResult') return {value:{OK:true,Value:{Size:0,Mode:0x8000,IsDir:false,ModTime:0}}};
        assert.equal(method,'OpenFileResult');seen=args;return {value:{OK:false,Error:{Code:code}}};
    }};
    const handler=messageHandler(h,cb,'test');
    const post=globalThis.postMessage;
    const request=async(size)=>{
        seen=undefined;
        await handler({data:{method:'path_open_existing',path:'#task/test/w/out.txt',access:'write',truncate:true,maxExistingSize:size}});
        const encoded=cb.data.slice(0,cb.len[0]);
        globalThis.postMessage=()=>{cb.data.set(encoded);cb.len[0]=encoded.length;Atomics.store(cb.ctrl,0,1);};
        try{return cb.call('decode',{});}finally{globalThis.postMessage=post;}
    };
    for(const size of [0,0x7ffffffe,0x7fffffff,0x80000000,Number.MAX_SAFE_INTEGER]) {
        assert.deepEqual(await request(size),{ok:false,errno:75});
        assert.equal(seen.MaxExistingSize,BigInt(size));assert.equal(seen.Truncate,true);
    }
    for(const size of [undefined,null,-1,1.5,NaN,Infinity,2**53,1n,'1']) {
        assert.deepEqual(await request(size),{ok:false,errno:22});assert.equal(seen,undefined);
    }
    code='EEXIST';assert.deepEqual(await request(0),{ok:false,errno:17});
});
