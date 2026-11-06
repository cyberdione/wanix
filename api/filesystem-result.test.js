import assert from "node:assert/strict";
import {validateFstatResult} from "./filesystem-result.js";
for(const Size of [0,0n,12n])assert.equal(validateFstatResult({OK:true,Value:{Size,Mode:0,IsDir:false,ModTime:0},extra:true}).Value.Size,Number(Size));
assert.equal(validateFstatResult({OK:false,Error:{Code:"EBADF"}}).Error.Code,"EBADF");
for(const value of [{OK:false,Error:{Code:"unknown"}},{OK:true,Value:{Size:2n**60n,Mode:0,IsDir:false,ModTime:0}},{OK:false,Value:0,Error:{Code:"EBADF"}}])assert.throws(()=>validateFstatResult(value));
