package api

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"tractor.dev/toolkit-go/duplex/rpc"
	"tractor.dev/wanix/fs"
	"tractor.dev/wanix/fs/pstat"
)

type filesystemError struct {
	Code string `cbor:"Code"`
}
type filesystemResult[T any] struct {
	OK    bool             `cbor:"OK"`
	Value *T               `cbor:"Value,omitempty"`
	Error *filesystemError `cbor:"Error,omitempty"`
}
type statValue struct {
	Size    int64     `cbor:"Size"`
	Mode    uint32    `cbor:"Mode"`
	IsDir   bool      `cbor:"IsDir"`
	ModTime time.Time `cbor:"ModTime"`
}

func filesystemCode(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ENOENT:
			return "ENOENT"
		case syscall.EACCES:
			return "EACCES"
		case syscall.EPERM:
			return "EPERM"
		case syscall.EEXIST:
			return "EEXIST"
		case syscall.EINVAL:
			return "EINVAL"
		case syscall.ENOTDIR:
			return "ENOTDIR"
		case syscall.EISDIR:
			return "EISDIR"
		case syscall.ELOOP:
			return "ELOOP"
		case syscall.ENAMETOOLONG:
			return "ENAMETOOLONG"
		case syscall.EIO:
			return "EIO"
		default:
			return "EIO"
		}
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "ENOENT"
	case errors.Is(err, fs.ErrPermission):
		return "EACCES"
	case errors.Is(err, fs.ErrExist):
		return "EEXIST"
	case errors.Is(err, fs.ErrInvalid):
		return "EINVAL"
	default:
		return "EIO"
	}
}
func failedFilesystemResult[T any](err error) filesystemResult[T] {
	return filesystemResult[T]{Error: &filesystemError{Code: filesystemCode(err)}}
}
func resultPath(c *rpc.Call) (string, error) {
	var args []string
	if err := c.Receive(&args); err != nil {
		return "", err
	}
	if len(args) != 1 {
		return "", fmt.Errorf("expected one path argument")
	}
	return args[0], nil
}
func (s *syscaller) openResult(r rpc.Responder, c *rpc.Call) {
	path, err := resultPath(c)
	if err != nil {
		_ = r.Return(err)
		return
	}
	f, err := s.task.NS().Open(path)
	if err != nil {
		_ = r.Return(failedFilesystemResult[uint64](err))
		return
	}
	s.publishOpenResult(r, f, path)
}

func (s *syscaller) publishOpenResult(r rpc.Responder, f fs.File, path string) {
	// Until OpenFD takes ownership, every exceptional path must close the file.
	owned := true
	defer func() {
		if owned {
			_ = f.Close()
		}
	}()
	fd := s.task.OpenFD(f, path)
	owned = false // OpenFD also closes f if the task has exited.
	if fd < 0 {
		_ = r.Return(failedFilesystemResult[uint64](fs.ErrClosed))
		return
	}
	value := uint64(fd)
	// Return failure cannot prove nondelivery. Do not retry; release our allocation.
	if err := r.Return(filesystemResult[uint64]{OK: true, Value: &value}); err != nil {
		_ = s.task.CloseFD(fd)
	}
}
func (s *syscaller) statResult(r rpc.Responder, c *rpc.Call)  { s.returnStatResult(r, c, false) }
func (s *syscaller) lstatResult(r rpc.Responder, c *rpc.Call) { s.returnStatResult(r, c, true) }
func (s *syscaller) returnStatResult(r rpc.Responder, c *rpc.Call, nofollow bool) {
	path, err := resultPath(c)
	if err != nil {
		_ = r.Return(err)
		return
	}
	var fi fs.FileInfo
	if nofollow {
		fi, err = fs.Lstat(s.task.NS(), path)
	} else {
		fi, err = fs.Stat(s.task.NS(), path)
	}
	if err != nil {
		_ = r.Return(failedFilesystemResult[statValue](err))
		return
	}
	value := statValue{Size: fi.Size(), Mode: pstat.FileModeToUnixMode(fi.Mode()), IsDir: fs.IsDirInfo(fi), ModTime: fi.ModTime()}
	_ = r.Return(filesystemResult[statValue]{OK: true, Value: &value})
}
