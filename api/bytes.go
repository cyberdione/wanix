package api

import (
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

// bytesArg accepts both CBOR byte strings and the numeric-array form emitted
// by Node's structured-cloned Uint8Array. Browser and native GoJS workers
// therefore share the same Wanix API without a host-specific filesystem path.
func bytesArg(value any) ([]byte, error) {
	if tag, ok := value.(cbor.Tag); ok {
		return bytesArg(tag.Content)
	}
	if data, ok := value.([]byte); ok {
		return data, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("want byte string, got %T", value)
	}
	data := make([]byte, len(values))
	for i, value := range values {
		switch n := value.(type) {
		case uint64:
			if n > 255 {
				return nil, fmt.Errorf("byte %d out of range", n)
			}
			data[i] = byte(n)
		case int64:
			if n < 0 || n > 255 {
				return nil, fmt.Errorf("byte %d out of range", n)
			}
			data[i] = byte(n)
		default:
			return nil, fmt.Errorf("byte %d has type %T", i, value)
		}
	}
	return data, nil
}
