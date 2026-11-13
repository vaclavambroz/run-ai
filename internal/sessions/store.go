package sessions

import (
	"context"
	"sync"

	"github.com/vaclavambroz/run-ai/internal/providers"
)

// MemoryStore is intentionally small and is useful for tests and ephemeral
// clients. Production compatibility uses internal/legacy.MapStore.
type MemoryStore struct {
	mu       sync.RWMutex
	mappings map[string]Mapping
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{mappings: make(map[string]Mapping)}
}

func mappingKey(provider providers.Name, key string) string {
	return string(provider) + "\x00" + key
}

func (s *MemoryStore) Lookup(_ context.Context, provider providers.Name, key string) (Mapping, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	mapping, ok := s.mappings[mappingKey(provider, key)]
	if !ok {
		return Mapping{}, ErrNotFound
	}
	return mapping, nil
}

func (s *MemoryStore) Put(_ context.Context, mapping Mapping) error {
	if err := ValidateKey(mapping.Key); err != nil {
		return err
	}
	if err := ValidateID(mapping.ID); err != nil {
		return err
	}
	if _, ok := providers.Lookup(mapping.Provider); !ok {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mappings[mappingKey(mapping.Provider, mapping.Key)] = mapping
	return nil
}
