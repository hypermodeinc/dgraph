//go:build integration || upgrade

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/dgraphtest"
	"github.com/hypermodeinc/dgraph/v24/testutil"
)

func (ssuite *SystestTestSuite) TestFacetJsonInputSupportsAnyOfTerms() {
	t := ssuite.T()

	type Node struct {
		UID string `json:"uid,omitempty"`
	}

	type Resource struct {
		Node
		AccessToPermission string `json:"access.to|permission,omitempty"`
		AccessToInherit    bool   `json:"access.to|inherit"`
	}

	type User struct {
		Node
		Name     string    `json:"name,omitempty"`
		AccessTo *Resource `json:"access.to,omitempty"`
	}

	in := User{
		Node: Node{
			UID: "_:a",
		},
		AccessTo: &Resource{
			Node: Node{
				UID: "_:b",
			},
			AccessToPermission: "WRITE",
			AccessToInherit:    false,
		},
	}
	js, err := json.Marshal(&in)
	require.NoError(t, err, "the marshaling should have succeeded")

	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	txn := gcli.NewTxn()
	assigned, err := txn.Mutate(context.Background(), &api.Mutation{
		CommitNow: true,
		SetJson:   js,
	})
	require.NoError(t, err, "the mutation should have succeeded")

	// Upgrade
	ssuite.Upgrade()

	q := `{
  direct(func: uid(%s)) {
    uid
    access.to @filter(uid(%s)) @facets(anyofterms(permission, "READ WRITE")) @facets(permission,inherit) {
      uid
    }
  }
}`
	query := fmt.Sprintf(q, assigned.Uids["a"], assigned.Uids["b"])

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	resp, err := gcli.NewReadOnlyTxn().Query(context.Background(), query)
	require.NoError(t, err, "the query should have succeeded")

	//var respUser User
	dgraphapi.CompareJSON(fmt.Sprintf(`
	{
		"direct":[
			{
				"uid":"%s",
				"access.to":{
					"uid":"%s",
					"access.to|inherit": false,
					"access.to|permission": "WRITE"
				}
			}
		]
	}`, assigned.Uids["a"], assigned.Uids["b"]), string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestListWithLanguages() {
	t := ssuite.T()

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	err = gcli.SetSchema(ctx, dgo.RootNamespace,
		`pred: [string] @lang .`)
	require.Error(t, err)
}

func (ssuite *SystestTestSuite) TestNQuadMutation() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `xid: string @index(exact) .`))

	txn := gcli.NewTxn()
	assigned, err := txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
			_:breakfast <name> "" .
			_:breakfast <nil_name> "_nil_" .
			_:breakfast <xid> "breakfast" .
			_:breakfast <fruit> _:banana .
			_:breakfast <fruit> _:apple .
			_:breakfast <cereal> _:weetbix .
			_:banana <xid> "banana" .
			_:apple <xid> "apple" .
			_:weetbix <xid> "weetbix" .
		`),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	const breakfastQuery = `
	{
		q(func: eq(xid, "breakfast")) {
			name
			nil_name
			extra
			fruit {
				xid
			}
			cereal {
				xid
			}
		}
	}`

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	txn = gcli.NewTxn()
	resp, err := txn.Query(ctx, breakfastQuery)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{ "q": [ {
		"fruit": [
			{ "xid": "apple" },
			{ "xid": "banana" }
		],
		"cereal": [
			{ "xid": "weetbix" }
		],
		"name": "",
		"nil_name": "_nil_"
	}]}`, string(resp.Json))

	txn = gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		DelNquads: []byte(fmt.Sprintf(`
			<%s> <fruit>  <%s> .
			<%s> <cereal> <%s> .
			<%s> <name> * .
			<%s> <nil_name> * .`,
			assigned.Uids["breakfast"], assigned.Uids["banana"],
			assigned.Uids["breakfast"], assigned.Uids["weetbix"],
			assigned.Uids["breakfast"], assigned.Uids["breakfast"])),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	txn = gcli.NewTxn()
	resp, err = txn.Query(ctx, breakfastQuery)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{ "q": [ {
		"fruit": [
			{ "xid": "apple" }
		]
	}]}`, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestDeleteAllReverseIndex() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, "link: [uid] @reverse ."))
	assignedIds, err := gcli.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte("_:a <link> _:b ."),
	})
	require.NoError(t, err)
	aId := assignedIds.Uids["a"]
	bId := assignedIds.Uids["b"]

	/**
	we must run a query first before the next delete transaction, the
	reason is that a mutation does not wait for the previous mutation
	to finish completely with a commitTs from zero. If we run the
	deletion directly, and the previous mutation has not received a
	commitTs from zero yet, then the deletion will skip the link, and
	essentially be treated as a no-op. As a result, when we query it
	again following the reverse link, the link would still exist, and
	the test would fail.

	Running a query would make sure that the previous mutation for
	creating the link has completed with a commitTs from zero, and the
	subsequent deletion is done *AFTER* the link creation.
	*/
	_, _ = gcli.NewReadOnlyTxn().Query(ctx, fmt.Sprintf("{ q(func: uid(%s)) { link { uid } }}", aId))

	_, _ = gcli.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		DelNquads: []byte(fmt.Sprintf("<%s> <link> * .", aId)),
	})

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	resp, err := gcli.NewTxn().Query(ctx, fmt.Sprintf("{ q(func: uid(%s)) { ~link { uid } }}", bId))
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"q":[]}`, string(resp.Json))

	assignedIds, err = gcli.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(fmt.Sprintf("<%s> <link> _:c .", aId)),
	})
	require.NoError(t, err)
	cId := assignedIds.Uids["c"]

	resp, err = gcli.NewTxn().Query(ctx, fmt.Sprintf("{ q(func: uid(%s)) { ~link { uid } }}", cId))
	require.NoError(t, err)
	dgraphapi.CompareJSON(fmt.Sprintf(`{"q":[{"~link": [{"uid": "%s"}]}]}`, aId), string(resp.Json))
}

func (ssuite *SystestTestSuite) TestNormalizeEdgeCases() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, "xid: string @index(exact) ."))

	_, err = gcli.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			_:root <xid> "root" .
			_:root <link> _:a .
			_:root <link> _:b .
		`),
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	for _, test := range []struct{ query, want string }{
		{
			`{ q(func: uid(0x1)) @normalize { count(uid) } } `,
			`{ "q": [] }`,
		},
		{
			`{ q(func: uid(0x1)) @normalize { c: count(uid) } } `,
			`{ "q": [ {"c": 1} ] }`,
		},
		{
			`{ q(func: uid(0x1)) @normalize { count: count(uid) } } `,
			`{ "q": [ {"count": 1} ] }`,
		},
		{
			`{ q(func: eq(xid, "root")) @normalize { link { count(uid) } } } `,
			`{ "q": [] }`,
		},
		{
			`{ q(func: eq(xid, "root")) @normalize { link { c: count(uid) } } } `,
			`{ "q": [ {"c": 2} ] }`,
		},
		{
			`{ q(func: eq(xid, "root")) @normalize { link { count: count(uid) } } } `,
			`{ "q": [ {"count": 2} ] }`,
		},
		{
			`{ q(func: eq(xid, "root")) @normalize { count(link) } } `,
			`{ "q": [] }`,
		},
		{
			`{ q(func: eq(xid, "root")) @normalize { c: count(link) } } `,
			`{ "q": [ {"c": 2} ] }`,
		},
		{
			`{ q(func: eq(xid, "root")) @normalize { count: count(link) } } `,
			`{ "q": [ {"count": 2} ] }`,
		},
		{
			`{ q(func: uid(0x01)) @normalize { one as math(1) uno: val(one) } } `,
			`{ "q": [ {"uno": 1} ] }`,
		},
		{
			`
			{ var(func: uid(0x1, 0x2)) { ones as math(1) }
			q() @normalize { sum(val(ones)) } }
			`,
			`{ "q": [] }`,
		},
		{
			`
			{ var(func: uid(0x1, 0x2)) { ones as math(1) }
			q() @normalize { s: sum(val(ones)) } }
			`,
			`{ "q": [ {"s": 2} ] }`,
		},
	} {
		resp, err := gcli.NewTxn().Query(ctx, test.query)
		require.NoError(t, err)
		require.JSONEq(t, test.want, string(resp.GetJson()))
	}
}

func (ssuite *SystestTestSuite) TestFacetOrder() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `name: string @index(exact) .`))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
			_:a <name> "Alice" .
			_:a <friend> _:b (age=13,car="Honda") .
			_:b <name> "Bob" .
			_:a <friend> _:c (age=15,car="Tesla") .
			_:c <name> "Charlie" .
			_:a <friend> _:d .
			_:d <name> "Bubble" .
			_:a <friend> _:e .
			_:a <friend> _:f (age=20,car="Hyundai") .
			_:f <name> "Abc" .
		`),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	const friendQuery = `
	{
		q(func: eq(name, "Alice")) {
			name
			friend(orderdesc: name) @facets {
				name
			}
		}
	}`

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	txn = gcli.NewTxn()
	ctx = context.Background()
	resp, err := txn.Query(ctx, friendQuery)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`
	{
		"q":[
			{
				"friend":[
					{
						"name":"Charlie",
						"friend|age": 15,
						"friend|car": "Tesla"
					},
					{
						"name":"Bubble"
					},
					{
						"name":"Bob",
						"friend|age": 13,
						"friend|car": "Honda"
					},
					{
						"name":"Abc",
						"friend|age": 20,
						"friend|car": "Hyundai"
					}
				],
				"name":"Alice"
			}
		]
	}
	`, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestFacetsOnScalarList() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	dbSchema := `
		name: string @index(exact) .
		friend: [string] .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetJson: []byte(`
			{
				"name":"Alice",
				"friend": ["Joshua", "David", "Josh"],
				"friend|from": {
					"0": "school",
					"2": "college"
				},
				"friend|age": {
					"1": 20,
					"2": 21
				}
			}
		`),
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	const friendQuery = `
	{
		q(func: eq(name, "Alice")) {
			name
			friend @facets
		}
	}`

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	txn = gcli.NewTxn()
	ctx = context.Background()
	resp, err := txn.Query(ctx, friendQuery)
	var res struct {
		Q []struct {
			Friend     []string          `json:"friend"`
			FriendFrom map[string]string `json:"friend|from"`
			FriendAge  map[string]int    `json:"friend|age"`
		} `json:"q"`
	}
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(resp.Json, &res))
	require.Equal(t, 1, len(res.Q))
	require.Equal(t, 3, len(res.Q[0].Friend))
	require.Equal(t, 2, len(res.Q[0].FriendFrom))
	require.Equal(t, 2, len(res.Q[0].FriendAge))
	// Validate facets.
	friends := res.Q[0].Friend
	friendFrom := res.Q[0].FriendFrom
	friendAge := res.Q[0].FriendAge
	for i, friend := range friends {
		_, ok1 := friendFrom[strconv.Itoa(i)]
		_, ok2 := friendAge[strconv.Itoa(i)]
		if friend == "David" {
			require.True(t, !ok1 && ok2)
		} else if friend == "Josh" {
			require.True(t, ok1 && ok2)
		} else if friend == "Joshua" {
			require.True(t, ok1 && !ok2)
		} else {
			require.True(t, false, "FacetsOnScalarList: should not reach here")
		}
	}
}

// Shows fix for issue #1918.
func (ssuite *SystestTestSuite) TestLangAndSortBug() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	dbSchema := "name: string @index(exact) @lang ."
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			_:michael <name> "Michael" .
			_:michael <friend> _:sang .

			_:sang <name> "상현"@ko .
			_:sang <name> "Sang Hyun"@en .
		`),
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	txn = gcli.NewTxn()
	defer func() { require.NoError(t, txn.Discard(ctx)) }()
	resp, err := txn.Query(ctx, `
	{
	  q(func: eq(name, "Michael")) {
	    name
	    friend (orderdesc: name@.) {
	      name@en
	    }
	  }
	}`)
	require.NoError(t, err)
	require.JSONEq(t, `
	{"q":[{
		"name": "Michael",
		"friend": [{ "name@en": "Sang Hyun" }]
	}]}
	`, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestSortFacetsReturnNil() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	txn := gcli.NewTxn()
	assigned, err := txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			_:michael <name> "Michael" .
			_:michael <friend> _:sang (since=2012-01-02) .
			_:sang <name> "Sang Hyun" .

			_:michael <friend> _:alice (since=2014-01-02) .
			_:alice <name> "Alice" .


			_:michael <friend> _:charlie .
			_:charlie <name> "Charlie" .
			`),
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	michael := assigned.Uids["michael"]
	ctx = context.Background()
	txn = gcli.NewTxn()
	resp, err := txn.Query(ctx, `
	{
	  q(func: uid(`+michael+`)) {
	    name
	    friend @facets(orderdesc: since) {
	      name
	    }
	  }
	}`)
	require.NoError(t, err)
	require.JSONEq(t, `
		{
			"q":[
				{
					"name":"Michael",
					"friend":[
						{
							"name":"Alice",
							"friend|since":"2014-01-02T00:00:00Z"
						},
						{
							"name":"Sang Hyun",
							"friend|since":"2012-01-02T00:00:00Z"
						},
						{
							"name":"Charlie"
						}
					]
				}
			]
		}
	`, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestSchemaAfterDeleteNode() {
	// Upgrade
	ssuite.Upgrade()

	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, "married: bool ."))

	txn := gcli.NewTxn()
	assigned, err := txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			_:michael <name> "Michael" .
			_:michael <friend> _:sang .
			`),
	})
	require.NoError(t, err)
	michael := assigned.Uids["michael"]

	testutil.VerifySchema(t, gcli.Dgraph, testutil.SchemaOptions{UserPreds: `{"predicate":"friend","type":"uid","list":true},` +
		`{"predicate":"married","type":"bool"},` +
		`{"predicate":"name","type":"default"}`})

	require.NoError(t, gcli.DropPredicate(ctx, dgo.RootNamespace, "married"))

	// Lets try to do a S P * deletion. Schema for married shouldn't be rederived.
	_, err = gcli.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		DelNquads: []byte(`
		    <` + michael + `> * * .
			`),
	})
	require.NoError(t, err)

	testutil.VerifySchema(t, gcli.Dgraph, testutil.SchemaOptions{UserPreds: `{"predicate":"friend","type":"uid","list":true},` +
		`{"predicate":"name","type":"default"}`})
}

func (ssuite *SystestTestSuite) TestFullTextEqual() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, "text: string @index(fulltext) ."))

	texts := []string{"bat man", "aqua man", "bat cave", "bat", "man", "aqua", "cave"}
	var rdfs bytes.Buffer
	for i, text := range texts {
		fmt.Fprintf(&rdfs, "_:node%d <text> %q .\n", i, text)
	}
	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: rdfs.Bytes(),
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	for _, text := range texts {
		resp, err := gcli.NewTxn().Query(ctx, `
		{
			q(func: eq(text, "`+text+`")) {
				text
			}
		}`)
		require.NoError(t, err)
		require.Equal(t, `{"q":[{"text":"`+text+`"}]}`, string(resp.GetJson()))
	}

	for _, bad := range []string{"cave dweller", "bat ears", "elephant"} {
		resp, err := gcli.NewTxn().Query(ctx, `
		{
			q(func: eq(text, "`+bad+`")) {
				text
			}
		}`)
		require.NoError(t, err)
		require.Equal(t, `{"q":[]}`, string(resp.GetJson()))
	}
}

func (ssuite *SystestTestSuite) TestJSONBlankNode() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	txn := gcli.NewTxn()
	assigned, err := txn.Mutate(ctx, &api.Mutation{
		SetJson: []byte(`
			{"uid": "_:michael", "name": "Michael", "friend": [{ "uid": "_:sang", "name": "Sang Hyun"}, { "uid": "_:alice", "name": "Alice"}]}
		`),
		CommitNow: true,
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	require.Equal(t, 3, len(assigned.Uids))
	michael := assigned.Uids["michael"]
	alice := assigned.Uids["alice"]
	ctx = context.Background()
	resp, err := gcli.NewTxn().Query(ctx, `
	{
	  q(func: uid(`+michael+`)) {
	    name
	    friend @filter(uid(`+alice+`)){
	      name
	    }
	  }
	}`)
	require.NoError(t, err)
	require.JSONEq(t, `{"q":[{"name":"Michael","friend":[{"name":"Alice"}]}]}`, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestScalarToList() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `pred: string @index(exact) .`))

	uids, err := gcli.NewTxn().Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`_:blank <pred> "first" .`),
		CommitNow: true,
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	uid := uids.Uids["blank"]

	q := `
	{
		me(func: eq(pred, "first")) {
			pred
		}
	}
	`
	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	resp, err := gcli.NewTxn().Query(ctx, q)
	require.NoError(t, err)
	require.Equal(t, `{"me":[{"pred":"first"}]}`, string(resp.Json))

	dbSchema := `pred: [string] @index(exact) .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))
	resp, err = gcli.NewTxn().Query(ctx, q)
	require.NoError(t, err)
	require.Equal(t, `{"me":[{"pred":["first"]}]}`, string(resp.Json))

	_, err = gcli.NewTxn().Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`<` + uid + `> <pred> "second" .`),
		CommitNow: true,
	})
	require.NoError(t, err)

	resp, err = gcli.NewTxn().Query(ctx, q)
	require.NoError(t, err)
	require.Equal(t, `{"me":[{"pred":["second","first"]}]}`, string(resp.Json))

	q2 := `
	{
		me(func: eq(pred, "second")) {
			pred
		}
	}
	`
	resp, err = gcli.NewTxn().Query(ctx, q2)
	require.NoError(t, err)
	require.Equal(t, `{"me":[{"pred":["second","first"]}]}`, string(resp.Json))

	_, err = gcli.NewTxn().Mutate(ctx, &api.Mutation{
		DelNquads: []byte(`<` + uid + `> <pred> "second" .`),
		SetNquads: []byte(`<` + uid + `> <pred> "third" .`),
		CommitNow: true,
	})
	require.NoError(t, err)

	resp, err = gcli.NewTxn().Query(ctx, q)
	require.NoError(t, err)
	require.Equal(t, `{"me":[{"pred":["third","first"]}]}`, string(resp.Json))

	_, err = gcli.NewTxn().Mutate(ctx, &api.Mutation{
		DelNquads: []byte(`<` + uid + `> <pred> "first" .`),
		CommitNow: true,
	})
	require.NoError(t, err)

	q3 := `
	{
		me(func: eq(pred, "third")) {
			pred
		}
	}
	`
	resp, err = gcli.NewTxn().Query(ctx, q3)
	require.NoError(t, err)
	require.Equal(t, `{"me":[{"pred":["third"]}]}`, string(resp.Json))

	resp, err = gcli.NewTxn().Query(ctx, q)
	require.NoError(t, err)
	require.Equal(t, `{"me":[]}`, string(resp.Json))

	resp, err = gcli.NewTxn().Query(ctx, q2)
	require.NoError(t, err)
	require.Equal(t, `{"me":[]}`, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestListToScalar() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `pred: [string] @index(exact) .`))

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	err = gcli.SetSchema(ctx, dgo.RootNamespace, `pred: string @index(exact) .`)
	require.Error(t, err)
	require.Contains(t, err.Error(),
		`Type can't be changed from list to scalar for attr: [pred] without dropping it first.`)

	require.NoError(t, gcli.DropPredicate(ctx, dgo.RootNamespace, `pred`))
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `pred: string @index(exact) .`))
}

func (ssuite *SystestTestSuite) TestSetAfterDeletionListType() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `property.test: [string] .`))

	m1 := []byte(`
		_:alice <property.test> "initial value" .
	`)
	txn := gcli.NewTxn()
	assigned, err := txn.Mutate(context.Background(),
		&api.Mutation{SetNquads: m1},
	)
	require.NoError(t, err)
	alice := assigned.Uids["alice"]
	require.NoError(t, txn.Commit(ctx))

	q := `
	{
		me(func: uid(` + alice + `)) {
			property.test
		}
	}
	`
	resp, err := gcli.NewTxn().Query(ctx, q)
	require.NoError(t, err)
	require.Equal(t, `{"me":[{"property.test":["initial value"]}]}`, string(resp.Json))

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	txn = gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		DelNquads: []byte(`<` + alice + `> <property.test> * .`),
	})
	require.NoError(t, err)

	resp, err = txn.Query(ctx, q)
	require.NoError(t, err)
	require.Equal(t, `{"me":[]}`, string(resp.Json))

	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`<` + alice + `> <property.test> "rewritten value". `),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	resp, err = gcli.NewTxn().Query(ctx, q)
	require.NoError(t, err)
	require.Equal(t, `{"me":[{"property.test":["rewritten value"]}]}`, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestEmptyNamesWithExact() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	dbSchema := `name: string @index(exact) @lang .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	_, err = gcli.NewTxn().Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
			_:alex <name> "Alex" .
			_:alex <name@en> "Alex" .
			_:y <name> "" .
			_:y <name@ko> "상현" .
			_:amit <name> "" .
			_:amit <name@en> "" .
			_:amit <name@hi> "अमित" .
		`),
		CommitNow: true,
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	resp, err := gcli.NewTxn().Query(context.Background(), `{
	  names(func: has(name)) @filter(eq(name, "")) {
		count(uid)
	  }
	}`)
	require.NoError(t, err)

	require.Equal(t, `{"names":[{"count":2}]}`, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestEmptyRoomsWithTermIndex() {
	dbSchema := `
		room: string @index(term) .
		office.room: [uid] .`
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	_, err = gcli.NewTxn().Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
			_:o <office> "office 1" .
			_:o <office.room> _:x .
			_:o <office.room> _:y .
			_:o <office.room> _:z .
			_:x <room> "room 1" .
			_:y <room> "room 2" .
			_:z <room> "" .
		`),
		CommitNow: true,
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	resp, err := gcli.NewTxn().Query(ctx, `{
		  offices(func: has(office)) {
			count(office.room @filter(eq(room, "")))
		  }
		}`)
	require.NoError(t, err)
	require.Equal(t, `{"offices":[{"count(office.room)":1}]}`, string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestDeleteWithExpandAll() {
	dbSchema := `
		to: [uid] .
		name: string .

		type Node {
			to
			name
		}
`

	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	assigned, err := gcli.NewTxn().Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
			_:a <dgraph.type> "Node" .
			_:b <dgraph.type> "Node" .

			_:a <to> _:b .
			_:b <name> "b" .
			_:a <to> _:c .
			_:a <to> _:d .
		`),
		CommitNow: true,
	})
	require.NoError(t, err)
	auid := assigned.Uids["a"]
	buid := assigned.Uids["b"]

	_, err = gcli.NewTxn().Mutate(ctx, &api.Mutation{
		DelNquads: []byte(`
			<` + auid + `> <to> <` + buid + `> .
			<` + buid + `> * * .
		`),
		CommitNow: true,
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	q := `query test($id: string) {
		  me(func: uid($id)) {
			expand(_all_) {
				uid
			}
		  }
		}`

	type Root struct {
		Me []map[string]interface{} `json:"me"`
	}

	var r Root
	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	resp, err := gcli.NewTxn().QueryWithVars(ctx, q, map[string]string{"$id": auid})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(resp.Json, &r))
	// S P O deletion shouldn't delete "to" .
	require.Equal(t, 1, len(r.Me[0]))

	// b should not have any predicates.
	resp, err = gcli.NewTxn().QueryWithVars(ctx, q, map[string]string{"$id": buid})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(resp.Json, &r))
	require.Equal(t, 0, len(r.Me))
}

func testTimeValue(t *testing.T, c *dgraphapi.GrpcClient, timeBytes []byte) {
	nquads := []*api.NQuad{
		{
			Subject:   "0x01",
			Predicate: "friend",
			ObjectId:  "0x02",
			Facets: []*api.Facet{
				{
					Key:     "since",
					Value:   timeBytes,
					ValType: api.Facet_DATETIME,
				},
			},
		},
	}
	mu := &api.Mutation{Set: nquads, CommitNow: true}
	ctx := context.Background()
	_, err := c.NewTxn().Mutate(ctx, mu)
	require.NoError(t, err)

	q := `query test($id: string) {
		  me(func: uid($id)) {
			friend @facets {
				uid
			}
		  }
		}`

	resp, err := c.NewTxn().QueryWithVars(ctx, q, map[string]string{"$id": "0x1"})
	require.NoError(t, err)
	require.Contains(t, string(resp.Json), "since")
}

func (ssuite *SystestTestSuite) TestFacetsUsingNQuadsError() {
	// test time in go binary format
	t := ssuite.T()
	timeBinary, err := time.Now().MarshalBinary()
	require.NoError(t, err)
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	testTimeValue(t, gcli, timeBinary)

	// test time in full RFC3339 string format
	testTimeValue(t, gcli, []byte(time.Now().Format(time.RFC3339)))

	// test time in partial string formats
	testTimeValue(t, gcli, []byte("2018"))
	testTimeValue(t, gcli, []byte("2018-01"))
	testTimeValue(t, gcli, []byte("2018-01-01"))
}

func (ssuite *SystestTestSuite) TestSkipEmptyPLForHas() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	_, err = gcli.NewTxn().Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
			_:u <__user> "true" .
			_:u <name> "u" .
			_:u1 <__user> "true" .
			_:u1 <name> "u1" .
		`),
		CommitNow: true,
	})
	require.NoError(t, err)

	q := `{
		  users(func: has(__user)) {
			name
		  }
		}`
	resp, err := gcli.NewTxn().Query(ctx, q)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"users":[{"name":"u"},{"name":"u1"}]}`, string(resp.Json))

	require.NoError(t, gcli.DropAll(ctx))

	// Upgrade
	ssuite.Upgrade()

	q = `{
		  users(func: has(__user)) {
			uid
			name
		  }
		}`
	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	resp, err = gcli.NewTxn().Query(ctx, q)
	require.NoError(t, err)
	require.JSONEq(t, `{"users": []}`, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestHasWithDash() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	dbSchema := `name: string @index(hash) .`
	require.NoError(t, (gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema)))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
			_:a <name> "Alice" .
			_:a <new-friend> _:b .
			_:b <name> "Bob" .
			_:a <new-friend> _:c .
			_:c <name> "Charlie" .
		`),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	const friendQuery = `
	{
		q(func: has(new-friend)) {
			new-friend {
				name
			}
		}
	}`

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	txn = gcli.NewTxn()
	resp, err := txn.Query(ctx, friendQuery)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"q":[{"new-friend":[{"name":"Bob"},{"name":"Charlie"}]}]}`, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestListGeoFilter() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	dbSchema := `
			name: string @index(term) .
			loc: [geo] @index(geo) .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	defer func() { require.NoError(t, txn.Discard(ctx)) }()
	_, err = txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			_:a <name> "read"  .
			_:b <name> "write" .
			_:c <name> "admin" .

			_:a <loc> "{'type':'Point','coordinates':[-122.4220186,37.772318]}"^^<geo:geojson> .
			_:b <loc> "{'type':'Point','coordinates':[-62.4220186,37.772318]}"^^<geo:geojson>  .
			_:c <loc> "{'type':'Point','coordinates':[-62.4220186,37.772318]}"^^<geo:geojson>  .
			_:c <loc> "{'type':'Point','coordinates':[-122.4220186,37.772318]}"^^<geo:geojson> .
		`),
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	resp, err := gcli.NewTxn().Query(context.Background(), `{
		q(func: near(loc, [-122.4220186,37.772318], 1000)) {
			name
		}
	}`)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`
	{
		"q": [
			{
				"name": "read"
			},
			{
				"name": "admin"
			}
		]
	}
	`, string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestListRegexFilter() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	dbSchema := `
			name: string @index(term) .
			per: [string] @index(trigram) .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	defer func() { require.NoError(t, txn.Discard(ctx)) }()
	_, err = txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			_:a <name> "read"  .
			_:b <name> "write" .
			_:c <name> "admin" .

			_:a <per> "read"  .
			_:b <per> "write" .
			_:c <per> "read"  .
			_:c <per> "write" .
		`),
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	resp, err := gcli.NewTxn().Query(context.Background(), `{
		q(func: regexp(per, /^rea.*$/)) {
			name
		}
	}`)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`
	{
		"q": [
			{
				"name": "read"
			},
			{
				"name": "admin"
			}
		]
	}
	`, string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestRegexQueryWithVarsWithSlash() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	dbSchema := `data: [string] @index(trigram) .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	defer func() { require.NoError(t, txn.Discard(ctx)) }()
	_, err = txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			_:a <data> "abc/def"  .
			_:b <data> "fgh" .
			_:c <data> "ijk" .
		`),
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	// query without variable
	resp, err := gcli.NewTxn().Query(context.Background(), `{
		q(func: regexp(data, /\/def/)) {
			data
		}
	}`)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`
	{
		"q": [
			{
				"data": ["abc/def"]
			}
		]
	}
	`, string(resp.GetJson()))

	// query with variable
	resp, err = gcli.NewTxn().QueryWithVars(context.Background(), `
	query search($rex: string){
		q(func: regexp(data, $rex)) {
			data
		}
	}`, map[string]string{"$rex": "/\\/def/"})
	require.NoError(t, err)
	dgraphapi.CompareJSON(`
	{
		"q": [
			{
				"data": ["abc/def"]
			}
		]
	}
	`, string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestRegexQueryWithVars() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	dbSchema := `
		name: string @index(term) .
		per: [string] @index(trigram) .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	defer func() { require.NoError(t, txn.Discard(ctx)) }()
	_, err = txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			_:a <name> "read"  .
			_:b <name> "write" .
			_:c <name> "admin" .

			_:a <per> "read"  .
			_:b <per> "write" .
			_:c <per> "read"  .
			_:c <per> "write" .
		`),
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	resp, err := gcli.NewTxn().QueryWithVars(context.Background(), `
		query search($term: string){
			q(func: regexp(per, $term)) {
				name
			}
		}`, map[string]string{"$term": "/^rea.*$/"})
	require.NoError(t, err)
	dgraphapi.CompareJSON(`
	{
		"q": [
			{
				"name": "read"
			},
			{
				"name": "admin"
			}
		]
	}
	`, string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestGraphQLVarChild() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	dbSchema := `name: string @index(exact) .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	defer func() { require.NoError(t, txn.Discard(ctx)) }()
	au, err := txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			_:a <name> "Alice"  .
			_:b <name> "Bob" .
			_:c <name> "Charlie" .

			_:a <friend> _:c  .
			_:a <friend> _:b  .
		`),
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	a := au.Uids["a"]
	b := au.Uids["b"]
	ch := au.Uids["c"]

	// Try GraphQL variable with filter at root.
	resp, err := gcli.NewTxn().QueryWithVars(context.Background(), `
	query q($alice: string){
		q(func: eq(name, "Alice")) @filter(uid($alice)) {
			name
		}
	}`, map[string]string{"$alice": a})
	require.NoError(t, err)
	dgraphapi.CompareJSON(`
	{
		"q": [
			{
				"name": "Alice"
			}
		]
	}
	`, string(resp.GetJson()))

	// Try GraphQL variable with filter at child.
	resp, err = gcli.NewTxn().QueryWithVars(context.Background(), `
	query q($bob: string){
		q(func: eq(name, "Alice")) {
			name
			friend @filter(uid($bob)) {
				name
			}
		}
	}`, map[string]string{"$bob": b})
	require.NoError(t, err)
	dgraphapi.CompareJSON(`
	{
		"q": [
			{
				"name": "Alice",
				"friend": [
					{
						"name": "Bob"
					}
				]
			}
		]
	}
	`, string(resp.GetJson()))

	// Try GraphQL array variable with filter at child.
	friends := "[" + b + "," + ch + "]"
	resp, err = gcli.NewTxn().QueryWithVars(context.Background(), `
	query q($friends: string){
		q(func: eq(name, "Alice")) {
			name
			friend @filter(uid($friends)) {
				name
			}
		}
	}`, map[string]string{"$friends": friends})
	require.NoError(t, err)
	dgraphapi.CompareJSON(`
	{
		"q": [
			{
				"name": "Alice",
				"friend": [
					{
						"name": "Bob"
					},
					{
						"name": "Charlie"
					}
				]
			}
		]
	}
	`, string(resp.GetJson()))

}

func (ssuite *SystestTestSuite) TestMathGe() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	dbSchema := `name: string @index(exact) .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	defer func() { require.NoError(t, txn.Discard(ctx)) }()
	_, err = txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			_:root <name> "/" .
			_:root <container> _:c .
			_:c <name> "Foobar" .
		`),
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	// Try GraphQL variable with filter at root.
	resp, err := gcli.NewTxn().Query(context.Background(), `
	{
		q(func: eq(name, "/")) {
			containerCount as count(container)
			hasChildren: math(containerCount >= 1)
		}
	}`)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`
		{
		  "q": [
		    {
		      "count(container)": 1,
		      "hasChildren": true
		    }
		  ]
		}
	`, string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestHasDeletedEdge() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	txn := gcli.NewTxn()
	defer func() { require.NoError(t, txn.Discard(ctx)) }()

	assigned, err := txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			_:a <end> "" .
			_:b <end> "" .
			_:c <end> "" .
			_:d <start> "" .
			_:d2 <start> "" .
		`),
	})
	require.NoError(t, err)

	var ids []string
	for key, uid := range assigned.Uids {
		if !strings.HasPrefix(key, "d") {
			ids = append(ids, uid)
		}
	}
	sort.Strings(ids)

	type U struct {
		Uid string `json:"uid"`
	}

	getUids := func(txn *dgo.Txn) []string {
		resp, err := txn.Query(ctx, `{
			me(func: has(end)) { uid }
			you(func: has(end)) { count(uid) }
		}`)
		require.NoError(t, err)
		t.Logf("resp: %s\n", resp.GetJson())
		m := make(map[string][]U)
		require.NoError(t, json.Unmarshal(resp.GetJson(), &m))
		uids := m["me"]
		var result []string
		for _, uid := range uids {
			result = append(result, uid.Uid)
		}
		return result
	}

	txn = gcli.NewTxn()
	defer func() { require.NoError(t, txn.Discard(ctx)) }()
	uids := getUids(txn)
	require.Equal(t, 3, len(uids))
	for _, uid := range uids {
		require.Contains(t, ids, uid)
	}

	deleteMu := &api.Mutation{
		CommitNow: true,
	}
	deleteMu.DelNquads = []byte(fmt.Sprintf(`
		<%s> <end> * .
	`, ids[len(ids)-1]))
	t.Logf("deleteMu: %+v\n", deleteMu)
	_, err = txn.Mutate(ctx, deleteMu)
	require.NoError(t, err)

	txn = gcli.NewTxn()
	defer func() { require.NoError(t, txn.Discard(ctx)) }()
	uids = getUids(txn)
	require.Equal(t, 2, len(uids))
	for _, uid := range uids {
		require.Contains(t, ids, uid)
	}
	// Remove the last entry from ids.
	ids = ids[:len(ids)-1]

	// We must commit mutations before we expect them to show up as results in
	// queries, involving secondary indices.
	assigned, err = txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			_:d <end> "" .
		`),
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	require.Equal(t, 1, len(assigned.Uids))
	for _, uid := range assigned.Uids {
		ids = append(ids, uid)
	}

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	txn = gcli.NewTxn()
	defer func() { require.NoError(t, txn.Discard(ctx)) }()
	uids = getUids(txn)
	require.Equal(t, 3, len(uids))
	for _, uid := range uids {
		require.Contains(t, ids, uid)
	}
}

func (ssuite *SystestTestSuite) TestHasReverseEdge() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `follow: [uid] @reverse .`))
	txn := gcli.NewTxn()
	defer func() { require.NoError(t, txn.Discard(ctx)) }()

	_, err = txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			_:alice <name> "alice" .
			_:bob <name> "bob" .
			_:carol <name> "carol" .
			_:alice <follow> _:carol .
			_:bob <follow> _:carol .
		`),
	})
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	type F struct {
		Name string `json:"name"`
	}

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	txn = gcli.NewTxn()
	defer func() { require.NoError(t, txn.Discard(ctx)) }()
	resp, err := txn.Query(ctx, `{
		fwd(func: has(follow)) { name }
		rev(func: has(~follow)) { name }
		}`)
	require.NoError(t, err)

	t.Logf("resp: %s\n", resp.GetJson())
	m := make(map[string][]F)
	require.NoError(t, json.Unmarshal(resp.GetJson(), &m))

	fwds := m["fwd"]
	revs := m["rev"]

	require.Equal(t, len(fwds), 2)
	require.Contains(t, []string{"alice", "bob"}, fwds[0].Name)
	require.Contains(t, []string{"alice", "bob"}, fwds[1].Name)
	require.NotEqual(t, fwds[0].Name, fwds[1].Name)

	require.Equal(t, len(revs), 1)
	require.Equal(t, revs[0].Name, "carol")
}

func (ssuite *SystestTestSuite) TestMaxPredicateSize() {
	// Create a string that has more than 2^16 chars.
	var b strings.Builder
	for i := 0; i < 10000; i++ {
		b.WriteString("abcdefg")
	}
	largePred := b.String()

	// Verify that Alter requests with predicates that are too large are rejected.
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	err = gcli.SetSchema(ctx, dgo.RootNamespace, fmt.Sprintf(`%s: uid @reverse .`, largePred))
	require.Error(t, err)
	require.Contains(t, err.Error(), "Predicate name length cannot be bigger than 2^16")

	// Verify that Mutate requests with predicates that are too large are rejected.
	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(fmt.Sprintf(`_:test <%s> "value" .`, largePred)),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Predicate name length cannot be bigger than 2^16")
	_ = txn.Discard(ctx)

	// Do the same thing as above but for the predicates in DelNquads.
	txn = gcli.NewTxn()
	defer func() { require.NoError(t, txn.Discard(ctx)) }()
	_, err = txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		DelNquads: []byte(fmt.Sprintf(`_:test <%s> "value" .`, largePred)),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Predicate name length cannot be bigger than 2^16")
}

func (ssuite *SystestTestSuite) TestRestoreReservedPreds() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, gcli.DropAll(ctx))

	// Verify that the reserved predicates were restored to the schema.
	query := `schema(preds: dgraph.type) {predicate}`
	resp, err := gcli.NewReadOnlyTxn().Query(ctx, query)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"schema": [{"predicate":"dgraph.type"}]}`, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestDropData() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	schema := `
			name: string @index(term) .
			follow: [uid] @reverse .
		`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, schema))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
			_:alice <name> "alice" .
			_:bob <name> "bob" .
			_:carol <name> "carol" .
			_:alice <follow> _:carol .
			_:bob <follow> _:carol .
		`),
	})
	require.NoError(t, err)
	require.NoError(t, gcli.DropAllData(ctx))

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	// Check schema is still there.
	query := `schema(preds: [name, follow]) {predicate}`
	resp, err := gcli.NewReadOnlyTxn().Query(ctx, query)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"schema": [{"predicate":"name"}, {"predicate":"follow"}]}`, string(resp.Json))

	// Check data is gone.
	resp, err = gcli.NewTxn().Query(ctx, `{
		q(func: has(name)) {
			uid
			name
		}
	}`)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"q": []}`, string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestDropType() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace,
		`name: string .
		type Person {
			name
		}`))

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	// Check type has been added.
	query := `schema(type: Person) {}`
	resp, err := gcli.NewReadOnlyTxn().Query(ctx, query)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"types":[{"name":"Person", "fields":[{"name":"name"}]}]}`,
		string(resp.Json))

	require.NoError(t, gcli.DropType(ctx, dgo.RootNamespace, "Person"))

	// Check type is gone.
	resp, err = gcli.NewReadOnlyTxn().Query(ctx, query)
	require.NoError(t, err)
	dgraphapi.CompareJSON("{}", string(resp.Json))
}

func (ssuite *SystestTestSuite) TestCountIndexConcurrentSetDelUIDList() {
	t := ssuite.T()
	dgraphtest.ShouldSkipTest(t, "8631dab37c951b288f839789bbabac5e7088b58f", ssuite.dc.GetVersion())
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	dbSchema := `friend: [uid] @count .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	rand.Seed(time.Now().Unix())
	maxUID := 100
	txnTotal := uint64(1000)
	txnCur := uint64(0)

	insertedMap := make(map[int]struct{})
	var l sync.Mutex

	numRoutines := 10
	var wg sync.WaitGroup
	wg.Add(numRoutines)
	for i := 0; i < numRoutines; i++ {
		go func(dg *dgraphapi.GrpcClient, wg *sync.WaitGroup) {
			defer wg.Done()
			for {
				if atomic.AddUint64(&txnCur, 1) > txnTotal {
					break
				}
				id := 2 + int(rand.Int31n(int32(maxUID))) // 1 id subject id.
				mu := &api.Mutation{
					CommitNow: true,
				}

				mu.SetNquads = []byte(fmt.Sprintf("<0x1> <friend> <%#x> .", id))
				_, err := dg.NewTxn().Mutate(context.Background(), mu)
				if err != nil && err != dgo.ErrAborted {
					require.Failf(t, "unable to inserted uid with err: %s", err.Error())
				}
				if err == nil { // Successful insertion.
					l.Lock()
					if _, ok := insertedMap[id]; !ok {
						insertedMap[id] = struct{}{}
					}
					l.Unlock()
				}
			}
		}(gcli, &wg)
	}
	wg.Wait()

	q := fmt.Sprintf(`{
		me(func: eq(count(friend), %d)) {
			uid
		}
	}`, len(insertedMap))
	resp, err := gcli.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err, "the query should have succeeded")
	dgraphapi.CompareJSON(`{"me":[{"uid": "0x1"}]}`, string(resp.GetJson()))

	// Now start deleting UIDs.
	var insertedUids []int
	for uid := range insertedMap {
		insertedUids = append(insertedUids, uid)
	}
	// Avoid deleting at least one uid. There might be a scenario where all inserted uids
	// are deleted and we are querying for 0 count, which results in error.
	insertedCount := len(insertedMap)
	insertedUids = insertedUids[:len(insertedUids)-1]
	deletedMap := make(map[int]struct{})
	txnCur = uint64(0)

	wg.Add(numRoutines)
	for i := 0; i < numRoutines; i++ {
		go func(dg *dgraphapi.GrpcClient, wg *sync.WaitGroup) {
			defer wg.Done()
			for {
				if atomic.AddUint64(&txnCur, 1) > txnTotal {
					break
				}
				id := insertedUids[rand.Intn(len(insertedUids))]
				mu := &api.Mutation{
					CommitNow: true,
				}

				mu.DelNquads = []byte(fmt.Sprintf("<0x1> <friend> <%#x> .", id))
				_, err := dg.NewTxn().Mutate(context.Background(), mu)
				if err != nil && err != dgo.ErrAborted {
					require.Failf(t, "unable to delete uid with err: %s", err.Error())
				}
				if err == nil { // Successful deletion.
					l.Lock()
					if _, ok := deletedMap[id]; !ok {
						deletedMap[id] = struct{}{}
					}
					l.Unlock()
				}
			}
		}(gcli, &wg)
	}
	wg.Wait()

	q = fmt.Sprintf(`{
		me(func: eq(count(friend), %d)) {
			uid
		}
	}`, insertedCount-len(deletedMap))
	resp, err = gcli.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err, "the query should have succeeded")
	dgraphapi.CompareJSON(`{"me":[{"uid": "0x1"}]}`, string(resp.GetJson()))

	// Delete all friends now.
	mu := &api.Mutation{
		CommitNow: true,
		DelNquads: []byte("<0x1> <friend> * ."),
	}
	_, err = gcli.NewTxn().Mutate(context.Background(), mu)
	require.NoError(t, err, "mutation to delete all friends should have been succeeded")

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	resp, err = gcli.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err, "the query should have succeeded")
	dgraphapi.CompareJSON(`{"me":[]}`, string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestCountIndexConcurrentSetDelScalarPredicate() {
	t := ssuite.T()
	dgraphtest.ShouldSkipTest(t, "8631dab37c951b288f839789bbabac5e7088b58f", ssuite.dc.GetVersion())
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `name: string @index(exact) @count .`))

	rand.Seed(time.Now().Unix())
	txnTotal := uint64(100)
	txnCur := uint64(0)

	numRoutines := 10
	var wg sync.WaitGroup
	wg.Add(numRoutines)
	for i := 0; i < numRoutines; i++ {
		go func(dg *dgraphapi.GrpcClient, wg *sync.WaitGroup) {
			defer wg.Done()
			for {
				if atomic.AddUint64(&txnCur, 1) > txnTotal {
					break
				}
				id := int(rand.Int31n(int32(10000)))
				mu := &api.Mutation{
					CommitNow: true,
				}

				mu.SetNquads = []byte(fmt.Sprintf("<0x1> <name> \"name%d\" .", id))
				_, err := dg.NewTxn().Mutate(context.Background(), mu)
				if err != nil && err != dgo.ErrAborted {
					require.Fail(t, "unable to inserted uid with err: %s", err)
				}
			}
		}(gcli, &wg)
	}
	wg.Wait()

	q := `{
		q(func: eq(count(name), 1)) {
			name
		}
	}`
	resp, err := gcli.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err, "the query should have succeeded")
	var s struct {
		Q []struct {
			Name string `json:"name"`
		} `json:"q"`
	}
	require.NoError(t, json.Unmarshal(resp.GetJson(), &s))
	require.Equal(t, 1, len(s.Q))
	require.Contains(t, s.Q[0].Name, "name")

	// Upgrade
	ssuite.Upgrade()

	// Now delete the inserted name.
	mu := &api.Mutation{
		CommitNow: true,
		DelNquads: []byte(fmt.Sprintf("<0x1> <name> \"%s\" .", s.Q[0].Name)),
	}
	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	dbSchema := `name: string @index(exact) .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	// We need to rebuild count index after the mutable map changes
	dbSchema = `name: string @index(exact) @count .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	_, err = gcli.NewTxn().Mutate(context.Background(), mu)
	require.NoError(t, err, "mutation to delete name should have been succeeded")

	// Querying should return 0 uids.
	ctx = context.Background()
	resp, err = gcli.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err, "the query should have succeeded")
	dgraphapi.CompareJSON(`{"q":[]}`, string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestCountIndexNonlistPredicateDelete() {
	t := ssuite.T()
	dgraphtest.ShouldSkipTest(t, "8631dab37c951b288f839789bbabac5e7088b58f", ssuite.dc.GetVersion())
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `name: string @index(exact) @count .`))

	// Insert single record for uid 0x1.
	mu := &api.Mutation{
		CommitNow: true,
		SetNquads: []byte("<0x1> <name> \"name1\" ."),
	}

	_, err = gcli.NewTxn().Mutate(context.Background(), mu)
	require.NoError(t, err, "unable to insert name for first time")

	// query it using count index.
	q := `{
		q(func: eq(count(name), 1)) {
			uid
		}
	}`
	resp, err := gcli.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err, "the query should have succeeded")
	dgraphapi.CompareJSON(`{"q": [{"uid": "0x1"}]}`, string(resp.GetJson()))

	// Upgrade
	ssuite.Upgrade()

	// Delete by some other name.
	mu = &api.Mutation{
		CommitNow: true,
		DelNquads: []byte("<0x1> <name> \"othername\" ."),
	}

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	_, err = gcli.NewTxn().Mutate(ctx, mu)
	require.NoError(t, err, "unable to delete other name")

	// Query it using count index.
	resp, err = gcli.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err, "the query should have succeeded")
	dgraphapi.CompareJSON(`{"q": [{"uid": "0x1"}]}`, string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestReverseCountIndexDelete() {
	t := ssuite.T()
	dgraphtest.ShouldSkipTest(t, "8631dab37c951b288f839789bbabac5e7088b58f", ssuite.dc.GetVersion())
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	dbSchema := `friend: [uid] @count @reverse .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	mu := &api.Mutation{
		CommitNow: true,
	}
	mu.SetNquads = []byte(`
	<0x1> <friend> <0x2> .
	<0x1> <friend> <0x3> .`)
	_, err = gcli.NewTxn().Mutate(ctx, mu)
	require.NoError(t, err, "unable to insert friends")

	q := `{
		me(func: eq(count(~friend), 1)) {
			uid
		}
	}`
	resp, err := gcli.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err, "the query should have succeeded")
	dgraphapi.CompareJSON(`{"me":[{"uid": "0x2"}, {"uid": "0x3"}]}`, string(resp.GetJson()))

	// Upgrade
	ssuite.Upgrade()

	// Delete one friend for <0x1>.
	mu = &api.Mutation{
		CommitNow: true,
		DelNquads: []byte("<0x1> <friend> <0x2> ."),
	}
	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	_, err = gcli.NewTxn().Mutate(ctx, mu)
	require.NoError(t, err, "unable to delete friend")

	resp, err = gcli.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err, "the query should have succeeded")
	dgraphapi.CompareJSON(`{"me":[{"uid": "0x3"}]}`, string(resp.GetJson()))

}

func (ssuite *SystestTestSuite) TestReverseCountIndex() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	// This test checks that we consider reverse count index keys while doing conflict detection
	// for transactions. See https://github.com/hypermodeinc/dgraph/issues/3893 for more details.
	dbSchema := `friend: [uid] @count @reverse .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	mu := &api.Mutation{
		CommitNow: true,
	}
	mu.SetJson = []byte(`{"name": "Alice"}`)
	assigned, err := gcli.NewTxn().Mutate(ctx, mu)
	require.NoError(t, err)

	first := ""
	for _, uid := range assigned.Uids {
		first = uid
		break
	}
	require.NotEmpty(t, first)

	numRoutines := 10
	var wg sync.WaitGroup
	wg.Add(numRoutines)
	for i := 0; i < numRoutines; i++ {
		go func(dg *dgraphapi.GrpcClient, id string, wg *sync.WaitGroup) {
			defer wg.Done()
			mu := &api.Mutation{
				CommitNow: true,
			}
			mu.SetJson = []byte(`{"uid": "_:b", "friend": [{"uid": "` + id + `"}]}`)
			for i := 0; i < 10; i++ {
				_, err := dg.NewTxn().Mutate(context.Background(), mu)
				if err == nil || err != dgo.ErrAborted {
					break
				}
			}

			require.Equal(t, 1, len(assigned.Uids))
		}(gcli, first, &wg)
	}
	wg.Wait()

	q := `{
  me(func: eq(count(~friend), 10)) {
	  name
	  count(~friend)
  }
}`
	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	resp, err := gcli.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err, "the query should have succeeded")
	dgraphapi.CompareJSON(`{"me":[{"name":"Alice","count(~friend)":10}]}`, string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestTypePredicateCheck() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	// Reject schema updates if the types have missing predicates.
	// Update is rejected because name is not in the schema.
	require.Error(t, gcli.SetSchema(ctx, dgo.RootNamespace, `
		type Person {
			name
		}`))
	require.Contains(t, err.Error(), "Schema does not contain a matching predicate for field")

	// Update is accepted because name is not in the schema but is present in the same
	// update.
	ctx = context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `
		name: string .

		type Person {
			name
		}`))

	// Type with reverse predicate is not accepted if the original predicate does not exist.
	require.Error(t, gcli.SetSchema(context.Background(), dgo.RootNamespace, `
	type Person {
		name
		<~parent>
	}`))
	require.Contains(t, err.Error(), "Schema does not contain a matching predicate for field")
	require.Contains(t, err.Error(), "Schema does not contain a matching predicate for field")

	// Type with reverse predicate is accepted if the original predicate exists.
	ctx = context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `
		parent: [uid] @reverse .

		type Person {
			name
			<~parent>
		}`))
}

func (ssuite *SystestTestSuite) TestInternalPredicateCheck() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	// Schema update is rejected because uid is reserved for internal use.
	require.Error(t, gcli.SetSchema(ctx, dgo.RootNamespace, `uid: string .`))
	require.Contains(t, err.Error(), "Cannot create user-defined predicate with internal name uid")

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`_:bob <uid> "bobId" .`),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "Cannot create user-defined predicate with internal name uid")
}

func (ssuite *SystestTestSuite) TestInferSchemaAsList() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	txn := gcli.NewTxn()
	_, err = txn.Mutate(context.Background(), &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
		_:bob <name> "Bob" .
		_:bob <name> "Bob Marley" .
		_:alice <nickname> "Alice" .
		_:carol <nickname> "Carol" .`),
	})

	// Upgrade
	ssuite.Upgrade()

	require.NoError(t, err)
	query := `schema(preds: [name, nickname]) {
		list
	}`
	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	resp, err := gcli.NewReadOnlyTxn().Query(context.Background(), query)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"schema": [{"predicate":"name", "list":true},
		{"predicate":"nickname"}]}`, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestInferSchemaAsListJSON() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	txn := gcli.NewTxn()
	_, err = txn.Mutate(context.Background(), &api.Mutation{
		CommitNow: true,
		SetJson: []byte(`
			[{"name": ["Bob","Bob Marley"]}, {"nickname": "Alice"}, {"nickname": "Carol"}]`),
	})

	// Upgrade
	ssuite.Upgrade()

	require.NoError(t, err)
	query := `schema(preds: [name, nickname]) {
		list
	}`
	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	resp, err := gcli.NewReadOnlyTxn().Query(context.Background(), query)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"schema": [{"predicate":"name", "list":true},
		{"predicate":"nickname"}]}`, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestForceSchemaAsListJSON() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	txn := gcli.NewTxn()
	_, err = txn.Mutate(context.Background(), &api.Mutation{
		CommitNow: true,
		SetJson: []byte(`
			[{"name": ["Bob"]}, {"nickname": "Alice"}, {"nickname": "Carol"}]`),
	})

	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	query := `schema(preds: [name, nickname]) {
		list
	}`
	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	resp, err := gcli.NewReadOnlyTxn().Query(context.Background(), query)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"schema": [{"predicate":"name", "list":true},
		{"predicate":"nickname"}]}`, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestForceSchemaAsSingleJSON() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	txn := gcli.NewTxn()
	_, err = txn.Mutate(context.Background(), &api.Mutation{
		CommitNow: true,
		SetJson: []byte(`
			[{"person": {"name": "Bob"}}, {"nickname": "Alice"}, {"nickname": "Carol"}]`),
	})

	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	query := `schema(preds: [person, nickname]) {
		list
	}`
	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	resp, err := gcli.NewReadOnlyTxn().Query(context.Background(), query)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"schema": [{"predicate":"person"}, {"predicate":"nickname"}]}`,
		string(resp.Json))
}

func (ssuite *SystestTestSuite) TestOverwriteUidPredicates() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, gcli.DropAll(ctx))

	dbSchema := `
		best_friend: uid .
		name: string @index(exact) .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(context.Background(), &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
		_:alice <name> "Alice" .
		_:bob <name> "Bob" .
		_:alice <best_friend> _:bob .`),
	})
	require.NoError(t, err)

	q := `{
  me(func: eq(name, Alice)) {
	name
	best_friend {
		name
	}
  }
}`
	resp, err := gcli.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"me":[{"name":"Alice","best_friend": {"name": "Bob"}}]}`,
		string(resp.GetJson()))

	upsertQuery := `query { alice as var(func: eq(name, Alice)) }`
	upsertMutation := &api.Mutation{
		SetNquads: []byte(`
		_:carol <name> "Carol" .
		uid(alice) <best_friend> _:carol .`),
	}
	req := &api.Request{
		Query:     upsertQuery,
		Mutations: []*api.Mutation{upsertMutation},
		CommitNow: true,
	}
	_, err = gcli.NewTxn().Do(ctx, req)
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	resp, err = gcli.NewReadOnlyTxn().Query(context.Background(), q)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"me":[{"name":"Alice","best_friend": {"name": "Carol"}}]}`,
		string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestOverwriteUidPredicatesReverse() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, gcli.DropAll(ctx))

	dbSchema := `
		best_friend: uid @reverse .
		name: string @index(exact) .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(context.Background(), &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
		_:alice <name> "Alice" .
		_:bob <name> "Bob" .
		_:alice <best_friend> _:bob .`),
	})
	require.NoError(t, err)

	q := `{
  me(func: eq(name, Alice)) {
	name
	best_friend {
		name
	}
  }
}`
	resp, err := gcli.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"me":[{"name":"Alice","best_friend": {"name": "Bob"}}]}`,
		string(resp.GetJson()))

	reverseQuery := `{
		reverse(func: has(~best_friend)) {
			name
			~best_friend {
				name
			}
		}}`
	resp, err = gcli.NewReadOnlyTxn().Query(ctx, reverseQuery)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"reverse":[{"name":"Bob","~best_friend": [{"name": "Alice"}]}]}`,
		string(resp.GetJson()))

	upsertQuery := `query { alice as var(func: eq(name, Alice)) }`
	upsertMutation := &api.Mutation{
		SetNquads: []byte(`
		_:carol <name> "Carol" .
		uid(alice) <best_friend> _:carol .`),
	}
	req := &api.Request{
		Query:     upsertQuery,
		Mutations: []*api.Mutation{upsertMutation},
		CommitNow: true,
	}
	_, err = gcli.NewTxn().Do(ctx, req)
	require.NoError(t, err)

	resp, err = gcli.NewReadOnlyTxn().Query(ctx, reverseQuery)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"reverse":[{"name":"Carol","~best_friend": [{"name": "Alice"}]}]}`,
		string(resp.GetJson()))

	// Delete the triples and verify the reverse edge is gone.
	upsertQuery = `query {
		alice as var(func: eq(name, Alice))
		carol as var(func: eq(name, Carol)) }`
	upsertMutation = &api.Mutation{
		DelNquads: []byte(`
		uid(alice) <best_friend> uid(carol) .`),
	}
	req = &api.Request{
		Query:     upsertQuery,
		Mutations: []*api.Mutation{upsertMutation},
		CommitNow: true,
	}
	_, err = gcli.NewTxn().Do(ctx, req)
	require.NoError(t, err)

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	resp, err = gcli.NewReadOnlyTxn().Query(ctx, reverseQuery)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"reverse":[]}`,
		string(resp.GetJson()))

	resp, err = gcli.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"me":[{"name":"Alice"}]}`,
		string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestOverwriteUidPredicatesMultipleTxn() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, gcli.DropAll(ctx))

	dbSchema := `
		best_friend: uid .
		name: string @index(exact) .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	resp, err := gcli.NewTxn().Mutate(context.Background(), &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
		_:alice <name> "Alice" .
		_:bob <name> "Bob" .
		_:alice <best_friend> _:bob .`),
	})
	require.NoError(t, err)

	alice := resp.Uids["alice"]
	bob := resp.Uids["bob"]

	txn := gcli.NewTxn()
	_, err = txn.Mutate(context.Background(), &api.Mutation{
		DelNquads: []byte(fmt.Sprintf("<%s> <best_friend> <%s> .", alice, bob)),
	})
	require.NoError(t, err)

	_, err = txn.Mutate(context.Background(), &api.Mutation{
		SetNquads: []byte(fmt.Sprintf(`<%s> <best_friend> _:carl .
		_:carl <name> "Carl" .`, alice)),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(context.Background()))

	// Upgrade
	ssuite.Upgrade()

	query := fmt.Sprintf(`{
		me(func:uid(%s)) {
			name
			best_friend {
				name
			}
		}
	}`, alice)

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	resp, err = gcli.NewReadOnlyTxn().Query(context.Background(), query)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"me":[{"name":"Alice","best_friend": {"name": "Carl"}}]}`,
		string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestDeleteAndQuerySameTxn() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	dbSchema := `name: string @index(exact) .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	// Add data and commit the transaction.
	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		CommitNow: true,
		SetNquads: []byte(`
		_:alice <name> "Alice" .
		_:bob <name> "Bob" .`),
	})
	require.NoError(t, err)

	// Create a new transaction. Delete data and verify queries using the same transaction
	// see the change.
	upsertQuery := `query { bob as var(func: eq(name, Bob)) }`
	upsertMutation := &api.Mutation{
		DelNquads: []byte(`
		uid(bob) <name> * .`),
	}
	req := &api.Request{
		Query:     upsertQuery,
		Mutations: []*api.Mutation{upsertMutation},
	}
	txn2 := gcli.NewTxn()
	_, err = txn2.Do(ctx, req)
	require.NoError(t, err)

	q := `{ me(func: has(name)) { name } }`
	resp, err := txn2.Query(ctx, q)
	if err != nil {
		if !ssuite.CheckAllowedErrorPreUpgrade(err) {
			require.NoError(t, err)
		} else {
			t.Skip()
		}
	}
	dgraphapi.CompareJSON(`{"me":[{"name":"Alice"}]}`,
		string(resp.GetJson()))
	require.NoError(t, txn2.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	// Verify that changes are reflected after the transaction is committed.
	resp, err = gcli.NewReadOnlyTxn().Query(context.Background(), q)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{"me":[{"name":"Alice"}]}`,
		string(resp.GetJson()))
}

func (ssuite *SystestTestSuite) TestAddAndQueryZeroTimeValue() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `val: datetime .`))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
			_:value <val> "0000-01-01T00:00:00Z" .
		`),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	const datetimeQuery = `
	{
		q(func: has(val)) {
			val
		}
	}`

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	txn = gcli.NewTxn()
	resp, err := txn.Query(ctx, datetimeQuery)
	require.NoError(t, err)
	dgraphapi.CompareJSON(`{
		"q": [
		  {
			"val": "0000-01-01T00:00:00Z"
		  }
		]
	  }`, string(resp.Json))
}
