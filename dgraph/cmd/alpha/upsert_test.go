//go:build integration

/*
 * Copyright 2025 Hypermode Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package alpha

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/testutil"
	"github.com/hypermodeinc/dgraph/v24/x"
)

type QueryResult struct {
	Queries map[string][]struct {
		UID string
	}
}

func splitPreds(ps []string) []string {
	for i, p := range ps {
		ps[i] = x.ParseAttr(strings.SplitN(p, "-", 2)[1])
	}

	return ps
}

func TestUpsertExample0(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`email: string @index(exact) .`))

	// Mutation with wrong name
	m1 := `
upsert {
  query {
    q(func: eq(email, "email@company.io")) {
      v as uid
    }
  }

  mutation {
    set {
      uid(v) <name> "Wrong" .
      uid(v) <email> "email@company.io" .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"email", "name"}, splitPreds(mr.preds))
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 0, len(result.Queries["q"]))

	// query should return the wrong name
	q1 := `
{
  q(func: has(email)) {
    uid
    name
    email
  }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Contains(t, res, "Wrong")

	// mutation with correct name
	m2 := `
upsert {
  query {
    q(func: eq(email, "email@company.io")) {
      v as uid
    }
  }

  mutation {
    set {
      uid(v) <name> "Ashish" .
    }
  }
}`
	mr, err = mutationWithTs(mutationInp{body: m2, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"name"}, splitPreds(mr.preds))
	result = QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 1, len(result.Queries["q"]))

	// query should return correct name
	res, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Contains(t, res, "Ashish")
}

func TestUpsertNoCloseBracketRDF(t *testing.T) {
	SetupBankExample(t)

	m1 := `
upsert {
  query {
    u as var(func: has(amount)) {
      amt as amount
    }
    me() {
      max_amt as max(val(amt))
    }
  }

  mutation {
    set {
      uid(u) <amount> val(max_amt .
    }
  }
}`
	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.Contains(t, err.Error(), "Expected ')' while reading function found: '.'")
}

func TestUpsertNoCloseBracketJSON(t *testing.T) {
	SetupBankExample(t)

	m1 := `
{
  "query": "{ u as var(func: has(amount)) { amt as amount} me () {  updated_amt as math(amt+1)}}",
  "set": [
    {
      "uid": "uid(u)",
      "amount": "val(updated_amt"
    }
  ]
}
`
	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/json", commitNow: true})
	require.Contains(t, err.Error(), "brackets are not closed properly")
}

func TestUpsertExampleJSON(t *testing.T) {
	SetupBankExample(t)

	m1 := `
{
  "query": "{ q(func: has(amount)) { u as uid \n amt as amount \n updated_amt as math(amt+1)}}",
  "set": [
    {
      "uid": "uid(u)",
      "amount": "val(updated_amt)"
    }
  ]
}
`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/json", commitNow: true})
	require.NoError(t, err)
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 3, len(result.Queries["q"]))

	q1 := `
{
  q(func: has(name)) {
    name
    amount
  }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	expectedRes := `
{
  "data": {
    "q": [{
       "name": "user3",
       "amount": 1001.000000
     }, {
       "name": "user1",
       "amount": 11.000000
     }, {
       "name": "user2",
       "amount": 101.000000
     }]
   }
}`
	require.NoError(t, err)
	testutil.CompareJSON(t, res, expectedRes)
}

func TestUpsertExample0JSON(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`email: string .`))
	require.NoError(t, alterSchema(`email: string @index(exact) .`))

	// Mutation with wrong name
	m1 := `
{
  "query": "{me(func: eq(email, \"email@company.io\")) {v as uid}}",
  "set": [
    {
      "uid": "uid(v)",
      "name": "Wrong"
    },
    {
      "uid": "uid(v)",
      "email": "email@company.io"
    }
  ]
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/json", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)

	// query should return the wrong name
	q1 := `
{
  q(func: has(email)) {
    uid
    name
    email
  }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Contains(t, res, "Wrong")

	// mutation with correct name
	m2 := `
{
  "query": "{me(func: eq(email, \"email@company.io\")) {v as uid}}",
  "set": [
    {
      "uid": "uid(v)",
      "name": "Ashish"
    }
  ]
}`
	mr, err = mutationWithTs(mutationInp{body: m2, typ: "application/json", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"name"}, splitPreds(mr.preds))

	// query should return correct name
	res, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Contains(t, res, "Ashish")
}

func TestUpsertNoVarErr(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
age: int @index(int) .
friend: uid @reverse .`))

	m1 := `
upsert {
  query {
    me(func: eq(age, 34)) {
      ...fragmentA
      friend {
        ...fragmentA
        age
      }
    }
  }

  fragment fragmentA {
    uid
  }

  mutation {
    set {
      _:user1 <age> "45" .
    }
  }
}`
	resp, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.Equal(t, []string{"age"}, splitPreds(resp.preds))
}

func TestUpsertWithFragment(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
age: int @index(int) .
friend: uid @reverse .`))

	m1 := `
upsert {
  query {
    me(func: eq(age, 34)) {
      friend {
        ...fragmentA
      }
    }
  }

  fragment fragmentA {
    variable as uid
  }

  mutation {
    set {
      uid(variable) <age> "45" .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.Equal(t, 0, len(mr.keys))
	require.Equal(t, []string{"age"}, splitPreds(mr.preds))

	// Ensure that another run works too
	mr, err = mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.Equal(t, 0, len(mr.keys))
	require.Equal(t, []string{"age"}, splitPreds(mr.preds))
}

func TestUpsertInvalidErr(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
age: int @index(int) .
name: string @index(exact) .
friend: uid @reverse .`))

	m1 := `
{
  set {
    uid(variable) <age> "45" .
  }
}`
	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.Contains(t, err.Error(), "variables [variable] not defined")
}

func TestUpsertUndefinedVarErr(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
age: int @index(int) .
name: string @index(exact) .
friend: uid @reverse .`))

	m1 := `
upsert {
  query {
    me(func: eq(age, 34)) {
      friend {
        ...fragmentA
      }
    }
  }

  fragment fragmentA {
    variable as uid
  }

  mutation {
    set {
      uid(42) <age> "45" .
      uid(variable) <age> "45" .
    }
  }
}`
	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.Contains(t, err.Error(), "Some variables are used but not defined")
	require.Contains(t, err.Error(), "Defined:[variable]")
	require.Contains(t, err.Error(), "Used:[42 variable]")
}

func TestUpsertUnusedVarErr(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
age: int @index(int) .
name: string @index(exact) .
friend: uid @reverse .`))

	m1 := `
upsert {
  query {
    me(func: eq(age, 34)) {
      var2 as uid
      friend {
        ...fragmentA
      }
    }
  }

  fragment fragmentA {
    var1 as uid
    name
  }

  mutation {
    set {
      uid(var2) <age> "45" .
    }
  }
}`
	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.Contains(t, err.Error(), "Some variables are defined but not used")
	require.Contains(t, err.Error(), "Defined:[var1 var2]")
	require.Contains(t, err.Error(), "Used:[var2]")
}

func TestUpsertExampleNode(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
age: int @index(int) .
name: string @index(exact) @lang .
friend: uid @reverse .`))

	m0 := `
{
  set {
    _:user1 <age> "23" .
    _:user1 <name@en> "user1" .
    _:user2 <age> "34" .
    _:user2 <name@en> "user2" .
    _:user3 <age> "56" .
    _:user3 <name@en> "user3" .
  }
}`
	_, err := mutationWithTs(mutationInp{body: m0, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	m1 := `
upsert {
  query {
    var(func: has(age)) {
      a as age
    }

    q(func: uid(a), orderdesc: val(a), first: 1) {
      u as uid
      name
      age
    }
  }

  mutation {
    set {
      uid(	u) <oldest> "true" .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 1, len(result.Queries["q"]))

	q1 := `
{
  q(func: has(oldest)) {
    name@en
    age
    oldest
  }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Contains(t, res, "user3")
	require.Contains(t, res, "56")
	require.Contains(t, res, "true")

	m2 := `
upsert {
  query {
    user1(func: eq(name@en, "user1")) {
      u1 as uid
    }
  }

  mutation {
    delete {
      uid (u1) <name> * .
    }
  }
}`
	mr, err = mutationWithTs(mutationInp{body: m2, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result = QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 1, len(result.Queries["user1"]))

	q2 := `
{
  q(func: eq(name@en, "user1")) {
    name@en
    age
  }
}`
	res, _, err = queryWithTs(queryInp{body: q2, typ: "application/dql"})
	require.NoError(t, err)
	require.NotContains(t, res, "user1")
}

func TestUpsertExampleEdge(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
age: int @index(int) .
name: string @index(exact) @lang .
friend: uid @reverse .`))

	m0 := `
{
  set {
    _:user1 <age> "23" .
    _:user1 <name@en> "user1" .
    _:user2 <age> "34" .
    _:user2 <name@en> "user2" .
    _:user3 <age> "56" .
    _:user3 <name@en> "user3" .
  }
}`
	_, err := mutationWithTs(mutationInp{body: m0, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	m1 := `
upsert {
  query {
    user1(func: eq(name@en, "user1")) {
      u1 as uid
    }

    user2(func: eq(name@en, "user2")) {
      u2 as uid
    }
  }

  mutation {
    set {
      uid ( u1 ) <friend> uid ( u2 ) .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 1, len(result.Queries["user1"]))
	require.Equal(t, 1, len(result.Queries["user2"]))

	q1 := `
{
  q(func: eq(name@en, "user1")) {
    friend {
      name@en
    }
  }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Contains(t, res, "user2")

	m2 := `
upsert {
  query {
    user1(func: eq(name@en, "user1")) {
      u1 as uid
    }

    user2(func: eq(name@en, "user2")) {
      u2 as uid
    }
  }

  mutation {
    delete {
      uid (u1) <friend> uid ( u2 ) .
    }
  }
}`
	mr, err = mutationWithTs(mutationInp{body: m2, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result = QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 1, len(result.Queries["user1"]))
	require.Equal(t, 1, len(result.Queries["user2"]))

	q2 := `
{
  q(func: eq(name@en, "user1")) {
    friend {
      name@en
    }
  }
}`
	res, _, err = queryWithTs(queryInp{body: q2, typ: "application/dql"})
	require.NoError(t, err)
	require.NotContains(t, res, "user2")
}

func TestUpsertExampleNodeJSON(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
age: int @index(int) .
name: string @index(exact) @lang .
friend: uid @reverse .`))

	m0 := `
{
  set {
    _:user1 <age> "23" .
    _:user1 <name@en> "user1" .
    _:user2 <age> "34" .
    _:user2 <name@en> "user2" .
    _:user3 <age> "56" .
    _:user3 <name@en> "user3" .
  }
}`
	_, err := mutationWithTs(mutationInp{body: m0, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	m1 := `
{
  "query": "{var(func: has(age)) {a as age} oldest(func: uid(a), orderdesc: val(a), first: 1) {u as uid}}",
  "set": [
    {
      "uid": "uid(u)",
      "oldest": "true"
    }
  ]
}`
	_, err = mutationWithTs(mutationInp{body: m1, typ: "application/json", commitNow: true})
	require.NoError(t, err)

	q1 := `
{
  q(func: has(oldest)) {
    name@en
    age
    oldest
  }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Contains(t, res, "user3")
	require.Contains(t, res, "56")
	require.Contains(t, res, "true")

	m2 := `
{
  "query": "{user1(func: eq(name@en, \"user1\")) {u1 as uid}}",
  "delete": [
    {
      "uid": "uid (u1)",
      "name": null
    }
  ]
}`
	_, err = mutationWithTs(mutationInp{body: m2, typ: "application/json", commitNow: true})
	require.NoError(t, err)

	q2 := `
{
  q(func: eq(name@en, "user1")) {
    name@en
    age
  }
}`
	res, _, err = queryWithTs(queryInp{body: q2, typ: "application/dql"})
	require.NoError(t, err)
	require.NotContains(t, res, "user1")
}

func TestUpsertExampleEdgeJSON(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
age: int @index(int) .
name: string @index(exact) @lang .
friend: uid @reverse .`))

	m0 := `
{
  set {
    _:user1 <age> "23" .
    _:user1 <name@en> "user1" .
    _:user2 <age> "34" .
    _:user2 <name@en> "user2" .
    _:user3 <age> "56" .
    _:user3 <name@en> "user3" .
  }
}`
	_, err := mutationWithTs(mutationInp{body: m0, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	m1 := `
{
  "query": "{user1(func: eq(name@en, \"user1\")) {u1 as uid} user2(func: eq(name@en, \"user2\")) {u2 as uid}}",
  "set": [
      {
        "uid": "uid(u1)",
        "friend": "uid  (u2 ) "
    }
  ]
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/json", commitNow: true})
	require.NoError(t, err)
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 1, len(result.Queries["user1"]))
	require.Equal(t, 1, len(result.Queries["user2"]))

	q1 := `
{
  q(func: eq(name@en, "user1")) {
    friend {
      name@en
    }
  }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Contains(t, res, "user2")

	m3 := `
{
  "query": "{user1(func: eq(name@en, \"user1\")) {u1 as uid} user2(func: eq(name@en, \"user2\")) {u2 as uid}}",
  "delete": [
    {
      "uid": "uid (u1)",
      "friend": "uid ( u2 )"
    }
  ]
}`
	_, err = mutationWithTs(mutationInp{body: m3, typ: "application/json", commitNow: true})
	require.NoError(t, err)

	q3 := `
{
  q(func: eq(name@en, "user1")) {
    friend {
      name@en
    }
  }
}`
	res, _, err = queryWithTs(queryInp{body: q3, typ: "application/dql"})
	require.NoError(t, err)
	require.NotContains(t, res, "user2")
}

func TestUpsertBlankNodeWithVar(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`name: string @index(exact) @lang .`))

	m := `
upsert {
  query {
    q(func: eq(name, "user1")) {
      u as uid
    }
  }

  mutation {
    set {
      uid(u) <name> "user1" .
      _:u <name> "user2" .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 0, len(result.Queries["q"]))

	q := `
{
  users(func: has(name)) {
    uid
    name
  }
}`
	res, _, err := queryWithTs(queryInp{body: q, typ: "application/dql"})
	require.NoError(t, err)
	require.Contains(t, res, "user1")
	require.Contains(t, res, "user2")
}

func TestUpsertParallel(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
email: string @index(exact) @upsert .
name: string @index(exact) @lang .
friend: uid @reverse .`))

	m := `
upsert {
  query {
    user1(func: eq(email, "user1@dgraph.io")) {
      u1 as uid
    }

    user2(func: eq(email, "user2@dgraph.io")) {
      u2 as uid
    }

    user3(func: eq(email, "user3@dgraph.io")) {
      u3 as uid
    }
  }

  mutation {
    set {
      uid(u1) <email> "user1@dgraph.io" .
      uid(u1) <name> "user1" .
      uid(u2) <email> "user2@dgraph.io" .
      uid(u2) <name> "user2" .
      uid(u1) <friend> uid(u2) .
    }

    delete {
      uid(u3) <friend> uid(u1) .
      uid(u1) <friend> uid(u3) .
      uid(u3) <name> * .
    }
  }
}`
	doUpsert := func(wg *sync.WaitGroup) {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			err := dgo.ErrAborted
			for err != nil && strings.Contains(err.Error(), "Transaction has been aborted. Please retry") {
				_, err = mutationWithTs(mutationInp{body: m, typ: "application/rdf", commitNow: true})
			}

			require.NoError(t, err)
		}
	}

	// 10 routines each doing parallel upsert 10 times
	var wg sync.WaitGroup
	wg.Add(10)
	for i := 0; i < 10; i++ {
		go doUpsert(&wg)
	}
	wg.Wait()

	q := `
{
  user1(func: eq(email, "user1@dgraph.io")) {
    name
    email
    friend {
      name
      email
    }
  }
}`
	res, _, err := queryWithTs(queryInp{body: q, typ: "application/dql"})
	require.NoError(t, err)
	expected := `
{
  "data": {
    "user1": [
      {
        "name": "user1",
        "email": "user1@dgraph.io",
        "friend": {
          "name": "user2",
          "email": "user2@dgraph.io"
        }
      }
    ]
  }
}`
	testutil.CompareJSON(t, expected, res)
}

func TestUpsertDeleteNonExistent(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
email: string @index(exact) @upsert .
name: string @index(exact) @lang .
friend: uid @reverse .`))

	m := `
upsert {
  query {
    user1(func: eq(name@en, "user1")) {
      u1 as uid
    }

    user2(func: eq(name@en, "user2")) {
      u2 as uid
    }
  }

  mutation {
    delete {
      uid (u1) <friend> uid ( u2 ) .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 0, len(result.Queries["q"]))
}

func TestConditionalUpsertExample0(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`email: string @index(exact) .`))

	// Mutation with wrong name
	m1 := `
upsert {
  query {
    q(func: eq(email, "email@company.io")) {
      v as uid
    }
  }

  mutation @if(eq(len(v), 0)) {
    set {
      uid(v) <name> "Wrong" .
      uid(v) <email> "email@company.io" .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"email", "name"}, splitPreds(mr.preds))
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 0, len(result.Queries["q"]))

	// Trying again, should be a NOOP
	mr, err = mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result = QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))

	// query should return the wrong name
	q1 := `
{
  q(func: has(email)) {
    uid
    name
    email
  }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Contains(t, res, "Wrong")

	// mutation with correct name
	m2 := `
upsert {
  query {
    q(func: eq(email, "email@company.io")) {
      v as uid
    }
  }

  mutation @if(eq(len(v), 1)) {
    set {
      uid(v) <name> "Ashish" .
    }
  }
}`
	mr, err = mutationWithTs(mutationInp{body: m2, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"name"}, splitPreds(mr.preds))
	result = QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 1, len(result.Queries["q"]))

	// query should return correct name
	res, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Contains(t, res, "Ashish")
}

func TestConditionalUpsertExample0JSON(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`email: string @index(exact) .`))

	// Mutation with wrong name
	m1 := `
{
  "query": "{q(func: eq(email, \"email@company.io\")) {v as uid}}",
  "cond": " @if(eq(len(v), 0)) ",
  "set": [
    {
      "uid": "uid(v)",
      "name": "Wrong"
    },
    {
      "uid": "uid(v)",
      "email": "email@company.io"
    }
  ]
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/json", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 0, len(result.Queries["q"]))

	// query should return the wrong name
	q1 := `
{
  q(func: has(email)) {
    uid
    name
    email
  }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Contains(t, res, "Wrong")

	// mutation with correct name
	m2 := `
{
  "query": "{q(func: eq(email, \"email@company.io\")) {v as uid}}",
  "cond": "@if(eq(len(v), 1))",
  "set": [
    {
      "uid": "uid(v)",
      "name": "Ashish"
    }
  ]
}`
	mr, err = mutationWithTs(mutationInp{body: m2, typ: "application/json", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"name"}, splitPreds(mr.preds))
	result = QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 1, len(result.Queries["q"]))
	// query should return correct name
	res, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.Contains(t, res, "Ashish")
}

func populateCompanyData(t *testing.T) {
	require.NoError(t, alterSchema(`
email: string @index(exact) .
works_for: string @index(exact) .
works_with: [uid] .`))

	m1 := `
{
  set {
    _:user1 <name> "user1" .
    _:user1 <email> "user1@company1.io" .
    _:user1 <works_for> "company1" .

    _:user2 <name> "user2" .
    _:user2 <email> "user2@company1.io" .
    _:user2 <works_for> "company1" .

    _:user3 <name> "user3" .
    _:user3 <email> "user3@company2.io" .
    _:user3 <works_for> "company2" .

    _:user4 <name> "user4" .
    _:user4 <email> "user4@company2.io" .
    _:user4 <works_for> "company2" .
  }
}`
	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
}

func TestUpsertMultiValue(t *testing.T) {
	require.NoError(t, dropAll())
	populateCompanyData(t)

	// add color to all employees of company1
	m2 := `
upsert {
  query {
    q(func: eq(works_for, "company1")) {
      u as uid
    }
  }

  mutation {
    set {
      uid(u) <color> "red" .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m2, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"color"}, splitPreds(mr.preds))
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 2, len(result.Queries["q"]))
	q2 := `
{
  q(func: eq(works_for, "%s")) {
    name
    works_for
    color
    works_with
  }
}`
	res, _, err := queryWithTs(queryInp{body: fmt.Sprintf(q2, "company1"), typ: "application/dql"})
	require.NoError(t, err)
	testutil.CompareJSON(t, `{"data":{"q":[{"name":"user1","works_for":"company1","color":"red"},`+
		`{"name":"user2","works_for":"company1","color":"red"}]}}`, res)

	// delete color for employess of company1 and set color for employees of company2
	m3 := `
upsert {
  query {
    user1(func: eq(works_for, "company1")) {
      c1 as uid
    }
    user2(func: eq(works_for, "company2")) {
      c2 as uid
    }
  }

  mutation @if(le(len(c1), 100) AND lt(len(c2), 100)) {
    delete {
      uid(c1) <color> * .
    }

    set {
      uid(c2) <color> "blue" .
    }
  }
}`
	mr, err = mutationWithTs(mutationInp{body: m3, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result = QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 2, len(result.Queries["user1"]))
	require.Equal(t, 2, len(result.Queries["user2"]))

	// The following mutation should have no effect on the state of the database
	m4 := `
upsert {
  query {
    c1 as var(func: eq(works_for, "company1"))
    c2 as var(func: eq(works_for, "company2"))
  }

  mutation @if(gt(len(c1), 2) OR ge(len(c2), 3)) {
    delete {
      uid(c1) <color> * .
    }

    set {
      uid(c2) <color> "blue" .
    }
  }
}`
	mr, err = mutationWithTs(mutationInp{body: m4, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	res, _, err = queryWithTs(queryInp{body: fmt.Sprintf(q2, "company1"), typ: "application/dql"})
	require.NoError(t, err)
	testutil.CompareJSON(t, `{"data":{"q":[{"name":"user1","works_for":"company1"},`+
		`{"name":"user2","works_for":"company1"}]}}`, res)

	res, _, err = queryWithTs(queryInp{body: fmt.Sprintf(q2, "company2"), typ: "application/dql"})
	require.NoError(t, err)
	testutil.CompareJSON(t, `{"data":{"q":[{"name":"user3","works_for":"company2","color":"blue"},`+
		`{"name":"user4","works_for":"company2","color":"blue"}]}}`, res)
}

func TestUpsertMultiValueEdge(t *testing.T) {
	require.NoError(t, dropAll())
	populateCompanyData(t)

	// All employees of company1 now works with all employees of company2
	m1 := `
upsert {
  query {
    c1 as var(func: eq(works_for, "company1"))
    c2 as var(func: eq(works_for, "company2"))
  }

  mutation @if(eq(len(c1), 2) AND eq(len(c2), 2)) {
    set {
      uid(c1) <works_with> uid(c2) .
      uid(c2) <works_with> uid(c1) .
    }
  }
}`
	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	q1 := `
{
  q(func: eq(works_for, "%s")) {
    name
    works_with {
      name
    }
  }
}`
	res, _, err := queryWithTs(queryInp{body: fmt.Sprintf(q1, "company1"), typ: "application/dql"})
	require.NoError(t, err)
	testutil.CompareJSON(t, `{"data":{"q":[{"name":"user2","works_with":[{"name":"user3"},{"name":"user4"}]},`+
		`{"name":"user1","works_with":[{"name":"user3"},{"name":"user4"}]}]}}`, res)

	res, _, err = queryWithTs(queryInp{body: fmt.Sprintf(q1, "company2"), typ: "application/dql"})
	require.NoError(t, err)
	testutil.CompareJSON(t, `{"data":{"q":[{"name":"user3","works_with":[{"name":"user1"},{"name":"user2"}]},`+
		`{"name":"user4","works_with":[{"name":"user1"},{"name":"user2"}]}]}}`, res)

	// user1 and user3 do not work with each other anymore
	m2 := `
upsert {
  query {
    user1(func: eq(email, "user1@company1.io")) {
      u1 as uid
    }
    user2(func: eq(email, "user3@company2.io")) {
      u3 as uid
    }
  }

  mutation @if(eq(len(u1), 1) AND eq(len(u3), 1)) {
    delete {
      uid (u1) <works_with> uid (u3) .
      uid (u3) <works_with> uid (u1) .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m2, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 1, len(result.Queries["user1"]))
	require.Equal(t, 1, len(result.Queries["user2"]))

	res, _, err = queryWithTs(queryInp{body: fmt.Sprintf(q1, "company1"), typ: "application/dql"})
	require.NoError(t, err)
	testutil.CompareJSON(t, `{"data":{"q":[{"name":"user1","works_with":[{"name":"user4"}]},`+
		`{"name":"user2","works_with":[{"name":"user4"},{"name":"user3"}]}]}}`, res)

	res, _, err = queryWithTs(queryInp{body: fmt.Sprintf(q1, "company2"), typ: "application/dql"})
	require.NoError(t, err)
	testutil.CompareJSON(t, `{"data":{"q":[{"name":"user3","works_with":[{"name":"user2"}]},`+
		`{"name":"user4","works_with":[{"name":"user1"},{"name":"user2"}]}]}}`, res)
}

func TestUpsertEdgeWithBlankNode(t *testing.T) {
	require.NoError(t, dropAll())
	populateCompanyData(t)

	// Add a new employee who works with every employee in company2
	m1 := `
upsert {
  query {
    c1 as var(func: eq(works_for, "company1"))
    c2 as var(func: eq(works_for, "company2"))
  }

  mutation @if(lt(len(c1), 3)) {
    set {
      _:user5 <name> "user5" .
      _:user5 <email> "user5@company1.io" .
      _:user5 <works_for> "company1" .
      _:user5 <works_with> uid(c2) .
    }
  }
}`
	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	q1 := `
{
  q(func: eq(email, "user5@company1.io")) {
    name
    email
    works_for
    works_with {
      name
    }
  }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	testutil.CompareJSON(t, `{"data":{"q":[{"name":"user5","email":"user5@company1.io",`+
		`"works_for":"company1","works_with":[{"name":"user3"},{"name":"user4"}]}]}}`, res)
}

func TestConditionalUpsertWithFilterErr(t *testing.T) {
	require.NoError(t, dropAll())
	populateCompanyData(t)

	m1 := `
upsert {
  query {
    me(func: eq(email, "email@company.io")) {
      v as uid
    }
  }

  mutation @filter(eq(len(v), 0)) {
    set {
      uid(v) <name> "Wrong" .
      uid(v) <email> "email@company.io" .
    }
  }
}`
	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.Contains(t, err.Error(), "Expected @if, found [@filter]")
}

func TestConditionalUpsertMissingAtErr(t *testing.T) {
	require.NoError(t, dropAll())
	populateCompanyData(t)

	m1 := `
upsert {
  query {
    me(func: eq(email, "email@company.io")) {
      v as uid
    }
  }

  mutation if(eq(len(v), 0)) {
    set {
      uid(v) <name> "Wrong" .
      uid(v) <email> "email@company.io" .
    }
  }
}`
	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.Contains(t, err.Error(), `Unrecognized character inside mutation: U+0028 '('`)
}

func TestConditionalUpsertDoubleIfErr(t *testing.T) {
	require.NoError(t, dropAll())
	populateCompanyData(t)

	m1 := `
upsert {
  query {
    me(func: eq(email, "email@company.io")) {
      v as uid
    }
  }

  mutation @if(eq(len(v), 0)) @if(eq(len(v), 0)) {
    set {
      uid(v) <name> "Wrong" .
      uid(v) <email> "email@company.io" .
    }
  }
}`
	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.Contains(t, err.Error(), "Expected { at the start of block")
}

func TestConditionalUpsertMissingRightRoundErr(t *testing.T) {
	require.NoError(t, dropAll())
	populateCompanyData(t)

	m1 := `
upsert {
  query {
    me(func: eq(email, "email@company.io")) {
      v as uid
    }
  }

  mutation @if(eq(len(v), 0) {
    set {
      uid(v) <name> "Wrong" .
      uid(v) <email> "email@company.io" .
    }
  }
}`
	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.Contains(t, err.Error(), "Matching brackets not found")
}

func TestUpsertDeleteOnlyYourPost(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
name: string @index(exact) .
content: string @index(exact) .`))

	m1 := `
{
  set {
    _:user1 <name> "user1" .
    _:user2 <name> "user2" .
    _:user3 <name> "user3" .
    _:user4 <name> "user4" .

    _:post1 <content> "post1" .
    _:post1 <author> _:user1 .

    _:post2 <content> "post2" .
    _:post2 <author> _:user1 .

    _:post3 <content> "post3" .
    _:post3 <author> _:user2 .

    _:post4 <content> "post4" .
    _:post4 <author> _:user3 .

    _:post5 <content> "post5" .
    _:post5 <author> _:user3 .

    _:post6 <content> "post6" .
    _:post6 <author> _:user3 .
  }
}`

	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	// user2 trying to delete the post4
	m2 := `
upsert {
  query {
    var(func: eq(content, "post4")) {
      p4 as uid
      author {
        n3 as name
      }
    }

    u2 as var(func: eq(val(n3), "user2"))
  }

  mutation @if(eq(len(u2), 1)) {
    delete {
      uid(p4) <content> * .
      uid(p4) <author> * .
    }
  }
}`
	_, err = mutationWithTs(mutationInp{body: m2, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	// post4 must still exist
	q2 := `
{
  post(func: eq(content, "post4")) {
    content
  }
}`
	res, _, err := queryWithTs(queryInp{body: q2, typ: "application/dql"})
	require.NoError(t, err)
	require.Contains(t, res, "post4")

	// user3 deleting the post4
	m3 := `
upsert {
  query {
    var(func: eq(content, "post4")) {
      p4 as uid
      author {
        n3 as name
      }
    }

    u4 as var(func: eq(val(n3), "user3"))
  }

  mutation @if(eq(len(u4), 1)) {
    delete {
      uid(p4) <content> * .
      uid(p4) <author> * .
    }
  }
}`
	_, err = mutationWithTs(mutationInp{body: m3, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	// post4 shouldn't exist anymore
	res, _, err = queryWithTs(queryInp{body: q2, typ: "application/dql"})
	require.NoError(t, err)
	require.NotContains(t, res, "post4")
}

func TestUpsertMultiTypeUpdate(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
name: string @index(exact) .
branch: string .
age: int .
active: bool .
openDate: dateTime .
password: password .
loc: geo .
amount: float .`))

	m1 := `
{
  set {
    _:user1 <name> "user1" .
    _:user1 <branch> "Fuller Street, San Francisco" .
    _:user1 <amount> "10" .
    _:user1 <age> "30" .
    _:user1 <active> "1" .
    _:user1 <openDate> "1980-01-01" .
    _:user1 <password> "password" .
    _:user1 <loc> "{'type':'Point','coordinates':[-122.4220186,37.772318]}"^^<geo:geojson> .

    _:user2 <name> "user2" .
    _:user2 <branch> "Fuller Street, San Francisco" .
    _:user2 <amount> "10" .
    _:user2 <age> "30" .
    _:user2 <active> "1" .
    _:user2 <openDate> "1980-01-01" .
    _:user2 <password> "password" .
    _:user2 <loc> "{'type':'Point','coordinates':[-122.4220186,37.772318]}"^^<geo:geojson> .

    _:user3 <name> "user3" .
    _:user3 <branch> "Fuller Street, San Francisco" .
    _:user3 <amount> "10" .
    _:user3 <age> "30" .
    _:user3 <active> "1" .
    _:user3 <password> "password" .
    _:user3 <loc> "{'type':'Point','coordinates':[-122.4220186,37.772318]}"^^<geo:geojson> .
  }
}`

	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	q1 := `
{
  q(func: has(branch)) {
    name
    branch
    amount
    age
    active
    openDate
    password
    loc
  }
}`
	expectedRes, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)

	m2 := `
upsert {
  query {
    q(func: has(amount)) {
      u as uid
      amt as amount
      n as name
      b as branch
      a as age
      ac as active
      open as openDate
      pass as password
      l as loc
    }
  }

  mutation {
    set {
      uid(u ) <amount> val(amt) .
      uid(u) <name> val (n) .
      uid(u) <branch> val( b) .
      uid(u) <age> val(a) .
      uid(u) <active> val(ac) .
      uid(u) <openDate> val(open) .
      uid(u) <password> val(pass) .
      uid(u) <loc> val(l) .
    }
  }
}`

	// This test is to ensure that all the types are being
	// parsed correctly by the val function.
	// User3 doesn't have all the fields. This test also ensures
	// that val works when not all records have the values
	mr, err := mutationWithTs(mutationInp{body: m2, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 3, len(result.Queries["q"]))

	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	testutil.CompareJSON(t, res, expectedRes)
}

func TestUpsertWithValueVar(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`amount: int .`))
	_, err := mutationWithTs(mutationInp{
		body: `{ set { _:p <amount> "0" . } }`, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	const (
		// this upsert block increments the value of the counter by one
		m = `
upsert {
  query {
    var(func: has(amount)) {
      amount as amount
      amt as math(amount+1)
    }
  }
  mutation {
    set {
      uid(amt) <amount> val(amt) .
    }
  }
}`

		q = `
{
  q(func: has(amount)) {
    amount
  }
}`
	)

	for count := 1; count < 3; count++ {
		_, err = mutationWithTs(mutationInp{body: m, typ: "application/rdf", commitNow: true})
		require.NoError(t, err)

		got, _, err := queryWithTs(queryInp{body: q, typ: "application/dql"})
		require.NoError(t, err)

		require.JSONEq(t, fmt.Sprintf(`{"data":{"q":[{"amount":%d}]}}`, count), got)
	}
}

func TestValInSubject(t *testing.T) {
	m3 := `
upsert {
  query {
    u as var(func: has(amount)) {
      amt as amount
    }
  }
  mutation {
    set {
      val(amt) <amount> 1 .
    }
  }
}
`

	_, err := mutationWithTs(mutationInp{body: m3, typ: "application/rdf", commitNow: true})
	require.Contains(t, err.Error(), "while lexing val(amt) <amount> 1")
}

func TestUpperCaseFunctionErrorMsg(t *testing.T) {
	m1 := `
upsert {
  query {
    u as var(func: has(amount)) {
      amt as amount
    }
  }
  mutation {
    set {
      uid(u) <amount> VAL(amt) .
    }
  }
}
`
	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.Contains(t, err.Error(), "Invalid input: V at lexText")

	m2 := `
upsert {
  query {
    u as var(func: has(amount)) {
      amt as amount
    }
  }
  mutation {
    set {
      UID(u) <amount> val(amt) .
    }
  }
}
`
	_, err = mutationWithTs(mutationInp{body: m2, typ: "application/rdf", commitNow: true})
	require.Contains(t, err.Error(), "Invalid input: U at lexText")
}

func TestBigFloat(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
name: string @index(exact) .
branch: string .
amount: bigfloat .`))

	m1 := `
{
  set {
    _:user1 <name> "user1" .
    _:user1 <amount> "10.00000000000000000001" .
  }
}`

	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	q1 := `
{
  q(func: has(name)) {
    name
    amount
  }
}`

	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)

	expectedRes := `{"data":{"q":[{"name":"user1","amount":10.00000000000000000001}]}}`
	require.EqualValues(t, expectedRes, res)
}

func SetupBankExample(t *testing.T) string {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
name: string @index(exact) .
branch: string .
amount: bigfloat .`))

	m1 := `
{
  set {
    _:user1 <name> "user1" .
    _:user1 <amount> "10" .

    _:user2 <name> "user2" .
    _:user2 <amount> "100" .

    _:user3 <name> "user3" .
    _:user3 <amount> "1000" .
  }
}`

	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	q1 := `
{
  q(func: has(name)) {
    name
    amount
  }
}`
	expectedRes, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)

	return expectedRes
}

func TestBigFloatInvalidInput(t *testing.T) {
	SetupBankExample(t)
	m1 := `
{
  set {
    _:user4 <name> "user4" .
    _:user4 <amount> "100.000.0" .
  }
}`

	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.Contains(t, err.Error(), `cannot unmarshal "100.000.0" into a *big.Float`)
}

func TestUpsertSanityCheck(t *testing.T) {
	expectedRes := SetupBankExample(t)

	// Checking for error when some wrong field is being used
	m1 := `
upsert {
  query {
    u as var(func: has(amount)) {
      amt as nofield
    }
  }

  mutation {
    set {
      uid(u) <amount> val(amt) .
    }
  }
}`

	q1 := `
{
  q(func: has(name)) {
    name
    amount
  }
}`

	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	testutil.CompareJSON(t, res, expectedRes)
}

func TestUpsertDeleteWrongValue(t *testing.T) {
	expectedRes := SetupBankExample(t)

	// Checking that delete and val should only
	// delete if the value of variable matches
	m1 := `
upsert {
  query {
    u as var(func: has(amount)) {
      amt as amount
      updated_amt as  math(amt+1)
    }
  }
  mutation {
    delete {
      uid(u) <amount> val(updated_amt) .
    }
  }
}`

	q1 := `
{
  q(func: has(name)) {
    name
    amount
  }
}`
	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	// There should be no change
	testutil.CompareJSON(t, res, expectedRes)
}

func TestUpsertDeleteRightValue(t *testing.T) {
	SetupBankExample(t)
	// Checking Bulk Delete in Val
	m1 := `
upsert {
  query {
    u as var(func: has(amount)) {
      amt as amount
    }
  }

  mutation {
    delete {
      uid(u) <amount> val(amt) .
    }
  }
}
`
	q1 := `
{
  q(func: has(name)) {
    name
    amount
  }
}`

	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.NotContains(t, res, "amount")
}

func TestUpsertBulkUpdateValue(t *testing.T) {
	SetupBankExample(t)

	// Resetting the val in upsert to check if the
	// values are not switched and the interest is added
	m1 := `
  upsert {
    query {
      u as var(func: has(amount)) {
        amt as amount
        updated_amt as math(amt+1.123456789101112)
      }
    }

    mutation {
      set {
        uid(u) <amount> val(updated_amt) .
      }
    }
  }
  `

	q1 := `
{
  q(func: has(name)) {
    name
    amount
  }
}`
	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	expectedRes := `
{
  "data": {
    "q": [{
       "name": "user3",
       "amount": 1001.123456789101112
     }, {
       "name": "user1",
       "amount": 11.123456789101112
     }, {
       "name": "user2",
       "amount": 101.123456789101112
     }]
   }
}`
	require.NoError(t, err)
	testutil.CompareJSON(t, res, expectedRes)

}

func TestAggregateValBulkUpdate(t *testing.T) {
	SetupBankExample(t)
	q1 := `
{
  q(func: has(name)) {
    name
    amount
  }
}`

	// Checking support for bulk update values
	// to aggregate variable in upsert
	m1 := `
upsert {
  query {
    u as q(func: has(amount)) {
      amt as amount
    }
    me() {
      max_amt as max(val(amt))
    }
  }

  mutation {
    set {
      uid(u) <amount> val(max_amt) .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 3, len(result.Queries["q"]))

	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	expectedRes := `
{
  "data": {
    "q": [{
       "name": "user3",
       "amount": 1000.000000
     }, {
       "name": "user1",
       "amount": 1000.000000
     }, {
       "name": "user2",
       "amount": 1000.000000
     }]
   }
}`
	require.NoError(t, err)
	testutil.CompareJSON(t, res, expectedRes)
}

func TestUpsertEmptyUID(t *testing.T) {
	SetupBankExample(t)
	m1 := `
upsert {
  query {
    var(func: has(amount)) {
      amt as amount
    }
    me() {
      max_amt as max(val(amt))
    }
    v as q(func: eq(name, "Michael")) {
      amount
    }
  }

  mutation {
    set {
      uid(v) <amount> val(max_amt) .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 0, len(result.Queries["q"]))
}

func TestUpsertBulkUpdateBranch(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
name: string @index(exact) .
branch: string .
amount: float .`))

	m1 := `
{
  set {
    _:user1 <name> "user1" .
    _:user1 <branch> "Fuller Street, San Francisco" .
    _:user1 <amount> "10" .

    _:user2 <name> "user2" .
    _:user2 <branch> "Fuller Street, San Francisco" .
    _:user2 <amount> "100" .

    _:user3 <name> "user3" .
    _:user3 <branch> "Fuller Street, San Francisco" .
    _:user3 <amount> "1000" .
  }
}`

	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	// Bulk Update: update everyone's branch
	m2 := `
upsert {
  query {
    q(func: has(branch)) {
      u as uid
    }
  }

  mutation {
    set {
      uid(u) <branch> "Fuller Street, SF" .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m2, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 3, len(result.Queries["q"]))

	q2 := `
{
  q(func: has(branch)) {
    name
    branch
    amount
  }
}`
	res, _, err := queryWithTs(queryInp{body: q2, typ: "application/dql"})
	require.NoError(t, err)
	require.NotContains(t, res, "San Francisco")
	require.Contains(t, res, "user1")
	require.Contains(t, res, "user2")
	require.Contains(t, res, "user3")
	require.Contains(t, res, "Fuller Street, SF")
	// Bulk Delete: delete everyone's branch
	m3 := `
upsert {
  query {
    q(func: has(branch)) {
      u as uid
    }
  }

  mutation {
    delete {
      uid(u) <branch> * .
    }
  }
}`
	mr, err = mutationWithTs(mutationInp{body: m3, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result = QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 3, len(result.Queries["q"]))

	res, _, err = queryWithTs(queryInp{body: q2, typ: "application/dql"})
	require.NoError(t, err)
	require.NotContains(t, res, "San Francisco")
	require.NotContains(t, res, "Fuller Street, SF")
}

func TestDeleteCountIndex(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
<game_answer>: uid @count @reverse .
<name>: int @index(int) .`))

	m1 := `
{
  set {
    _:1 <game_answer> _:2 .
    _:1 <name> "1" .
    _:2 <game_answer> _:3 .
    _:2 <name> "2" .
    _:4 <game_answer> _:2 .
    _:3 <name> "3" .
    _:4 <name> "4" .
  }
}`

	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	m2 := `
upsert {
  query {
    u3 as var(func: eq(name, "3"))
    u2 as var(func: eq(name, "2"))
  }
  mutation {
    delete {
        uid(u2) <game_answer> uid(u3) .
    }
  }
}`
	_, err = mutationWithTs(mutationInp{body: m2, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	q1 := `
{
    me(func: eq(count(~game_answer), 1)) {
      name
      count(~game_answer)
    }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.NotContains(t, res, "count(~game_answer)")
}

func TestUpsertVarOnlyUsedInQuery(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
name: string @index(exact) .
branch: string .
amount: float .`))

	m1 := `
{
  set {
    _:user1 <name> "user1" .
    _:user1 <branch> "Fuller Street, San Francisco" .
    _:user1 <amount> "10" .
  }
}`

	_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	// Bulk Update: update everyone's branch
	m2 := `
upsert {
  query {
    u as var(func: has(branch))

    me(func: uid(u)) {
      branch
    }
  }

  mutation {
    set {
      _:a <branch> "Fuller Street, SF" .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m2, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 0, len(result.Queries["q"]))
}

func TestEmptyRequest(t *testing.T) {
	// We are using the dgo client in this test here to test the grpc interface
	require.NoError(t, dg.Alter(context.Background(), &api.Operation{
		DropOp: api.Operation_ALL,
	}))
	require.NoError(t, dg.Alter(context.Background(), &api.Operation{
		Schema: `
name: string @index(exact) .
branch: string .
amount: float .`}))

	req := &api.Request{}
	_, err := dg.NewTxn().Do(context.Background(), req)
	require.Contains(t, strings.ToLower(err.Error()), "empty request")
}

// This mutation (upsert) has one independent query and one independent mutation.
func TestMutationAndQueryButNoUpsert(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
email: string @index(exact) .
works_for: string @index(exact) .
works_with: [uid] .`))

	m1 := `
upsert {
  query {
    q(func: eq(works_for, "company1")) {
      uid
      name
    }
  }

  mutation {
    set {
      _:user1 <name> "user1" .
      _:user1 <email> "user1@company1.io" .
      _:user1 <works_for> "company1" .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 0, len(result.Queries["q"]))
	require.Equal(t, []string{"email", "name", "works_for"}, splitPreds(mr.preds))
}

func TestMultipleMutation(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`email: string @index(exact) .`))

	m1 := `
upsert {
  query {
    q(func: eq(email, "email@company.io")) {
      v as uid
    }
  }

  mutation @if(not(eq(len(v), 0))) {
    set {
      uid(v) <name> "not_name" .
      uid(v) <email> "not_email@company.io" .
    }
  }

  mutation @if(eq(len(v), 0)) {
    set {
      _:user <name> "name" .
      _:user <email> "email@company.io" .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"email", "name"}, splitPreds(mr.preds))
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 0, len(result.Queries["q"]))

	q1 := `
{
  q(func: eq(email, "email@company.io")) {
    name
  }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	expectedRes := `
{
  "data": {
    "q": [{
      "name": "name"
     }]
   }
}`
	require.NoError(t, err)
	testutil.CompareJSON(t, res, expectedRes)

	// This time the other mutation will get executed
	_, err = mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)

	q2 := `
{
  q(func: eq(email, "not_email@company.io")) {
    name
  }
}`
	res, _, err = queryWithTs(queryInp{body: q2, typ: "application/dql"})
	require.NoError(t, err)

	expectedRes = `
{
  "data": {
    "q": [{
      "name": "not_name"
     }]
   }
}`
	require.NoError(t, err)
	testutil.CompareJSON(t, res, expectedRes)
}

func TestMultiMutationWithoutIf(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`email: string @index(exact) .`))

	m1 := `
upsert {
  query {
    me(func: eq(email, "email@company.io")) {
      v as uid
    }
  }

  mutation @if(not(eq(len(v), 0))) {
    set {
      uid(v) <name> "not_name" .
      uid(v) <email> "not_email@company.io" .
    }
  }

  mutation @if(eq(len(v), 0)) {
    set {
      _:user <name> "name" .
    }
  }

  mutation {
    set {
      _:user <email> "email@company.io" .
    }
  }

  mutation {
    set {
      _:other <name> "other" .
      _:other <email> "other@company.io" .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"email", "name"}, splitPreds(mr.preds))

	q1 := `
{
  q(func: has(email)) {
    name
  }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	expectedRes := `
{
  "data": {
    "q": [{
      "name": "name"
     },
     {
      "name": "other"
    }]
   }
}`
	require.NoError(t, err)
	testutil.CompareJSON(t, res, expectedRes)
}

func TestMultiMutationCount(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
  email: string @index(exact) .
  count: int .`))

	m1 := `
upsert {
  query {
    q(func: eq(email, "email@company.io")) {
      v as uid
      c as count
      nc as math(c+1)
    }
  }

  mutation @if(eq(len(v), 0)) {
    set {
      uid(v) <name> "name" .
      uid(v) <email> "email@company.io" .
      uid(v) <count> "1" .
    }
  }

  mutation @if(not(eq(len(v), 0))) {
    set {
      uid(v) <count> val(nc) .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"count", "email", "name"}, splitPreds(mr.preds))

	q1 := `
{
  q(func: has(email)) {
    count
  }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	expectedRes := `
{
  "data": {
    "q": [{
       "count": 1
     }]
   }
}`
	require.NoError(t, err)
	testutil.CompareJSON(t, res, expectedRes)

	// second time, using Json mutation
	m1Json := `
{
  "query": "{q(func: eq(email, \"email@company.io\")) {v as uid\n c as count\n nc as math(c+1)}}",
  "mutations": [
    {
      "set": [
        {
          "uid": "uid(v)",
          "name": "name",
          "email": "email@company.io",
          "count": "1"
        }
      ],
      "cond": "@if(eq(len(v), 0))"
    },
    {
      "set": [
        {
          "uid": "uid(v)",
          "count": "val(nc)"
        }
      ],
      "cond": "@if(not(eq(len(v), 0)))"
    }
  ]
}`
	mr, err = mutationWithTs(mutationInp{body: m1Json, typ: "application/json", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"count"}, splitPreds(mr.preds))

	res, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql"})
	expectedRes = `
{
  "data": {
    "q": [{
       "count": 2
     }]
   }
}`
	require.NoError(t, err)
	testutil.CompareJSON(t, res, expectedRes)
}

func TestMultipleMutationMerge(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`
name: string @index(term) .
email: [string] @index(exact) @upsert .`))

	m1 := `
{
  set {
    _:user1 <name> "user1" .
    _:user1 <email> "user_email1@company1.io" .
    _:user2 <name> "user2" .
    _:user2 <email> "user_email2@company1.io" .
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"email", "name"}, splitPreds(mr.preds))

	q1 := `
{
  q(func: has(name)) {
    uid
  }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	var result struct {
		Data struct {
			Q []struct {
				UID string `json:"uid"`
			} `json:"q"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(res), &result))
	require.Equal(t, 2, len(result.Data.Q))

	m2 := `
upsert {
  query {
    # filter is needed to ensure that we do not get same UIDs in u1 and u2
    q1(func: eq(email, "user_email1@company1.io")) @filter(not(eq(email, "user_email2@company1.io"))) {
      u1 as uid
    }

    q2(func: eq(email, "user_email2@company1.io")) @filter(not(eq(email, "user_email1@company1.io"))) {
      u2 as uid
    }

    q3(func: eq(email, "user_email1@company1.io")) @filter(eq(email, "user_email2@company1.io")) {
      u3 as uid
    }
  }

  # case when both emails do not exist
  mutation @if(eq(len(u1), 0) AND eq(len(u2), 0) AND eq(len(u3), 0)) {
    set {
      _:user <name> "user" .
      _:user <email> "user_email1@company1.io" .
      _:user <email> "user_email2@company1.io" .
    }
  }

  # case when email1 exists but email2 does not
  mutation @if(eq(len(u1), 1) AND eq(len(u2), 0) AND eq(len(u3), 0)) {
    set {
      uid(u1) <email> "user_email2@company1.io" .
    }
  }

  # case when email1 does not exist but email2 exists
  mutation @if(eq(len(u1), 0) AND eq(len(u2), 1) AND eq(len(u3), 0)) {
    set {
      uid(u2) <email> "user_email1@company1.io" .
    }
  }

  # case when both emails exist and needs merging
  mutation @if(eq(len(u1), 1) AND eq(len(u2), 1) AND eq(len(u3), 0)) {
    set {
      _:user <name> "user" .
      _:user <email> "user_email1@company1.io" .
      _:user <email> "user_email2@company1.io" .
    }

    delete {
      uid(u1) <name> * .
      uid(u1) <email> * .
      uid(u2) <name> * .
      uid(u2) <email> * .
    }
  }
}`
	mr, err = mutationWithTs(mutationInp{body: m2, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"email", "name"}, splitPreds(mr.preds))

	res, _, err = queryWithTs(queryInp{body: q1, typ: "application/dql"})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(res), &result))
	require.Equal(t, 1, len(result.Data.Q))

	// Now, data is all correct. So, following mutation should be no-op
	mr, err = mutationWithTs(mutationInp{body: m2, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, 0, len(mr.preds))
}

func TestJsonOldAndNewAPI(t *testing.T) {
	require.NoError(t, dropAll())
	populateCompanyData(t)

	m1 := `
{
  "query": "{q(func: eq(works_for, \"company1\")) {u as uid}}",
  "set": [
    {
      "uid": "uid(u)",
      "color": "red"
    }
  ],
  "cond": "@if(gt(len(u), 0))",
  "mutations": [
    {
      "set": [
        {
          "uid": "uid(u)",
          "works_with": {
            "uid": "0x01"
          }
        }
      ],
      "cond": "@if(gt(len(u), 0))"
    }
  ]
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/json", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"color", "works_with"}, splitPreds(mr.preds))
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 2, len(result.Queries["q"]))
	q2 := `
  {
    q(func: eq(works_for, "%s")) {
      name
      works_for
      color
      works_with {
        uid
      }
    }
  }`
	res, _, err := queryWithTs(queryInp{body: fmt.Sprintf(q2, "company1"), typ: "application/dql"})
	require.NoError(t, err)
	testutil.CompareJSON(t, `
  {
    "data": {
      "q": [
        {
          "name": "user1",
          "works_for": "company1",
          "color": "red",
          "works_with": [
            {
              "uid": "0x1"
            }
          ]
        },
        {
          "name": "user2",
          "works_for": "company1",
          "color": "red",
          "works_with": [
            {
              "uid": "0x1"
            }
          ]
        }
      ]
    }
  }`, res)
}

func TestJsonNewAPI(t *testing.T) {
	require.NoError(t, dropAll())
	populateCompanyData(t)

	m1 := `
{
  "query": "{q(func: eq(works_for, \"company1\")) {u as uid}}",
  "mutations": [
    {
      "set": [
        {
          "uid": "uid(u)",
          "works_with": {
            "uid": "0x01"
          }
        }
      ],
      "cond": "@if(gt(len(u), 0))"
    },
    {
      "set": [
        {
          "uid": "uid(u)",
          "color": "red"
        }
      ],
      "cond": "@if(gt(len(u), 0))"
    }
  ]
}`
	mr, err := mutationWithTs(mutationInp{body: m1, typ: "application/json", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"color", "works_with"}, splitPreds(mr.preds))
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 2, len(result.Queries["q"]))
	q2 := `
  {
    q(func: eq(works_for, "%s")) {
      name
      works_for
      color
      works_with {
        uid
      }
    }
  }`
	res, _, err := queryWithTs(queryInp{body: fmt.Sprintf(q2, "company1"), typ: "application/dql"})
	require.NoError(t, err)
	testutil.CompareJSON(t, `
  {
    "data": {
      "q": [
        {
          "name": "user1",
          "works_for": "company1",
          "color": "red",
          "works_with": [
            {
              "uid": "0x1"
            }
          ]
        },
        {
          "name": "user2",
          "works_for": "company1",
          "color": "red",
          "works_with": [
            {
              "uid": "0x1"
            }
          ]
        }
      ]
    }
  }`, res)
}

func TestUpsertMultiValueJson(t *testing.T) {
	require.NoError(t, dropAll())
	populateCompanyData(t)

	// add color to all employees of company1
	m2 := `
{
  "query": "{q(func: eq(works_for, \"company1\")) {u as uid}}",
  "mutations": [
    {
      "set": [
        {
          "uid": "uid(u)",
          "color": "red"
        }
      ],
      "cond": "@if(gt(len(u), 0))"
    }
  ]
}`

	mr, err := mutationWithTs(mutationInp{body: m2, typ: "application/json", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"color"}, splitPreds(mr.preds))
	result := QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 2, len(result.Queries["q"]))
	q2 := `
{
  q(func: eq(works_for, "%s")) {
    name
    works_for
    color
    works_with
  }
}`
	res, _, err := queryWithTs(queryInp{body: fmt.Sprintf(q2, "company1"), typ: "application/dql"})
	require.NoError(t, err)
	testutil.CompareJSON(t, `{"data":{"q":[{"name":"user1","works_for":"company1","color":"red"},`+
		`{"name":"user2","works_for":"company1","color":"red"}]}}`, res)

	// delete color for employess of company1 and set color for employees of company2
	m3 := `
{
  "query": "{u1(func: eq(works_for, \"company1\")) {c1 as uid} u2(func: eq(works_for, \"company2\")) {c2 as uid}}",
  "mutations": [
    {
      "delete": [
        {
          "uid": "uid(c1)",
          "color": null
        }
      ],
      "cond": "@if(le(len(c1), 100) AND lt(len(c2), 100))"
    },
    {
      "set": [
        {
          "uid": "uid(c2)",
          "color": "blue"
        }
      ],
      "cond": "@if(le(len(c1), 100) AND lt(len(c2), 100))"
    }
  ]
}`
	mr, err = mutationWithTs(mutationInp{body: m3, typ: "application/json", commitNow: true})
	require.NoError(t, err)
	result = QueryResult{}
	require.NoError(t, json.Unmarshal(mr.data, &result))
	require.Equal(t, 2, len(result.Queries["u1"]))
	require.Equal(t, 2, len(result.Queries["u2"]))
}

func TestValVarWithBlankNode(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, alterSchema(`version: int .`))

	m := `
upsert {
  query {
    q(func: has(version), orderdesc: version, first: 1) {
      Ver as version
      VerIncr as math(Ver + 1)
    }

    me() {
      sVerIncr as sum(val(VerIncr))
    }
  }

  mutation @if(gt(len(VerIncr), 0)) {
    set {
      _:newNode <version> val(sVerIncr) .
    }
  }

  mutation @if(eq(len(VerIncr), 0)) {
    set {
      _:newNode <version> "1" .
    }
  }
}`
	mr, err := mutationWithTs(mutationInp{body: m, typ: "application/rdf", commitNow: true})
	require.NoError(t, err)
	require.True(t, len(mr.keys) == 0)
	require.Equal(t, []string{"version"}, splitPreds(mr.preds))

	for i := 0; i < 10; i++ {
		mr, err = mutationWithTs(mutationInp{body: m, typ: "application/rdf", commitNow: true})
		require.NoError(t, err)
		require.True(t, len(mr.keys) == 0)
		require.Equal(t, []string{"version"}, splitPreds(mr.preds))
	}

	q1 := `
{
  q(func: has(version), orderdesc: version, first: 1) {
    version
  }
}`
	res, _, err := queryWithTs(queryInp{body: q1, typ: "application/dql"})
	expectedRes := `
{
  "data": {
    "q": [{
       "version": 11
     }]
   }
}`
	require.NoError(t, err)
	testutil.CompareJSON(t, res, expectedRes)
}

// This test may fail sometimes because ACL token
// can get expired while the mutations is running.
func upsertTooBigTest(t *testing.T) { //nolint:unused
	require.NoError(t, dropAll())

	for i := 0; i < 1e6+1; {
		fmt.Printf("ingesting entries starting i=%v\n", i)

		sb := strings.Builder{}
		for j := 0; j < 1e4; j++ {
			_, err := sb.WriteString(fmt.Sprintf("_:%v <number> \"%v\" .\n", i, i))
			require.NoError(t, err)
			i++
		}

		m1 := fmt.Sprintf(`{set{%s}}`, sb.String())
		_, err := mutationWithTs(mutationInp{body: m1, typ: "application/rdf", commitNow: true})
		require.NoError(t, err)
	}

	// Upsert should fail
	m2 := `
upsert {
  query {
    u as var(func: has(number))
  }

  mutation {
    set {
      uid(u) <test> "test" .
    }
  }
}`
	_, err := mutationWithTs(mutationInp{body: m2, typ: "application/rdf", commitNow: true})
	require.Contains(t, err.Error(), "variable [u] has too many UIDs (>1m)")

	// query should work
	q2 := `
{
  q(func: has(number)) {
    uid
    number
  }
}`
	_, _, err = queryWithTs(queryInp{body: q2, typ: "application/dql"})
	require.NoError(t, err)
}

func TestLargeStringIndex(t *testing.T) {
	require.NoError(t, dropAll())

	// Exact Index
	require.NoError(t, alterSchemaWithRetry(`name_exact: string @index(exact) .`))
	bigval := strings.Repeat("biggest value", 7000)
	mu := fmt.Sprintf(`{ set { <0x01> <name_exact> "%v" . } }`, bigval)
	require.Contains(t, runMutation(mu).Error(), "value in the mutation is too large for the index")

	// Term Index
	require.NoError(t, alterSchemaWithRetry(`name_term: string @index(term) .`))
	bigval = strings.Repeat("biggestvalue", 7000)
	mu = fmt.Sprintf(`{ set { <0x01> <name_term> "%v" . } }`, bigval)
	require.Contains(t, runMutation(mu).Error(), "value in the mutation is too large for the index")
	// while the value is large, term index is fine
	bigval = strings.Repeat("biggestvalue", 3000)
	mu = fmt.Sprintf(`{ set { <0x01> <name_term> "%v %v" . } }`, bigval, bigval)
	require.NoError(t, runMutation(mu))

	// FullText Index
	require.NoError(t, alterSchemaWithRetry(`name_fulltext: string @index(fulltext) .`))
	bigval = strings.Repeat("biggestvalue", 7000)
	mu = fmt.Sprintf(`{ set { <0x01> <name_fulltext> "%v" . } }`, bigval)
	require.Contains(t, runMutation(mu).Error(), "value in the mutation is too large for the index")
	// while the value is large, fulltext index is fine
	bigval = strings.Repeat("biggestvalue", 3000)
	mu = fmt.Sprintf(`{ set { <0x01> <name_fulltext> "%v %v" . } }`, bigval, bigval)
	require.NoError(t, runMutation(mu))

	// Trigram Index
	require.NoError(t, alterSchemaWithRetry(`name_trigram: string @index(trigram) .`))
	bigval = strings.Repeat("biggestvalue", 7000)
	mu = fmt.Sprintf(`{ set { <0x01> <name_trigram> "%v" . } }`, bigval)
	require.NoError(t, runMutation(mu))

	// Large Predicate
	bigpred := strings.Repeat("large-predicate", 2000)
	require.NoError(t, alterSchemaWithRetry(fmt.Sprintf(`%v: string @index(exact) .`, bigpred)))
	bigval = strings.Repeat("biggestvalue", 3000)
	mu = fmt.Sprintf(`{ set { <0x01> <%v> "%v" . } }`, bigpred, bigval)
	require.Contains(t, runMutation(mu).Error(), "value in the mutation is too large for the index")

	// name_term has index term. Now, if we create an exact index, that will fail.
	// This is because one of the value has small terms but the whole value is bigger
	// than the limit. In such a case, indexing will fail and the schema should not change.
	require.NoError(t, alterSchemaWithRetry(`name_term: string @index(exact) .`))
	dqlSchema, err := runGraphqlQuery(`schema{}`)
	require.NoError(t, err)
	require.Contains(t, dqlSchema,
		`{"predicate":"name_term","type":"string","index":true,"tokenizer":["term"]}`)
}
