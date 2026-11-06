import test from 'node:test';
import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import {CallBuffer, WanixHandle} from './lib.js';
globalThis.self = {};
const {messageHandler, runLinux} = await import('./worker.js');

test('atomic existing open uses real handle, API fd-zero fixture and broker codec', async () => {
    const fixtures = JSON.parse(await readFile(new URL('../../api/filesystem_result.fixture.json', import.meta.url)));
    const cb = new CallBuffer(new SharedArrayBuffer(1024));
    const saved = globalThis.postMessage;
    const decode = hex => {
        globalThis.postMessage = () => {const b=Buffer.from(hex,'hex'); cb.data.set(b); cb.len[0]=b.length; Atomics.store(cb.ctrl,0,1);};
        try {return cb.call('fixture',{});} finally {globalThis.postMessage=saved;}
    };
    const zero = decode(fixtures.openZero);
    const h=Object.create(WanixHandle.prototype), calls=[];
    let content='original', error=null, regular=true;
    h.peer={async call(method,args) {
        calls.push([method,args]);
        if(method==='LstatResult') return {value:{OK:true,Value:{Mode:regular?0x8000:0xa000,IsDir:false,Size:content.length,ModTime:0}}};
        if(method==='OpenFileResult') {
            if(error) return {value:{OK:false,Error:{Code:error}}};
            assert.equal(args.Create,false); assert.equal(args.Exclusive,false); assert.equal(args.Perm,0);
            if(args.Truncate) content='';
            return {value:zero};
        }
        if(method==='OpenResult') return {value:zero};
        assert.fail(method);
    }};
    const broker=messageHandler(h,cb,'test');
    const request=async (access='read',truncate=false,path='#task/test/w/out.txt')=>{
        calls.length=0;
        await broker({data:{method:'path_open_existing',path,access,truncate}});
        return decode(Buffer.from(cb.data.slice(0,cb.len[0])).toString('hex'));
    };
    assert.deepEqual(await request(),{ok:true,fd:0}); assert.equal(content,'original');
    assert.deepEqual(await request('readwrite',true),{ok:true,fd:0}); assert.equal(content,'');
    content='keep';
    assert.deepEqual(await request('read',true),{ok:false,errno:22}); assert.equal(calls.length,0);
    error='ENOSYS';
    // A foreign/unsupported backend may return ENOSYS after namespace checks.
    // Never escape that decision via the less constrained legacy OpenResult.
    assert.deepEqual(await request('write'),{ok:false,errno:38});
    assert.deepEqual(calls.map(c=>c[0]),['LstatResult','OpenFileResult']);
    assert.deepEqual(await request('write',true),{ok:false,errno:38});
    assert.deepEqual(calls.map(c=>c[0]),['LstatResult','OpenFileResult']); assert.equal(content,'keep');
    for(const [code,errno] of [['EACCES',13],['ENOENT',2],['EIO',5]]) {
        error=code; assert.deepEqual(await request('write'),{ok:false,errno});
        assert.equal(calls.some(c=>c[0]==='OpenResult'),false);
    }
    assert.deepEqual(await request('write',true,'#task/other/w/out.txt'),{ok:false,errno:13}); assert.equal(calls.length,0);
    regular=false; assert.deepEqual(await request('write',true),{ok:false,errno:13}); assert.equal(calls.length,1);
});

test('actual syscall dispatch maps asm-generic TRUNC through real CallBuffer',async()=>{
    const compile=WebAssembly.compile, instantiate=WebAssembly.instantiate, saved=globalThis.postMessage;
    const memory=new WebAssembly.Memory({initial:1}), cb=new CallBuffer(new SharedArrayBuffer(1024)), calls=[];
    globalThis.postMessage=request=>{calls.push(request);cb.respond(request.method==='path_open_existing'?{ok:true,fd:0}:true);};
    WebAssembly.compile=async()=>({});
    WebAssembly.instantiate=async(_,imports)=>({exports:{memory,_start(){
        new Uint8Array(memory.buffer).set(new TextEncoder().encode('out.txt\0'),128);
        const syscall=imports['ferrule.linux.syscall'].syscall;
        assert.equal(syscall(56,-100,128,0o1000,0),-22); assert.equal(calls.length,0);
        for(const flags of [0,1,2,0o1001,0o1002]) {
            assert.ok(syscall(56,-100,128,flags,0)>=3);
            assert.deepEqual(calls.at(-1),{method:'path_open_existing',path:'#task/test/w/out.txt',access:['read','write','readwrite'][flags&3],create:false,exclusive:false,truncate:!!(flags&0o1000),perm:0});
        }
        for(const flags of [128,1024,0o4000]) {const n=calls.length;assert.equal(syscall(56,-100,128,flags,0),-38);assert.equal(calls.length,n);}
    }}});
    try {await runLinux({data:{bin:Uint8Array.of(0,97,115,109,1,0,0,0,0),tid:'test',args:[],env:[]}},cb);}
    finally {WebAssembly.compile=compile;WebAssembly.instantiate=instantiate;globalThis.postMessage=saved;}
});
