import { test } from 'node:test';
import assert from 'node:assert/strict';
globalThis.self = {};
const { runLinux, messageHandler } = await import('./worker.js');

test('actual dispatch preserves existing bytes, rejects features before calls, and enforces direction', async () => {
    const compile = WebAssembly.compile, instantiate = WebAssembly.instantiate;
    const memory = new WebAssembly.Memory({initial: 1}), calls = [];
    const content = Uint8Array.of(65,66,67);
    let response = {ok: true, fd: 12};
    WebAssembly.compile = async () => ({});
    WebAssembly.instantiate = async (_, imports) => ({exports: {memory, _start() {
        const s = imports['ferrule.linux.syscall'].syscall;
        const bytes = new Uint8Array(memory.buffer), view = new DataView(memory.buffer);
        bytes.set(new TextEncoder().encode('out.txt\0'), 128);
        const open = flags => s(56, -100, 128, flags, 0o600);
        for (const flags of [3, 64, 128, 1024, 64|128|512, 2|1024,
            64|1, 64|2, 0x8000, 0x10000, 0x20000, 0x80000, -1]) {
            const n = calls.length;
            assert.equal(open(flags), -38, String(flags));
            assert.equal(calls.length, n);
        }
        assert.equal(open(512), -22);
        for (const flags of [0,1,2]) {
            const fd = open(flags); assert.ok(fd >= 3);
            if (flags !== 1) {
                assert.equal(s(63, fd, 512, 3), 3);
                assert.deepEqual(bytes.slice(512,515), content);
            }
            const denied = flags === 0 ? 64 : flags === 1 ? 63 : null;
            if (denied) {
                const n = calls.length;
                for (const count of [0,1]) assert.equal(s(denied, fd, 512, count), -9);
                for (const cnt of [0,1]) {
                    view.setUint32(256,512,true); view.setUint32(260,0,true);
                    assert.equal(s(denied === 64 ? 66 : 65, fd, 256, cnt), -9);
                }
                assert.equal(calls.length,n);
            }
        }
        assert.deepEqual(content, Uint8Array.of(65,66,67));
        for (const errno of [2,13,5]) { response = {ok:false,errno}; assert.equal(open(0),-errno); }
        response = {__wanix_error:'bad response'}; assert.equal(open(0),-5);
    }}});
    try {
        await runLinux({data:{bin:Uint8Array.of(0,97,115,109,1,0,0,0,0),tid:'test',args:[],env:[]}},
            {call(method,args) {
                calls.push([method,args]);
                if (method === 'path_open_existing') return response;
                if (method === 'fd_read') return content.slice(args.at,args.at+args.count);
                if (method === 'fd_close' || method === 'exit') return true;
                assert.fail(method);
            }});
    } finally { WebAssembly.compile=compile; WebAssembly.instantiate=instantiate; }
});

test('existing-open broker preserves typed errno and never creates or truncates', async () => {
    const replies=[], calls=[];
    let error;
    const fs={async lstatResult(path) { calls.push('lstat'); if(error) throw error; return {OK:true,Value:{Mode:0x8000,IsDir:false}}; },
        async openFileResult() { calls.push('open'); return {OK:true, Value:8}; }};
    const handler=messageHandler(fs,{respond(v){replies.push(v);}},'test');
    const request=path=>handler({data:{method:'path_open_existing',path}});
    await request('#task/other/w/out.txt'); assert.deepEqual(replies.pop(),{ok:false,errno:13});
    assert.deepEqual(calls,[]);
    for (const [code,errno] of [['ENOENT',2],['EACCES',13],['EIO',5]]) {
        error=Object.assign(new Error('opaque'),{code});
        await request('#task/test/w/out.txt'); assert.deepEqual(replies.pop(),{ok:false,errno});
    }
    error=new Error('untyped backend failure');
    await request('#task/test/w/out.txt'); assert.deepEqual(replies.pop(),{ok:false,errno:5});
    error=null; await request('#task/test/w/out.txt'); assert.deepEqual(replies.pop(),{ok:true,fd:8});
    assert.deepEqual(calls,['lstat','lstat','lstat','lstat','lstat','open']);
});
