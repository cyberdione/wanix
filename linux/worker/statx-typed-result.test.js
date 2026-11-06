import assert from "node:assert/strict";
import {fstatBrokerResult,fdSizeReply} from "./statx-core.js";
assert.deepEqual(fstatBrokerResult({OK:false,Error:{Code:"EBADF"}}),{ok:false,errno:9});
assert.equal(fdSizeReply({ok:false,errno:9}),-9);
assert.equal(fdSizeReply({ok:true,size:0}),0);
for(const errno of ["EBADF",-9,1.5,999])assert.equal(fdSizeReply({ok:false,errno}),-5);
assert.deepEqual(fstatBrokerResult({OK:true,Value:{Size:0n}}),{ok:true,size:0});

for(const result of [
 {OK:true,Value:{Size:0},Error:{Code:"EBADF"}},
 {OK:false,Error:{Code:"EBADF"},Value:{Size:0}}
])assert.deepEqual(fstatBrokerResult(result),{ok:false,errno:5});
for(const reply of [{ok:true,size:0,errno:9},{ok:false,errno:9,size:0}])assert.equal(fdSizeReply(reply),-5);
