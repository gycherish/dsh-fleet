package uplink

import (
	"errors"
	"sort"
	"sync"
)

// ErrAlreadyConnected reports a second live connection for one node id.
var ErrAlreadyConnected = errors.New("uplink: node already has a live connection")

// Registry tracks which nodes are currently reachable.
//
// One connection per node, and a newcomer loses. Letting the newcomer take
// over would let a node stuck in a crash-restart loop repeatedly evict a
// healthy connection, which is the worse failure: the console would flap
// instead of showing one stable machine.
type Registry struct {
	mu    sync.RWMutex
	conns map[string]*Conn
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{conns: map[string]*Conn{}} }

// Add registers a live connection.
func (r *Registry) Add(c *Conn) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, live := r.conns[c.NodeID]; live {
		return ErrAlreadyConnected
	}
	r.conns[c.NodeID] = c
	return nil
}

// Remove deregisters c, but only if it is still the registered connection for
// its node. The guard matters during a reconnect race: without it, a departing
// old connection would evict the new one that just replaced it.
func (r *Registry) Remove(c *Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.conns[c.NodeID]; ok && current == c {
		delete(r.conns, c.NodeID)
	}
}

// Get returns the live connection for a node id.
func (r *Registry) Get(nodeID string) (*Conn, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.conns[nodeID]
	return c, ok
}

// Online lists connected node ids in stable order.
func (r *Registry) Online() []string {
	r.mu.RLock()
	ids := make([]string, 0, len(r.conns))
	for id := range r.conns {
		ids = append(ids, id)
	}
	r.mu.RUnlock()
	sort.Strings(ids)
	return ids
}
