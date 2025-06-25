/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package edgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/chunker"
	"github.com/hypermodeinc/dgraph/v25/dql"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/worker"
	"github.com/hypermodeinc/dgraph/v25/x"
)

func makeNquad(sub, pred string, val *api.Value) *api.NQuad {
	return &api.NQuad{
		Subject:     sub,
		Predicate:   pred,
		ObjectValue: val,
	}
}

func makeNquadEdge(sub, pred, obj string) *api.NQuad {
	return &api.NQuad{
		Subject:   sub,
		Predicate: pred,
		ObjectId:  obj,
	}
}

func makeOp(schema string) *api.Operation {
	return &api.Operation{
		Schema: schema,
	}
}

func TestParseNQuads(t *testing.T) {
	nquads := `
		_:a <predA> "A" .
		_:b <predB> "B" .
		# this line is a comment
		_:a <join> _:b .
	`
	nqs, _, err := chunker.ParseRDFs([]byte(nquads))
	require.NoError(t, err)
	require.Equal(t, []*api.NQuad{
		makeNquad("_:a", "predA", &api.Value{Val: &api.Value_DefaultVal{DefaultVal: "A"}}),
		makeNquad("_:b", "predB", &api.Value{Val: &api.Value_DefaultVal{DefaultVal: "B"}}),
		makeNquadEdge("_:a", "join", "_:b"),
	}, nqs)
}

func TestValNquads(t *testing.T) {
	nquads := `uid(m) <name> val(f) .`
	_, _, err := chunker.ParseRDFs([]byte(nquads))
	require.NoError(t, err)
}

func TestParseNQuadsWindowsNewline(t *testing.T) {
	nquads := "_:a <predA> \"A\" .\r\n_:b <predB> \"B\" ."
	nqs, _, err := chunker.ParseRDFs([]byte(nquads))
	require.NoError(t, err)
	require.Equal(t, []*api.NQuad{
		makeNquad("_:a", "predA", &api.Value{Val: &api.Value_DefaultVal{DefaultVal: "A"}}),
		makeNquad("_:b", "predB", &api.Value{Val: &api.Value_DefaultVal{DefaultVal: "B"}}),
	}, nqs)
}

func TestParseNQuadsDelete(t *testing.T) {
	nquads := `_:a * * .`
	nqs, _, err := chunker.ParseRDFs([]byte(nquads))
	require.NoError(t, err)
	require.Equal(t, []*api.NQuad{
		makeNquad("_:a", x.Star, &api.Value{Val: &api.Value_DefaultVal{DefaultVal: x.Star}}),
	}, nqs)
}

func TestValidateKeys(t *testing.T) {
	tests := []struct {
		name    string
		nquad   string
		noError bool
	}{
		{name: "test 1", nquad: `_:alice <knows> "stuff" ( "key 1" = 12 ) .`},
		{name: "test 2", nquad: `_:alice <knows> "stuff" ( "key	1" = 12 ) .`},
		{name: "test 3", nquad: `_:alice <knows> "stuff" ( ~key1 = 12 ) .`},
		{name: "test 4", nquad: `_:alice <knows> "stuff" ( "~key1" = 12 ) .`},
		{name: "test 5", nquad: `_:alice <~knows> "stuff" ( "key 1" = 12 ) .`},
		{name: "test 6", nquad: `_:alice <~knows> "stuff" ( "key	1" = 12 ) .`},
		{name: "test 7", nquad: `_:alice <~knows> "stuff" ( key1 = 12 ) .`},
		{name: "test 8", nquad: `_:alice <~knows> "stuff" ( "key1" = 12 ) .`},
		{name: "test 9", nquad: `_:alice <~knows> "stuff" ( "key	1" = 12 ) .`},
		{name: "test 10", nquad: `_:alice <knows> "stuff" ( key1 = 12 , "key 2" = 13 ) .`},
		{name: "test 11", nquad: `_:alice <knows> "stuff" ( "key1" = 12, key2 = 13 , "key	3" = "a b" ) .`},
		{name: "test 12", nquad: `_:alice <knows~> "stuff" ( key1 = 12 ) .`},
		{name: "test 13", nquad: `_:alice <knows> "stuff" ( key1 = 12 ) .`, noError: true},
		{name: "test 14", nquad: `_:alice <knows@some> "stuff" .`, noError: true},
		{name: "test 15", nquad: `_:alice <knows@some@en> "stuff" .`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nq, _, err := chunker.ParseRDFs([]byte(tc.nquad))
			require.NoError(t, err)

			err = validateKeys(nq[0])
			if tc.noError {
				require.NoError(t, err, "Unexpected error for: %+v", nq)
			} else {
				require.Error(t, err, "Expected an error: %+v", nq)
			}
		})
	}
}

func TestParseSchemaFromAlterOperation(t *testing.T) {
	md := metadata.New(map[string]string{"namespace": "123"})
	ctx := metadata.NewIncomingContext(context.Background(), md)
	dir := t.TempDir()
	ps, err := badger.OpenManaged(badger.DefaultOptions(dir))
	x.Check(err)
	defer ps.Close()
	schema.Init(ps)

	tests := []struct {
		name    string
		schema  string
		noError bool
	}{
		{
			name: "test no duplicate predicates and types",
			schema: `
  				name: string @index(fulltext, term) .
  				age: int @index(int) @upsert .
  				friend: [uid] @count @reverse .
			`,
			noError: true,
		},
		{
			name: "test duplicate predicates error",
			schema: `
				name: string @index(fulltext, term) .
				age: int @index(int) @upsert .
				friend: [uid] @count @reverse .
				friend: [uid] @count @reverse .
			`,
			noError: false,
		},
		{
			name: "test duplicate predicates error 2",
			schema: `
				name: string @index(fulltext, term) .
				age: int @index(int) @upsert .
				friend: [uid] @count @reverse .
				friend: int @index(int) @count @reverse .
			`,
			noError: false,
		},
		{
			name: "test duplicate types error",
			schema: `
				name: string .
				age: int .
				type Person {
					name
				}
				type Person {
					age
				}
			`,
			noError: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			op := makeOp(tc.schema)

			_, err := parseSchemaFromAlterOperation(ctx, op.Schema)

			if tc.noError {
				require.NoError(t, err, "Unexpected error for: %+v", tc.schema)
			} else {
				require.Error(t, err, "Expected an error: %+v", tc.schema)
			}
		})
	}

}

func TestGetHash(t *testing.T) {
	h := sha256.New()
	_, err := h.Write([]byte("0xa0x140x313233343536373839"))
	require.NoError(t, err)

	worker.Config.AclSecretKeyBytes = x.Sensitive("123456789")
	require.Equal(t, hex.EncodeToString(h.Sum(nil)), getHash(10, 20))
}

func TestVerifyUniqueWithinMutationBoundsChecks(t *testing.T) {
	t.Run("gmuIndex out of bounds", func(t *testing.T) {
		qc := &queryContext{
			gmuList: []*dql.Mutation{
				{
					Set: []*api.NQuad{
						{
							Subject:   "_:a",
							Predicate: "username",
							ObjectValue: &api.Value{
								Val: &api.Value_StrVal{StrVal: "user1"},
							},
						},
					},
				},
			},
			// Reference gmuList[1], which is out of bounds.
			uniqueVars: map[uint64]uniquePredMeta{
				encodeIndex(1, 0): {},
			},
		}
		err := verifyUniqueWithinMutation(qc)
		require.NoError(t, err)
	})

	t.Run("rdfIndex out of bounds", func(t *testing.T) {
		qc := &queryContext{
			gmuList: []*dql.Mutation{
				{
					Set: []*api.NQuad{
						// Only one element at index 0.
						{
							Subject:   "_:a",
							Predicate: "username",
							ObjectValue: &api.Value{
								Val: &api.Value_StrVal{StrVal: "user1"},
							},
						},
					},
				},
			},
			// Reference Set[1], which is out of bounds.
			uniqueVars: map[uint64]uniquePredMeta{
				encodeIndex(0, 1): {},
			},
		}
		err := verifyUniqueWithinMutation(qc)
		require.NoError(t, err)
	})

	t.Run("gmuList element is nil", func(t *testing.T) {
		qc := &queryContext{
			gmuList: []*dql.Mutation{
				nil, // Element at index 0 is nil.
			},
			uniqueVars: map[uint64]uniquePredMeta{
				encodeIndex(0, 0): {},
			},
		}
		err := verifyUniqueWithinMutation(qc)
		require.NoError(t, err)
	})

	t.Run("Set slice is nil", func(t *testing.T) {
		qc := &queryContext{
			gmuList: []*dql.Mutation{
				{
					Set: nil, // Set slice is nil.
				},
			},
			uniqueVars: map[uint64]uniquePredMeta{
				encodeIndex(0, 0): {},
			},
		}
		err := verifyUniqueWithinMutation(qc)
		require.NoError(t, err)
	})
}
