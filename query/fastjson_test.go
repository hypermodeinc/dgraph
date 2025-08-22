/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package query

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/task"
	"github.com/hypermodeinc/dgraph/v25/testutil"
	"github.com/hypermodeinc/dgraph/v25/types"
)

func subgraphWithSingleResultAndSingleValue(val *pb.TaskValue) *SubGraph {
	return &SubGraph{
		Params:    params{Alias: "query"},
		SrcUIDs:   &pb.List{Uids: []uint64{1}},
		DestUIDs:  &pb.List{Uids: []uint64{1}},
		uidMatrix: []*pb.List{{Uids: []uint64{1}}},
		Children: []*SubGraph{
			{
				Attr:      "val",
				SrcUIDs:   &pb.List{Uids: []uint64{1}},
				uidMatrix: []*pb.List{{}},
				valueMatrix: []*pb.ValueList{
					// UID 1
					{
						Values: []*pb.TaskValue{val},
					},
				},
			},
		},
	}
}

func assertJSON(t *testing.T, expected string, sg *SubGraph) {
	buf, err := ToJson(context.Background(), &Latency{}, []*SubGraph{sg}, nil)
	require.NoError(t, err)
	require.Equal(t, expected, string(buf))
}

func TestSubgraphToFastJSON(t *testing.T) {
	t.Run("With a string result", func(t *testing.T) {
		sg := subgraphWithSingleResultAndSingleValue(task.FromString("ABC"))
		assertJSON(t, `{"query":[{"val":"ABC"}]}`, sg)
	})

	t.Run("With an integer result", func(t *testing.T) {
		sg := subgraphWithSingleResultAndSingleValue(task.FromInt(42))
		assertJSON(t, `{"query":[{"val":42}]}`, sg)
	})

	t.Run("With a valid float result", func(t *testing.T) {
		sg := subgraphWithSingleResultAndSingleValue(task.FromFloat(42.0))
		assertJSON(t, `{"query":[{"val":42}]}`, sg)
	})

	t.Run("With invalid floating points", func(t *testing.T) {
		assertJSON(t, `{"query":[]}`, subgraphWithSingleResultAndSingleValue(task.FromFloat(math.NaN())))
		assertJSON(t, `{"query":[]}`, subgraphWithSingleResultAndSingleValue(task.FromFloat(math.Inf(1))))
	})
}

func TestEncode(t *testing.T) {
	enc := newEncoder()

	t.Run("with uid list predicate", func(t *testing.T) {
		root := enc.newNode(0)
		friendNode1 := enc.newNode(enc.idForAttr("friend"))
		require.NoError(t, enc.AddValue(friendNode1, enc.idForAttr("name"),
			types.Val{Tid: types.StringID, Value: "alice"}))
		friendNode2 := enc.newNode(enc.idForAttr("friend"))
		require.NoError(t, enc.AddValue(friendNode2, enc.idForAttr("name"),
			types.Val{Tid: types.StringID, Value: "bob"}))

		enc.AddListChild(root, friendNode1)
		enc.AddListChild(root, friendNode2)

		enc.buf.Reset()
		require.NoError(t, enc.encode(root))
		testutil.CompareJSON(t, `
		{
			"friend":[
				{
					"name":"alice"
				},
				{
					"name":"bob"
				}
			]
		}
		`, enc.buf.String())
	})

	t.Run("with value list predicate", func(t *testing.T) {
		root := enc.newNode(0)
		require.NoError(t, enc.AddValue(root, enc.idForAttr("name"),
			types.Val{Tid: types.StringID, Value: "alice"}))
		require.NoError(t, enc.AddValue(root, enc.idForAttr("name"),
			types.Val{Tid: types.StringID, Value: "bob"}))

		enc.buf.Reset()
		require.NoError(t, enc.encode(root))
		testutil.CompareJSON(t, `
		{
			"name":[
				"alice",
				"bob"
			]
		}
		`, enc.buf.String())
	})

	t.Run("with uid predicate", func(t *testing.T) {
		root := enc.newNode(0)

		person := enc.newNode(enc.idForAttr("person"))
		require.NoError(t, enc.AddValue(person, enc.idForAttr("name"), types.Val{Tid: types.StringID, Value: "alice"}))
		require.NoError(t, enc.AddValue(person, enc.idForAttr("age"), types.Val{Tid: types.IntID, Value: 25}))
		enc.AddListChild(root, person)

		enc.buf.Reset()
		require.NoError(t, enc.encode(root))
		testutil.CompareJSON(t, `
		{
			"person":[
				{
					"name":"alice",
					"age":25
				}
			]
		}
		`, enc.buf.String())
	})
}
