package mesh

import "sync"

// broadcaster fans out PTY output chunks to any number of live subscribers
// (attach / watch), keyed by session ID. Subscribers that fall behind get
// chunks dropped rather than blocking the PTY read loop.
type broadcaster struct {
	mu   sync.Mutex
	subs map[string]map[chan []byte]struct{}
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: make(map[string]map[chan []byte]struct{})}
}

func (b *broadcaster) subscribe(sessionID string) (<-chan []byte, func()) {
	ch := make(chan []byte, 64)
	b.mu.Lock()
	if b.subs[sessionID] == nil {
		b.subs[sessionID] = make(map[chan []byte]struct{})
	}
	b.subs[sessionID][ch] = struct{}{}
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		delete(b.subs[sessionID], ch)
		if len(b.subs[sessionID]) == 0 {
			delete(b.subs, sessionID)
		}
		b.mu.Unlock()
	}
	return ch, cancel
}

func (b *broadcaster) publish(sessionID string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[sessionID] {
		select {
		case ch <- data:
		default:
		}
	}
}
