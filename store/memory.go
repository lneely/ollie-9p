package store

import (
	"context"
	"fmt"
	"os"
	"sync"
)

// MemoryStore is an in-memory key/value store implementing Store.
// Values are []byte blobs held in RAM; nothing is persisted to disk.
// All operations are safe for concurrent use.
type MemoryStore struct {
	mu      sync.RWMutex
	entries map[string][]byte
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: make(map[string][]byte)}
}

// Get returns the current value for key, or nil if absent.
func (m *MemoryStore) Get(key string) []byte {
	m.mu.RLock()
	v := m.entries[key]
	m.mu.RUnlock()
	return v
}

// Set overwrites the value for key.
func (m *MemoryStore) Set(key string, value []byte) {
	m.mu.Lock()
	m.entries[key] = value
	m.mu.Unlock()
}

// --- Store interface ---

func (m *MemoryStore) Stat(name string) (os.FileInfo, error) {
	m.mu.RLock()
	_, ok := m.entries[name]
	sz := int64(len(m.entries[name]))
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%s: not found", name)
	}
	return &SyntheticFileInfo{Name_: name, Mode_: 0444, Size_: sz}, nil
}

func (m *MemoryStore) List() ([]os.DirEntry, error) {
	m.mu.RLock()
	entries := make([]os.DirEntry, 0, len(m.entries))
	for name := range m.entries {
		entries = append(entries, FileEntry(name, 0444))
	}
	m.mu.RUnlock()
	return entries, nil
}

func (m *MemoryStore) Open(name string) (StoreEntry, error) {
	notBlocking := func(context.Context, string) ([]byte, error) {
		return nil, fmt.Errorf("blocking read not supported")
	}
	return &EntryConfig{
		StatFn: func() (os.FileInfo, error) { return m.Stat(name) },
		ReadFn: func() ([]byte, error) {
			m.mu.RLock()
			v := make([]byte, len(m.entries[name]))
			copy(v, m.entries[name])
			m.mu.RUnlock()
			return v, nil
		},
		WriteFn:        func([]byte) error { return fmt.Errorf("%s: read-only", name) },
		BlockingReadFn: notBlocking,
	}, nil
}

func (m *MemoryStore) Create(name string) error {
	m.mu.Lock()
	m.entries[name] = nil
	m.mu.Unlock()
	return nil
}

func (m *MemoryStore) Delete(name string) error {
	m.mu.Lock()
	_, ok := m.entries[name]
	delete(m.entries, name)
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%s: not found", name)
	}
	return nil
}

func (m *MemoryStore) Rename(oldName, newName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.entries[oldName]
	if !ok {
		return fmt.Errorf("%s: not found", oldName)
	}
	m.entries[newName] = v
	delete(m.entries, oldName)
	return nil
}
