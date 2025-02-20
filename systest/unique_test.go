//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

//nolint:lll
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/dgraph/cmd/live"
	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/dgraphtest"
)

const emailQuery = `{
		q(func: eq(email, "example@email.com")) {
			email
		}
    }`

const emailQueryWithUid = `{
     	q(func: eq(email, "example@email.com")) {
	        email
	        uid
	    }
	}`

func setUpDgraph(t *testing.T) *dgraphapi.GrpcClient {
	c := dgraphtest.ComposeCluster{}
	dg, close, err := c.Client()
	require.NoError(t, err)
	defer close()
	require.NoError(t, err)
	require.NoError(t, dg.LoginUser(context.Background(), "groot", "password"))
	require.NoError(t, dg.DropAll(context.Background()))
	return dg
}

func TestSchema(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetSchema(context.Background(), dgo.RootNamespace, `email: string @unique  @index(exact)  .`))
	resp, err := dg.Query(`schema{ }`)
	require.NoError(t, err)
	var sch live.Schema
	require.NoError(t, json.Unmarshal(resp.GetJson(), &sch))
	for _, pred := range sch.Predicates {
		if pred.Predicate == "email" {
			require.Equal(t, true, pred.Unique)
		}
	}

	require.NoError(t, dg.DropAll(context.Background()))
	err = dg.SetupSchema(`email: string @unique  .`)
	require.Error(t, err)
	require.ErrorContains(t, err, "index for predicate [email] is missing,"+
		" add either hash or exact index with @unique")
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))

	require.NoError(t, dg.DropAll(context.Background()))
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(hash)  .`))

	require.NoError(t, dg.DropAll(context.Background()))
	err = dg.SetupSchema(`mobile: int @unique   .`)
	require.Error(t, err)
	require.ErrorContains(t, err, "index for predicate [mobile] is missing, add int index with @unique")
	require.NoError(t, dg.SetupSchema(`mobile: int @unique  @index(int)  .`))

	require.NoError(t, dg.DropAll(context.Background()))
	err = dg.SetupSchema(`email: string @unique  @index(trigram)  .`)
	require.Error(t, err)
	require.ErrorContains(t, err, "index for predicate [email] is missing,"+
		" add either hash or exact index with @unique")
	require.NoError(t, dg.DropAll(context.Background()))
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(trigram,exact)  .`))

	// drop @upsert
	require.NoError(t, dg.DropAll(context.Background()))
	err = dg.SetupSchema(`email: string @unique  @index(exact) .`)
	require.NoError(t, err)
	// try to drop @upsert
	err = dg.SetupSchema(`email: string @unique  @index(exact) .`)

	require.ErrorContains(t, err, "could not drop @upsert from [email]"+
		" predicate when @unique directive specified")
	require.NoError(t, dg.SetupSchema(`email: string @unique @upsert  @index(exact)  .`))
	// drop index
	err = dg.SetupSchema(`email: string @unique @upsert .`)
	require.Error(t, err)
	require.ErrorContains(t, err, "could not drop index [exact] from [email]"+
		" predicate when @unique directive specified")
}

func TestUniqueTwoMutationSingleBlankNode(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))

	rdf := `_:a <email> "example@email.com" .	`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	resp, err := dg.Query(emailQuery)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{ "q": [{"email": "example@email.com"}]}`, string(resp.Json)))
	_, err = dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [example@email.com] for predicate [email]")
}

func TestUniqueOneMutationSameValueTwoBlankNode(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))

	rdf := `_:a <email> "example@email.com" .
	        _:b <email> "example@email.com" .	`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [example@email.com] for predicate [email]")

	resp, err := dg.Query(emailQuery)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{ "q": [ ]}`, string(resp.Json)))
}

func TestUniqueOneMutationSameValueSingleBlankNode(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))

	rdf := `_:a <email> "example@email.com" .
	        _:a <email> "example@email.com" .	`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	resp, err := dg.Query(emailQuery)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{ "q": [ {	"email": "example@email.com"}]}`,
		string(resp.Json)))
}

func TestUniqueTwoMutattionsTwoHardCodedUIDs(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))

	rdf := `<0x5> <email> "example@email.com" .	`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)

	resp, err := dg.Query(emailQueryWithUid)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{ "q": [ {"email": "example@email.com","uid" :"0x5"}]}`,
		string(resp.Json)))

	rdf = `<0x6> <email> "example@email.com" .	`
	_, err = dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [example@email.com] for predicate [email]")
}

func TestUniqueHardCodedUidsWithDiffrentNotation(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))

	rdf := `<0xad> <email> "example@email.com" .	`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)

	resp, err := dg.Query(emailQueryWithUid)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{ "q": [ {"email": "example@email.com","uid" :"0xad"}]}`,
		string(resp.Json)))

	rdf = `<0o255> <email> "example@email.com" .	`
	_, err = dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	resp, err = dg.Query(emailQueryWithUid)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{ "q": [ {"email": "example@email.com","uid" :"0xad"}]}`,
		string(resp.Json)))

	rdf = `<0b10101101> <email> "example@email.com" .	`
	_, err = dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	resp, err = dg.Query(emailQueryWithUid)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{ "q": [ {"email": "example@email.com","uid" :"0xad"}]}`,
		string(resp.Json)))

	rdf = `<173> <email> "example@email.com" .	`
	_, err = dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	resp, err = dg.Query(emailQueryWithUid)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{ "q": [ {"email": "example@email.com","uid" :"0xad"}]}`,
		string(resp.Json)))

}

func TestUniqueSingleMutattionsOneHardCodedUIDSameValue(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))

	rdf := `<0x5> <email> "example@email.com" .
	        <0x5> <email> "example@email.com" .	`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	resp, err := dg.Query(emailQueryWithUid)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{ "q": [ {	
		"email": "example@email.com",
		"uid":"0x5"
	}]}`, string(resp.Json)))
}

func TestUniqueOneMutattionsTwoHardCodedUIDsDiffValue(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))

	rdf := `<0x5> <email> "example@email.com" .
	        <0x6> <email> "example@email.com" .	`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [example@email.com] for predicate [email]")

	resp, err := dg.Query(emailQuery)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{ "q": [ ]}`, string(resp.Json)))
}

func TestUniqueUpsertMutation(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))
	rdf := `_:a <email> "example@email.com" .
	        _:b <email> "example1@email.com" .	`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)

	query := `query{
				v as person(func: eq(email, "example@email.com")) {
					email
				}
		    }`
	mu := &api.Mutation{
		SetNquads: []byte(`uid(v) <email> "example@email.com" .  `),
	}
	_, err = dg.Upsert(query, mu)
	require.NoError(t, err)
	resp, err := dg.Query(emailQuery)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{ "q": [{"email": "example@email.com"} ]}`, string(resp.Json)))

	mu = &api.Mutation{
		SetNquads: []byte(` uid(v) <email> "example1@email.com" .`),
	}
	_, err = dg.Upsert(query, mu)
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [example1@email.com] for predicate [email]")
}

func TestUniqueWithConditionalUpsertMutation(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))
	rdf := `_:a <email> "example@email.com" .
	        _:b <email> "example1@email.com" .	`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	resp, err := dg.Query(emailQuery)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{ "q": [{"email": "example@email.com"} ]}`, string(resp.Json)))

	query := `query{
		         v as person(func: eq(email, "example@email.com")) {
						email
				}
	        }`
	mu := &api.Mutation{
		SetNquads: []byte(`uid(v) <email> "example@email.com" .  `),
		Cond:      "@if(eq(len(v),1))",
	}
	_, err = dg.Upsert(query, mu)
	require.NoError(t, err)

	query = `query{
		        v as person(func: eq(email, "example1@email.com")) {
						email
					}
	       }`
	mu = &api.Mutation{
		SetNquads: []byte(`uid(v) <email> "example@email.com" .  `),
		Cond:      "@if(eq(len(v),1))",
	}
	_, err = dg.Upsert(query, mu)
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [example@email.com] for predicate [email]")
}

func TestUniqueUpsertMutationWithValOf(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))
	rdf := `_:a <email> "example@email.com" .
	        _:b <email> "example1@email.com" .	`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)

	query := `query{
			    v as person(func: eq(email, "example@email.com")) {
							email
					}
		    }`
	mu := &api.Mutation{
		SetNquads: []byte(`uid(v) <email> "example@email.com" .  `),
	}
	_, err = dg.Upsert(query, mu)
	require.NoError(t, err)

	query = `query{
		       v as person(func: eq(email, "example@email.com")) {
						email
					}
		       var(func: eq(email, "example1@email.com")) {
				        w as	email
				    }
	        }`
	mu = &api.Mutation{
		SetNquads: []byte(`uid(v) <email> val(w) . `),
	}
	_, err = dg.Upsert(query, mu)
	require.NoError(t, err)

	// test unique using aggregate var
	query = `query{
		       v as person(func: eq(email, "example@email.com")) {
						email
					}
		       var(func: eq(email, "example1@email.com")) {
				        emails as	email
				    }
		       me() {
		    	        d as  min(val(emails))
			        }
	        }`
	mu = &api.Mutation{
		SetNquads: []byte(` uid(v) <email> val(d) . `),
	}

	_, err = dg.Upsert(query, mu)
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [example1@email.com] for predicate [email]")

	query = `query{
	           var(func: eq(email, "example1@email.com")) {
			             emails as	email
			        }
	           me() {
		                 d as  min(val(emails))
		            }
            }`
	mu = &api.Mutation{
		SetNquads: []byte(`<0x100> <email> val(d) . `),
	}
	_, err = dg.Upsert(query, mu)
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [example1@email.com] for predicate [email]")

	query = `query{
	           var(func: eq(email, "example1@email.com")) {
			             emails as	email
			        }
	           me() {
		                d as  min(val(emails))
		            }
            }`
	mu = &api.Mutation{
		SetNquads: []byte(`_:a <email> val(d) . `),
	}
	_, err = dg.Upsert(query, mu)
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [example1@email.com] for predicate [email]")
}

func TestUniqueUpsertSingleMutationTwoBlankNode(t *testing.T) {
	schema := `email: string @unique  @index(exact)  .
	           fullName :string @index(exact) .`
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(schema))
	rdf := `_:a <email> "person1@email.com" .
	        _:a <fullName> "person2@email.com" .
	        _:b <email> "person2@email.com" .
	        _:b <fullName> "person1@email.com"	.`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)

	query := `query{
			    v as person(func: eq(fullName, "person1@email.com")) {
					     w as	fullName
				    }
		    }`
	mu := &api.Mutation{
		SetNquads: []byte(`uid(v) <email> val(w) .  `),
	}
	_, err = dg.Upsert(query, mu)
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [person1@email.com] for predicate [email]")

	query = `query{
			   v as person(func: eq(fullName, "person1@email.com")) {
						w as	email
					}
		    }`
	mu = &api.Mutation{
		SetNquads: []byte(`uid(v) <email> val(w) .  `),
	}
	_, err = dg.Upsert(query, mu)
	require.NoError(t, err)
}

func TestUniqueForInt(t *testing.T) {
	schema := `mobile: int @unique  @index(int)  .`
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(schema))
	rdf := `_:a <mobile> "1234567890" .	`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	_, err = dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [1234567890] for predicate [mobile]")
	query := `{
		q(func: eq(mobile, 1234567890)) {
			mobile
		}
    }`
	resp, err := dg.Query(query)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{ "q": [ {	"mobile": 1234567890}]}`, string(resp.Json)))
}

func TestUniqueForLangDirective(t *testing.T) {
	schema := `name: string @unique @lang @index(exact)  .
	           email: string .`
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(schema))
	rdf := `_:a <name@hi> "अमित" .	`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	_, err = dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [अमित] for predicate [name]")
	_, err = dg.Mutate(&api.Mutation{
		SetNquads: []byte(`_:a <name@en> "अमित" .`),
		CommitNow: true,
	})
	require.NoError(t, err)
}

func TestUniqueTwoWaySwapMutation(t *testing.T) {
	schema := `email: string @unique  @index(exact)  .`
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(schema))
	rdf := `<0x22> <email> "aman@dgraph.io" .
	        <0x33> <email> "shiva@dgraph.io" .	`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	rdf = `<0x33> <email> "aman@dgraph.io" .
	       <0x22> <email> "shiva@dgraph.io" .`
	_, err = dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	query := `query{
		var(func: eq(email, "aman@dgraph.io")) {
				 emails as	email
			 }
		me() {
				 d as  min(val(emails))
			 }
	 }`
	mu := &api.Mutation{
		SetNquads: []byte(` <0x10> <email> val(d) . 
		                    <0x33> <email> "harshil@dgraph.io" .`),
	}

	_, err = dg.Upsert(query, mu)
	require.NoError(t, err)
}

func TestUnqueFourWaySwapMutation(t *testing.T) {
	schema := `email: string @unique  @index(exact)  .`
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(schema))
	rdf := `<0x22> <email> "aman@dgraph.io" .
	        <0x33> <email> "shiva@dgraph.io" .
	        <0x44> <email> "jassi@dgraph.io" .
	        <0x55> <email> "siddesh@dgraph.io" .`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	rdf = `<0x33> <email> "aman@dgraph.io" .
	       <0x22> <email> "shiva@dgraph.io" .
	       <0x55> <email> "jassi@dgraph.io" .
	       <0x44> <email> "siddesh@dgraph.io" .`
	_, err = dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	rdf = `<0x44> <email> "aman@dgraph.io" .
	       <0x55> <email> "shiva@dgraph.io" .
	       <0x22> <email> "jassi@dgraph.io" .
	       <0x33> <email> "siddesh@dgraph.io" .`
	_, err = dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	rdf = `<0x55> <email> "aman@dgraph.io" .
	       <0x44> <email> "shiva@dgraph.io" .
	       <0x33> <email> "jassi@dgraph.io" .
	       <0x22> <email> "siddesh@dgraph.io" .`
	_, err = dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
}

func TestUniqueDeleteMutation(t *testing.T) {
	schema := `email: string  @unique @index(exact)  .`
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(schema))
	rdf := `<0x22> <email> "example1@gmail.com" .
	        <0x33> <email> "example2@gmail.com"  .`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	query := `query{
			    u as var(func: eq(email, "example1@gmail.com"))
            }`
	mu := &api.Mutation{
		SetNquads: []byte(`<0x100> <email> "example1@gmail.com" . `),
		DelNquads: []byte(`uid(u) <email> "example1@gmail.com"  .`),
	}
	_, err = dg.Upsert(query, mu)
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [example1@gmail.com]"+
		" for predicate [email]")
}

func TestConcurrencyMutationsDiffrentValuesForDiffrentBlankNode(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))
	concurrency := 1000
	wg := &sync.WaitGroup{}
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = dg.Mutate(&api.Mutation{
				SetNquads: []byte(fmt.Sprintf(`_:a <email> "example%v@email.com" .`, i)),
				CommitNow: true,
			})
		}(i)
	}
	wg.Wait()
	query := `{
		       allMails(func: has(email)) {
		              count(uid)
		            }
			}`
	resp, err := dg.Query(query)
	require.NoError(t, err)
	// there should be 1000 emails in DB.
	require.NoError(t, dgraphapi.CompareJSON(`{"allMails":[{"count":1000}]}`, string(resp.Json)))
}

func TestUniqueTwoTxnWithoutCommit(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))
	ctx := context.Background()
	rdf := `_:a <email> "example@email.com" .	`
	txn1 := dg.NewTxn()
	_, err := txn1.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(rdf),
	})
	require.NoError(t, err)

	txn2 := dg.NewTxn()
	_, err = txn2.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(rdf),
	})
	require.NoError(t, err)

	require.NoError(t, txn1.Commit(ctx))
	require.Error(t, txn2.Commit(ctx))
	resp, err := dg.Query(emailQuery)
	require.NoError(t, err)
	// there should be only one email data as expected.
	require.NoError(t, dgraphapi.CompareJSON(`{"q":[{"email":"example@email.com"}]}`, string(resp.Json)))
}

func TestUniqueSingelTxnDuplicteValuesWithoutCommit(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))
	ctx := context.Background()
	rdf := `_:a <email> "example@email.com" .	`
	txn1 := dg.NewTxn()
	_, err := txn1.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(rdf),
	})
	require.NoError(t, err)
	_, err = txn1.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(rdf),
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [example@email.com] for predicate [email]")

	err = txn1.Commit(ctx)
	require.Error(t, err)
	require.ErrorContains(t, err, "Transaction has already been committed or discarded")
}

func TestConcurrency2(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique @upsert  @index(exact)  .`))
	concurrency := 100
	rand.Seed(time.Now().UnixNano())
	errChan := make(chan error)
	wg := &sync.WaitGroup{}
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 1; j < concurrency; j++ {
				rN := rand.Intn(concurrency)
				_, err := dg.Mutate(&api.Mutation{
					SetNquads: []byte(fmt.Sprintf(`_:%v <email> "example%v@email.com" .`, rN, rN)),
					CommitNow: true,
				})
				if err != nil {
					errChan <- err
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(errChan)
	}()

	for err := range errChan {
		if !(strings.Contains(err.Error(), "Transaction has been aborted. Please retry")) &&
			!(strings.Contains(err.Error(), "could not insert duplicate value")) {
			t.Fatal(err.Error())
		}

	}

	for i := 0; i < concurrency; i++ {
		emailQuery := fmt.Sprintf(`{
			q(func: eq(email, "example%v@email.com")) {
				email
			}
		}`, i)
		resp, err := dg.Query(emailQuery)
		require.NoError(t, err)
		err1 := dgraphapi.CompareJSON(fmt.Sprintf(`{"q":[{"email":"example%v@email.com"}]}`, i), string(resp.Json))
		err2 := dgraphapi.CompareJSON(fmt.Sprintf(`{ "q": [ ]}`), string(resp.Json))
		if err1 != nil && err2 != nil {
			t.Fatal()
		}
	}
}

func TestUniqueUpsertWithoutSpaceInQuery(t *testing.T) {
	schema := `email: string @unique  @index(exact)  .`
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(schema))
	rdf := `_:a <email> "test@test.io"  .`
	query := `{UNIQS as var(func: eq(userEmail, "test@test.io")) { count: count(uid) }}`

	r := &api.Request{
		Query: query,
		Mutations: []*api.Mutation{
			{
				Cond:      "@if(eq(len(UNIQS), 0))",
				SetNquads: []byte(rdf),
			},
		},
		CommitNow: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	txn := dg.NewTxn()
	_, err := txn.Do(ctx, r)
	require.NoError(t, err)

	txn = dg.NewTxn()
	_, err = txn.Do(ctx, r)

	require.Error(t, err)
}

func TestUniqueWithNonLatinPredName(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`<ईमेल#$%&>: string @unique @lang @index(exact)  .`))

	rdf := `_:a <ईमेल#$%&@hi> "example@email.com" .	`
	_, err := dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.NoError(t, err)
	query := `{
		q(func: eq(<ईमेल#$%&>@hi, "example@email.com")) {
			<ईमेल#$%&>@hi
		}
    }`
	resp, err := dg.Query(query)
	require.NoError(t, err)
	var data map[string]interface{}
	require.NoError(t, json.Unmarshal(resp.Json, &data))
	require.Equal(t, data["q"].([]interface{})[0].(map[string]interface{})["ईमेल#$%&@hi"].(string), "example@email.com")

	_, err = dg.Mutate(&api.Mutation{
		SetNquads: []byte(rdf),
		CommitNow: true,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [example@email.com] for predicate [ईमेल#$%&]")
}

func TestUniqueMultipleMutationsInSingleReqWithDelNqd(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))

	mu1 := &api.Mutation{
		SetNquads: []byte(`<0x1> <email> "unique@email.com" .`),
		CommitNow: true,
	}
	_, err := dg.Mutate(mu1)
	require.NoError(t, err)

	mu1 = &api.Mutation{
		SetNquads: []byte(`_:a <email> "example@email.com" .`),
		CommitNow: true,
	}
	mu2 := &api.Mutation{
		DelNquads: []byte(`<0x1> * * .`),
		CommitNow: true,
	}
	mu3 := &api.Mutation{
		SetNquads: []byte(`_:b <email> "example@email.com" .`),
		CommitNow: true,
	}
	req := &api.Request{
		Mutations: []*api.Mutation{mu1, mu2, mu3},
		CommitNow: true,
	}
	_, err = dg.NewTxn().Do(context.Background(), req)
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [example@email.com] for predicate [email]")
}

func TestUniqueMultipleMutationsInSingleReq(t *testing.T) {
	dg := setUpDgraph(t)
	require.NoError(t, dg.SetupSchema(`email: string @unique  @index(exact)  .`))

	mu1 := &api.Mutation{
		SetNquads: []byte(`_:a <email> "example@email.com" .`),
		CommitNow: true,
	}
	mu2 := &api.Mutation{
		SetNquads: []byte(`_:b <email> "example@email.com" .`),
		CommitNow: true,
	}
	req := &api.Request{
		Mutations: []*api.Mutation{mu1, mu2},
		CommitNow: true,
	}
	_, err := dg.NewTxn().Do(context.Background(), req)
	require.Error(t, err)
	require.ErrorContains(t, err, "could not insert duplicate value [example@email.com] for predicate [email]")
}
