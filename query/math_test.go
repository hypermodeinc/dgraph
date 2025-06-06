/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package query

import (
	"math"
	"math/big"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v25/types"
)

func BenchmarkProcessBinaryLargeMap(b *testing.B) {
	shardMap1 := types.NewShardedMap()
	shardMap2 := types.NewShardedMap()

	for i := 0; i < 10000000; i++ {
		shardMap1.Set(uint64(i), types.Val{Tid: types.IntID, Value: int64(i)})
		shardMap2.Set(uint64(i), types.Val{Tid: types.IntID, Value: int64(i)})
	}

	tree := &mathTree{
		Fn: "+",
		Child: []*mathTree{
			{Var: "v1", Val: shardMap1},
			{Var: "v2", Val: shardMap2},
		},
	}

	b.Run("ProcessBinary", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			processBinary(tree)
		}
	})
}

func createShardedMap(id uint64, val types.Val) *types.ShardedMap {
	k := types.NewShardedMap()
	k.Set(id, val)
	return k
}

func TestVector(t *testing.T) {
	tree := &mathTree{
		Fn: "sqrt",
		Child: []*mathTree{{
			Fn: "dot",
			Child: []*mathTree{
				{
					Fn: "-",
					Child: []*mathTree{
						{
							Var: "v1",
							Val: createShardedMap(0, types.Val{Tid: types.VFloatID, Value: []float32{1.0, 2.0}}),
						},
						{
							Var: "v2",
							Val: createShardedMap(123, types.Val{Tid: types.VFloatID, Value: []float32{1.0, 2.0}}),
						},
					},
				},
				{
					Fn: "-",
					Child: []*mathTree{
						{
							Var: "v1",
							Val: createShardedMap(0, types.Val{Tid: types.VFloatID, Value: []float32{1.0, 2.0}}),
						},
						{
							Var: "v2",
							Val: createShardedMap(123, types.Val{Tid: types.VFloatID, Value: []float32{1.0, 2.0}}),
						},
					},
				},
			},
		}},
	}

	require.NoError(t, evalMathTree(tree))
}

func TestProcessBinary(t *testing.T) {
	tests := []struct {
		in  *mathTree
		out types.Val
	}{
		{in: &mathTree{
			Fn: "+",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
			}},
			out: types.Val{Tid: types.IntID, Value: int64(4)},
		},
		{in: &mathTree{
			Fn: "+",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(2)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: float64(4)},
		},
		{in: &mathTree{
			Fn: "+",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(2)}},
				{Const: types.Val{Tid: types.FloatID, Value: float64(2)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: float64(4)},
		},
		{in: &mathTree{
			Fn: "+",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(48038396025285290)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
			}},
			out: types.Val{Tid: types.IntID, Value: int64(48038396025285292)},
		},
		{in: &mathTree{
			Fn: "-",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(100)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(1)}},
			}},
			out: types.Val{Tid: types.IntID, Value: int64(99)},
		},
		{in: &mathTree{
			Fn: "-",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(100)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(1)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: float64(99)},
		},
		{in: &mathTree{
			Fn: "-",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(100)}},
				{Const: types.Val{Tid: types.FloatID, Value: float64(1)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: float64(99)},
		},
		{in: &mathTree{
			Fn: "*",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(3)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(3)}},
			}},
			out: types.Val{Tid: types.IntID, Value: int64(9)},
		},
		{in: &mathTree{
			Fn: "*",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(3)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(3)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: float64(9)},
		},
		{in: &mathTree{
			Fn: "*",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(3)}},
				{Const: types.Val{Tid: types.FloatID, Value: float64(3)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: float64(9)},
		},
		{in: &mathTree{
			Fn: "/",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(12)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(4)}},
			}},
			out: types.Val{Tid: types.IntID, Value: int64(3)},
		},
		{in: &mathTree{
			Fn: "/",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(12)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(4)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: float64(3)},
		},
		{in: &mathTree{
			Fn: "/",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(12)}},
				{Const: types.Val{Tid: types.FloatID, Value: float64(4)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: float64(3)},
		},
		{in: &mathTree{
			Fn: "%",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(10)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
			}},
			out: types.Val{Tid: types.IntID, Value: int64(0)},
		},
		{in: &mathTree{
			Fn: "%",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(10)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: float64(0)},
		},
		{in: &mathTree{
			Fn: "%",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(10)}},
				{Const: types.Val{Tid: types.FloatID, Value: float64(2)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: float64(0)},
		},
		{in: &mathTree{
			Fn: "max",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(1)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(100)}},
			}},
			out: types.Val{Tid: types.IntID, Value: int64(100.0)},
		},
		{in: &mathTree{
			Fn: "max",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(1)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(100)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: float64(100.0)},
		},
		{in: &mathTree{
			Fn: "max",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(1)}},
				{Const: types.Val{Tid: types.FloatID, Value: float64(100)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: float64(100.0)},
		},
		{in: &mathTree{
			Fn: "min",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(1)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(100)}},
			}},
			out: types.Val{Tid: types.IntID, Value: int64(1.0)},
		},
		{in: &mathTree{
			Fn: "min",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(1)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(100)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: float64(1.0)},
		},
		{in: &mathTree{
			Fn: "min",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(1)}},
				{Const: types.Val{Tid: types.FloatID, Value: float64(100)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: float64(1.0)},
		},
		{in: &mathTree{
			Fn: "logbase",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(16)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: 4.0},
		},
		{in: &mathTree{
			Fn: "pow",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(3)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: 8.0},
		},
		{in: &mathTree{
			Fn: "+",
			Child: []*mathTree{
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.0005, 0.25001, 0.7500001}}},
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.0005, 0.25001, 0.7500001}}},
			}},
			out: types.Val{
				Tid:   types.VFloatID,
				Value: []float32{0.001, 0.50002, 1.5000002}},
		},
		{in: &mathTree{
			Fn: "+",
			Child: []*mathTree{
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.5, 0.25, 0.75}}},
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.5, 0.75, 0.25}}},
			}},
			out: types.Val{
				Tid:   types.VFloatID,
				Value: []float32{1.0, 1.0, 1.0}},
		},
		{in: &mathTree{
			Fn: "-",
			Child: []*mathTree{
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{1.0, 1.0, 1.0}}},
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.5, 0.25, 0.75}}},
			}},
			out: types.Val{
				Tid:   types.VFloatID,
				Value: []float32{0.5, 0.75, 0.25}},
		},
		{in: &mathTree{
			Fn: "*",
			Child: []*mathTree{
				{Const: types.Val{
					Tid:   types.FloatID,
					Value: float64(2)}},
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.25, 0.5, 0.75}}},
			}},
			out: types.Val{
				Tid:   types.VFloatID,
				Value: []float32{0.5, 1.0, 1.5}},
		},
		{in: &mathTree{
			Fn: "*",
			Child: []*mathTree{
				{Const: types.Val{
					Tid:   types.IntID,
					Value: int64(2)}},
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.25, 0.5, 0.75}}},
			}},
			out: types.Val{
				Tid:   types.VFloatID,
				Value: []float32{0.5, 1.0, 1.5}},
		},
		{in: &mathTree{
			Fn: "*",
			Child: []*mathTree{
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.25, 0.5, 0.75}}},
				{Const: types.Val{
					Tid:   types.IntID,
					Value: int64(2)}},
			}},
			out: types.Val{
				Tid:   types.VFloatID,
				Value: []float32{0.5, 1.0, 1.5}},
		},
		{in: &mathTree{
			Fn: "/",
			Child: []*mathTree{
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.5, 1.0, 1.5}}},
				{Const: types.Val{
					Tid:   types.FloatID,
					Value: float64(2)}},
			}},
			out: types.Val{
				Tid:   types.VFloatID,
				Value: []float32{0.25, 0.5, 0.75}},
		},
		{in: &mathTree{
			Fn: "dot",
			Child: []*mathTree{
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{1.0, 2.0, 3.0}}},
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{1.0, 2.0, 3.0}}},
			}},
			out: types.Val{
				Tid:   types.FloatID,
				Value: float64(14.0)},
		},
	}
	for _, tc := range tests {
		t.Logf("Test: %s", tc.in.Fn)
		require.NoError(t, processBinary(tc.in))
		require.EqualValues(t, tc.out, tc.in.Const)
	}

	errorTests := []struct {
		name string
		in   *mathTree
		err  error
	}{
		{in: &mathTree{
			Fn: "+",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(9223372036854775800)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(10)}},
			}},
			err:  ErrorIntOverflow,
			name: "Addition integer overflow",
		},
		{in: &mathTree{
			Fn: "+",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(-10)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(-9223372036854775800)}},
			}},
			err:  ErrorIntOverflow,
			name: "Addition integer underflow",
		},
		{in: &mathTree{
			Fn: "-",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(9223372036854775800)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(-10)}},
			}},
			err:  ErrorIntOverflow,
			name: "Subtraction integer overflow",
		},
		{in: &mathTree{
			Fn: "-",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(-10)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(9223372036854775800)}},
			}},
			err:  ErrorIntOverflow,
			name: "Subtraction integer underflow",
		},
		{in: &mathTree{
			Fn: "*",
			Child: []*mathTree{
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.5, 0.25, 0.75}}},
				{Const: types.Val{
					Tid:   types.FloatID,
					Value: float64(1.243e40)}},
			}},
			err:  ErrorFloat32Overflow,
			name: "Float overflow Mul",
		},
		{in: &mathTree{
			Fn: "/",
			Child: []*mathTree{
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.5, 0.25, 0.75}}},
				{Const: types.Val{
					Tid:   types.FloatID,
					Value: float64(1.243e40)}},
			}},
			err:  ErrorFloat32Overflow,
			name: "Float overflow",
		},
		{in: &mathTree{
			Fn: "/",
			Child: []*mathTree{
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.5, 0.25, 0.75}}},
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.5, 0.25, 0.75}}},
			}},
			err:  errors.New("invalid types 2, 2 for func /"),
			name: "Invalid type in matchType",
		},
		{in: &mathTree{
			Fn: "dot",
			Child: []*mathTree{
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.5, 0.25, 0.75}}},
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.5, 0.25, 0.75, 0.1}}},
			}},
			err:  ErrorVectorsNotMatch,
			name: "Vectors length not match",
		},
		{in: &mathTree{
			Fn: "/",
			Child: []*mathTree{
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.5, 0.25, 0.75}}},
				{Const: types.Val{
					Tid:   types.IntID,
					Value: int64(0)}},
			}},
			err:  ErrorDivisionByZero,
			name: "Vector division by 0",
		},
		{in: &mathTree{
			Fn: "+",
			Child: []*mathTree{
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{0.5, 0.25, 0.75}}},
				{Const: types.Val{
					Tid:   types.IntID,
					Value: int64(3)}},
			}},
			err:  ErrorArgsDisagree,
			name: "Left and right arguments must match",
		},
		{in: &mathTree{
			Fn: "*",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(9223372036854775)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(10000)}},
			}},
			err:  ErrorIntOverflow,
			name: "Multiplication integer overflow",
		},
		{in: &mathTree{
			Fn: "*",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(-10000)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(9223372036854775)}},
			}},
			err:  ErrorIntOverflow,
			name: "Multiplication integer underflow",
		},
		{in: &mathTree{
			Fn: "/",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(23)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(0)}},
			}},
			err:  ErrorDivisionByZero,
			name: "Division int zero",
		},
		{in: &mathTree{
			Fn: "/",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(23)}},
				{Const: types.Val{Tid: types.FloatID, Value: float64(0)}},
			}},
			err:  ErrorDivisionByZero,
			name: "Division float zero",
		},
		{in: &mathTree{
			Fn: "%",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(23)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(0)}},
			}},
			err:  ErrorDivisionByZero,
			name: "Modulo int zero",
		},
		{in: &mathTree{
			Fn: "%",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(23)}},
				{Const: types.Val{Tid: types.FloatID, Value: float64(0)}},
			}},
			err:  ErrorDivisionByZero,
			name: "Modulo float zero",
		},
		{in: &mathTree{
			Fn: "pow",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(-2)}},
				{Const: types.Val{Tid: types.FloatID, Value: float64(1.7)}},
			}},
			err:  ErrorFractionalPower,
			name: "Fractional negative power",
		},
		{in: &mathTree{
			Fn: "logbase",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(-2)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
			}},
			err:  ErrorNegativeLog,
			name: "Log negative integer numerator",
		},
		{in: &mathTree{
			Fn: "logbase",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(-2)}},
			}},
			err:  ErrorNegativeLog,
			name: "Log negative integer denominator",
		},
		{in: &mathTree{
			Fn: "logbase",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(-2)}},
				{Const: types.Val{Tid: types.FloatID, Value: float64(2)}},
			}},
			err:  ErrorNegativeLog,
			name: "Log negative float numerator",
		},
		{in: &mathTree{
			Fn: "logbase",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(2)}},
				{Const: types.Val{Tid: types.FloatID, Value: float64(-2)}},
			}},
			err:  ErrorNegativeLog,
			name: "Log negative float denominator",
		},
	}

	for _, tc := range errorTests {
		t.Logf("Test %s", tc.name)
		err := processBinary(tc.in)
		require.EqualError(t, err, tc.err.Error())
	}
}

func TestProcessBinaryBigFloat(t *testing.T) {
	tests := []struct {
		in  *mathTree
		out types.Val
	}{
		{in: &mathTree{
			Fn: "+",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(2.15)}},
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(1.15)}},
			}},
			out: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(3.3)},
		},
		{in: &mathTree{
			Fn: "-",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(100)}},
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(1)}},
			}},
			out: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(99)},
		},
		{in: &mathTree{
			Fn: "*",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(3)}},
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(3)}},
			}},
			out: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(9)},
		},
		{in: &mathTree{
			Fn: "/",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(12)}},
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(4)}},
			}},
			out: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(3)},
		},
		{in: &mathTree{
			Fn: "max",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(1)}},
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(100)}},
			}},
			out: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(100)},
		},
		{in: &mathTree{
			Fn: "min",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(1)}},
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(100)}},
			}},
			out: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(1)},
		},
	}
	for _, tc := range tests {
		t.Logf("Test %s", tc.in.Fn)
		err := processBinary(tc.in)
		require.NoError(t, err)
		require.EqualValues(t, tc.out, tc.in.Const)
	}
}

func TestProcessUnary(t *testing.T) {
	float3 := *new(big.Float).SetPrec(200)
	float3.SetFloat64(3.1)
	sqrt3 := *new(big.Float).SetPrec(200)
	sqrt3.Sqrt(&float3)

	tests := []struct {
		in  *mathTree
		out types.Val
	}{
		{in: &mathTree{
			Fn: "u-",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
			}},
			out: types.Val{Tid: types.IntID, Value: int64(-2.0)},
		},
		{in: &mathTree{
			Fn: "u-",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.BigFloatID, Value: float3}},
			}},
			out: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(-3.1).SetPrec(200)},
		},
		{in: &mathTree{
			Fn: "ln",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(15)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: 2.70805020110221},
		},
		{in: &mathTree{
			Fn: "exp",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(1)}},
			}},
			out: types.Val{Tid: types.FloatID, Value: 2.718281828459045},
		},
		{in: &mathTree{
			Fn: "sqrt",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: 9.0}},
			}},
			out: types.Val{Tid: types.FloatID, Value: 3.0},
		},
		{in: &mathTree{
			Fn: "sqrt",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.BigFloatID, Value: float3}},
			}},
			out: types.Val{Tid: types.BigFloatID, Value: sqrt3},
		},
		{in: &mathTree{
			Fn: "floor",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: 2.5}},
			}},
			out: types.Val{Tid: types.FloatID, Value: 2.0},
		},
		{in: &mathTree{
			Fn: "floor",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.BigFloatID, Value: sqrt3}},
			}},
			out: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(1).SetPrec(200)},
		},
		{in: &mathTree{
			Fn: "ceil",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: 2.5}},
			}},
			out: types.Val{Tid: types.FloatID, Value: 3.0},
		},
		{in: &mathTree{
			Fn: "ceil",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.BigFloatID, Value: sqrt3}},
			}},
			out: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(2).SetPrec(200)},
		},
		{in: &mathTree{
			Fn: "u-",
			Child: []*mathTree{
				{Const: types.Val{
					Tid:   types.VFloatID,
					Value: []float32{1.25, 2.5, 3.25}}},
			}},
			out: types.Val{
				Tid:   types.VFloatID,
				Value: []float32{-1.25, -2.5, -3.25}},
		},
	}
	for _, tc := range tests {
		t.Logf("Test %s", tc.in.Fn)
		require.NoError(t, processUnary(tc.in))
		require.EqualValues(t, tc.out, tc.in.Const)
	}

	errorTests := []struct {
		name string
		in   *mathTree
		err  error
	}{
		{in: &mathTree{
			Fn: "ln",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(-2)}},
			}},
			err:  ErrorNegativeLog,
			name: "Negative int ln",
		},
		{in: &mathTree{
			Fn: "ln",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(-2)}},
			}},
			err:  ErrorNegativeLog,
			name: "Negative float ln",
		},
		{in: &mathTree{
			Fn: "u-",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(math.MinInt64)}},
			}},
			err:  ErrorIntOverflow,
			name: "Negation int overflow",
		},
		{in: &mathTree{
			Fn: "sqrt",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.IntID, Value: int64(-2)}},
			}},
			err:  ErrorNegativeRoot,
			name: "Negative int sqrt",
		},
		{in: &mathTree{
			Fn: "sqrt",
			Child: []*mathTree{
				{Const: types.Val{Tid: types.FloatID, Value: float64(-2)}},
			}},
			err:  ErrorNegativeRoot,
			name: "Negative float sqrt",
		},
	}

	for _, tc := range errorTests {
		t.Logf("Test %s", tc.name)
		err := processUnary(tc.in)
		require.EqualError(t, err, tc.err.Error())
	}
}

func TestProcessBinaryBoolean(t *testing.T) {
	tests := []struct {
		in  *mathTree
		out types.Val
	}{
		{in: &mathTree{
			Fn: "<",
			Child: []*mathTree{
				{Val: createShardedMap(0, types.Val{Tid: types.IntID, Value: int64(1)})},
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
			}},
			out: types.Val{Tid: types.BoolID, Value: true},
		},
		{in: &mathTree{
			Fn: ">",
			Child: []*mathTree{
				{Val: createShardedMap(0, types.Val{Tid: types.IntID, Value: int64(1)})},
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
			}},
			out: types.Val{Tid: types.BoolID, Value: false},
		},
		{in: &mathTree{
			Fn: "<=",
			Child: []*mathTree{
				{Val: createShardedMap(0, types.Val{Tid: types.IntID, Value: int64(1)})},
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
			}},
			out: types.Val{Tid: types.BoolID, Value: true},
		},
		{in: &mathTree{
			Fn: ">=",
			Child: []*mathTree{
				{Val: createShardedMap(0, types.Val{Tid: types.IntID, Value: int64(1)})},
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
			}},
			out: types.Val{Tid: types.BoolID, Value: false},
		},
		{in: &mathTree{
			Fn: "==",
			Child: []*mathTree{
				{Val: createShardedMap(0, types.Val{Tid: types.IntID, Value: int64(1)})},
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
			}},
			out: types.Val{Tid: types.BoolID, Value: false},
		},
		{in: &mathTree{
			Fn: "!=",
			Child: []*mathTree{
				{Val: createShardedMap(0, types.Val{Tid: types.IntID, Value: int64(1)})},
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
			}},
			out: types.Val{Tid: types.BoolID, Value: true},
		},
	}
	for _, tc := range tests {
		t.Logf("Test %s", tc.in.Fn)
		require.NoError(t, processBinaryBoolean(tc.in))
		val, ok := tc.in.Val.Get(0)
		require.Equal(t, true, ok)
		require.EqualValues(t, tc.out, val)
	}
}

func TestBigFloatMathsBoolean(t *testing.T) {
	tests := []struct {
		in  *mathTree
		out types.Val
	}{
		{in: &mathTree{
			Fn: "==",
			Child: []*mathTree{
				{Val: createShardedMap(0, types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(2.123)})},
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(2.123)}},
			}},
			out: types.Val{Tid: types.BoolID, Value: true},
		},
		{in: &mathTree{
			Fn: "!=",
			Child: []*mathTree{
				{Val: createShardedMap(0, types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(2.4623)})},
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(3.623)}},
			}},
			out: types.Val{Tid: types.BoolID, Value: true},
		},
		{in: &mathTree{
			Fn: ">=",
			Child: []*mathTree{
				{Val: createShardedMap(0, types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(2.123)})},
				{Val: createShardedMap(0, types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(4.123)})},
			}},
			out: types.Val{Tid: types.BoolID, Value: false},
		},
		{in: &mathTree{
			Fn: "<=",
			Child: []*mathTree{
				{Val: createShardedMap(0, types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(2.123)})},
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(3.992)}},
			}},
			out: types.Val{Tid: types.BoolID, Value: true},
		},
		{in: &mathTree{
			Fn: ">",
			Child: []*mathTree{
				{Val: createShardedMap(0, types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(2.45)})},
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(3.43)}},
			}},
			out: types.Val{Tid: types.BoolID, Value: false},
		},
		{in: &mathTree{
			Fn: "<",
			Child: []*mathTree{
				{Val: createShardedMap(0, types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(2.1213)})},
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(2.1232)}},
			}},
			out: types.Val{Tid: types.BoolID, Value: true},
		},
	}
	for _, tc := range tests {
		t.Logf("Test %s", tc.in.Fn)
		require.NoError(t, processBinaryBoolean(tc.in))
		val, ok := tc.in.Val.Get(0)
		require.Equal(t, true, ok)
		require.EqualValues(t, tc.out, val)
	}
}

func TestProcessTernary(t *testing.T) {
	tests := []struct {
		in  *mathTree
		out types.Val
	}{
		{in: &mathTree{
			Fn: "cond",
			Child: []*mathTree{
				{Val: createShardedMap(0, types.Val{Tid: types.BoolID, Value: true})},
				{Const: types.Val{Tid: types.IntID, Value: int64(1)}},
				{Const: types.Val{Tid: types.IntID, Value: int64(2)}},
			}},
			out: types.Val{Tid: types.IntID, Value: int64(1)},
		},
		{in: &mathTree{
			Fn: "cond",
			Child: []*mathTree{
				{Val: createShardedMap(0, types.Val{Tid: types.BoolID, Value: false})},
				{Const: types.Val{Tid: types.FloatID, Value: 1.0}},
				{Const: types.Val{Tid: types.FloatID, Value: 2.0}},
			}},
			out: types.Val{Tid: types.FloatID, Value: 2.0},
		},
		{in: &mathTree{
			Fn: "cond",
			Child: []*mathTree{
				{Val: createShardedMap(0, types.Val{Tid: types.BoolID, Value: false})},
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(1.456)}},
				{Const: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(2.123)}},
			}},
			out: types.Val{Tid: types.BigFloatID, Value: *big.NewFloat(2.123)},
		},
	}
	for _, tc := range tests {
		t.Logf("Test: %s", tc.in.Fn)
		err := processTernary(tc.in)
		require.NoError(t, err)
		val, ok := tc.in.Val.Get(0)
		require.Equal(t, true, ok)
		require.EqualValues(t, tc.out, val)
	}
}
