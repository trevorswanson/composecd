package controller

import "sync"

// Update invalidates a browser panel. It never carries credentials or values.
// Change is already the public name of a per-resource diff.
type Update struct {
	Kind string
	Name string
}

type subscriptions struct {
	mu        sync.Mutex
	next      uint64
	listeners map[uint64]chan Update
}

// Subscribe receives app, host and operation changes. A slow subscriber gets a
// coalesced invalidation: rendering always reads the latest controller state.
func (c *Controller) Subscribe() (<-chan Update, func()) {
	b := &c.changes
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.listeners == nil {
		b.listeners = map[uint64]chan Update{}
	}
	b.next++
	id := b.next
	ch := make(chan Update, 1)
	b.listeners[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.listeners[id]; ok {
			delete(b.listeners, id)
			close(ch)
		}
	}
}

func (c *Controller) publish(kind, name string) {
	b := &c.changes
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.listeners {
		select {
		case ch <- Update{Kind: kind, Name: name}:
		default:
		}
	}
}
