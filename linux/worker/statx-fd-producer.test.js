// Diagnostic transformed-source closure unit; NOT module import or wire coverage.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import vm from "node:vm";
import { exactStatSize, validBackendFD, fstatBrokerResult } from "./statx-core.js";
const hash = bytes => createHash("sha256").update(bytes).digest("hex");
const source = await readFile(new URL("./worker.js", import.meta.url), "utf8");
assert.equal(hash(source), "275f4b1a47d400ab81f5dfd159f486990aaf55a92ad7dec009aa15f4671fea34");
const startMarker = "function messageHandler(";
const endMarker = "\nfunction extractWasm(";
assert.equal(source.split(startMarker).length, 2);
assert.equal(source.split(endMarker).length, 2);
const start = source.indexOf(startMarker);
const separator = source.indexOf(endMarker, start);
// Include the one LF separator in the RAW pin; never trim source.
const closure = source.slice(start, separator + 1);
assert.equal(hash(closure), "76ac02585ced691b100829ed05f3121bb5fbc0b6ba3394af63afd8f29ff7dd6c");
const core = await readFile(new URL("./statx-core.js", import.meta.url));
assert.equal(hash(core), "fd21146cbc717730aaa0df503c8aa4f9ab80e37cca63a885eef4aa3d41d157c4");
// Host Error injection makes actual instanceof Error branch agree with host fakes.
const context = vm.createContext({ exactStatSize, validBackendFD, fstatBrokerResult, Error });
vm.runInContext(closure + "\nglobalThis.handlerFactory = messageHandler;", context);
const cases = [
 {name:"fd zero",fd:0,stat:{Size:0},value:0},
 {name:"fd max",fd:0xffffffff,stat:{Size:37n},value:37},
 {name:"missing fd",error:"invalid backend fd",invalid:true},
 {name:"bigint fd",fd:0n,error:"invalid backend fd",invalid:true},
 {name:"overflow fd",fd:0x100000000,error:"invalid backend fd",invalid:true},
 {name:"missing metadata",fd:3,stat:null,error:"missing stat metadata"},
 {name:"malformed size",fd:3,stat:{Size:null},error:"invalid stat size"},
 {name:"backend Error",fd:3,failure:new Error("backend missing"),error:"backend missing"},
 {name:"backend string",fd:3,failure:"backend denied",error:"backend denied"},
];
for (const c of cases) {
 const calls=[],responses=[];
 const fs={ async fstatResult(fd){calls.push(fd);if("failure"in c)throw c.failure;return c.stat === null ? null : {OK:true,Value:c.stat};},stat(){assert.fail("fd request must not call path stat");} };
 const handler=context.handlerFactory(fs,{respond(value){responses.push(value);}},"diagnostic");
 await handler({data:{method:"fd_stat_size",fd:c.fd}});
 assert.deepEqual(calls,c.invalid?[]:[c.fd],c.name);
 assert.equal(responses.length,1,c.name);
 if("error"in c){assert.equal(responses[0].ok,false,c.name);assert.equal(responses[0].errno,c.invalid?22:5,c.name);assert.equal(Object.hasOwn(responses[0],"size"),false,c.name);}
 else {assert.equal(responses[0].ok,true,c.name);assert.equal(responses[0].size,c.value,c.name);assert.equal(Object.hasOwn(responses[0],"errno"),false,c.name);}
}
for (const [name,result,errno] of [
 ["typed EBADF",{OK:false,Error:{Code:"EBADF"}},9],
 ["contradictory DTO",{OK:true,Value:{Size:0},Error:{Code:"EBADF"}},5],
]) {
 const calls=[],responses=[];
 const fs={async fstatResult(fd){calls.push(fd);return result;},stat(){assert.fail("fd request must not call path stat");}};
 const handler=context.handlerFactory(fs,{respond(value){responses.push(value);}},"diagnostic");
 await handler({data:{method:"fd_stat_size",fd:7}});
 assert.deepEqual(calls,[7],name);assert.equal(responses.length,1,name);
 assert.equal(responses[0].ok,false,name);assert.equal(responses[0].errno,errno,name);
 assert.equal(Object.hasOwn(responses[0],"size"),false,name);
}
console.log("11 actual FD producer closure cases passed (transformed-source unit only)");
