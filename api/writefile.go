package api

import (
	"tractor.dev/toolkit-go/duplex/rpc"
	"tractor.dev/wanix/fs"
)

func (s *syscaller) writeFile(r rpc.Responder, c *rpc.Call) {
	var args []any
	c.Receive(&args)

	// log.Println("WriteFile", args)

	name, ok := args[0].(string)
	if !ok {
		panic("arg 0 is not a string")
	}

	data, err := bytesArg(args[1])
	if err != nil {
		r.Return(err)
		return
	}

	err = fs.WriteFile(s.task.NS(), name, data, 0x644)
	if err != nil {
		r.Return(err)
		return
	}
}
