/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

// Package types contains some very common utilities used by Dgraph. These utilities
// are of "miscellaneous" nature, e.g., error checking.
package types

import (
	"sync"
)

type ShardedMap struct {
	Shards []map[uint64]Val
}

const NumShards = 30

func NewShardedMap() *ShardedMap {
	shards := make([]map[uint64]Val, NumShards)
	for i := range shards {
		shards[i] = make(map[uint64]Val)
	}
	return &ShardedMap{Shards: shards}
}

func (s *ShardedMap) Merge(other *ShardedMap, ag func(a, b Val) Val) {
	var wg sync.WaitGroup
	for i := range s.Shards {
		wg.Add(1)
		go func(i int) {
			for k, v := range s.Shards[i] {
				val, ok := other.Shards[i][k]
				if !ok {
					continue
				}
				s.Shards[i][k] = ag(val, v)
			}
			for k, v := range other.Shards[i] {
				if _, ok := s.Shards[i][k]; ok {
					continue
				}
				s.Shards[i][k] = v
			}
			wg.Done()
		}(i)
	}

	wg.Wait()
}

func (s *ShardedMap) GetShardOrNil(key int) map[uint64]Val {
	if s == nil {
		return make(map[uint64]Val)
	}
	return s.Shards[key]
}

func (s *ShardedMap) init() {
	if s == nil {
		s = NewShardedMap()
	}
}

func (s *ShardedMap) getShard(key uint64) map[uint64]Val {
	return s.Shards[key%uint64(len(s.Shards))]
}

func (s *ShardedMap) Set(key uint64, value Val) {
	if s == nil {
		s.init()
	}
	shard := s.getShard(key)
	shard[key] = value
}

func (s *ShardedMap) Get(key uint64) (Val, bool) {
	if s == nil {
		return Val{}, false
	}
	shard := s.getShard(key)
	val, ok := shard[key]
	return val, ok
}

func (s *ShardedMap) Len() int {
	if s == nil {
		return 0
	}
	var count int
	for _, shard := range s.Shards {
		count += len(shard)
	}
	return count
}

func (s *ShardedMap) Iterate(f func(uint64, Val) error) error {
	for _, shard := range s.Shards {
		for k, v := range shard {
			if err := f(k, v); err != nil {
				return err
			}
		}
	}
	return nil
}
