// Actual Go-codec DTO re-encoding fixtures, NOT original RPC frame capture.
import assert from "node:assert/strict";
import {readFile,lstat} from "node:fs/promises";
import {join,isAbsolute} from "node:path";
import {createHash} from "node:crypto";
import {pathToFileURL} from "node:url";
import {validateFstatResult} from "./filesystem-result.js";
assert.equal(process.argv.length,5,"exactly fixture directory, decoder file and SHA256 required");
const [dir,decoderPath,decoderSHA]=process.argv.slice(2);
assert.ok(isAbsolute(dir)&&isAbsolute(decoderPath),"absolute paths required");
assert.match(decoderSHA,/^[0-9a-f]{64}$/);
assert.ok((await lstat(dir)).isDirectory(),"fixture directory required");
const decoderStat=await lstat(decoderPath);
assert.ok(decoderStat.isFile()&&!decoderStat.isSymbolicLink(),"regular nonsymlink decoder leaf required");
assert.equal(createHash("sha256").update(await readFile(decoderPath)).digest("hex"),decoderSHA);
// Trusted caller dependency: hash-to-native-import path race is not eliminated.
const cbor=await import(pathToFileURL(decoderPath).href);
assert.equal(typeof cbor.default?.decode,"function","CJS default.decode required");
const {decode}=cbor.default;
const success=validateFstatResult(decode(await readFile(join(dir,"success.cbor"))));
assert.equal(success.OK,true);assert.deepEqual(success.Value,{Size:37,Mode:0o100600,IsDir:false,ModTime:1700000000});
for(const [name,code]of [["closed","EBADF"],["missing","EBADF"],["noargs","EINVAL"],["string","EINVAL"],["overflow","EINVAL"],["wrapped","EACCES"]]){
 const result=validateFstatResult(decode(await readFile(join(dir,name+".cbor"))));assert.equal(result.OK,false);assert.equal(result.Error.Code,code);
}
console.log("7 actual Go-codec DTO fixtures validated; no original RPC-frame or FD0 ownership claim");
