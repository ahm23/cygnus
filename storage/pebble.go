package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/cockroachdb/pebble"
	"go.uber.org/zap"
)

const (
	// Key prefixes for different data types
	filePrefix     = "file:"
	merklePrefix   = "merkle:"
	providerPrefix = "provider:"
)

// PebbleStore wraps PebbleDB for storing file metadata and merkle trees
type PebbleStore struct {
	db     *pebble.DB
	logger *zap.Logger
	mu     sync.RWMutex
}

// NewPebbleStore creates a new PebbleDB store
func NewPebbleStore(dataDir string, logger *zap.Logger) (*PebbleStore, error) {
	dbPath := filepath.Join(dataDir, "index")

	opts := &pebble.Options{}
	db, err := pebble.Open(dbPath, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to open pebble db: %w", err)
	}

	return &PebbleStore{
		db:     db,
		logger: logger,
	}, nil
}

// Close closes the PebbleDB connection
func (ps *PebbleStore) Close() error {
	return ps.db.Close()
}

// Set stores a key-value pair
func (ps *PebbleStore) Set(ctx context.Context, key string, value []byte) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	return ps.db.Set([]byte(key), value, pebble.Sync)
}

// Get retrieves a value by key
func (ps *PebbleStore) Get(ctx context.Context, key string) ([]byte, error) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	value, closer, err := ps.db.Get([]byte(key))
	if err != nil {
		if err == pebble.ErrNotFound {
			return nil, fmt.Errorf("key not found: %s", key)
		}
		return nil, err
	}
	defer closer.Close()

	// Copy the value since PebbleDB returns a slice that may be reused
	result := make([]byte, len(value))
	copy(result, value)
	return result, nil
}

// Delete removes a key-value pair
func (ps *PebbleStore) Delete(ctx context.Context, key string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	return ps.db.Delete([]byte(key), pebble.Sync)
}

// Has checks if a key exists
func (ps *PebbleStore) Has(ctx context.Context, key string) (bool, error) {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	_, closer, err := ps.db.Get([]byte(key))
	if err != nil {
		if err == pebble.ErrNotFound {
			return false, nil
		}
		return false, err
	}
	closer.Close()
	return true, nil
}

// SetJSON stores a JSON-encoded value
func (ps *PebbleStore) SetJSON(ctx context.Context, key string, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("failed to marshal json: %w", err)
	}
	return ps.Set(ctx, key, data)
}

// GetJSON retrieves and unmarshals a JSON value
func (ps *PebbleStore) GetJSON(ctx context.Context, key string, dest interface{}) error {
	data, err := ps.Get(ctx, key)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return fmt.Errorf("failed to unmarshal json: %w", err)
	}
	return nil
}

// SetHash stores a hash map (for merkle tree data)
func (ps *PebbleStore) SetHash(ctx context.Context, key string, fields map[string]interface{}) error {
	data, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("failed to marshal hash: %w", err)
	}
	return ps.Set(ctx, key, data)
}

// GetHash retrieves a hash map
func (ps *PebbleStore) GetHash(ctx context.Context, key string) (map[string]interface{}, error) {
	data, err := ps.Get(ctx, key)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal hash: %w", err)
	}
	return result, nil
}

// IteratePrefix iterates over all keys with a given prefix
func (ps *PebbleStore) IteratePrefix(ctx context.Context, prefix string, fn func(key string, value []byte) error) error {
	ps.mu.RLock()
	defer ps.mu.RUnlock()

	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: []byte(prefix),
		UpperBound: []byte(prefix + "\xff"),
	})
	if err != nil {
		return err
	}
	defer iter.Close()

	for iter.First(); iter.Valid(); iter.Next() {
		key := string(iter.Key())
		value := make([]byte, len(iter.Value()))
		copy(value, iter.Value())

		if err := fn(key, value); err != nil {
			return err
		}
	}

	return iter.Error()
}

// Keys returns all keys with a given prefix
func (ps *PebbleStore) Keys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	err := ps.IteratePrefix(ctx, prefix, func(key string, value []byte) error {
		keys = append(keys, key)
		return nil
	})
	return keys, err
}

// FileKey returns the storage key for a file metadata
func FileKey(fileID string) string {
	return filePrefix + fileID
}

// MerkleKey returns the storage key for a merkle tree
func MerkleKey(fileID string) string {
	return merklePrefix + fileID
}

// ProviderKey returns the storage key for provider data
func ProviderKey(key string) string {
	return providerPrefix + key
}
