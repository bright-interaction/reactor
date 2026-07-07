package vault

import (
	"context"
	"sync"
)

// MemoryBackend is an in-process Backend used by tests + the hand-written
// example workflow in week 2. The SQL backends (sqlite + postgres) land
// alongside the rest of the vault schema in week 8.
type MemoryBackend struct {
	mu   sync.RWMutex
	rows map[string][]byte
}

// NewMemoryBackend returns an empty in-memory backend.
func NewMemoryBackend() *MemoryBackend {
	return &MemoryBackend{rows: map[string][]byte{}}
}

// Put persists a blob.
func (b *MemoryBackend) Put(_ context.Context, id string, blob []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]byte, len(blob))
	copy(cp, blob)
	b.rows[id] = cp
	return nil
}

// Get returns the blob or ErrNotFound.
func (b *MemoryBackend) Get(_ context.Context, id string) ([]byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	v, ok := b.rows[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := make([]byte, len(v))
	copy(cp, v)
	return cp, nil
}

// Delete removes a row. No error if absent.
func (b *MemoryBackend) Delete(_ context.Context, id string) error {
	b.mu.Lock()
	delete(b.rows, id)
	b.mu.Unlock()
	return nil
}

// List returns the current set of credential IDs in arbitrary order.
func (b *MemoryBackend) List(_ context.Context) ([]string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.rows))
	for id := range b.rows {
		out = append(out, id)
	}
	return out, nil
}
