/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/badger/v4"
	"github.com/hypermodeinc/dgraph/v25/posting"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/types"
	"github.com/hypermodeinc/dgraph/v25/x"
)

func TestReverseEdge(t *testing.T) {
	dir, err := os.MkdirTemp("", "storetest_")
	x.Check(err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	x.Check(err)
	pstore = ps
	// Not using posting list cache
	posting.Init(ps, 0, false)
	Init(ps)
	err = schema.ParseBytes([]byte("revc: [uid] @reverse @count ."), 1)
	require.NoError(t, err)

	ctx := context.Background()
	txn := posting.Oracle().RegisterStartTs(5)
	attr := x.AttrInRootNamespace("revc")

	edge := &pb.DirectedEdge{
		ValueId: 2,
		Attr:    attr,
		Entity:  1,
		Op:      pb.DirectedEdge_DEL,
	}

	x.Check(newRunMutation(ctx, edge, txn))
	x.Check(newRunMutation(ctx, edge, txn))

	pl, err := txn.Get(x.DataKey(attr, 1))
	require.NoError(t, err)
	pl.RLock()
	c := pl.GetLength(5)
	pl.RUnlock()
	require.Equal(t, 0, c)
}

func TestReverseEdgeSetDel(t *testing.T) {
	dir, err := os.MkdirTemp("", "storetest_")
	x.Check(err)
	defer os.RemoveAll(dir)

	opt := badger.DefaultOptions(dir)
	ps, err := badger.OpenManaged(opt)
	x.Check(err)
	pstore = ps
	// Not using posting list cache
	posting.Init(ps, 0, false)
	Init(ps)
	err = schema.ParseBytes([]byte("revc: [uid] @reverse @count ."), 1)
	require.NoError(t, err)

	ctx := context.Background()
	txn := posting.Oracle().RegisterStartTs(5)
	attr := x.AttrInRootNamespace("revc")

	edgeDel := &pb.DirectedEdge{
		ValueId: 2,
		Attr:    attr,
		Entity:  3,
		Op:      pb.DirectedEdge_DEL,
	}

	edgeSet1 := &pb.DirectedEdge{
		ValueId: 2,
		Attr:    attr,
		Entity:  1,
		Op:      pb.DirectedEdge_SET,
	}

	edgeSet2 := &pb.DirectedEdge{
		ValueId: 2,
		Attr:    attr,
		Entity:  3,
		Op:      pb.DirectedEdge_SET,
	}

	edgeSet3 := &pb.DirectedEdge{
		ValueId: 2,
		Attr:    attr,
		Entity:  4,
		Op:      pb.DirectedEdge_SET,
	}

	x.Check(newRunMutation(ctx, edgeSet1, txn))
	x.Check(newRunMutation(ctx, edgeSet2, txn))
	x.Check(newRunMutation(ctx, edgeSet3, txn))
	x.Check(newRunMutation(ctx, edgeDel, txn))

	pl, err := txn.Get(x.ReverseKey(attr, 2))
	require.NoError(t, err)
	pl.RLock()
	c := pl.GetLength(5)
	pl.RUnlock()
	require.Equal(t, 2, c)
}

func TestConvertEdgeType(t *testing.T) {
	var testEdges = []struct {
		input     *pb.DirectedEdge
		to        types.TypeID
		expectErr bool
		output    *pb.DirectedEdge
	}{
		{
			input: &pb.DirectedEdge{
				Value: []byte("set edge"),
				Attr:  x.AttrInRootNamespace("name"),
			},
			to:        types.StringID,
			expectErr: false,
			output: &pb.DirectedEdge{
				Value:     []byte("set edge"),
				Attr:      x.AttrInRootNamespace("name"),
				ValueType: 9,
			},
		},
		{
			input: &pb.DirectedEdge{
				Value: []byte("set edge"),
				Attr:  x.NamespaceAttr(0xf2, "name"),
				Op:    pb.DirectedEdge_DEL,
			},
			to:        types.StringID,
			expectErr: false,
			output: &pb.DirectedEdge{
				Value:     []byte("set edge"),
				Attr:      x.NamespaceAttr(0xf2, "name"),
				Op:        pb.DirectedEdge_DEL,
				ValueType: 9,
			},
		},
		{
			input: &pb.DirectedEdge{
				ValueId: 123,
				Attr:    x.AttrInRootNamespace("name"),
			},
			to:        types.StringID,
			expectErr: true,
		},
		{
			input: &pb.DirectedEdge{
				Value: []byte("set edge"),
				Attr:  x.AttrInRootNamespace("name"),
			},
			to:        types.UidID,
			expectErr: true,
		},
	}

	for _, testEdge := range testEdges {
		err := ValidateAndConvert(testEdge.input,
			&pb.SchemaUpdate{
				ValueType: pb.Posting_ValType(testEdge.to),
			})
		if testEdge.expectErr {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
			require.True(t, reflect.DeepEqual(testEdge.input, testEdge.output))
		}
	}

}

func TestValidateEdgeTypeError(t *testing.T) {
	edge := &pb.DirectedEdge{
		Value: []byte("set edge"),
		Attr:  x.AttrInRootNamespace("name"),
	}

	err := ValidateAndConvert(edge,
		&pb.SchemaUpdate{
			ValueType: pb.Posting_ValType(types.DateTimeID),
		})
	require.Error(t, err)
}

func TestTypeSanityCheck(t *testing.T) {
	// Empty field name check.
	typeDef := &pb.TypeUpdate{
		Fields: []*pb.SchemaUpdate{
			{
				Predicate: x.AttrInRootNamespace(""),
			},
		},
	}
	err := typeSanityCheck(typeDef)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Field in type definition must have a name")

	// Object type without object name.
	typeDef = &pb.TypeUpdate{
		Fields: []*pb.SchemaUpdate{
			{
				Predicate: x.AttrInRootNamespace("name"),
				ValueType: pb.Posting_OBJECT,
			},
		},
	}
	err = typeSanityCheck(typeDef)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Field with value type OBJECT must specify the name")

	// Field with directive.
	typeDef = &pb.TypeUpdate{
		Fields: []*pb.SchemaUpdate{
			{
				Predicate: x.AttrInRootNamespace("name"),
				Directive: pb.SchemaUpdate_REVERSE,
			},
		},
	}
	err = typeSanityCheck(typeDef)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Field in type definition cannot have a directive")

	// Field with tokenizer.
	typeDef = &pb.TypeUpdate{
		Fields: []*pb.SchemaUpdate{
			{
				Predicate: x.AttrInRootNamespace("name"),
				Tokenizer: []string{"int"},
			},
		},
	}
	err = typeSanityCheck(typeDef)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Field in type definition cannot have tokenizers")
}
