package api

import (
 "bytes"
 "context"
 "fmt"
 "io"
 "io/fs"
 "os"
 "path/filepath"
 "syscall"
 "testing"
 "time"
 "tractor.dev/toolkit-go/duplex/codec"
)
type fstatRPCFile struct { statErr error }
func (f *fstatRPCFile) Read([]byte)(int,error){return 0,io.EOF}
func (f *fstatRPCFile) Close()error{return nil}
func (f *fstatRPCFile) Stat()(fs.FileInfo,error){if f.statErr!=nil{return nil,f.statErr};return fstatRPCInfo{},nil}
type fstatRPCInfo struct{}
func (fstatRPCInfo) Name()string{return "fixture"}
func (fstatRPCInfo) Size()int64{return 37}
func (fstatRPCInfo) Mode()fs.FileMode{return 0600}
func (fstatRPCInfo) ModTime()time.Time{return time.Unix(1700000000,123456789).UTC()}
func (fstatRPCInfo) IsDir()bool{return false}
func (fstatRPCInfo) Sys()any{return nil}
func TestFstatResultRPCFixture(t *testing.T){
 client,task:=startTestServerWithTask(t)
 fd:=task.OpenFD(&fstatRPCFile{},"fixture")
 if fd!=4{t.Fatalf("actual initial allocated FD=%d want4; no FD0 RPC proof",fd)}
 call:=func(args []any)filesystemResult[statValue]{t.Helper();var result filesystemResult[statValue];if _,err:=client.Call(context.Background(),"FstatResult",args,&result);err!=nil{t.Fatal(err)};return result}
 success:=call([]any{uint64(fd)})
 if !success.OK||success.Value==nil||success.Error!=nil{t.Fatalf("success envelope %#v",success)}
 v:=success.Value
 if v.Size!=37||v.Mode!=0100600||v.IsDir||v.ModTime.Unix()!=1700000000||v.ModTime.Nanosecond()!=0{t.Fatalf("actual CBOR success %#v",v)}
 if err:=task.CloseFD(fd);err!=nil{t.Fatal(err)}
 fixtures:=map[string]any{"success.cbor":success}
 for _,c:=range []struct{name string;args []any;code string}{
 {"closed",[]any{uint64(fd)},"EBADF"},{"missing",[]any{uint64(0xffffffff)},"EBADF"},
 {"noargs",[]any{},"EINVAL"},{"string",[]any{"4"},"EINVAL"},{"overflow",[]any{uint64(0x100000000)},"EINVAL"},
 }{r:=call(c.args);if r.OK||r.Value!=nil||r.Error==nil||r.Error.Code!=c.code{t.Fatalf("%s %#v",c.name,r)};fixtures[c.name+".cbor"]=r}
 bad:=task.OpenFD(&fstatRPCFile{statErr:fmt.Errorf("wrapped: %w",syscall.EACCES)},"bad");defer task.CloseFD(bad)
 r:=call([]any{uint64(bad)});if r.OK||r.Error==nil||r.Error.Code!="EACCES"{t.Fatalf("wrapped errno %#v",r)};fixtures["wrapped.cbor"]=r
 var zero bytes.Buffer
 if err:=(codec.CBORCodec{}).Encoder(&zero).Encode(uint64(0));err!=nil{t.Fatal(err)}
 var decoded uint64;if err:=(codec.CBORCodec{}).Decoder(&zero).Decode(&decoded);err!=nil||decoded!=0{t.Fatalf("codec zero %d %v",decoded,err)}
 // Optional explicitly granted output is DTO re-encoding, NOT original RPC frames.
 if output:=os.Getenv("WANIX_FSTAT_FIXTURE_DIR");output!=""{
  if !filepath.IsAbs(output){t.Fatal("fixture directory must be absolute")}
  if err:=os.Mkdir(output,0700);err!=nil{t.Fatal(err)}
  for name,value:=range fixtures{var b bytes.Buffer;if err:=(codec.CBORCodec{}).Encoder(&b).Encode(value);err!=nil{t.Fatal(err)};f,err:=os.OpenFile(filepath.Join(output,name),os.O_CREATE|os.O_EXCL|os.O_WRONLY,0600);if err!=nil{t.Fatal(err)};_,err=f.Write(b.Bytes());closeErr:=f.Close();if err!=nil{t.Fatal(err)};if closeErr!=nil{t.Fatal(closeErr)}}
 }
}
