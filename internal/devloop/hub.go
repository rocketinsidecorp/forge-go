package devloop

import "sync"

// maxClients bounds the open event streams: a development machine has a few
// tabs, and a runaway page must not exhaust file descriptors.
const maxClients = 32

// clientBuffer is how many events a slow client may lag behind; further
// events are dropped for it, and it catches up on the next one.
const clientBuffer = 8

type event struct{ name, data string }

// hub fans events out to the browsers that hold an event stream open.
type hub struct {
	mu      sync.Mutex
	clients map[chan event]struct{}
	closed  bool
}

func newHub() *hub { return &hub{clients: make(map[chan event]struct{})} }

// subscribe registers a client. ok is false when the hub is closed or full.
func (h *hub) subscribe() (events <-chan event, unsubscribe func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || len(h.clients) >= maxClients {
		return nil, nil, false
	}
	ch := make(chan event, clientBuffer)
	h.clients[ch] = struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.clients[ch]; ok {
			delete(h.clients, ch)
			close(ch)
		}
	}, true
}

func (h *hub) broadcast(e event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- e:
		default: // slow client: drop rather than block the runner
		}
	}
}

// close ends every stream and refuses new ones.
func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.clients {
		delete(h.clients, ch)
		close(ch)
	}
}
