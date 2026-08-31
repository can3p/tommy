package memory

import "github.com/can3p/tommy/core/event"

type entry struct {
	ev  *event.Event
	seq uint64
}

// ring is a fixed-capacity circular buffer holding entries oldest-first.
type ring struct {
	buf   []*entry
	start int // index of the oldest entry
	n     int
}

func newRing(capacity int) *ring {
	if capacity < 1 {
		capacity = 1
	}
	return &ring{buf: make([]*entry, capacity)}
}

func (r *ring) len() int { return r.n }

// push appends e, returning the entry it evicted, if any.
func (r *ring) push(e *entry) *entry {
	if r.n < len(r.buf) {
		r.buf[(r.start+r.n)%len(r.buf)] = e
		r.n++
		return nil
	}
	evicted := r.buf[r.start]
	r.buf[r.start] = e
	r.start = (r.start + 1) % len(r.buf)
	return evicted
}

// at returns the i-th entry, 0 being the oldest.
func (r *ring) at(i int) *entry { return r.buf[(r.start+i)%len(r.buf)] }

// remove drops the entry with the given id, compacting the buffer.
func (r *ring) remove(id event.ID) bool {
	for i := 0; i < r.n; i++ {
		if r.at(i).ev.ID != id {
			continue
		}
		for j := i; j < r.n-1; j++ {
			r.buf[(r.start+j)%len(r.buf)] = r.at(j + 1)
		}
		r.buf[(r.start+r.n-1)%len(r.buf)] = nil
		r.n--
		return true
	}
	return false
}
