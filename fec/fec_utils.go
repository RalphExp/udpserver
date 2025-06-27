package fec

import (
	"sync"
	"time"
)

// currentMs returns current elapsed monotonic milliseconds since program startup
func currentMs() uint32 { return uint32(time.Since(refTime) / time.Millisecond) }

func _itimediff(later, earlier uint32) int32 {
	return (int32)(later - earlier)
}

var (
	// a system-wide packet buffer shared among sending, receiving and FEC
	// to mitigate high-frequency memory allocation for packets, bytes from xmitBuf
	// is aligned to 64bit
	xmitBuf sync.Pool

	// maximum packet size
	mtuLimit = 1500

	// monotonic reference time point
	refTime time.Time = time.Now()
)

func init() {
	xmitBuf.New = func() interface{} {
		return make([]byte, mtuLimit)
	}
}
