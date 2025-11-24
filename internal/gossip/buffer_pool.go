package gossip

import (
	"sync"
)

var (
	// String slice pool for replica lists
	stringSlicePool = sync.Pool{
		New: func() interface{} {
			return make([]string, 0, 10)
		},
	}

	// Byte slice pool for temporary buffers
	byteSlicePool = sync.Pool{
		New: func() interface{} {
			return make([]byte, 0, 512)
		},
	}
)

func GetStringSlice() []string {
	slice := stringSlicePool.Get().([]string)
	return slice[:0]
}

func PutStringSlice(slice []string) {
	if cap(slice) > 1000 {
		return
	}
	stringSlicePool.Put(slice[:0])
}

func GetByteSlice() []byte {
	slice := byteSlicePool.Get().([]byte)
	return slice[:0]
}

func PutByteSlice(slice []byte) {
	if cap(slice) > 65536 {
		return
	}
	byteSlicePool.Put(slice[:0])
}
