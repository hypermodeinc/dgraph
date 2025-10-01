// SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
// SPDX-License-Identifier: Apache-2.0

// Package types contains some very common utilities used by Dgraph. These utilities
// are of "miscellaneous" nature, e.g., error checking.
package types

import (
	"sync"

	"github.com/dgryski/go-farm"
)

// LockedShardedMap is a thread-safe, sharded map with generic key-value types.
type LockedShardedMap[K comparable, V any] struct {
	shards []map[K]V
	locks  []sync.RWMutex
}

// NewLockedShardedMap creates a new LockedShardedMap.
func NewLockedShardedMap[K comparable, V any]() *LockedShardedMap[K, V] {
	shards := make([]map[K]V, NumShards)
	locks := make([]sync.RWMutex, NumShards)
	for i := range shards {
		shards[i] = make(map[K]V)
	}
	return &LockedShardedMap[K, V]{shards: shards, locks: locks}
}

func (s *LockedShardedMap[K, V]) getShardIndex(key K) int {
	// Only works for integer-like keys (uint64 etc). For generic types,
	// a better hash function is needed. We'll assume uint64 for now.
	switch k := any(key).(type) {
	case uint64:
		return int(k % uint64(NumShards))
	case string:
		return int(farm.Fingerprint64([]byte(k)) % uint64(NumShards))
	default:
		panic("LockedShardedMap only supports uint64 and string keys for now")
	}
}

func (s *LockedShardedMap[K, V]) Set(key K, value V) {
	if s == nil {
		return
	}
	index := s.getShardIndex(key)
	s.locks[index].Lock()
	defer s.locks[index].Unlock()
	s.shards[index][key] = value
}

func (s *LockedShardedMap[K, V]) Get(key K) (V, bool) {
	var zero V
	if s == nil {
		return zero, false
	}
	index := s.getShardIndex(key)
	s.locks[index].RLock()
	defer s.locks[index].RUnlock()
	val, ok := s.shards[index][key]
	return val, ok
}

func (s *LockedShardedMap[K, V]) Update(key K, update func(V, bool) V) {
	if s == nil {
		return
	}
	index := s.getShardIndex(key)
	s.locks[index].Lock()
	defer s.locks[index].Unlock()
	val, ok := s.shards[index][key]
	s.shards[index][key] = update(val, ok)
}

func (s *LockedShardedMap[K, V]) Merge(other *LockedShardedMap[K, V], ag func(a, b V) V) {
	var wg sync.WaitGroup
	for i := range s.shards {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			otherShard := other.shards[i]
			for k, v := range otherShard {
				s.locks[i].Lock()
				if existing, ok := s.shards[i][k]; ok {
					s.shards[i][k] = ag(existing, v)
				} else {
					s.shards[i][k] = v
				}
				s.locks[i].Unlock()
			}
		}(i)
	}
	wg.Wait()
}

func (s *LockedShardedMap[K, V]) Len() int {
	if s == nil {
		return 0
	}
	var count int
	for i := range s.shards {
		s.locks[i].RLock()
		count += len(s.shards[i])
		s.locks[i].RUnlock()
	}
	return count
}

func (s *LockedShardedMap[K, V]) ParallelIterate(f func(K, V) error) error {
	if s == nil {
		return nil
	}

	var (
		wg    sync.WaitGroup
		errCh = make(chan error, 1)
		once  sync.Once
	)

	for i := range s.shards {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			s.locks[i].RLock()
			defer s.locks[i].RUnlock()

			for k, v := range s.shards[i] {
				if err := f(k, v); err != nil {
					once.Do(func() {
						errCh <- err
					})
					return
				}
			}
		}(i)
	}

	// Wait in a separate goroutine so we can still select on errCh.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case err := <-errCh:
		return err
	case <-done:
		return nil
	}
}

func (s *LockedShardedMap[K, V]) Iterate(f func(K, V) error) error {
	if s == nil {
		return nil
	}
	for i := range s.shards {
		s.locks[i].RLock()
		for k, v := range s.shards[i] {
			if err := f(k, v); err != nil {
				s.locks[i].RUnlock()
				return err
			}
		}
		s.locks[i].RUnlock()
	}
	return nil
}

func (s *LockedShardedMap[K, V]) IsEmpty() bool {
	if s == nil {
		return true
	}
	for i := range s.shards {
		s.locks[i].RLock()
		if len(s.shards[i]) > 0 {
			s.locks[i].RUnlock()
			return false
		}
		s.locks[i].RUnlock()
	}
	return true
}
