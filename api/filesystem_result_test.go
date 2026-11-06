package api
import("testing";"fmt";"syscall";"tractor.dev/wanix/fs")
func TestFstatFilesystemCode(t *testing.T){
 for _,v:=range []struct{err error;want string}{{syscall.EBADF,"EBADF"},{fmt.Errorf("wrapped: %w",fs.ErrNotExist),"ENOENT"},{fs.ErrPermission,"EACCES"},{fmt.Errorf("ENOENT text"),"EIO"}}{if got:=filesystemCode(v.err);got!=v.want{t.Fatalf("got %s want %s",got,v.want)}}
}
