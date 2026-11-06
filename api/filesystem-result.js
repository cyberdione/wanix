// Existing filesystem-result names and casing; unknown extra keys remain accepted.
export class FilesystemResultProtocolError extends Error {
 constructor(){super("invalid filesystem result envelope");this.name="FilesystemResultProtocolError";}
}
const codes=new Set(["EPERM","ENOENT","EIO","EBADF","EACCES","EEXIST","ENOTDIR","EISDIR","EINVAL","ENAMETOOLONG","ELOOP"]);
const record=x=>x!==null && typeof x==="object" && !Array.isArray(x);
const owns=(x,k)=>Object.prototype.hasOwnProperty.call(x,k);
export function validateFstatResult(result){
 if(!record(result)||typeof result.OK!=="boolean")throw new FilesystemResultProtocolError();
 if(!result.OK){
  if(owns(result,"Value")||!record(result.Error)||!codes.has(result.Error.Code))throw new FilesystemResultProtocolError();
  return result;
 }
 if(owns(result,"Error")||!record(result.Value))throw new FilesystemResultProtocolError();
 const v=result.Value;
 if(!["Size","Mode","IsDir","ModTime"].every(k=>owns(v,k)))throw new FilesystemResultProtocolError();
 let size=v.Size;
 if(typeof size==="bigint" && size>=0n && size<=BigInt(Number.MAX_SAFE_INTEGER))size=Number(size);
 if(typeof size!=="number"||!Number.isSafeInteger(size)||size<0||!Number.isInteger(v.Mode)||v.Mode<0||v.Mode>0xffffffff||typeof v.IsDir!=="boolean"||typeof v.ModTime!=="number"||!Number.isFinite(v.ModTime))throw new FilesystemResultProtocolError();
 return {...result,Value:{...v,Size:size}};
}
