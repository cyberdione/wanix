import assert from "node:assert/strict";
import {validBackendFD,fdStatSize} from "./statx-core.js";
for(const fd of [0,0xffffffff])assert.equal(validBackendFD(fd),true);
for(const fd of [-1,0x100000000,0n,1.5,null,undefined])assert.equal(validBackendFD(fd),false);
const calls=[];const caller={call(method,args){calls.push([method,args]);assert.equal(method,"fd_stat_size");return {ok:true,size:12};}};
for(const backend of [0,0xffffffff])assert.equal(fdStatSize(7,new Map([[7,{handle:{fd:backend}}]]),{},caller,false),12);
assert.deepEqual(calls.map(x=>x[1].fd),[0,0xffffffff]);
const count=calls.length;
assert.equal(fdStatSize(8,new Map(),{},caller,false),-9);
for(const fd of [0x100000000,0n,undefined])assert.equal(fdStatSize(7,new Map([[7,{handle:{fd}}]]),{},caller,false),-5);
assert.equal(fdStatSize(-100,new Map(),{},caller,false),-95);
assert.equal(fdStatSize(0,new Map(),{},caller,true),-95);
assert.equal(calls.length,count);
console.log("finite FD wire/ownership helper cases PASS; unlink/flags/broker not covered");

assert.equal(fdStatSize(7,new Map([[7,{handle:{fd:0}}]]),{},{call(){return {ok:false,errno:9};}},false),-9);
