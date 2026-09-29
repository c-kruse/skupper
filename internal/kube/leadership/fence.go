package leadership

import (
	"errors"
	"sync"
)

var ErrNotLeader = errors.New("controller is not the active leader")

// Fence is the server-side authority gate. It is intentionally checked at the
// operation/message boundary because Service endpoints and existing connections
// lag readiness changes.
type Fence struct {
	mu      sync.RWMutex
	open    bool
	revoked bool
	done    chan struct{}
	closed  bool
}

func NewFence() *Fence {
	done := make(chan struct{})
	close(done)
	return &Fence{done: done, closed: true}
}

func (f *Fence) Open() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open || f.revoked {
		return f.open
	}
	f.done = make(chan struct{})
	f.closed = false
	f.open = true
	return true
}

func (f *Fence) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open = false
	f.revoked = true
	if !f.closed {
		close(f.done)
		f.closed = true
	}
}

func (f *Fence) Check() error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if !f.open {
		return ErrNotLeader
	}
	return nil
}

func (f *Fence) Done() <-chan struct{} {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.done
}
