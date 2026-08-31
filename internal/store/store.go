// Package store implements a tiny local-disk object store that mimics the
// way Zerodha used S3 as the handoff layer between pipeline stages.
//
// The interesting detail replicated here is the *prefix partitioning*
// strategy: S3 rate-limits requests per key prefix (3,500 PUT / 5,500 GET
// per second per prefix), so Zerodha sharded keys across ten fixed
// prefixes ("0/".."9/") to multiply the effective rate limit tenfold.
package store

import (
	"crypto/sha1"
	"fmt"
	"os"
	"path/filepath"
)

// Store is a local stand-in for an S3 bucket.
type Store struct {
	root string
}

func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

// partition returns the "0".."9" shard prefix for a key, exactly like
// hashing object keys across ten fixed S3 prefixes.
func partition(key string) string {
	h := sha1.Sum([]byte(key))
	return fmt.Sprintf("%d", int(h[0])%10)
}

// Key returns the fully partitioned object key, e.g. "7/pdfs/CL0042.pdf".
func (s *Store) Key(key string) string {
	return filepath.Join(partition(key), key)
}

func (s *Store) path(key string) string {
	return filepath.Join(s.root, s.Key(key))
}

// Put writes an object under its partitioned key.
func (s *Store) Put(key string, data []byte) (string, error) {
	p := s.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	return s.Key(key), os.WriteFile(p, data, 0o644)
}

// PutFile moves an existing local file into the store.
func (s *Store) PutFile(key, src string) (string, error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return "", err
	}
	return s.Put(key, data)
}

// Get reads an object back by its (unpartitioned) key.
func (s *Store) Get(key string) ([]byte, error) {
	return os.ReadFile(s.path(key))
}
