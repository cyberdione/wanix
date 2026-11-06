package api

import (
 "errors"
 "syscall"
 "time"
 "tractor.dev/toolkit-go/duplex/rpc"
 "tractor.dev/wanix/fs"
 "tractor.dev/wanix/fs/pstat"
)
type filesystemError struct { Code string `cbor:"Code"` }
type filesystemResult[T any] struct {
 OK bool `cbor:"OK"`
 Value *T `cbor:"Value,omitempty"`
 Error *filesystemError `cbor:"Error,omitempty"`
}
type statValue struct {
 Size int64 `cbor:"Size"`
 Mode uint32 `cbor:"Mode"`
 IsDir bool `cbor:"IsDir"`
 ModTime time.Time `cbor:"ModTime"`
}
func filesystemCode(err error) string {
 var errno syscall.Errno
 if errors.As(err,&errno) {
  switch errno {
  case syscall.EBADF:return "EBADF"
  case syscall.ENOENT:return "ENOENT"
  case syscall.EACCES:return "EACCES"
  case syscall.EPERM:return "EPERM"
  case syscall.EEXIST:return "EEXIST"
  case syscall.EINVAL:return "EINVAL"
  case syscall.ENOTDIR:return "ENOTDIR"
  case syscall.EISDIR:return "EISDIR"
  case syscall.ELOOP:return "ELOOP"
  case syscall.ENAMETOOLONG:return "ENAMETOOLONG"
  default:return "EIO"
  }
 }
 switch {
 case errors.Is(err,fs.ErrNotExist):return "ENOENT"
 case errors.Is(err,fs.ErrPermission):return "EACCES"
 case errors.Is(err,fs.ErrExist):return "EEXIST"
 case errors.Is(err,fs.ErrInvalid):return "EINVAL"
 default:return "EIO"
 }
}
func failedFilesystemResult[T any](code string) filesystemResult[T] {
 return filesystemResult[T]{Error:&filesystemError{Code:code}}
}
func (s *syscaller) fstatResult(r rpc.Responder,c *rpc.Call) {
 var args []any
 if err:=c.Receive(&args);err!=nil || len(args)!=1 {
  _=r.Return(failedFilesystemResult[statValue]("EINVAL"));return
 }
 fd,ok:=args[0].(uint64)
 if !ok || fd>0xffffffff {_=r.Return(failedFilesystemResult[statValue]("EINVAL"));return}
 f,_,err:=s.task.FD(int(fd))
 if err!=nil {_=r.Return(failedFilesystemResult[statValue]("EBADF"));return}
 fi,err:=f.Stat()
 if err!=nil {_=r.Return(failedFilesystemResult[statValue](filesystemCode(err)));return}
 value:=statValue{Size:fi.Size(),Mode:pstat.FileModeToUnixMode(fi.Mode()),IsDir:fs.IsDirInfo(fi),ModTime:fi.ModTime()}
 _=r.Return(filesystemResult[statValue]{OK:true,Value:&value})
}
