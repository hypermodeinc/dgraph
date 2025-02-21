//go:build integration || upgrade

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/testutil"
	"github.com/hypermodeinc/dgraph/v24/x"
)

func (ssuite *SystestTestSuite) TestSchemaQueryCleanup() {
	t := ssuite.T()
	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	require.NoError(t, gcli.DropAll(context.Background()))
}

func (ssuite *SystestTestSuite) TestMultipleBlockEval() {
	t := ssuite.T()

	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	dbSchema := `
      entity: string @index(exact) .
      stock: [uid] @reverse .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
      _:e1 <entity> "672E1D63-4921-420C-90A8-5B39DD77B89F" .
      _:e1 <entity.type> "chair" .
      _:e1 <entity.price> "2999.99"^^<xs:float> .

      _:e2 <entity> "03B9CB73-7424-4BE5-AE39-97CC4D2F3A21" .
      _:e2 <entity.type> "sofa" .
      _:e2 <entity.price> "899.0"^^<xs:float> .

      _:e1 <combo> _:e2 .
      _:e2 <combo> _:e1 .

      _:e3 <entity> "BDFD64A3-5CA8-41F0-98D6-E65A9DAE092D" .
      _:e3 <entity.type> "desk" .
      _:e3 <entity.price> "578.99"^^<xs:float> .

      _:e4 <entity> "AE1D1A85-9C26-4A1D-9B0B-00A8BBCFDDA0" .
      _:e4 <entity.type> "lamp" .
      _:e4 <entity.price> "199.99"^^<xs:float> .

      _:e3 <combo> _:e4 .
      _:e4 <combo> _:e3 .

      _:e5 <entity> "9021E504-65B7-4939-8C02-F73CFD5635F6" .
      _:e5 <entity.type> "table" .
      _:e5 <entity.price> "1899.98"^^<xs:float> .

      # table has no combo

      _:s1 <stock> _:e1 .
      _:s1 <stock.in> "100"^^<xs:int> .
      _:s1 <stock.note> "Over-stocked" .
      _:e1 <stock> _:s1 .

      _:s2 <stock> _:e2 .
      _:s2 <stock.in> "20"^^<xs:int> .
      _:s2 <stock.note> "Running low, order more" .
      _:e2 <stock> _:s2 .

      _:s3 <stock> _:e3 .
      _:s3 <stock.in> "25"^^<xs:int> .
      _:s3 <stock.note> "Delicate needs insurance" .
      _:e3 <stock> _:s3 .

      _:s4 <stock> _:e4 .
      _:s4 <stock.out> "true"^^<xs:boolean> .
      _:s4 <stock.note> "Out of stock" .
      _:e4 <stock> _:s4 .

      _:s5 <stock> _:e5 .
      _:s5 <stock.out> "true"^^<xs:boolean> .
      _:s5 <stock.note> "Out of stock" .
      _:e5 <stock> _:s5 .
    `),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	tests := []struct {
		in  string
		out string
	}{
		{in: "672E1D63-4921-420C-90A8-5B39DD77B89F",
			out: `{"q": [{
        "notes": [{
          "stock.note": "Over-stocked"
        }],
        "sku": "672E1D63-4921-420C-90A8-5B39DD77B89F",
        "type": "chair",
        "combos": 1
      }]}`},
		{in: "03B9CB73-7424-4BE5-AE39-97CC4D2F3A21",
			out: `{"q": [{
        "notes": [{
          "stock.note": "Running low, order more"
        }],
        "sku": "03B9CB73-7424-4BE5-AE39-97CC4D2F3A21",
        "type": "sofa",
        "combos": 1
      }]}`},
		{in: "BDFD64A3-5CA8-41F0-98D6-E65A9DAE092D",
			out: `{"q": [{
        "notes": [{
          "stock.note": "Delicate needs insurance"
        }],
        "sku": "BDFD64A3-5CA8-41F0-98D6-E65A9DAE092D",
        "type": "desk",
        "combos": 1
      }]}`},
		{in: "AE1D1A85-9C26-4A1D-9B0B-00A8BBCFDDA0",
			out: `{"q": [{
      "combos": 1,
      "notes": [{
          "stock.note": "Out of stock"
      }],
      "sku": "AE1D1A85-9C26-4A1D-9B0B-00A8BBCFDDA0",
      "type": "lamp"
    }]}`},
		{in: "9021E504-65B7-4939-8C02-F73CFD5635F6",
			out: `{"q":[]}`},
	}

	queryFmt := `
  {
    filter_uids as var(func: eq(entity, "%s"))
      @filter(has(entity.type) and not(has(stock.out)) and (has(combo)))

    entity_uids as var (func: uid(filter_uids)) @filter()

    var(func: uid(entity_uids)) {
      stock_uid as stock
    }

    var(func: uid(entity_uids)) {
      stock {
        available as stock @filter(has(entity.price) and not(has(stock.out)))
      }
    }

    var(func: uid(available)) @cascade {
      combo {
        cnt_combos as math(1)
        combo {
          ~stock {
            ~stock @filter(has(combo))
          }
        }
      }
      available_combos as sum(val(cnt_combos))
    }

    q(func: uid(available)) {
      sku: entity
      type: entity.type
      notes: ~stock @filter(uid(stock_uid)) {
        stock.note
      }
      combos: val(available_combos)
    }
  }`

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	txn = gcli.NewTxn()
	for _, tc := range tests {
		resp, err := txn.Query(ctx, fmt.Sprintf(queryFmt, tc.in))
		require.NoError(t, err)
		dgraphapi.CompareJSON(tc.out, string(resp.Json))
	}
}

func (ssuite *SystestTestSuite) TestUnmatchedVarEval() {
	t := ssuite.T()

	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	dbSchema := `
      item: string @index(hash) .
      style.type: string .
      style.name: string .
      style.cool: bool .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
      _:a <item> "chair" .
      _:a <style.name> "Modern leather chair" .
      _:a <style.cool> "true" .

      _:b <item> "chair" .
      _:b <style.name> "Broken beanbag" .
      _:b <style.cool> "false"^^<xs:boolean> .

      _:c <item> "sofa" .
      _:c <style.name> "U-shape sectional" .
      _:c <style.cool> "true"^^<xs:boolean> .

      _:d <item> "table" .
      _:d <style.name> "Glass top marble table" .
      _:d <style.cool> "true"^^<xs:boolean> .
    `),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	tests := []struct {
		in  string
		out string
	}{
		{
			in: `
      {
        items as var(func: eq(item, "chair")) @filter(has(style.name)) @cascade {
          is_cool as style.cool
        }
        q(func: eq(item, "chair")) @filter(eq(val(is_cool), false) AND uid(items)) {
          item
          style.name
          style.cool
          is_cool
        }
      }`,
			out: `
      {
        "q": [
          {
            "item": "chair",
            "style.cool": false,
            "style.name": "Broken beanbag"
          }
        ]
      }`,
		},
		{
			// filtering by dummy (no such pred) reduces to empty set.
			// is_cool would be assigned as default type to aid in filter eval.
			// @filter(eq(val(is_cool), false) would effectively evaluate: "" eq "false"
			in: `
      {
        items as var(func: eq(item, "chair")) @filter(has(dummy)) @cascade {
          is_cool as style.cool
        }
        q(func: eq(item, "chair")) @filter(eq(val(is_cool), false) AND uid(items)) {
          item
          style.name
          style.cool
          is_cool
        }
      }`,
			out: `{"q": []}`,
		},
	}

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	txn = gcli.NewTxn()
	for _, tc := range tests {
		resp, err := txn.Query(ctx, tc.in)
		require.NoError(t, err)
		dgraphapi.CompareJSON(tc.out, string(resp.Json))
	}
}

func (ssuite *SystestTestSuite) TestSchemaQuery() {
	t := ssuite.T()

	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	dbSchema := `name: string @index(exact) .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`_:n1 <name> "srfrog" .`),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	testutil.VerifySchema(t, gcli.Dgraph, testutil.SchemaOptions{UserPreds: `
	  {
        "predicate": "name",
        "type": "string",
        "index": true,
        "tokenizer": [
          "exact"
        ]
      }`})
}

func (ssuite *SystestTestSuite) TestSchemaQueryPredicate1() {
	t := ssuite.T()

	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	dbSchema := `
		name: string @index(exact) .
		age: int .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
      _:p1 <name> "srfrog" .
      _:p1 <age> "25"^^<xs:int> .
      _:p2 <name> "mary" .
      _:p2 <age> "22"^^<xs:int> .
      _:p1 <friends> _:p2 .
    `),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	txn = gcli.NewTxn()
	resp, err := txn.Query(ctx, `schema {name}`)
	require.NoError(t, err)
	js := `
  {
    "schema": [
	  {
		"predicate": "dgraph.drop.op"
	  },
	  {
		"predicate": "dgraph.graphql.p_query"
	  },
      {
        "predicate": "dgraph.xid"
	  },
      {
        "predicate": "dgraph.password"
      },
	  {
		  "predicate": "dgraph.acl.rule"
	  },
	  {
		  "predicate": "dgraph.rule.predicate"
	  },
	  {
		  "predicate": "dgraph.rule.permission"
	  },
	  {
        "predicate": "dgraph.graphql.schema"
	  },
	  {
        "predicate": "dgraph.graphql.xid"
	  },
      {
        "predicate": "dgraph.user.group"
      },
      {
        "predicate": "friends"
      },
      {
        "predicate": "dgraph.type"
      },
      {
        "predicate": "name"
      },
      {
        "predicate": "age"
      }
    ],
	"types": [` + testutil.GetInternalTypes(false) + `
	]
  }`
	dgraphapi.CompareJSON(js, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestSchemaQueryPredicate2() {
	t := ssuite.T()

	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `name: string @index(exact) .`))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`_:n1 <name> "srfrog" .`),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	txn = gcli.NewTxn()
	resp, err := txn.Query(ctx, `schema(pred: [name]) {}`)
	require.NoError(t, err)
	js := `
  {
    "schema": [
      {
        "predicate": "name",
        "type": "string",
        "index": true,
        "tokenizer": [
          "exact"
        ]
      }
    ]
  }`
	dgraphapi.CompareJSON(js, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestSchemaQueryPredicate3() {
	t := ssuite.T()

	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	dbSchema := `
      name: string @index(exact) .
      age: int .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
      _:p1 <name> "srfrog" .
      _:p1 <age> "25"^^<xs:int> .
      _:p2 <name> "mary" .
      _:p2 <age> "22"^^<xs:int> .
      _:p1 <friends> _:p2 .
    `),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	txn = gcli.NewTxn()
	resp, err := txn.Query(ctx, `
    schema(pred: [age]) {
      name
      type
    }
  `)
	require.NoError(t, err)
	js := `
  {
    "schema": [
      {
        "predicate": "age",
        "type": "int"
      }
    ]
  }`
	dgraphapi.CompareJSON(js, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestSchemaQueryHTTP() {
	t := ssuite.T()

	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `name: string @index(exact) .`))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`_:n1 <name> "srfrog" .`),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	hcli, err := ssuite.dc.HTTPClient()
	require.NoError(t, err)
	err = hcli.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace)
	require.NotNil(t, hcli.AccessJwt, "token is nil")
	require.NoError(t, err)

	res, err := hcli.PostDqlQuery(`schema{}`)
	require.NoError(t, err)
	require.NotNil(t, res)

	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(res, &m))
	require.NotNil(t, m["extensions"])
	dgraphapi.CompareJSON(testutil.GetFullSchemaJSON(testutil.SchemaOptions{UserPreds: `
	  {
        "predicate": "name",
        "type": "string",
        "index": true,
        "tokenizer": [
          "exact"
        ]
      }`}), string(m["data"]))
}

func (ssuite *SystestTestSuite) TestFuzzyMatch() {
	t := ssuite.T()

	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	dbSchema := `
      term: string @index(trigram) .
      name: string .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
      _:t0 <term> "" .
      _:t1 <term> "road" .
      _:t2 <term> "avenue" .
      _:t3 <term> "street" .
      _:t4 <term> "boulevard" .
      _:t5 <term> "drive" .
      _:t6 <term> "route" .
      _:t7 <term> "pass" .
      _:t8 <term> "pathway" .
      _:t9 <term> "lane" .
      _:ta <term> "highway" .
      _:tb <term> "parkway" .
      _:tc <term> "motorway" .
      _:td <term> "high road" .
      _:te <term> "side street" .
      _:tf <term> "dual carriageway" .
      _:n0 <name> "srfrog" .
    `),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	tests := []struct {
		in, out, failure string
	}{
		{
			in:  `{q(func:match(term, drive, 0)) {term}}`,
			out: `{"q":[{"term":"drive"}]}`,
		},
		{
			in:  `{q(func:match(term, "plano", 1)) {term}}`,
			out: `{"q":[]}`,
		},
		{
			in:  `{q(func:match(term, "plano", 2)) {term}}`,
			out: `{"q":[{"term":"lane"}]}`,
		},
		{
			in:  `{q(func:match(term, "plano", 8)) {term}}`,
			out: `{"q":[{"term":"lane"}]}`,
		},
		{
			in: `{q(func:match(term, way, 8)) {term}}`,
			out: `{"q":[
        {"term": "highway"},
        {"term": "pathway"},
        {"term": "parkway"},
        {"term": "motorway"}
      ]}`,
		},
		{
			in: `{q(func:match(term, pway, 8)) {term}}`,
			out: `{"q":[
        {"term": "highway"},
        {"term": "pathway"},
        {"term": "parkway"},
        {"term": "motorway"}
      ]}`,
		},
		{
			in: `{q(func:match(term, high, 8)) {term}}`,
			out: `{"q":[
        {"term": "highway"},
        {"term": "high road"}
      ]}`,
		},
		{
			in: `{q(func:match(term, str, 8)) {term}}`,
			out: `{"q":[
        {"term": "street"},
        {"term": "side street"}
      ]}`,
		},
		{
			in: `{q(func:match(term, strip, 8)) {term}}`,
			out: `{"q":[
        {"term": "street"},
        {"term": "side street"}
      ]}`,
		},
		{
			in:  `{q(func:match(term, strip, 3)) {term}}`,
			out: `{"q":[{"term": "street"}]}`,
		},
		{
			in: `{q(func:match(term, "carigeway", 8)) {term}}`,
			out: `{"q":[
        {"term": "highway"},
        {"term": "motorway"},
        {"term": "dual carriageway"},
        {"term": "pathway"},
        {"term": "parkway"}
      ]}`,
		},
		{
			in: `{q(func:match(term, "carigeway", 4)) {term}}`,
			out: `{"q":[
        {"term": "highway"},
        {"term": "parkway"}
      ]}`,
		},
		{
			in: `{q(func:match(term, "dualway", 8)) {term}}`,
			out: `{"q":[
        {"term": "highway"},
        {"term": "pathway"},
        {"term": "parkway"},
        {"term": "motorway"}
      ]}`,
		},
		{
			in:  `{q(func:match(term, "dualway", 2)) {term}}`,
			out: `{"q":[]}`,
		},
		{
			in:      `{q(func:match(name, "someone", 8)) {name}}`,
			failure: `Attribute name is not indexed with type trigram`,
		},
	}
	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	for _, tc := range tests {
		resp, err := gcli.NewTxn().Query(ctx, tc.in)
		if tc.failure != "" {
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.failure)
			continue
		}
		require.NoError(t, err)
		dgraphapi.CompareJSON(tc.out, string(resp.Json))
	}
}

func (ssuite *SystestTestSuite) TestCascadeParams() {
	t := ssuite.T()

	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `name: string @index(fulltext) .`))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
		_:alice1 <name> "Alice 1" .
		_:alice1 <age> "23" .
		_:alice2 <name> "Alice 2" .
		_:alice3 <name> "Alice 3" .
		_:alice3 <age> "32" .
		_:bob <name> "Bob" .
		_:chris <name> "Chris" .
		_:dave <name> "Dave" .
		_:alice1 <friend> _:bob (close=true) .
		_:alice1 <friend> _:dave .
		_:alice2 <friend> _:chris (close=false) .
		  _:bob <friend> _:chris .
	`),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	tests := []struct {
		in, out string
	}{
		{
			// value preds Parameterized at root.
			in: `
			{
				q(func: anyoftext(name, "Alice")) @cascade(name, age)   {
					name
					age
					friend {
						name
					}
				}
		  	}`,
			out: `{
				"q": [
				  {
					"name": "Alice 1",
					"age": "23",
					"friend": [
					  {
						"name": "Bob"
					  },
					  {
						"name": "Dave"
					  }
					]
				  },
				  {
					"name": "Alice 3",
					"age": "32"
				  }
				]
			  }`,
		},
		// value and uid preds in root cascade params
		{
			in: `{
				q(func: anyoftext(name, "Alice")) @cascade(name, age, friend)   {
				  name
				  age
					  friend {
					  name
				  }
				}
			  }
			  `,
			out: `{
				"q": [
				  {
					"name": "Alice 1",
					"age": "23",
					"friend": [
					  {
						"name": "Bob"
					  },
					  {
						"name": "Dave"
					  }
					]
				  }
				]
			  }`,
		},
		{
			// Plain cascade at root, implicit at lower level
			in: `{
				q(func: anyoftext(name, "Alice")) @cascade {
				  name
				  age
					friend {
					  name
				  	  age
				  	}
				}
			}
			  `,
			out: `{
				"q": []
			}`,
		},
		{
			// @cascade(__all__) is same as @cascade
			in: `{
				q(func: anyoftext(name, "Alice")) @cascade(__all__) {
				  name
				  age
					friend {
					  name
				  	  age
				  	}
				}
			}
			  `,
			out: `{
				"q": []
			}`,
		},
		{
			// Plain cascade at root, explicit at lower level
			in: `{
				q(func: anyoftext(name, "Alice")) @cascade {
				  name
				  age
					friend @cascade {
					  name
				  	  age
				    }
				}
			}
			  `,
			out: `{
				"q": []
			}`,
		},
		{
			// No cascade anywhere.
			in: `
			{
				q(func: anyoftext(name, "Alice")) {
				  name
				  age
				  friend {
				    name
				    age
				  }
				}
			}
			`,
			out: `
			{
				"q": [
				  {
					"name": "Alice 1",
					"age": "23",
					"friend": [
					  {
						"name": "Bob"
					  },
					  {
						"name": "Dave"
					  }
					]
				  },
				  {
					"name": "Alice 2",
					"friend": [
					  {
						"name": "Chris"
					  }
					]
				  },
				  {
					"name": "Alice 3",
					"age": "32"
				  }
				]
			}
			`,
		},

		// Parameterized at lower level.
		{
			in: `
			{
				q(func: anyoftext(name, "Alice")) {
				  name
				  age
				  friend @cascade(name, age) {
					name
				    age
				  }
				}
			}
			`,
			out: `
			{
				"q": [
				  {
					"name": "Alice 1",
					"age": "23"
				  },
				  {
					"name": "Alice 2"
				  },
				  {
					"name": "Alice 3",
					"age": "32"
				  }
				]
			}
			`,
		},

		// Parameterized at root and lower level.
		{
			in: `
			{
				q(func: anyoftext(name, "Alice")) @cascade(friend) {
				  name
				  age
				  friend @cascade(name, age) {
					name
				    age
				  }
				}
			}
			`,
			out: `
			{
				"q": []
			}
			`,
		},

		// cascade at root and parameterized at lower level.
		{
			in: `
			{
				q(func: anyoftext(name, "Alice")) @cascade {
				  name
				  friend @cascade(name) {
						  name
						  age
				  }
				}
			}
			`,
			out: `
			{
				"q": [
				  {
					"name": "Alice 1",
					"friend": [
					  {
						"name": "Bob"
					  },
					  {
						"name": "Dave"
					  }
					]
				  },
				  {
					"name": "Alice 2",
					"friend": [
					  {
						"name": "Chris"
					  }
					]
				  }
				]
			}
			`,
		},

		// Param Cascade at root, facet filtering at lower level. Contrast this with
		// next query/response.
		{
			in: `
			{
				q(func: anyoftext(name, "Alice")) @cascade(friend) {
				  name
				  age
				  friend @facets(eq(close, false)) {
					name
				  }
				}
			}
			`,
			out: `
			{
				"q": [
				  {
					"name": "Alice 2",
					"friend": [
					  {
						"name": "Chris"
					  }
					]
				  }
				]
			}
			`,
		},

		// @cascade at root, facet-filtering at lower level. This is why we implemented
		// the Parameterized Cascade.
		{
			in: `
			{
				q(func: anyoftext(name, "Alice")) @cascade {
				  name
				  age
				  friend @facets(eq(close, false)) {
					name
				  }
				}
			}
			`,
			out: `
			{
				"q": []
			}
			`,
		},

		// Parameterized at root, facet-filtering at next level. Implicit cascade at inner-levels.
		{
			in: `
			{
				q(func: anyoftext(name, "Alice")) @cascade(friend) {
				  name
				  age
				  friend @facets(eq(close, true)) {
					name
					friend {
					  name
					}
				  }
				}
			}
			`,
			out: `
			{
				"q": [
				  {
					"name": "Alice 1",
					"age": "23",
					"friend": [
					  {
						"name": "Bob",
						"friend": [
						  {
							"name": "Chris"
						  }
						]
					  }
					]
				  }
				]
			}
			`,
		},

		// Same idea as above with facets' OR operator.
		{
			in: `
			{
				q(func: anyoftext(name, "Alice")) @cascade(friend) {
				  name
					age
				    friend @facets(eq(close, true) OR eq(close, false)) {
				      name
				    }
				}
			}
			`,
			out: `
			{
				"q": [
				  {
					"name": "Alice 1",
					"age": "23",
					"friend": [
					  {
						"name": "Bob"
					  }
					]
				  },
				  {
					"name": "Alice 2",
					"friend": [
					  {
						"name": "Chris"
					  }
					]
				  }
				]
			}
			`,
		},

		// Non existent param in cascade - ignored.
		{
			in: `
			{
				q(func: anyoftext(name, "Alice")) @cascade(foo) {
				  name
					age
         		    friend @facets(eq(close, true) OR eq(close, false)) {
					  name
					  age
				    }
				}
			}
			`,
			out: `
			{
				"q": [
					{
					  "name": "Alice 1",
					  "age": "23",
					  "friend": [
						{
						  "name": "Bob"
						}
					  ]
					},
					{
					  "name": "Alice 2",
					  "friend": [
						{
						  "name": "Chris"
						}
					  ]
					},
					{
					  "name": "Alice 3",
					  "age": "32"
					}
				  ]
			}
			`,
		},
	}

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	for _, tc := range tests {
		resp, err := gcli.NewTxn().Query(ctx, tc.in)
		require.NoError(t, err)
		dgraphapi.CompareJSON(tc.out, string(resp.Json))
	}
}

func (ssuite *SystestTestSuite) TestQueryHashIndex() {
	t := ssuite.T()

	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	dbSchema := `name: string @index(hash) @lang .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
      _:p0 <name> "" .
      _:p1 <name> "0" .
      _:p2 <name> "srfrog" .
      _:p3 <name> "Lorem ipsum" .
      _:p4 <name> "Lorem ipsum dolor sit amet" .
      _:p5 <name> "Lorem ipsum dolor sit amet, consectetur adipiscing elit" .
      _:p6 <name> "Lorem ipsum"@en .
      _:p7 <name> "Lorem ipsum dolor sit amet"@en .
      _:p8 <name> "Lorem ipsum dolor sit amet, consectetur adipiscing elit"@en .
      _:p9 <name> "Lorem ipsum dolor sit amet, consectetur adipiscing elit. Sed varius tellus ut sem bibendum, eu tristique augue congue. Praesent eget odio tincidunt, pellentesque ante sit amet, tempus sem. Donec et tellus et diam facilisis egestas ut ac risus. Proin feugiat risus tristique erat condimentum placerat. Nulla eget ligula tempus, blandit leo vel, accumsan tortor. Phasellus et felis in diam ultricies porta nec in ipsum. Phasellus id leo sagittis, bibendum enim ut, pretium lectus. Quisque ac ex viverra, suscipit turpis sed, scelerisque metus. Sed non dui facilisis, viverra leo eget, vulputate erat. Etiam nec enim sed nisi imperdiet cursus. Suspendisse sed ligula non nisi pharetra varius." .
      _:pa <name> ""@fr .
    `),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	tests := []struct {
		in, out string
	}{
		{
			in: `schema(pred: [name]) {}`,
			out: `
      {
        "schema": [
          {
            "index": true,
            "lang": true,
            "predicate": "name",
            "tokenizer": [
              "hash"
            ],
            "type": "string"
          }
        ]
      }`,
		},
		{
			in:  `{q(func:eq(name,"")){name}}`,
			out: `{"q": [{"name":""}]}`,
		},
		{
			in:  `{q(func:eq(name,"0")){name}}`,
			out: `{"q": [{"name":"0"}]}`,
		},
		{
			in:  `{q(func:eq(name,"srfrog")){name}}`,
			out: `{"q": [{"name":"srfrog"}]}`,
		},
		{
			in:  `{q(func:eq(name,"Lorem ipsum")){name}}`,
			out: `{"q": [{"name":"Lorem ipsum"}]}`,
		},
		{
			in:  `{q(func:eq(name,"Lorem ipsum dolor sit amet")){name}}`,
			out: `{"q": [{"name":"Lorem ipsum dolor sit amet"}]}`,
		},
		{
			in:  `{q(func:eq(name@en,"Lorem ipsum")){name@en}}`,
			out: `{"q": [{"name@en":"Lorem ipsum"}]}`,
		},
		{
			in:  `{q(func:eq(name@.,"Lorem ipsum dolor sit amet")){name@en}}`,
			out: `{"q": [{"name@en":"Lorem ipsum dolor sit amet"}]}`,
		},
		{
			in:  `{q(func:eq(name,["srfrog"])){name}}`,
			out: `{"q": [{"name":"srfrog"}]}`,
		},
		{
			in:  `{q(func:eq(name,["srfrog","srf","srfrogg","sr","s"])){name}}`,
			out: `{"q": [{"name":"srfrog"}]}`,
		},
		{
			in:  `{q(func:eq(name,["Lorem ipsum","Lorem ipsum dolor sit amet, consectetur adipiscing elit",""])){name}}`,
			out: `{"q": [{"name":""},{"name":"Lorem ipsum"},{"name":"Lorem ipsum dolor sit amet, consectetur adipiscing elit"}]}`,
		},
		{
			in:  `{q(func:eq(name,["Lorem ipsum","Lorem ipsum","Lorem ipsum","Lorem ipsum","Lorem ipsum"])){name}}`,
			out: `{"q": [{"name":"Lorem ipsum"}]}`,
		},
		{
			in:  `{q(func:eq(name@en,["Lorem ipsum","Lorem ipsum dolor sit amet, consectetur adipiscing elit",""])){name@en}}`,
			out: `{"q": [{"name@en":"Lorem ipsum"},{"name@en":"Lorem ipsum dolor sit amet, consectetur adipiscing elit"}]}`,
		},
		{
			in:  `{q(func:eq(name@en,["Lorem ipsum","Lorem ipsum","Lorem ipsum","Lorem ipsum","Lorem ipsum"])){name@en}}`,
			out: `{"q": [{"name@en":"Lorem ipsum"}]}`,
		},
		{
			in:  `{q(func:eq(name@.,"")){name@fr}}`,
			out: `{"q": [{"name@fr":""}]}`,
		},
	}

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	for _, tc := range tests {
		resp, err := gcli.NewTxn().Query(ctx, tc.in)
		require.NoError(t, err)
		dgraphapi.CompareJSON(tc.out, string(resp.Json))
	}
}

func (ssuite *SystestTestSuite) TestRegexpToggleTrigramIndex() {
	t := ssuite.T()

	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	dbSchema := `name: string @index(term) @lang .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
      _:x1 <name> "The luck is in the details" .
      _:x1 <name> "The art is in the details"@en .
      _:x1 <name> "L'art est dans les détails"@fr .

      _:x2 <name> "Detach yourself from ostentation" .
      _:x2 <name> "Detach yourself from artificiality"@en .
      _:x2 <name> "Desprenderse de la artificialidad"@es .
    `),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	tests := []struct {
		in, out string
	}{
		{
			in:  `{q(func:has(name)) @filter(regexp(name, /art/)) {name}}`,
			out: `{"q":[]}`,
		},
		{
			in:  `{q(func:has(name)) @filter(regexp(name@es, /art/)) {name@es}}`,
			out: `{"q":[{"name@es": "Desprenderse de la artificialidad"}]}`,
		},
		{
			in:  `{q(func:has(name)) @filter(regexp(name@fr, /art/)) {name@fr}}`,
			out: `{"q":[{"name@fr": "L'art est dans les détails"}]}`,
		},
	}

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	t.Log("testing without trigram index")
	ctx = context.Background()
	for _, tc := range tests {
		resp, err := gcli.NewTxn().Query(ctx, tc.in)
		require.NoError(t, err)
		dgraphapi.CompareJSON(tc.out, string(resp.Json))
	}

	dbSchema = `name: string @index(trigram) @lang .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	t.Log("testing with trigram index")
	for _, tc := range tests {
		resp, err := gcli.NewTxn().Query(ctx, tc.in)
		require.NoError(t, err)
		dgraphapi.CompareJSON(tc.out, string(resp.Json))
	}

	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, `
      name: string @index(term) @lang .`))

	t.Log("testing without trigram index at root")
	_, err = gcli.NewTxn().Query(ctx, `{q(func:regexp(name, /art/)) {name}}`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Attribute name does not have trigram index for regex matching.")
}

func (ssuite *SystestTestSuite) TestEqWithAlteredIndexOrder() {
	t := ssuite.T()

	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	// first, let's set the schema with term before trigram
	ctx := context.Background()
	dbSchema := `name: string @index(term, trigram) .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	// fill up some data
	txn := gcli.NewTxn()
	_, err = txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
      _:x1 <name> "Alice" .
      _:x2 <name> "Bob" .
    `),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	// querying with eq should work
	q := `{q(func: eq(name, "Alice")) {name}}`
	expectedResult := `{"q":[{"name":"Alice"}]}`
	resp, err := gcli.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err)
	dgraphapi.CompareJSON(expectedResult, string(resp.Json))

	// now, let's set the schema with trigram before term
	dbSchema = `name: string @index(trigram, term) .`
	require.NoError(t, gcli.SetSchema(ctx, dgo.RootNamespace, dbSchema))

	// querying with eq should still work
	resp, err = gcli.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err)
	dgraphapi.CompareJSON(expectedResult, string(resp.Json))
}

func (ssuite *SystestTestSuite) TestGroupByUidWorks() {
	t := ssuite.T()

	gcli, cleanup, err := doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx := context.Background()
	txn := gcli.NewTxn()
	assigned, err := txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(`
      _:x1 <name> "horsejr" .
      _:x2 <name> "srfrog" .
      _:x3 <name> "missbug" .
    `),
	})
	require.NoError(t, err)
	require.NoError(t, txn.Commit(ctx))

	// Upgrade
	ssuite.Upgrade()

	tests := []struct {
		in, out string
	}{
		{
			in:  fmt.Sprintf(`{q(func:uid(%s)) @groupby(uid) {count(uid)}}`, assigned.Uids["x1"]),
			out: `{}`,
		},
		{
			in: fmt.Sprintf(`{q(func:uid(%s)) @groupby(name) {count(uid)}}`, assigned.Uids["x1"]),
			out: `{"q":[{
				"@groupby":[{
					"count": 1,
					"name": "horsejr"
				}]}]}`,
		},
		{
			in:  `{q(func:has(dummy)) @groupby(uid) {}}`,
			out: `{}`,
		},
		{
			in:  `{q(func:has(name)) @groupby(dummy) {}}`,
			out: `{}`,
		},
	}
	gcli, cleanup, err = doGrpcLogin(ssuite)
	defer cleanup()
	require.NoError(t, err)
	ctx = context.Background()
	for _, tc := range tests {
		resp, err := gcli.NewTxn().Query(ctx, tc.in)
		require.NoError(t, err)
		dgraphapi.CompareJSON(tc.out, string(resp.Json))
	}
}

func doGrpcLogin(ssuite *SystestTestSuite) (*dgraphapi.GrpcClient, func(), error) {
	gcli, cleanup, err := ssuite.dc.Client()
	if err != nil {
		return nil, nil, errors.Wrap(err, "error creating grpc client")
	}
	err = gcli.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword)
	if err != nil {
		return nil, nil, errors.Wrap(err, "groot login into galaxy namespace failed")
	}
	return gcli, cleanup, nil
}
