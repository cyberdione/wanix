// Diagnostic transformed-source closure unit; NOT module import or wire coverage.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import vm from "node:vm";
import { exactStatSize, validBackendFD } from "./statx-core.js";
const hash = bytes => createHash("sha256").update(bytes).digest("hex");
const source = await readFile(new URL("./worker.js", import.meta.url), "utf8");
assert.equal(hash(source), "f1a24d8c446221b5ca39610be6892d82c5fbb2a8bb90a74f8589c623b936ebc6");
const startMarker = "function messageHandler(";
const endMarker = "\nfunction extractWasm(";
assert.equal(source.split(startMarker).length, 2);
assert.equal(source.split(endMarker).length, 2);
const start = source.indexOf(startMarker);
const separator = source.indexOf(endMarker, start);
// Include the one LF separator in the RAW pin; never trim source.
const closure = source.slice(start, separator + 1);
assert.equal(hash(closure), "a8040306bdee28af5254e4c135158ce9f93962c0d30b8104379319fd194f6849");
const core = await readFile(new URL("./statx-core.js", import.meta.url));
assert.equal(hash(core), "3c326cd967e23f92100962b42976b26c4c03bcdf34817f06487749f6c169bd6c");
// Host Error injection makes actual instanceof Error branch agree with host fakes.
const context = vm.createContext({ exactStatSize, validBackendFD, Error });
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
 const fs={ async fstat(fd){calls.push(fd);if("failure"in c)throw c.failure;return c.stat;},stat(){assert.fail("fd request must not call path stat");} };
 const handler=context.handlerFactory(fs,{respond(value){responses.push(value);}},"diagnostic");
 await handler({data:{method:"fd_stat_size",fd:c.fd}});
 assert.deepEqual(calls,c.invalid?[]:[c.fd],c.name);
 assert.equal(responses.length,1,c.name);
 if("error"in c){assert.equal(responses[0].__wanix_error,c.error,c.name);assert.equal(Object.keys(responses[0]).join(","),"__wanix_error",c.name);}
 else assert.equal(responses[0],c.value,c.name);
}
console.log("9 actual FD producer closure cases passed (transformed-source unit only)");
