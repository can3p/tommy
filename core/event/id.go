package event

import (
	"crypto/rand"
	"encoding/hex"
	"sync/atomic"
	"time"
)

var idCounter atomic.Uint64

// NewID returns a lexicographically sortable, collision-resistant identifier:
// a millisecond timestamp, a process-local counter and 4 random bytes. Sortable
// ids keep "newest first" stable even when several events share a timestamp.
func NewID() string {
	var b [8]byte
	ms := uint64(time.Now().UnixMilli())
	for i := 0; i < 6; i++ {
		b[5-i] = byte(ms >> (8 * i))
	}
	n := idCounter.Add(1)
	b[6] = byte(n >> 8)
	b[7] = byte(n)

	var r [4]byte
	_, _ = rand.Read(r[:])

	out := make([]byte, 0, 24)
	out = append(out, []byte(hex.EncodeToString(b[:]))...)
	out = append(out, []byte(hex.EncodeToString(r[:]))...)
	return string(out)
}
