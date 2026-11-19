import test from 'node:test';
import assert from 'node:assert/strict';
import {CallBuffer, WanixHandle} from './lib.js';
globalThis.self = {};
const {runLinux, messageHandler} = await import('./worker.js');

test('CREATE captures ordinary effective mode in actual dispatch and CallBuffer', async () => {
    const compile=WebAssembly.compile, instantiate=WebAssembly.instantiate, post=globalThis.postMessage;
    const cb=new CallBuffer(new SharedArrayBuffer(1024)), memory=new WebAssembly.Memory({initial:1});
    let syscall, changeMask=false;
    const calls=[];
    globalThis.postMessage=req=>{
        calls.push(req);
        if(changeMask && req.method==='path_open_existing') syscall(166,0o777);
        cb.respond(req.method==='path_open_existing'?{ok:true,fd:0}:true);
    };
    WebAssembly.compile=async()=>({});
    WebAssembly.instantiate=async(_,imports)=>({exports:{memory,_start(){
        syscall=imports['ferrule.linux.syscall'].syscall;
        new Uint8Array(memory.buffer).set(new TextEncoder().encode('out.txt\0'),128);
        const open=(flags,mode)=>syscall(56,-100,128,flags,mode);
        for(const mask of [0,0o022,0o077,0o777]) {
            syscall(166,mask);
            for(const mode of [0o666,0o755,0]) {
                assert.ok(open(0o100,mode)>=3);
                assert.deepEqual(calls.at(-1),{method:'path_open_existing',maxExistingSize:0x7fffffff,path:'#task/test/w/out.txt',access:'read',create:true,exclusive:false,truncate:false,perm:(mode&0o777)&~mask});
            }
        }
        syscall(166,0o022); changeMask=true;
        assert.ok(open(0o302,0o777)>=3);
        assert.equal(calls.at(-1).perm,0o755); assert.equal(calls.at(-1).exclusive,true);
        changeMask=false;
        assert.ok(open(0, -1)>=3); assert.equal(calls.at(-1).perm,0);
        for(const mode of [0o1000,0o2000,0o4000,0x80000000]) {
            const n=calls.length; assert.equal(open(0o100,mode),-38); assert.equal(calls.length,n);
        }
        for(const flags of [0o200,0o2000,0o1000]) {
            const n=calls.length; assert.equal(open(flags,0o666),flags===0o1000?-22:-38); assert.equal(calls.length,n);
        }
        // Read-only CREATE has a read-only guest file description.
        const fd=open(0o100,0o600), n=calls.length;
        assert.equal(syscall(64,fd,128,0),-9); assert.equal(calls.length,n);
    }}});
    try {await runLinux({data:{bin:Uint8Array.of(0,97,115,109,1,0,0,0,0),tid:'test',args:[],env:[]}},cb);}
    finally {WebAssembly.compile=compile;WebAssembly.instantiate=instantiate;globalThis.postMessage=post;}
});

test('CREATE broker uses actual seven-field API codec with no lstat or generic fallback',async()=>{
    const h=Object.create(WanixHandle.prototype), calls=[], replies=[];
    let content='keep', mode=0o640, exists=true, error;
    h.peer={async call(method,args){
        calls.push([method,args]); assert.equal(method,'OpenFileResult');
        assert.deepEqual(Object.keys(args).sort(),['Access','Create','Exclusive','MaxExistingSize','Path','Perm','Truncate']);
        if(error) return {value:{OK:false,Error:{Code:error}}};
        if(exists && args.Exclusive) return {value:{OK:false,Error:{Code:'EEXIST'}}};
        if(!exists) {exists=true;mode=args.Perm;content='';}
        if(args.Truncate) content='';
        return {value:{OK:true,Value:0}};
    }};
    const handler=messageHandler(h,{respond(v){replies.push(v);}},'test');
    const request=async(extra={})=>{
        calls.length=0;
        await handler({data:{method:'path_open_existing',maxExistingSize:0x7fffffff,path:'#task/test/w/out.txt',access:'readwrite',create:true,exclusive:false,truncate:false,perm:0,...extra}});
        return replies.pop();
    };
    assert.deepEqual(await request(),{ok:true,fd:0}); assert.equal(content,'keep');assert.equal(mode,0o640);
    assert.deepEqual(await request({exclusive:true,truncate:true}),{ok:false,errno:17});assert.equal(content,'keep');
    assert.deepEqual(await request({truncate:true}),{ok:true,fd:0});assert.equal(content,'');assert.equal(mode,0o640);
    exists=false;assert.deepEqual(await request(),{ok:true,fd:0});assert.equal(mode,0);
    for(const [code,errno] of [['EACCES',13],['ENOSYS',38],['ENOENT',2],['EIO',5]]) {
        error=code;assert.deepEqual(await request(),{ok:false,errno});assert.equal(calls.length,1);
    }
    for(const extra of [{path:'#task/foreign/w/out.txt'},{path:'#task/test/w/../out.txt'}]) {
        assert.deepEqual(await request(extra),{ok:false,errno:13});assert.equal(calls.length,0);
    }
    for(const extra of [{perm:512},{perm:-1},{perm:1.5},{create:false,perm:1},{exclusive:1},{create:1},{truncate:1},{create:false,exclusive:true},{access:'read',truncate:true}]) {
        assert.deepEqual(await request(extra),{ok:false,errno:22});assert.equal(calls.length,0);
    }
});
