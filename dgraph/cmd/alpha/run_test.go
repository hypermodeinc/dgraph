//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

//nolint:lll
package alpha

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/twpayne/go-geom"
	"github.com/twpayne/go-geom/encoding/geojson"
	"github.com/twpayne/go-geom/encoding/wkb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"

	"github.com/dgraph-io/dgo/v250"
	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/dgraphapi"
	"github.com/hypermodeinc/dgraph/v25/dgraphtest"
	"github.com/hypermodeinc/dgraph/v25/dql"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/query"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/x"
)

type defaultContextKey int

const (
	mutationAllowedKey defaultContextKey = iota
)

func defaultContext() context.Context {
	return context.WithValue(context.Background(), mutationAllowedKey, true)
}

var (
	addr         string
	alphaSockAdd string
	dg           *dgraphapi.GrpcClient
	hc           *dgraphapi.HTTPClient
	ts           uint64
	// the grootAccessJWT stores the access JWT extracted from the response
	// of http login
	token *Token
)

func timestamp() uint64 {
	return atomic.AddUint64(&ts, 1)
}

func processToFastJSON(q string) string {
	res, err := dql.Parse(dql.Request{Str: q})
	if err != nil {
		log.Fatal(err)
	}

	var l query.Latency
	ctx := defaultContext()
	qr := query.Request{Latency: &l, DqlQuery: &res, ReadTs: timestamp()}
	err = qr.ProcessQuery(ctx)

	if err != nil {
		log.Fatal(err)
	}

	buf, err := query.ToJson(context.Background(), &l, qr.Subgraphs, nil)
	if err != nil {
		log.Fatal(err)
	}
	return string(buf)
}

func runGraphqlQuery(q string) (string, error) {
	output, _, err := queryWithTs(queryInp{body: q, typ: "application/dql"})
	return output, err
}

func runJSONQuery(q string) (string, error) {
	output, _, err := queryWithTs(queryInp{body: q, typ: "application/json"})
	return output, err
}

func runMutation(m string) error {
	_, err := mutationWithTs(mutationInp{body: m, typ: "application/rdf", commitNow: true})
	return err
}

func runJSONMutation(m string) error {
	_, err := mutationWithTs(
		mutationInp{body: m, typ: "application/json", isJson: true, commitNow: true})
	return err
}

func alterSchema(s string) error {
	return alterSchemaHelper(s, false)
}

func alterSchemaInBackground(s string) error {
	return alterSchemaHelper(s, true)
}

func alterSchemaHelper(s string, bg bool) error {
	url := addr + "/alter"
	if bg {
		url += "?runInBackground=true"
	}

	_, _, err := runWithRetries("PUT", "", url, s)
	if err != nil {
		return errors.Wrapf(err, "while running request with retries")
	}

	return nil
}

func alterSchemaWithRetry(s string) error {
	var err error
	for i := 0; i < 3; i++ {
		if err = alterSchema(s); err == nil {
			return nil
		}
	}
	return err
}

func dropAll() error {
	op := `{"drop_all": true}`
	_, _, err := runWithRetries("PUT", "", addr+"/alter", op)
	return err
}

func deletePredicate(pred string) error {
	op := `{"drop_attr": "` + pred + `"}`
	_, _, err := runWithRetries("PUT", "", addr+"/alter", op)
	return err
}

func TestDeletePredicate(t *testing.T) {
	var m1 = `
	{
		set {
			<0x1> <friend> <0x2> .
			<0x1> <friend> <0x3> .
			<0x1> <name> "Alice" .
			<0x2> <name> "Alice1" .
			<0x3> <name> "Alice2" .
			<0x3> <age> "13" .
			<0x11> <salary> "100000" . # should be deleted from schema after we delete the predicate
		}
	}
	`

	var q1 = `
	{
		user(func: anyofterms(name, "alice")) {
			friend {
				name
			}
		}
	}
	`
	var q2 = `
	{
		user(func: uid(0x1, 0x2, 0x3)) {
			name
		}
	}
	`
	var q3 = `
	{
		user(func: uid(0x3)) {
			age
			~friend {
				name
			}
		}
	}
	`

	var q4 = `
		{
			user(func: uid(0x3)) {
				age
				friend {
					name
				}
			}
		}
	`

	var s1 = `
	friend: [uid] @reverse .
	name: string @index(term) .
	`

	var s2 = `
	friend: string @index(term) .
	`
	require.NoError(t, dropAll())
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	require.NoError(t, alterSchemaWithRetry(s1))
	require.NoError(t, runMutation(m1))

	output, err := runGraphqlQuery(q1)
	require.NoError(t, err)
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(output), &m))
	friends := m["data"].(map[string]interface{})["user"].([]interface{})[0].(map[string]interface{})["friend"].([]interface{})
	require.Equal(t, 2, len(friends))

	output, err = runGraphqlQuery(q2)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"user":[{"name":"Alice"},{"name":"Alice1"},{"name":"Alice2"}]}}`,
		output)

	output, err = runGraphqlQuery(q3)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"user":[{"age": "13", "~friend" : [{"name":"Alice"}]}]}}`, output)

	require.NoError(t, deletePredicate("friend"))
	require.NoError(t, deletePredicate("salary"))

	output, err = runGraphqlQuery(`schema{}`)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(dgraphapi.GetFullSchemaHTTPResponse(dgraphapi.SchemaOptions{
		UserPreds: `{"predicate":"age","type":"default"},` +
			`{"predicate":"name","type":"string","index":true, "tokenizer":["term"]}`}), output))

	output, err = runGraphqlQuery(q1)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"user": []}}`, output)

	output, err = runGraphqlQuery(q2)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"user": [{"name":"Alice"},{"name":"Alice1"},{"name":"Alice2"}]}}`, output)

	output, err = runGraphqlQuery(q4)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"user":[{"age": "13"}]}}`, output)

	// Lets try to change the type of predicates now.
	require.NoError(t, alterSchemaWithRetry(s2))
}

type S struct {
	Predicate string   `json:"predicate"`
	Type      string   `json:"type"`
	Index     bool     `json:"index"`
	Tokenizer []string `json:"tokenizer"`
}

type Received struct {
	Schema []S `json:"schema"`
}

func TestSchemaMutation(t *testing.T) {
	require.NoError(t, dropAll())
	var m = `
			name: string @index(term, exact) .
			alias: string @index(exact, term) .
			dob: dateTime @index(year) .
			film.film.initial_release_date: dateTime @index(year) .
			loc: geo @index(geo) .
			genre: [uid] @reverse .
			survival_rate : float .
			alive         : bool .
			age           : int .
			shadow_deep   : int .
			friend: [uid] @reverse .
			geometry: geo @index(geo) . `

	expected := S{
		Predicate: "name",
		Type:      "string",
		Index:     true,
		Tokenizer: []string{"term", "exact"},
	}

	require.NoError(t, alterSchemaWithRetry(m))
	output, err := runGraphqlQuery("schema {}")
	require.NoError(t, err)
	got := make(map[string]Received)
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	received, ok := got["data"]
	require.True(t, ok)

	var found bool
	for _, s := range received.Schema {
		if s.Predicate == "name" {
			found = true
			require.Equal(t, expected, s)
		}
	}
	require.True(t, found)
}

func TestSchemaMutation1(t *testing.T) {
	require.NoError(t, dropAll())
	var m = `
	{
		set {
			<0x1234> <pred1> "12345"^^<xs:string> .
			<0x1234> <pred2> "12345" .
		}
	}
`
	require.NoError(t, runMutation(m))

	output, err := runGraphqlQuery("schema {}")
	require.NoError(t, err)
	got := make(map[string]Received)
	require.NoError(t, json.Unmarshal([]byte(output), &got))
	received, ok := got["data"]
	require.True(t, ok)

	var count int
	for _, s := range received.Schema {
		if s.Predicate == "pred1" {
			require.Equal(t, "string", s.Type)
			count++
		} else if s.Predicate == "pred2" {
			require.Equal(t, "default", s.Type)
			count++
		}
	}
	require.Equal(t, 2, count)
}

// reverse on scalar type
func TestSchemaMutation2Error(t *testing.T) {
	var m = `
            age:string @reverse .
	`
	require.Error(t, alterSchema(m))
}

// index on uid type
func TestSchemaMutation3Error(t *testing.T) {
	var m = `
            age: uid @index .
	`
	require.Error(t, alterSchema(m))
}

func TestMutation4Error(t *testing.T) {
	t.Skip()
	var m = `
	{
		set {
      			<1> <_age_> "5" .
		}
	}
	`
	require.Error(t, runMutation(m))
}

func TestMutationSingleUid(t *testing.T) {
	// reset Schema
	require.NoError(t, schema.ParseBytes([]byte(""), 1))

	var s = `
            friend: uid .
	`
	require.NoError(t, alterSchema(s))

	var m = `
	{
		set {
			<0x1> <friend> <0x2> .
			<0x1> <friend> <0x3> .
		}
	}
	`
	require.NoError(t, runMutation(m))
}

// Verify a list uid predicate cannot be converted to a single-element predicate.
func TestSchemaMutationUidError1(t *testing.T) {
	// reset Schema
	require.NoError(t, schema.ParseBytes([]byte(""), 1))

	var s1 = `
            friend: [uid] .
	`
	require.NoError(t, alterSchemaWithRetry(s1))

	var s2 = `
            friend: uid .
	`
	require.Error(t, alterSchema(s2))
}

// add index
func TestSchemaMutationIndexAdd(t *testing.T) {
	var q1 = `
	{
		user(func:anyofterms(name, "Alice")) {
			name
		}
	}
	`
	var m = `
	{
		set {
                        # comment line should be ignored
			<0x1> <name> "Alice" .
		}
	}
	`

	var s = `
            name:string @index(term) .
	`

	// reset Schema
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	require.NoError(t, runMutation(m))

	// add index to name
	require.NoError(t, alterSchemaWithRetry(s))

	output, err := runGraphqlQuery(q1)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"user":[{"name":"Alice"}]}}`, output)

}

// Remove index
func TestSchemaMutationIndexRemove(t *testing.T) {
	var q1 = `
	{
		user(func:anyofterms(name, "Alice")) {
			name
		}
	}
	`
	var m = `
	{
		set {
                        # comment line should be ignored
			<0x1> <name> "Alice" .
		}
	}
	`

	var s1 = `
            name:string @index(term) .
	`
	var s2 = `
            name:string .
	`

	// reset Schema
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	// add index to name
	require.NoError(t, alterSchemaWithRetry(s1))
	require.NoError(t, runMutation(m))

	output, err := runGraphqlQuery(q1)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"user":[{"name":"Alice"}]}}`, output)

	// remove index
	require.NoError(t, alterSchemaWithRetry(s2))

	_, err = runGraphqlQuery(q1)
	require.Error(t, err)
}

// add reverse edge
func TestSchemaMutationReverseAdd(t *testing.T) {
	var q1 = `
	{
		user(func: uid(0x3)) {
			~friend {
				name
			}
		}
	}
	`
	var m = `
	{
		set {
                        # comment line should be ignored
			<0x1> <friend> <0x3> .
			<0x1> <name> "Alice" .
		}
	}
	`

	var s = `friend: [uid] @reverse .`

	// reset Schema
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	require.NoError(t, runMutation(m))

	// add index to name
	require.NoError(t, alterSchemaWithRetry(s))

	output, err := runGraphqlQuery(q1)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"user":[{"~friend" : [{"name":"Alice"}]}]}}`, output)

}

// Remove reverse edge
func TestSchemaMutationReverseRemove(t *testing.T) {
	var q1 = `
	{
		user(func: uid(0x3)) {
			~friend {
				name
			}
		}
	}
	`
	var m = `
	{
		set {
                        # comment line should be ignored
			<0x1> <friend> <0x3> .
			<0x1> <name> "Alice" .
		}
	}
	`

	var s1 = `
            friend: [uid] @reverse .
	`

	var s2 = `
            friend: [uid] .
	`

	// reset Schema
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	require.NoError(t, runMutation(m))

	// add reverse edge to name
	require.NoError(t, alterSchemaWithRetry(s1))

	output, err := runGraphqlQuery(q1)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"user":[{"~friend" : [{"name":"Alice"}]}]}}`, output)

	// remove reverse edge
	require.NoError(t, alterSchemaWithRetry(s2))

	_, err = runGraphqlQuery(q1)
	require.Error(t, err)
}

// add count edges
func TestSchemaMutationCountAdd(t *testing.T) {
	var q1 = `
	{
		user(func:eq(count(friend),4)) {
			name
		}
	}
	`
	var m = `
	{
		set {
                        # comment line should be ignored
			<0x1> <name> "Alice" .
			<0x01> <friend> <0x02> .
			<0x01> <friend> <0x03> .
			<0x01> <friend> <0x04> .
			<0x01> <friend> <0x05> .
		}
	}
	`

	var s = `
			friend: [uid] @count .
	`

	// reset Schema
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	require.NoError(t, runMutation(m))

	// add index to name
	require.NoError(t, alterSchemaWithRetry(s))

	time.Sleep(10 * time.Millisecond)
	output, err := runGraphqlQuery(q1)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"user":[{"name":"Alice"}]}}`, output)
}

func TestJsonMutation(t *testing.T) {
	var q1 = `
	{
		q(func: has(name)) {
			uid
			name
		}
	}
	`
	var q2 = `
	{
		q(func: has(name)) {
			name
		}
	}
	`
	var m1 = `
	{
		"set": [
			{
				"name": "Alice"
			},
			{
				"name": "Bob"
			}
		]
	}
	`
	var m2 = `
	{
		"delete": [
			{
				"uid": "%s",
				"name": null
			}
		]
	}
	`
	var s1 = `
            name: string @index(exact) .
	`
	require.NoError(t, dropAll())
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	require.NoError(t, alterSchemaWithRetry(s1))
	require.NoError(t, runJSONMutation(m1))

	output, err := runGraphqlQuery(q1)
	require.NoError(t, err)
	q1Result := map[string]interface{}{}
	require.NoError(t, json.Unmarshal([]byte(output), &q1Result))
	queryResults := q1Result["data"].(map[string]interface{})["q"].([]interface{})
	require.Equal(t, 2, len(queryResults))

	var uid string
	count := 0
	for i := 0; i < 2; i++ {
		name := queryResults[i].(map[string]interface{})["name"].(string)
		if name == "Alice" {
			uid = queryResults[i].(map[string]interface{})["uid"].(string)
			count++
		} else {
			require.Equal(t, "Bob", name)
		}
	}
	require.Equal(t, 1, count)
	require.NoError(t, runJSONMutation(fmt.Sprintf(m2, uid)))

	output, err = runGraphqlQuery(q2)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"q":[{"name":"Bob"}]}}`, output)
}

func TestJsonMutationNumberParsing(t *testing.T) {
	var q1 = `
	{
		q(func: has(n1)) {
			n1
			n2
		}
	}
	`
	var m1 = `
	{
		"set": [
			{
				"n1": 9007199254740995,
				"n2": 9007199254740995.0
			}
		]
	}
	`
	require.NoError(t, dropAll())
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	require.NoError(t, runJSONMutation(m1))

	output, err := runGraphqlQuery(q1)
	require.NoError(t, err)
	var q1Result struct {
		Data struct {
			Q []map[string]interface{} `json:"q"`
		} `json:"data"`
	}
	buffer := bytes.NewBuffer([]byte(output))
	dec := json.NewDecoder(buffer)
	dec.UseNumber()
	require.NoError(t, dec.Decode(&q1Result))
	require.Equal(t, 1, len(q1Result.Data.Q))

	n1, ok := q1Result.Data.Q[0]["n1"]
	require.True(t, ok)
	switch n1 := n1.(type) {
	case json.Number:
		require.False(t, strings.Contains(n1.String(), "."))
		i, err := n1.Int64()
		require.NoError(t, err)
		require.Equal(t, int64(9007199254740995), i)
	default:
		require.Fail(t, fmt.Sprintf("expected n1 of type int64, got %v (type %T)", n1, n1))
	}

	n2, ok := q1Result.Data.Q[0]["n2"]
	require.True(t, ok)
	switch n2 := n2.(type) {
	case json.Number:
		require.True(t, strings.Contains(n2.String(), "."))
		f, err := n2.Float64()
		require.NoError(t, err)
		require.Equal(t, 9007199254740995.0, f)
	default:
		require.Fail(t, fmt.Sprintf("expected n2 of type float64, got %v (type %T)", n2, n2))
	}
}

func TestDeleteAll(t *testing.T) {
	var q1 = `
	{
		user(func: uid(0x3)) {
			~friend {
				name
			}
		}
	}
	`
	var q2 = `
	{
		user(func: anyofterms(name, "alice")) {
			friend {
				name
			}
		}
	}
	`

	var m2 = `
	{
		delete{
			<0x1> <friend> * .
			<0x1> <name> * .
		}
	}
	`
	var m1 = `
	{
		set {
			<0x1> <friend> <0x2> .
			<0x1> <friend> <0x3> .
			<0x1> <name> "Alice" .
			<0x2> <name> "Alice1" .
			<0x3> <name> "Alice2" .
		}
	}
	`

	var s1 = `
      		friend: [uid] @reverse .
		name: string @index(term) .
	`
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	require.NoError(t, alterSchemaWithRetry(s1))

	require.NoError(t, runMutation(m1))

	output, err := runGraphqlQuery(q1)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"user":[{"~friend" : [{"name":"Alice"}]}]}}`, output)

	output, err = runGraphqlQuery(q2)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"user":[{"friend":[{"name":"Alice1"},{"name":"Alice2"}]}]}}`,
		output)

	require.NoError(t, runMutation(m2))

	output, err = runGraphqlQuery(q1)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"user": []}}`, output)

	output, err = runGraphqlQuery(q2)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"user": []}}`, output)
}

func TestDeleteAllSP1(t *testing.T) {
	var m = `
	{
		delete{
			<2000> * * .
		}
	}`
	require.NoError(t, runMutation(m))
}

var m5 = `
	{
		set {
                        # comment line should be ignored
			<ram> <name> "1"^^<xs:int> .
			<shyam> <name> "abc"^^<xs:int> .
		}
	}
`

var q5 = `
	{
		user(func: uid(<id>)) {
			name
		}
	}
`

func TestSchemaValidationError(t *testing.T) {
	_, err := dql.Parse(dql.Request{Str: m5})
	require.Error(t, err)
	output, err := runGraphqlQuery(strings.Replace(q5, "<id>", "0x8", -1))
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"user": []}}`, output)
}

func TestMutationError(t *testing.T) {
	var qErr = `
 	{
 		set {
 			<0x0> <name> "Alice" .
 		}
 	}
 `
	require.Error(t, runMutation(qErr))
}

var q1 = `
{
	al(func: uid( 0x1)) {
		status
		follows {
			status
			follows {
				status
				follows {
					status
				}
			}
		}
	}
}
`

// TODO: This might not work. Fix it later, if needed.
func BenchmarkQuery(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		processToFastJSON(q1)
	}
}

// change from uid to scalar or vice versa
func TestSchemaMutation4Error(t *testing.T) {
	var m = `
            age:int .
	`
	// reset Schema
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	require.NoError(t, alterSchemaWithRetry(m))

	m = `
	{
		set {
			<0x9> <age> "13" .
		}
	}
	`
	require.NoError(t, runMutation(m))

	m = `
	mutation {
		schema {
            age: uid .
		}
	}
	`
	require.Error(t, alterSchema(m))
}

// change from uid to scalar or vice versa
func TestSchemaMutation5Error(t *testing.T) {
	var m = `
            friends: [uid] .
	`
	// reset Schema
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	require.NoError(t, alterSchemaWithRetry(m))

	m = `
	{
		set {
			<0x8> <friends> <0x5> .
		}
	}
	`
	require.NoError(t, runMutation(m))

	m = `
            friends: string .
	`
	require.Error(t, alterSchema(m))
}

// A basic sanity check. We will do more extensive testing for multiple values in query.
func TestMultipleValues(t *testing.T) {
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	m := `
			occupations: [string] .
`
	require.NoError(t, alterSchemaWithRetry(m))

	m = `
		{
			set {
				<0x88> <occupations> "Pianist" .
				<0x88> <occupations> "Software Engineer" .
			}
		}
	`

	require.NoError(t, runMutation(m))

	q := `{
			me(func: uid(0x88)) {
				occupations
			}
		}`
	res, err := runGraphqlQuery(q)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"me":[{"occupations":["Software Engineer","Pianist"]}]}}`, res)
}

func TestListTypeSchemaChange(t *testing.T) {
	require.NoError(t, dropAll())
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	m := `
			occupations: [string] @index(term) .
	`
	require.NoError(t, alterSchemaWithRetry(m))

	m = `
		{
			set {
				<0x88> <occupations> "Pianist" .
				<0x88> <occupations> "Software Engineer" .
			}
		}
	`
	require.NoError(t, runMutation(m))

	q := `{
			me(func: uid(0x88)) {
				occupations
			}
		}`
	res, err := runGraphqlQuery(q)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"me":[{"occupations":["Software Engineer","Pianist"]}]}}`, res)

	q = `{
			me(func: anyofterms(occupations, "Engineer")) {
				occupations
			}
	}`
	res, err = runGraphqlQuery(q)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"me":[{"occupations":["Software Engineer","Pianist"]}]}}`, res)

	q = `{
			me(func: allofterms(occupations, "Software Engineer")) {
				occupations
			}
	}`
	res, err = runGraphqlQuery(q)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"me":[{"occupations":["Software Engineer","Pianist"]}]}}`, res)

	m = `
				occupations: string .
	`

	// Cant change from list-type to non-list till we have data.
	err = alterSchema(m)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Schema change not allowed from [string] => string")

	require.NoError(t, deletePredicate("occupations"))
	require.NoError(t, alterSchemaWithRetry(m))

	q = `schema{}`
	res, err = runGraphqlQuery(q)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(dgraphapi.GetFullSchemaHTTPResponse(
		dgraphapi.SchemaOptions{UserPreds: `{"predicate":"occupations","type":"string"}`}), res))
}

func TestDeleteAllSP2(t *testing.T) {
	s := `
	nodeType: string .
	name: string .
	date: datetime .
	weight: float .
	weightUnit: string .
	lifeLoad: int .
	stressLevel: int .
	plan: string .
	postMortem: string .

	type Node12345 {
		nodeType
		name
		date
		weight
		weightUnit
		lifeLoad
		stressLevel
		plan
		postMortem
	}
	`
	require.NoError(t, dropAll())
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	require.NoError(t, alterSchemaWithRetry(s))

	var m = `
	{
	  set {
	    <0x12345> <nodeType> "TRACKED_DAY" .
	    <0x12345> <name> "July 3 2017" .
	    <0x12345> <date> "2017-07-03T03:49:03+00:00" .
	    <0x12345> <weight> "262.3" .
	    <0x12345> <weightUnit> "pound" .
	    <0x12345> <lifeLoad> "5" .
	    <0x12345> <stressLevel> "3" .
	    <0x12345> <plan> "modest day" .
	    <0x12345> <postMortem> "win!" .
	    <0x12345> <dgraph.type> "Node12345" .
	  }
	}
	`
	require.NoError(t, runMutation(m))

	q := fmt.Sprintf(`
	{
	  me(func: uid(%s)) {
		name
	    date
	    weight
	    lifeLoad
	    stressLevel
	  }
	}`, "0x12345")

	output, err := runGraphqlQuery(q)
	require.NoError(t, err)
	require.JSONEq(t,
		`{"data": {"me":[{"name":"July 3 2017","date":"2017-07-03T03:49:03Z",`+
			`"weight":262.3,"lifeLoad":5,"stressLevel":3}]}}`, output)

	m = fmt.Sprintf(`
		{
			delete {
				<%s> * * .
			}
		}`, "0x12345")

	require.NoError(t, runMutation(m))

	output, err = runGraphqlQuery(q)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"me":[]}}`, output)
}

func TestDeleteScalarValue(t *testing.T) {
	var s = `name: string @index(exact) .`
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	require.NoError(t, alterSchemaWithRetry(s))

	var m = `
	{
	  set {
		<0x12345> <name> "xxx" .
		<0x12346> <name> "xxx" .
	  }
	}
	`
	require.NoError(t, runMutation(m))

	// This test has been flaky at the step that verifies whether the triple exists
	// after the first deletion. To try to combat that, verify the triple can be
	// queried before performing the deletion.
	q := `
	{
	  me(func: uid(0x12345)) {
		name
	  }
	}`
	for i := 0; i < 5; i++ {
		output, err := runGraphqlQuery(q)
		if err != nil || !assert.JSONEq(t, output, `{"data": {"me":[{"name":"xxx"}]}}`) {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		break
	}

	var d1 = `
    {
      delete {
        <0x12345> <name> "yyy" .
      }
    }
	`
	require.NoError(t, runMutation(d1))

	// Verify triple was not deleted because the value in the request did
	// not match the existing value.
	output, err := runGraphqlQuery(q)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"me":[{"name":"xxx"}]}}`, output)

	indexQuery := `
	{
		me(func: eq(name, "xxx")) {
			name
		}
	}
	`
	output, err = runGraphqlQuery(indexQuery)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"me":[{"name":"xxx"}, {"name":"xxx"}]}}`, output)

	var d2 = `
	{
      delete {
        <0x12345> <name> "xxx" .
      }
    }
	`
	require.NoError(t, runMutation(d2))

	// Verify triple was actually deleted this time.
	output, err = runGraphqlQuery(q)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"me":[]}}`, output)

	// Verify index was also updated this time and one of the triples got deleted.
	output, err = runGraphqlQuery(indexQuery)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"me":[{"name": "xxx"}]}}`, output)
}

func TestDeleteValueLang(t *testing.T) {
	var s = `name: string @lang .`
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	require.NoError(t, alterSchemaWithRetry(s))

	var m = `
	{
	  set {
	    <0x12345> <name> "Mark"@en .
	    <0x12345> <name> "Marco"@es .
	    <0x12345> <name> "Marc"@fr .
	  }
	}
	`
	require.NoError(t, runMutation(m))

	q := `
	{
	  me(func: uid(0x12345)) {
		name@*
	  }
	}`
	output, err := runGraphqlQuery(q)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"me":[
		{"name@en":"Mark", "name@es":"Marco", "name@fr":"Marc"}]}}`, output)

	var d1 = `
    {
      delete {
        <0x12345> <name@fr> * .
      }
    }
	`
	require.NoError(t, runMutation(d1))

	// Verify only the specific tagged value was deleted.
	output, err = runGraphqlQuery(q)
	require.NoError(t, err)
	require.JSONEq(t, output, `{"data": {"me":[{"name@en":"Mark", "name@es":"Marco"}]}}`)
}

func TestDropAll(t *testing.T) {
	var m1 = `
	{
		set{
			_:foo <name> "Foo" .
		}
	}`
	var q1 = `
	{
		q(func: allofterms(name, "Foo")) {
			uid
			name
		}
	}`

	s := `name: string @index(term) .`
	require.NoError(t, alterSchemaWithRetry(s))

	require.NoError(t, runMutation(m1))

	output, err := runGraphqlQuery(q1)
	require.NoError(t, err)
	q1Result := map[string]interface{}{}
	require.NoError(t, json.Unmarshal([]byte(output), &q1Result))
	queryResults := q1Result["data"].(map[string]interface{})["q"].([]interface{})
	name := queryResults[0].(map[string]interface{})["name"].(string)
	require.Equal(t, "Foo", name)

	require.NoError(t, dropAll())

	q3 := "schema{}"
	output, err = runGraphqlQuery(q3)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(dgraphapi.GetFullSchemaHTTPResponse(dgraphapi.SchemaOptions{}), output))

	// Reinstate schema so that we can re-run the original query.
	require.NoError(t, alterSchemaWithRetry(s))

	q5 := `
	{
		q(func: allofterms(name, "Foo")) {
			uid
			name
		}
	}`
	output, err = runGraphqlQuery(q5)
	require.NoError(t, err)
	require.JSONEq(t, `{"data": {"q":[]}}`, output)
}

func TestIllegalCountInQueryFn(t *testing.T) {
	s := `friend: [uid] @count .`
	require.NoError(t, alterSchemaWithRetry(s))

	q := `
	{
		q(func: eq(count(friend), 0)) {
			count
		}
	}`
	_, err := runGraphqlQuery(q)
	require.Error(t, err)
	require.Contains(t, err.Error(), "count")
	require.Contains(t, err.Error(), "zero")
}

// This test is from Github issue #2662.
// This test couldn't like in query package because that package tries to do some extra JSON
// marshal, which causes issues for this case.
func TestJsonUnicode(t *testing.T) {
	err := runJSONMutation(`{
  "set": [
  { "uid": "0x10", "log.message": "\u001b[32mHello World 1!\u001b[39m\n" }
  ]
}`)
	require.NoError(t, err)

	output, err := runGraphqlQuery(`{ node(func: uid(0x10)) { log.message }}`)
	require.NoError(t, err)
	require.Equal(t,
		`{"data":{"node":[{"log.message":"\u001b[32mHello World 1!\u001b[39m\n"}]}}`, output)
}

func TestGrpcCompressionSupport(t *testing.T) {
	conn, err := grpc.NewClient(alphaSockAdd,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.UseCompressor(gzip.Name)),
	)
	defer func() {
		require.NoError(t, conn.Close())
	}()
	require.NoError(t, err)

	dc := dgo.NewDgraphClient(api.NewDgraphClient(conn))
	require.NoError(t, dc.LoginIntoNamespace(context.Background(), x.GrootId, "password", x.RootNamespace))
	q := `schema {}`
	tx := dc.NewTxn()
	_, err = tx.Query(context.Background(), q)
	require.NoError(t, err)
}

func TestTypeMutationAndQuery(t *testing.T) {
	var m = `
	{
		"set": [
			{
				"name": "Alice",
				"dgraph.type": "Employee"
			},
			{
				"name": "Bob",
				"dgraph.type": "Employer"
			}
		]
	}
	`

	var q = `
	{
		q(func: has(name)) @filter(type(Employee)){
			uid
			name
		}
	}
	`

	var s = `
            name: string @index(exact) .
	`

	require.NoError(t, dropAll())
	require.NoError(t, alterSchemaWithRetry(s))
	require.NoError(t, runJSONMutation(m))

	output, err := runGraphqlQuery(q)
	require.NoError(t, err)
	result := map[string]interface{}{}
	require.NoError(t, json.Unmarshal([]byte(output), &result))
	queryResults := result["data"].(map[string]interface{})["q"].([]interface{})
	require.Equal(t, 1, len(queryResults))
	name := queryResults[0].(map[string]interface{})["name"].(string)
	require.Equal(t, "Alice", name)
}

func TestIPStringParsing(t *testing.T) {
	var addrRange []x.IPRange
	var err error

	addrRange, err = getIPsFromString("144.142.126.222:144.142.126.244")
	require.NoError(t, err)
	require.Equal(t, net.IPv4(144, 142, 126, 222), addrRange[0].Lower)
	require.Equal(t, net.IPv4(144, 142, 126, 244), addrRange[0].Upper)

	addrRange, err = getIPsFromString("144.142.126.254")
	require.NoError(t, err)
	require.Equal(t, net.IPv4(144, 142, 126, 254), addrRange[0].Lower)
	require.Equal(t, net.IPv4(144, 142, 126, 254), addrRange[0].Upper)

	addrRange, err = getIPsFromString("192.168.0.0/16")
	require.NoError(t, err)
	require.Equal(t, net.IPv4(192, 168, 0, 0), addrRange[0].Lower)
	require.Equal(t, net.IPv4(192, 168, 255, 255), addrRange[0].Upper)

	addrRange, err = getIPsFromString("example.org")
	require.NoError(t, err)
	require.NotEqual(t, net.IPv4zero, addrRange[0].Lower)

	addrRange, err = getIPsFromString("144.142.126.222:144.142.126.244,144.142.126.254" +
		",192.168.0.0/16,example.org")
	require.NoError(t, err)
	require.NotEqual(t, 0, len(addrRange))

	addrRange, err = getIPsFromString("fd03:b188:0f3c:9ec4::babe:face")
	require.NoError(t, err)
	require.NotEqual(t, net.IPv6zero, addrRange[0].Lower)
	require.Equal(t, addrRange[0].Lower, addrRange[0].Upper)

	addrRange, err = getIPsFromString("fd03:b188:0f3c:9ec4::/64")
	require.NoError(t, err)
	require.NotEqual(t, net.IPv6zero, addrRange[0].Lower)
	require.NotEqual(t, addrRange[0].Lower, addrRange[0].Upper)

	addrRange, err = getIPsFromString("")
	require.NoError(t, err)
	require.Equal(t, addrRange, []x.IPRange{})

	addrRange, err = getIPsFromString("fd03:b188:0f3c:9ec4")
	require.Nil(t, addrRange)
	require.Error(t, err)

	addrRange, err = getIPsFromString("192.168.0.0/160")
	require.Nil(t, addrRange)
	require.Error(t, err)

	addrRange, err = getIPsFromString("192.0.2:192.0.2.1")
	require.Nil(t, addrRange)
	require.Error(t, err)

	addrRange, err = getIPsFromString("192.0.2.1:192.0.2")
	require.Nil(t, addrRange)
	require.Error(t, err)

	addrRange, err = getIPsFromString("w.x.y.z:a.b.c.d")
	require.Nil(t, addrRange)
	require.Error(t, err)

}

func TestJSONQueryWithVariables(t *testing.T) {
	require.NoError(t, schema.ParseBytes([]byte(""), 1))
	m := `
			user_id: string @index(exact) @upsert .
			user_name: string @index(hash) .
			follows: [uid] @reverse .
`
	require.NoError(t, alterSchemaWithRetry(m))

	m = `
		{
			set {
				<0x1400> <user_id> "user1" .
				<0x1400> <user_name> "first user" .
				<0x1401> <user_id> "user2" .
				<0x1401> <user_name> "second user" .
				<0x1400> <follows> <0x1401> .
				<0x1402> <user_id> "user3" .
				<0x1402> <user_name> "third user" .
				<0x1401> <follows> <0x1402> .
				<0x1403> <user_id> "user4" .
				<0x1403> <user_name> "fourth user" .
				<0x1401> <follows> <0x1403> .
			}
		}
	`

	require.NoError(t, runMutation(m))

	q1 := `query all($userID: string) {
		q(func: eq(user_id, $userID)) {
			user_id
			user_name
		}
	}`
	p1 := params{
		Query: q1,
		Variables: map[string]string{
			"$userID": "user1",
		},
	}
	data, err := json.Marshal(p1)
	require.NoError(t, err)
	res, err := runJSONQuery(string(data))
	require.NoError(t, err)
	require.JSONEq(t, `{"data":{"q":[{"user_id":"user1","user_name":"first user"}]}}`, res)

	q2 := `query all($userID: string, $userName: string) {
		q(func: eq(user_id, $userID)) {
			user_id
			user_name
			follows @filter(eq(user_name, $userName)) {
				uid
				user_id
			}
		}
	}`
	p2 := params{
		Query: q2,
		Variables: map[string]string{
			"$userID":   "user2",
			"$userName": "fourth user",
		},
	}
	data, err = json.Marshal(p2)
	require.NoError(t, err)
	res, err = runJSONQuery(string(data))
	require.NoError(t, err)
	exp := `{"data":{"q":[{"user_id":"user2","user_name":"second user",` +
		`"follows":[{"uid":"0x1403","user_id":"user4"}]}]}}`
	require.JSONEq(t, exp, res)
}

func TestGeoDataInvalidString(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, dg.Alter(ctx, &api.Operation{DropAll: true}))
	require.NoError(t, dg.Alter(ctx, &api.Operation{Schema: `loc: geo .`}))

	n := &api.NQuad{
		Subject:   "_:test",
		Predicate: "loc",
		ObjectValue: &api.Value{
			Val: &api.Value_StrVal{
				StrVal: `{"type": "Point", "coordintaes": [1.0, 2.0]}`,
			},
		},
	}
	_, err := dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		Set:       []*api.NQuad{n},
	})
	require.Contains(t, err.Error(), "geom: unsupported layout NoLayout")
}

// This test shows that GeoVal API doesn't accept string data. Though, mutation
// succeeds querying the data returns an error. Ideally, we should not accept
// invalid data in a mutation though that is left as future work.
func TestGeoCorruptData(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, dg.Alter(ctx, &api.Operation{DropAll: true}))
	require.NoError(t, dg.Alter(ctx, &api.Operation{Schema: `loc: geo .`}))

	n := &api.NQuad{
		Subject:   "_:test",
		Predicate: "loc",
		ObjectValue: &api.Value{
			Val: &api.Value_GeoVal{
				GeoVal: []byte(`{"type": "Point", "coordinates": [1.0, 2.0]}`),
			},
		},
	}
	_, err := dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		Set:       []*api.NQuad{n},
	})
	require.NoError(t, err)

	q := `
{
  all(func: has(loc)) {
      uid
      loc
  }
}`
	_, err = dg.NewReadOnlyTxn().Query(ctx, q)
	require.Contains(t, err.Error(), "wkb: unknown byte order: 1111011")
}

// This test shows how we could use the GeoVal API to store geo data.
// As far as I (Aman) know, this is something that should not be used
// by a common user unless user knows what she is doing.
func TestGeoValidWkbData(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, dg.Alter(ctx, &api.Operation{DropAll: true}))
	require.NoError(t, dg.Alter(ctx, &api.Operation{Schema: `loc: geo .`}))
	s := `{"type": "Point", "coordinates": [1.0, 2.0]}`
	var gt geom.T
	x.Panic(geojson.Unmarshal([]byte(s), &gt))

	data, err := wkb.Marshal(gt, binary.LittleEndian)
	x.Panic(err)

	n := &api.NQuad{
		Subject:   "_:test",
		Predicate: "loc",
		ObjectValue: &api.Value{
			Val: &api.Value_GeoVal{
				GeoVal: data,
			},
		},
	}

	_, err = dg.NewTxn().Mutate(ctx, &api.Mutation{
		CommitNow: true,
		Set:       []*api.NQuad{n},
	})
	require.NoError(t, err)
	q := `
{
  all(func: has(loc)) {
      uid
      loc
  }
}`
	resp, err := dg.NewReadOnlyTxn().Query(ctx, q)
	require.NoError(t, err)
	require.Contains(t, string(resp.Json), `{"type":"Point","coordinates":[1,2]}`)
}

type Token struct {
	token *dgraphapi.HttpToken
	sync.RWMutex
}

func (t *Token) getAccessJWTToken() string {
	t.RLock()
	defer t.RUnlock()
	return t.token.AccessJwt
}

func (t *Token) refreshToken() error {
	t.Lock()
	defer t.Unlock()
	err := hc.LoginUsingToken(0)
	if err != nil {
		return err
	}
	t.token.AccessJwt = hc.AccessJwt
	t.token.RefreshToken = hc.RefreshToken
	return nil
}

func TestMain(m *testing.M) {
	conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).WithReplicas(1).WithACL(time.Hour)
	c, err := dgraphtest.NewLocalCluster(conf)

	x.Panic(err)
	x.Panic(c.Start())
	var cleanup func()
	dg, cleanup, err = c.Client()
	x.Panic(err)
	defer cleanup()

	x.Panic(dg.Login(context.Background(), dgraphapi.DefaultUser, dgraphapi.DefaultPassword))

	alphaGrpcPort, err := c.GetAlphaGrpcPublicPort(0)
	x.Panic(err)

	alphaSockAdd = "0.0.0.0:" + alphaGrpcPort
	alphaSockAddHttp, err := c.GetAlphaHttpPublicPort(0)
	x.Panic(err)
	addr = "http://0.0.0.0:" + alphaSockAddHttp
	zeroSockAdd, err := c.GetZeroGrpcPublicPort(0)
	x.Panic(err)

	// Increment lease, so that mutations work.
	conn, err := grpc.NewClient("0.0.0.0:"+zeroSockAdd, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	zc := pb.NewZeroClient(conn)
	if _, err := zc.AssignIds(context.Background(),
		&pb.Num{Val: 1e6, Type: pb.Num_UID}); err != nil {
		log.Fatal(err)
	}
	hc, err = c.HTTPClient()
	x.Panic(err)

	x.Panic(hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, 0))

	httpToken := hc.HttpToken
	token = &Token{
		token:   httpToken,
		RWMutex: sync.RWMutex{},
	}
	code := m.Run()
	if code != 0 {
		c.Cleanup(true)
	} else {
		c.Cleanup(false)
	}
	os.Exit(code)
}
