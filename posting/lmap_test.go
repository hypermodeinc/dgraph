/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package posting

import (
	"math"
	"math/rand"
	"testing"
)

func BenchmarkGet(b *testing.B) {
	// lmap := NewMap(false)
	var key []byte
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// i := uint64(rand.Int63())
			_ = uint64(rand.Int63())
			_, _ = getNew(key, nil, math.MaxUint64, false)
			// lmap.Get(i)
		}
	})
}

func BenchmarkGetLinear(b *testing.B) {
	var key []byte
	m := make(map[uint64]*List)
	for i := 0; i < b.N; i++ {
		k := uint64(i)
		if _, ok := m[k]; !ok {
			l, err := getNew(key, nil, math.MaxUint64, false)
			if err != nil {
				b.Error(err)
			}
			m[k] = l
		}
	}
}

func BenchmarkGetLinearBool(b *testing.B) {
	m := make(map[uint64]bool)
	for i := 0; i < b.N; i++ {
		k := uint64(i)
		if _, ok := m[k]; !ok {
			m[k] = true
		}
	}
}
