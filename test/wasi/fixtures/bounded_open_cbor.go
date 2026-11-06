//go:build ignore

// Decode the actual JavaScript duplex codec output, not a Go re-encoding.
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"tractor.dev/toolkit-go/duplex/codec"
)

type fixture struct {
	Name    string
	Hex     string
	Value   uint64
	Present bool
	Reject  bool
}

func main() {
	var cases []fixture
	if err := json.NewDecoder(os.Stdin).Decode(&cases); err != nil {
		panic(err)
	}
	for _, c := range cases {
		b, err := hex.DecodeString(c.Hex)
		if err != nil {
			panic(err)
		}
		var req map[string]any
		if err := (codec.CBORCodec{}).Decoder(bytes.NewReader(b)).Decode(&req); err != nil {
			panic(err)
		}
		raw, present := req["MaxExistingSize"]
		size, unsigned := raw.(uint64)
		// Same strict uint64/range guard used by OpenFileResult. Do not accept floats.
		accepted := unsigned && size <= 9007199254740991
		if present != c.Present || (c.Present && (accepted == c.Reject || (!c.Reject && size != c.Value))) {
			panic(fmt.Sprintf("%s: present=%v type=%T value=%v accepted=%v", c.Name, present, raw, raw, accepted))
		}
		wantFields := 6
		if c.Present {
			wantFields++
		}
		if len(req) != wantFields {
			panic(fmt.Sprintf("%s: fields=%d", c.Name, len(req)))
		}
		fmt.Printf("%s: fields=%d type=%T value=%v accepted=%v\n", c.Name, len(req), raw, raw, accepted)
	}
}
