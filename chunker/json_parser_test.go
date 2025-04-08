//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

//nolint:lll
package chunker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/golang/glog"
	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/dgraphtest"
	"github.com/hypermodeinc/dgraph/v24/testutil"
	"github.com/hypermodeinc/dgraph/v24/tok"
	"github.com/hypermodeinc/dgraph/v24/types"
	"github.com/hypermodeinc/dgraph/v24/x"
)

var (
	dg *dgraphapi.GrpcClient
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

type School struct {
	Name string `json:"name,omitempty"`
}

type address struct {
	Type   string    `json:"type,omitempty"`
	Coords []float64 `json:"coordinates,omitempty"`
}

type Person struct {
	Uid       string     `json:"uid,omitempty"`
	Namespace string     `json:"namespace,omitempty"`
	Name      string     `json:"name,omitempty"`
	Age       int        `json:"age,omitempty"`
	Married   *bool      `json:"married,omitempty"`
	Now       *time.Time `json:"now,omitempty"`
	Address   address    `json:"address,omitempty"` // geo value
	Friends   []Person   `json:"friend,omitempty"`
	School    *School    `json:"school,omitempty"`
}

type Product struct {
	Uid           string `json:"uid,omitempty"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Description_v string `json:"description_v"`
}

func Parse(b []byte, op int) ([]*api.NQuad, error) {
	nqs := NewNQuadBuffer(1000)
	err := nqs.ParseJSON(b, op)
	return nqs.nquads, err
}

// FastParse uses buf.FastParseJSON() simdjson parser.
func FastParse(b []byte, op int) ([]*api.NQuad, error) {
	nqs := NewNQuadBuffer(1000)
	err := nqs.FastParseJSON(b, op)
	return nqs.nquads, err
}

func (exp *Experiment) verify() {
	// insert the data into dgraph
	ctx := context.Background()
	require.NoError(exp.t, dg.Login(ctx, dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(exp.t, dg.Alter(ctx, &api.Operation{DropAll: true}), "drop all failed")
	require.NoError(exp.t, dg.Alter(ctx, &api.Operation{Schema: exp.schema}),
		"schema change failed")
	_, err := dg.NewTxn().Mutate(ctx,
		&api.Mutation{Set: exp.nqs, CommitNow: true})
	require.NoError(exp.t, err, "mutation failed")

	response, err := dg.NewReadOnlyTxn().Query(ctx, exp.query)
	require.NoError(exp.t, err, "query failed")
	testutil.CompareJSON(exp.t, exp.expected, string(response.GetJson()))
}

type Experiment struct {
	t        *testing.T
	nqs      []*api.NQuad
	schema   string
	query    string
	expected string
}

func TestNquadsFromJson1(t *testing.T) {
	tn := time.Now().UTC()
	m := true
	p := Person{
		Uid:       "1",
		Namespace: "0x2",
		Name:      "Alice",
		Age:       26,
		Married:   &m,
		Now:       &tn,
		Address: address{
			Type:   "Point",
			Coords: []float64{1.1, 2.0},
		},
	}

	b, err := json.Marshal(p)
	require.NoError(t, err)

	nq, err := Parse(b, SetNquads)
	require.NoError(t, err)
	require.Equal(t, 5, len(nq))

	fastNQ, err := FastParse(b, SetNquads)
	require.NoError(t, err)
	require.Equal(t, 5, len(fastNQ))

	exp := &Experiment{
		t:      t,
		nqs:    nq,
		schema: "name: string @index(exact) .",
		query: `{
			alice(func: eq(name, "Alice")) {
				name
				age
				married
				address
			}
		}`,
		expected: `{
			"alice": [{
				"name": "Alice",
				"age": 26,
				"married": true,
				"address": {"coordinates": [2,1.1], "type": "Point"}}
			]}
		`,
	}
	exp.verify()

	exp.nqs = fastNQ
	exp.verify()
}

func TestNquadsFromJson2(t *testing.T) {
	m := false

	p := Person{
		Name: "Alice",
		Friends: []Person{{
			Name:    "Charlie",
			Married: &m,
		}, {
			Uid:  "1000",
			Name: "Bob",
		}},
	}

	b, err := json.Marshal(p)
	require.NoError(t, err)

	nq, err := Parse(b, SetNquads)
	require.NoError(t, err)
	require.Equal(t, 6, len(nq))

	fastNQ, err := FastParse(b, SetNquads)
	require.NoError(t, err)
	require.Equal(t, 6, len(fastNQ))

	exp := &Experiment{
		t:      t,
		nqs:    nq,
		schema: "name: string @index(exact) .",
		query: `{alice(func: eq(name, "Alice")) {
name
friend {
  name
  married
}}}`,
		expected: `{"alice":[{
"name":"Alice",
"friend": [
{"name":"Charlie", "married":false},
{"name":"Bob"}
]
}]}`,
	}
	exp.verify()

	exp.nqs = fastNQ
	exp.verify()
}

func TestNquadsFromJson3(t *testing.T) {
	p := Person{
		Name: "Alice",
		School: &School{
			Name: "Wellington Public School",
		},
	}

	b, err := json.Marshal(p)
	require.NoError(t, err)
	nq, err := Parse(b, SetNquads)
	require.NoError(t, err)

	fastNQ, err := FastParse(b, SetNquads)
	require.NoError(t, err)

	exp := &Experiment{
		t:      t,
		nqs:    nq,
		schema: "name: string @index(exact) .",
		query: `{alice(func: eq(name, "Alice")) {
name
school {name}
}}`,
		expected: `{"alice":[{
"name":"Alice",
"school": [{"name":"Wellington Public School"}]
}]}`,
	}
	exp.verify()

	exp.nqs = fastNQ
	exp.verify()
}

func TestNquadsFromJson4(t *testing.T) {
	json := `[{"name":"Alice","mobile":"040123456","car":"MA0123", "age": 21, "weight": 58.7}]`

	nq, err := Parse([]byte(json), SetNquads)
	require.NoError(t, err)

	fastNQ, err := FastParse([]byte(json), SetNquads)
	require.NoError(t, err)

	exp := &Experiment{
		t:      t,
		nqs:    nq,
		schema: "name: string @index(exact) .",
		query: `{alice(func: eq(name, "Alice")) {
name
mobile
car
age
weight
}}`,
		expected: fmt.Sprintf(`{"alice":%s}`, json),
	}
	exp.verify()

	exp.nqs = fastNQ
	exp.verify()
}

func TestNquadsFromJsonMap(t *testing.T) {
	json := `{"name":"Alice",
"age": 25,
"friends": [{
"name": "Bob"
}]}`

	nq, err := Parse([]byte(json), SetNquads)
	require.NoError(t, err)

	fastNQ, err := FastParse([]byte(json), SetNquads)
	require.NoError(t, err)

	exp := &Experiment{
		t:      t,
		nqs:    nq,
		schema: "name: string @index(exact) .",
		query: `{people(func: eq(name, "Alice")) {
age
name
friends {name}
}}`,
		expected: fmt.Sprintf(`{"people":[%s]}`, json),
	}
	exp.verify()

	exp.nqs = fastNQ
	exp.verify()
}

func TestNquadsFromMultipleJsonObjects(t *testing.T) {
	json := `
[
  {
    "name": "A",
    "age": 25,
    "friends": [
      {
        "name": "A1",
        "friends": [
          {
            "name": "A11"
          },
          {
            "name": "A12"
          }
        ]
      },
     {
        "name": "A2",
        "friends": [
          {
            "name": "A21"
          },
          {
            "name": "A22"
          }
        ]
      }
    ]
  },
  {
    "name": "B",
    "age": 26,
    "friends": [
      {
        "name": "B1",
        "friends": [
          {
            "name": "B11"
          },
          {
            "name": "B12"
          }
        ]
      },
     {
        "name": "B2",
        "friends": [
          {
            "name": "B21"
          },
          {
            "name": "B22"
          }
        ]
      }
    ]
  }
]
`

	nq, err := Parse([]byte(json), SetNquads)
	require.NoError(t, err)

	fastNQ, err := FastParse([]byte(json), SetNquads)
	require.NoError(t, err)

	exp := &Experiment{
		t:      t,
		nqs:    nq,
		schema: "name: string @index(exact) .",
		query: `{people(func: has(age), orderasc: name) @recurse {
name
age
friends
}}`,
		expected: fmt.Sprintf(`{"people":%s}`, json),
	}
	exp.verify()

	exp.nqs = fastNQ
	exp.verify()
}

func TestJsonNumberParsing(t *testing.T) {
	tests := []struct {
		in  string
		out *api.Value
	}{
		{`{"uid": "1", "key": 9223372036854775299}`, &api.Value{Val: &api.Value_IntVal{IntVal: 9223372036854775299}}},
		{`{"uid": "1", "key": 9223372036854775299.0}`,
			&api.Value{Val: &api.Value_DoubleVal{DoubleVal: 9223372036854775299.0}}},
		{`{"uid": "1", "key": 27670116110564327426}`, nil},
		{`{"uid": "1", "key": "23452786"}`, &api.Value{Val: &api.Value_StrVal{StrVal: "23452786"}}},
		{`{"uid": "1", "key": "23452786.2378"}`, &api.Value{Val: &api.Value_StrVal{StrVal: "23452786.2378"}}},
		{`{"uid": "1", "key": -1e10}`, &api.Value{Val: &api.Value_DoubleVal{DoubleVal: -1e+10}}},
		{`{"uid": "1", "key": 0E-0}`, &api.Value{Val: &api.Value_DoubleVal{DoubleVal: 0}}},
	}

	for i, test := range tests {
		nqs, err := Parse([]byte(test.in), SetNquads)
		if i == 2 {
			fmt.Println(err)
		}
		if test.out != nil {
			require.NoError(t, err, "%T", err)
			require.Equal(t, makeNquad("1", "key", test.out), nqs[0])
		} else {
			require.Error(t, err)
		}

		fastNQ, err := FastParse([]byte(test.in), SetNquads)
		if i == 2 {
			fmt.Println(err)
		}
		if test.out != nil {
			require.NoError(t, err, "%T", err)
			require.Equal(t, makeNquad("1", "key", test.out), fastNQ[0])
		} else {
			require.Error(t, err)
		}
	}
}

func TestNquadsFromJson_UidOutofRangeError(t *testing.T) {
	json := `{"uid":"0xa14222b693e4ba34123","name":"Name","following":[{"name":"Bob"}],"school":[{"uid":"","name@en":"Crown Public School"}]}`

	_, err := Parse([]byte(json), SetNquads)
	require.Error(t, err)

	_, err = FastParse([]byte(json), SetNquads)
	require.Error(t, err)
}

func TestNquadsFromJsonArray(t *testing.T) {
	json := `[
		{
			"uid": "uid(Project10)",
			"Ticket.row": {
				"uid": "uid(x)"
			}
		},
		{
			"Project.columns": [
				{
					"uid": "uid(x)"
				}
			],
			"uid": "uid(Project3)"
		},
		{
			"Ticket.onColumn": {
				"uid": "uid(x)"
			},
			"uid": "uid(Ticket4)"
		}
	]`

	nqs, err := Parse([]byte(json), SetNquads)
	require.NoError(t, err)
	require.Equal(t, 3, len(nqs))

	nqs, err = FastParse([]byte(json), SetNquads)
	require.NoError(t, err)
	require.Equal(t, 3, len(nqs))
}

func TestNquadsFromJson_NegativeUidError(t *testing.T) {
	json := `{"uid":"-100","name":"Name","following":[{"name":"Bob"}],"school":[{"uid":"","name@en":"Crown Public School"}]}`

	_, err := Parse([]byte(json), SetNquads)
	require.Error(t, err)

	_, err = FastParse([]byte(json), SetNquads)
	require.Error(t, err)
}

func TestNquadsFromJson_EmptyUid(t *testing.T) {
	json := `{"uid":"","name":"Alice","following":[{"name":"Bob"}],"school":[{"uid":"",
"name":"Crown Public School"}]}`
	nq, err := Parse([]byte(json), SetNquads)
	require.NoError(t, err)

	fastNQ, err := FastParse([]byte(json), SetNquads)
	require.NoError(t, err)

	exp := &Experiment{
		t:      t,
		nqs:    nq,
		schema: "name: string @index(exact) .",
		query: `{alice(func: eq(name, "Alice")) {
name
following { name}
school { name}
}}`,
		expected: `{"alice":[{"name":"Alice","following":[{"name":"Bob"}],"school":[{
"name":"Crown Public School"}]}]}`,
	}
	exp.verify()

	exp.nqs = fastNQ
	exp.verify()
}

func TestNquadsFromJson_BlankNodes(t *testing.T) {
	json := `{"uid":"_:alice","name":"Alice","following":[{"name":"Bob"}],"school":[{"uid":"_:school","name":"Crown Public School"}]}`

	nq, err := Parse([]byte(json), SetNquads)
	require.NoError(t, err)

	fastNQ, err := FastParse([]byte(json), SetNquads)
	require.NoError(t, err)

	exp := &Experiment{
		t:      t,
		nqs:    nq,
		schema: "name: string @index(exact) .",
		query: `{alice(func: eq(name, "Alice")) {
name
following { name}
school { name}
}}`,
		expected: `{"alice":[{"name":"Alice","following":[{"name":"Bob"}],"school":[{
"name":"Crown Public School"}]}]}`,
	}
	exp.verify()

	exp.nqs = fastNQ
	exp.verify()
}

func TestNquadsDeleteEdges(t *testing.T) {
	json := `[{"uid": "0x1","name":null,"mobile":null,"car":null}]`
	nq, err := Parse([]byte(json), DeleteNquads)
	require.NoError(t, err)
	require.Equal(t, 3, len(nq))

	fastNQ, err := FastParse([]byte(json), DeleteNquads)
	require.NoError(t, err)
	require.Equal(t, 3, len(fastNQ))
}

func checkCount(t *testing.T, nq []*api.NQuad, pred string, count int) {
	for _, n := range nq {
		if n.Predicate == pred {
			require.Equal(t, count, len(n.Facets))
			break
		}
	}
}

func getMapOfFacets(facets []*api.Facet) map[string]*api.Facet {
	res := make(map[string]*api.Facet)
	for _, f := range facets {
		res[f.Key] = f
	}
	return res
}

func checkFacets(t *testing.T, nq []*api.NQuad, pred string, facets []*api.Facet) {
	for _, n := range nq {
		if n.Predicate == pred {
			require.Equal(t, len(facets), len(n.Facets),
				fmt.Sprintf("expected %d facets, got %d", len(facets), len(n.Facets)))

			expectedFacets := getMapOfFacets(facets)
			actualFacets := getMapOfFacets(n.Facets)
			for key, f := range expectedFacets {
				actualF, ok := actualFacets[key]
				if !ok {
					t.Fatalf("facet for key %s not found", key)
				}
				require.Equal(t, f, actualF, fmt.Sprintf("expected:%v\ngot:%v", f, actualF))
			}
		}
	}
}

func TestNquadsFromJsonFacets1(t *testing.T) {
	// test the 5 data types on facets, string, bool, int, float and datetime
	operation := "READ WRITE"
	operationTokens, err := tok.GetTermTokens([]string{operation})
	require.NoError(t, err, "unable to get tokens from the string %s", operation)

	timeStr := "2006-01-02T15:04:05Z"
	time, err := types.ParseTime(timeStr)
	if err != nil {
		t.Fatalf("unable to convert string %s to time", timeStr)
	}
	timeBinary, err := time.MarshalBinary()
	if err != nil {
		t.Fatalf("unable to marshal time %v to binary", time)
	}

	carPrice := 30000.56
	var priceBytes [8]byte
	u := math.Float64bits(carPrice)
	binary.LittleEndian.PutUint64(priceBytes[:], u)

	carAge := 3
	var ageBytes [8]byte
	binary.LittleEndian.PutUint64(ageBytes[:], uint64(carAge))

	json := fmt.Sprintf(`[{"name":"Alice","mobile":"040123456","car":"MA0123",`+
		`"mobile|operation": "%s",
         "car|first":true,
         "car|age": %d,
         "car|price": %f,
         "car|since": "%s"
}]`, operation, carAge, carPrice, timeStr)

	nq, err := Parse([]byte(json), SetNquads)
	require.NoError(t, err)
	require.Equal(t, 3, len(nq))

	fastNQ, err := FastParse([]byte(json), SetNquads)
	require.NoError(t, err)
	require.Equal(t, 3, len(fastNQ))

	for _, n := range nq {
		glog.Infof("%v", n)
	}

	for _, n := range fastNQ {
		glog.Infof("%v", n)
	}

	checkFacets(t, nq, "mobile", []*api.Facet{
		{
			Key:     "operation",
			Value:   []byte(operation),
			ValType: api.Facet_STRING,
			Tokens:  operationTokens,
		},
	})

	checkFacets(t, fastNQ, "mobile", []*api.Facet{
		{
			Key:     "operation",
			Value:   []byte(operation),
			ValType: api.Facet_STRING,
			Tokens:  operationTokens,
		},
	})

	checkFacets(t, nq, "car", []*api.Facet{
		{
			Key:     "first",
			Value:   []byte{1},
			ValType: api.Facet_BOOL,
		},
		{
			Key:     "age",
			Value:   ageBytes[:],
			ValType: api.Facet_INT,
		},
		{
			Key:     "price",
			Value:   priceBytes[:],
			ValType: api.Facet_FLOAT,
		},
		{
			Key:     "since",
			Value:   timeBinary,
			ValType: api.Facet_DATETIME,
		},
	})

	checkFacets(t, fastNQ, "car", []*api.Facet{
		{
			Key:     "first",
			Value:   []byte{1},
			ValType: api.Facet_BOOL,
		},
		{
			Key:     "age",
			Value:   ageBytes[:],
			ValType: api.Facet_INT,
		},
		{
			Key:     "price",
			Value:   priceBytes[:],
			ValType: api.Facet_FLOAT,
		},
		{
			Key:     "since",
			Value:   timeBinary,
			ValType: api.Facet_DATETIME,
		},
	})
}

func TestNquadsFromJsonFacets2(t *testing.T) {
	// Dave has uid facets which should go on the edge between Alice and Dave
	json := `[{"name":"Alice","friend":[{"name":"Dave","friend|close":"true"}]}]`

	nq, err := Parse([]byte(json), SetNquads)
	require.NoError(t, err)
	require.Equal(t, 3, len(nq))
	checkCount(t, nq, "friend", 1)

	fastNQ, err := FastParse([]byte(json), SetNquads)
	require.NoError(t, err)
	require.Equal(t, 3, len(fastNQ))
	checkCount(t, fastNQ, "friend", 1)
}

// Test valid facets json.
func TestNquadsFromJsonFacets3(t *testing.T) {
	json := `
	[
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
	]`

	nqs, err := Parse([]byte(json), SetNquads)
	require.NoError(t, err)
	require.Equal(t, 4, len(nqs))
	for _, nq := range nqs {
		predVal := nq.ObjectValue.GetStrVal()
		switch predVal {
		case "Alice":
			require.Equal(t, 0, len(nq.Facets))
		case "Joshua":
			require.Equal(t, 1, len(nq.Facets))
		case "David":
			require.Equal(t, 1, len(nq.Facets))
		case "Josh":
			require.Equal(t, 2, len(nq.Facets))
		}
	}

	fastNQ, err := FastParse([]byte(json), SetNquads)
	require.NoError(t, err)
	require.Equal(t, 4, len(fastNQ))
	for _, nq := range fastNQ {
		predVal := nq.ObjectValue.GetStrVal()
		switch predVal {
		case "Alice":
			require.Equal(t, 0, len(nq.Facets))
		case "Joshua":
			require.Equal(t, 1, len(nq.Facets))
		case "David":
			require.Equal(t, 1, len(nq.Facets))
		case "Josh":
			require.Equal(t, 2, len(nq.Facets))
		}
	}
}

// Test invalid facet format with scalar list predicate.
func TestNquadsFromJsonFacets4(t *testing.T) {
	type input struct {
		Name     string
		ErrorOut bool
		Json     string
	}

	inputs := []input{
		{
			"facets_should_be_map",
			true,
			`
			[
				{
					"name":"Alice",
					"friend": ["Joshua", "David", "Josh"],
					"friend|age": 20
				}
			]`,
		},
		{
			"predicate_should_be_list",
			true,
			`
			[
				{
					"name":"Alice",
					"friend": "Joshua",
					"friend|age": {
						"0": 20
					}
				}
			]`,
		},
		{
			"only_scalar_values_in_facet_map",
			true,
			`
			[
				{
					"name":"Alice",
					"friend": ["Joshua"],
					"friend|age": {
						"0": {
							"1": 20
						}
					}
				}
			]`,
		},
		{
			"invalid_key_in_facet_map",
			true,
			`
			[
				{
					"name":"Alice",
					"friend": ["Joshua"],
					"friend|age": {
						"a": 20
					}
				}
			]`,
		},
		{
			// Facets will be ignored here.
			"predicate_is_null",
			false,
			`
			[
				{
					"name":"Alice",
					"friend": null,
					"friend|age": {
						"0": 20
					}
				}
			]`,
		},
		{
			// Facets will be ignored here.
			"empty_scalar_list",
			false,
			`
			[
				{
					"name":"Alice",
					"friend": [],
					"friend|age": {
						"0": 20
					}
				}
			]`,
		},
		{
			"facet_map_is_null",
			false,
			`
			[
				{
					"name":"Alice",
					"friend": ["Joshua"],
					"friend|age": null
				}
			]`,
		},
		{
			"facet_vales_should_not_be_list",
			true,
			`
			[
				{
					"name":"Alice",
					"friend": ["Joshua", "David", "Josh"],
					"friend|age": ["20"]
				}
			]`,
		},
		{
			// Facets with higher index will be ignored.
			"facet_map_with_index_greater_than_scalarlist_length",
			false,
			`
			[
				{
					"name":"Alice",
					"friend": ["Joshua", "David", "Josh"],
					"friend|age": {
						"100": 30,
						"20": 28
					}
				}
			]`,
		},
	}

	for _, input := range inputs {
		_, err := Parse([]byte(input.Json), SetNquads)
		if input.ErrorOut {
			require.Error(t, err, "TestNquadsFromJsonFacets4-%s", input.Name)
		} else {
			require.NoError(t, err, "TestNquadsFromJsonFacets4-%s", input.Name)
		}

		_, err = FastParse([]byte(input.Json), SetNquads)
		if input.ErrorOut {
			require.Error(t, err, "TestNquadsFromJsonFacets4-%s", input.Name)
		} else {
			require.NoError(t, err, "TestNquadsFromJsonFacets4-%s", input.Name)
		}
	}
}

func TestNquadsFromJsonFacets5(t *testing.T) {
	// Dave has uid facets which should go on the edge between Alice and Dave,
	// AND Emily has uid facets which should go on the edge between Dave and Emily
	json := `[
		{
			"name": "Alice",
			"friend": [
				{
					"name": "Dave",
					"friend|close": true,
					"friend": [
						{
							"name": "Emily",
							"friend|close": true
						}
					]
				}
			]
		}
	]`

	nq, err := Parse([]byte(json), SetNquads)
	require.NoError(t, err)
	require.Equal(t, 5, len(nq))
	checkCount(t, nq, "friend", 1)

	fastNQ, err := FastParse([]byte(json), SetNquads)
	require.NoError(t, err)
	require.Equal(t, 5, len(fastNQ))
	checkCount(t, fastNQ, "friend", 1)
}

func TestNquadsFromJsonError1(t *testing.T) {
	p := Person{
		Name: "Alice",
		School: &School{
			Name: "Wellington Public School",
		},
	}

	b, err := json.Marshal(p)
	require.NoError(t, err)

	_, err = Parse(b, DeleteNquads)
	require.Error(t, err)
	require.ErrorIs(t, err, errEmptyUID)

	_, err = FastParse(b, DeleteNquads)
	require.Error(t, err)
	require.ErrorIs(t, err, errEmptyUID)
}

func TestNquadsFromJsonList(t *testing.T) {
	json := `{"address":["Riley Street","Redfern"],"phone_number":[123,9876],"points":[{"type":"Point", "coordinates":[1.1,2.0]},{"type":"Point", "coordinates":[2.0,1.1]}]}`

	nq, err := Parse([]byte(json), SetNquads)
	require.NoError(t, err)
	require.Equal(t, 6, len(nq))

	fastNQ, err := FastParse([]byte(json), SetNquads)
	require.NoError(t, err)
	require.Equal(t, 6, len(fastNQ))
}

func TestNquadsFromJsonDelete(t *testing.T) {
	json := `{"uid":1000,"friend":[{"uid":1001}]}`

	nq, err := Parse([]byte(json), DeleteNquads)
	require.NoError(t, err)
	require.Equal(t, nq[0], makeNquadEdge("1000", "friend", "1001"))

	fastNQ, err := FastParse([]byte(json), DeleteNquads)
	require.NoError(t, err)
	require.Equal(t, fastNQ[0], makeNquadEdge("1000", "friend", "1001"))
}

func TestNquadsFromJsonDeleteStar(t *testing.T) {
	json := `{"uid":1000,"name": null}`

	nq, err := Parse([]byte(json), DeleteNquads)
	require.NoError(t, err)

	fastNQ, err := FastParse([]byte(json), DeleteNquads)
	require.NoError(t, err)

	expected := &api.NQuad{
		Subject:   "1000",
		Predicate: "name",
		ObjectValue: &api.Value{
			Val: &api.Value_DefaultVal{
				DefaultVal: "_STAR_ALL",
			},
		},
	}

	require.Equal(t, expected, nq[0])
	require.Equal(t, expected, fastNQ[0])
}

func TestValInUpsert(t *testing.T) {
	json := `{"uid":1000, "name": "val(name)"}`
	nq, err := Parse([]byte(json), SetNquads)
	require.NoError(t, err)

	fastNQ, err := FastParse([]byte(json), SetNquads)
	require.NoError(t, err)

	expected := &api.NQuad{
		Subject:   "1000",
		Predicate: "name",
		ObjectId:  "val(name)",
	}

	require.Equal(t, expected, nq[0])
	require.Equal(t, expected, fastNQ[0])
}

func TestNquadsFromJsonDeleteStarLang(t *testing.T) {
	json := `{"uid":1000,"name@es": null}`

	nq, err := Parse([]byte(json), DeleteNquads)
	require.NoError(t, err)

	fastNQ, err := FastParse([]byte(json), DeleteNquads)
	require.NoError(t, err)

	expected := &api.NQuad{
		Subject:   "1000",
		Predicate: "name",
		ObjectValue: &api.Value{
			Val: &api.Value_DefaultVal{
				DefaultVal: "_STAR_ALL",
			},
		},
		Lang: "es",
	}

	require.Equal(t, expected, nq[0])
	require.Equal(t, expected, fastNQ[0])
}

func TestSetNquadNilValue(t *testing.T) {
	json := `{"uid":1000,"name": null}`

	nq, err := Parse([]byte(json), SetNquads)
	require.NoError(t, err)
	require.Equal(t, 0, len(nq))

	fastNQ, err := FastParse([]byte(json), SetNquads)
	require.NoError(t, err)
	require.Equal(t, 0, len(fastNQ))
}

// See PR #7737 to understand why this test exists.
func TestNquadsFromJsonEmptyFacet(t *testing.T) {
	json := `{"uid":1000,"doesnt|exist":null}`

	// fast
	buf := NewNQuadBuffer(-1)
	require.Nil(t, buf.FastParseJSON([]byte(json), DeleteNquads))
	buf.Flush()
	// needs to be empty, otherwise node gets deleted
	require.Equal(t, 0, len(<-buf.Ch()))

	// old
	buf = NewNQuadBuffer(-1)
	require.Nil(t, buf.ParseJSON([]byte(json), DeleteNquads))
	buf.Flush()
	// needs to be empty, otherwise node gets deleted
	require.Equal(t, 0, len(<-buf.Ch()))
}

func BenchmarkNoFacets(b *testing.B) {
	json := []byte(`[
	{
		"uid":123,
		"flguid":123,
		"is_validate":"xxxxxxxxxx",
		"createDatetime":"xxxxxxxxxx",
		"contains":{
			"createDatetime":"xxxxxxxxxx",
			"final_individ":"xxxxxxxxxx",
			"cm_bad_debt":"xxxxxxxxxx",
			"cm_bill_address1":"xxxxxxxxxx",
			"cm_bill_address2":"xxxxxxxxxx",
			"cm_bill_city":"xxxxxxxxxx",
			"cm_bill_state":"xxxxxxxxxx",
			"cm_zip":"xxxxxxxxxx",
			"zip5":"xxxxxxxxxx",
			"cm_customer_id":"xxxxxxxxxx",
			"final_gaid":"xxxxxxxxxx",
			"final_hholdid":"xxxxxxxxxx",
			"final_firstname":"xxxxxxxxxx",
			"final_middlename":"xxxxxxxxxx",
			"final_surname":"xxxxxxxxxx",
			"final_gender":"xxxxxxxxxx",
			"final_ace_prim_addr":"xxxxxxxxxx",
			"final_ace_sec_addr":"xxxxxxxxxx",
			"final_ace_urb":"xxxxxxxxxx",
			"final_ace_city_llidx":"xxxxxxxxxx",
			"final_ace_state":"xxxxxxxxxx",
			"final_ace_postal_code":"xxxxxxxxxx",
			"final_ace_zip4":"xxxxxxxxxx",
			"final_ace_dpbc":"xxxxxxxxxx",
			"final_ace_checkdigit":"xxxxxxxxxx",
			"final_ace_iso_code":"xxxxxxxxxx",
			"final_ace_cart":"xxxxxxxxxx",
			"final_ace_lot":"xxxxxxxxxx",
			"final_ace_lot_order":"xxxxxxxxxx",
			"final_ace_rec_type":"xxxxxxxxxx",
			"final_ace_remainder":"xxxxxxxxxx",
			"final_ace_dpv_cmra":"xxxxxxxxxx",
			"final_ace_dpv_ftnote":"xxxxxxxxxx",
			"final_ace_dpv_status":"xxxxxxxxxx",
			"final_ace_foreigncode":"xxxxxxxxxx",
			"final_ace_match_5":"xxxxxxxxxx",
			"final_ace_match_9":"xxxxxxxxxx",
			"final_ace_match_un":"xxxxxxxxxx",
			"final_ace_zip_move":"xxxxxxxxxx",
			"final_ace_ziptype":"xxxxxxxxxx",
			"final_ace_congress":"xxxxxxxxxx",
			"final_ace_county":"xxxxxxxxxx",
			"final_ace_countyname":"xxxxxxxxxx",
			"final_ace_factype":"xxxxxxxxxx",
			"final_ace_fipscode":"xxxxxxxxxx",
			"final_ace_error_code":"xxxxxxxxxx",
			"final_ace_stat_code":"xxxxxxxxxx",
			"final_ace_geo_match":"xxxxxxxxxx",
			"final_ace_geo_lat":"xxxxxxxxxx",
			"final_ace_geo_lng":"xxxxxxxxxx",
			"final_ace_ageo_pla":"xxxxxxxxxx",
			"final_ace_geo_blk":"xxxxxxxxxx",
			"final_ace_ageo_mcd":"xxxxxxxxxx",
			"final_ace_cgeo_cbsa":"xxxxxxxxxx",
			"final_ace_cgeo_msa":"xxxxxxxxxx",
			"final_ace_ap_lacscode":"xxxxxxxxxx",
			"final_dsf_businessflag":"xxxxxxxxxx",
			"final_dsf_dropflag":"xxxxxxxxxx",
			"final_dsf_throwbackflag":"xxxxxxxxxx",
			"final_dsf_seasonalflag":"xxxxxxxxxx",
			"final_dsf_vacantflag":"xxxxxxxxxx",
			"final_dsf_deliverytype":"xxxxxxxxxx",
			"final_dsf_dt_curbflag":"xxxxxxxxxx",
			"final_dsf_dt_ndcbuflag":"xxxxxxxxxx",
			"final_dsf_dt_centralflag":"xxxxxxxxxx",
			"final_dsf_dt_doorslotflag":"xxxxxxxxxx",
			"final_dsf_dropcount":"xxxxxxxxxx",
			"final_dsf_nostatflag":"xxxxxxxxxx",
			"final_dsf_educationalflag":"xxxxxxxxxx",
			"final_dsf_rectyp":"xxxxxxxxxx",
			"final_mailability_score":"xxxxxxxxxx",
			"final_occupancy_score":"xxxxxxxxxx",
			"final_multi_type":"xxxxxxxxxx",
			"final_deceased_flag":"xxxxxxxxxx",
			"final_dnm_flag":"xxxxxxxxxx",
			"final_dnc_flag":"xxxxxxxxxx",
			"final_dnf_flag":"xxxxxxxxxx",
			"final_prison_flag":"xxxxxxxxxx",
			"final_nursing_home_flag":"xxxxxxxxxx",
			"final_date_of_birth":"xxxxxxxxxx",
			"final_date_of_death":"xxxxxxxxxx",
			"vip_number":"xxxxxxxxxx",
			"vip_store_no":"xxxxxxxxxx",
			"vip_division":"xxxxxxxxxx",
			"vip_phone_number":"xxxxxxxxxx",
			"vip_email_address":"xxxxxxxxxx",
			"vip_first_name":"xxxxxxxxxx",
			"vip_last_name":"xxxxxxxxxx",
			"vip_gender":"xxxxxxxxxx",
			"vip_status":"xxxxxxxxxx",
			"vip_membership_date":"xxxxxxxxxx",
			"vip_expiration_date":"xxxxxxxxxx",
			"cm_date_addr_chng":"xxxxxxxxxx",
			"cm_date_entered":"xxxxxxxxxx",
			"cm_name":"xxxxxxxxxx",
			"cm_opt_on_acct":"xxxxxxxxxx",
			"cm_origin":"xxxxxxxxxx",
			"cm_orig_acq_source":"xxxxxxxxxx",
			"cm_phone_number":"xxxxxxxxxx",
			"cm_phone_number2":"xxxxxxxxxx",
			"cm_problem_cust":"xxxxxxxxxx",
			"cm_rm_list":"xxxxxxxxxx",
			"cm_rm_rented_list":"xxxxxxxxxx",
			"cm_tax_code":"xxxxxxxxxx",
			"email_address":"xxxxxxxxxx",
			"esp_email_id":"xxxxxxxxxx",
			"esp_sub_date":"xxxxxxxxxx",
			"esp_unsub_date":"xxxxxxxxxx",
			"cm_user_def_1":"xxxxxxxxxx",
			"cm_user_def_7":"xxxxxxxxxx",
			"do_not_phone":"xxxxxxxxxx",
			"company_num":"xxxxxxxxxx",
			"customer_id":"xxxxxxxxxx",
			"load_date":"xxxxxxxxxx",
			"activity_date":"xxxxxxxxxx",
			"email_address_hashed":"xxxxxxxxxx",
			"event_id":"",
			"contains":{
				"uid": 123,
				"flguid": 123,
				"is_validate":"xxxxxxxxxx",
				"createDatetime":"xxxxxxxxxx"
			}
		}
	}]`)

	// we're parsing 125 nquads at a time, so the MB/s == MNquads/s
	b.SetBytes(125)
	for n := 0; n < b.N; n++ {
		_, _ = Parse(json, SetNquads)
	}
}

func BenchmarkNoFacetsFast(b *testing.B) {
	json := []byte(`[
	{
		"uid":123,
		"flguid":123,
		"is_validate":"xxxxxxxxxx",
		"createDatetime":"xxxxxxxxxx",
		"contains":{
			"createDatetime":"xxxxxxxxxx",
			"final_individ":"xxxxxxxxxx",
			"cm_bad_debt":"xxxxxxxxxx",
			"cm_bill_address1":"xxxxxxxxxx",
			"cm_bill_address2":"xxxxxxxxxx",
			"cm_bill_city":"xxxxxxxxxx",
			"cm_bill_state":"xxxxxxxxxx",
			"cm_zip":"xxxxxxxxxx",
			"zip5":"xxxxxxxxxx",
			"cm_customer_id":"xxxxxxxxxx",
			"final_gaid":"xxxxxxxxxx",
			"final_hholdid":"xxxxxxxxxx",
			"final_firstname":"xxxxxxxxxx",
			"final_middlename":"xxxxxxxxxx",
			"final_surname":"xxxxxxxxxx",
			"final_gender":"xxxxxxxxxx",
			"final_ace_prim_addr":"xxxxxxxxxx",
			"final_ace_sec_addr":"xxxxxxxxxx",
			"final_ace_urb":"xxxxxxxxxx",
			"final_ace_city_llidx":"xxxxxxxxxx",
			"final_ace_state":"xxxxxxxxxx",
			"final_ace_postal_code":"xxxxxxxxxx",
			"final_ace_zip4":"xxxxxxxxxx",
			"final_ace_dpbc":"xxxxxxxxxx",
			"final_ace_checkdigit":"xxxxxxxxxx",
			"final_ace_iso_code":"xxxxxxxxxx",
			"final_ace_cart":"xxxxxxxxxx",
			"final_ace_lot":"xxxxxxxxxx",
			"final_ace_lot_order":"xxxxxxxxxx",
			"final_ace_rec_type":"xxxxxxxxxx",
			"final_ace_remainder":"xxxxxxxxxx",
			"final_ace_dpv_cmra":"xxxxxxxxxx",
			"final_ace_dpv_ftnote":"xxxxxxxxxx",
			"final_ace_dpv_status":"xxxxxxxxxx",
			"final_ace_foreigncode":"xxxxxxxxxx",
			"final_ace_match_5":"xxxxxxxxxx",
			"final_ace_match_9":"xxxxxxxxxx",
			"final_ace_match_un":"xxxxxxxxxx",
			"final_ace_zip_move":"xxxxxxxxxx",
			"final_ace_ziptype":"xxxxxxxxxx",
			"final_ace_congress":"xxxxxxxxxx",
			"final_ace_county":"xxxxxxxxxx",
			"final_ace_countyname":"xxxxxxxxxx",
			"final_ace_factype":"xxxxxxxxxx",
			"final_ace_fipscode":"xxxxxxxxxx",
			"final_ace_error_code":"xxxxxxxxxx",
			"final_ace_stat_code":"xxxxxxxxxx",
			"final_ace_geo_match":"xxxxxxxxxx",
			"final_ace_geo_lat":"xxxxxxxxxx",
			"final_ace_geo_lng":"xxxxxxxxxx",
			"final_ace_ageo_pla":"xxxxxxxxxx",
			"final_ace_geo_blk":"xxxxxxxxxx",
			"final_ace_ageo_mcd":"xxxxxxxxxx",
			"final_ace_cgeo_cbsa":"xxxxxxxxxx",
			"final_ace_cgeo_msa":"xxxxxxxxxx",
			"final_ace_ap_lacscode":"xxxxxxxxxx",
			"final_dsf_businessflag":"xxxxxxxxxx",
			"final_dsf_dropflag":"xxxxxxxxxx",
			"final_dsf_throwbackflag":"xxxxxxxxxx",
			"final_dsf_seasonalflag":"xxxxxxxxxx",
			"final_dsf_vacantflag":"xxxxxxxxxx",
			"final_dsf_deliverytype":"xxxxxxxxxx",
			"final_dsf_dt_curbflag":"xxxxxxxxxx",
			"final_dsf_dt_ndcbuflag":"xxxxxxxxxx",
			"final_dsf_dt_centralflag":"xxxxxxxxxx",
			"final_dsf_dt_doorslotflag":"xxxxxxxxxx",
			"final_dsf_dropcount":"xxxxxxxxxx",
			"final_dsf_nostatflag":"xxxxxxxxxx",
			"final_dsf_educationalflag":"xxxxxxxxxx",
			"final_dsf_rectyp":"xxxxxxxxxx",
			"final_mailability_score":"xxxxxxxxxx",
			"final_occupancy_score":"xxxxxxxxxx",
			"final_multi_type":"xxxxxxxxxx",
			"final_deceased_flag":"xxxxxxxxxx",
			"final_dnm_flag":"xxxxxxxxxx",
			"final_dnc_flag":"xxxxxxxxxx",
			"final_dnf_flag":"xxxxxxxxxx",
			"final_prison_flag":"xxxxxxxxxx",
			"final_nursing_home_flag":"xxxxxxxxxx",
			"final_date_of_birth":"xxxxxxxxxx",
			"final_date_of_death":"xxxxxxxxxx",
			"vip_number":"xxxxxxxxxx",
			"vip_store_no":"xxxxxxxxxx",
			"vip_division":"xxxxxxxxxx",
			"vip_phone_number":"xxxxxxxxxx",
			"vip_email_address":"xxxxxxxxxx",
			"vip_first_name":"xxxxxxxxxx",
			"vip_last_name":"xxxxxxxxxx",
			"vip_gender":"xxxxxxxxxx",
			"vip_status":"xxxxxxxxxx",
			"vip_membership_date":"xxxxxxxxxx",
			"vip_expiration_date":"xxxxxxxxxx",
			"cm_date_addr_chng":"xxxxxxxxxx",
			"cm_date_entered":"xxxxxxxxxx",
			"cm_name":"xxxxxxxxxx",
			"cm_opt_on_acct":"xxxxxxxxxx",
			"cm_origin":"xxxxxxxxxx",
			"cm_orig_acq_source":"xxxxxxxxxx",
			"cm_phone_number":"xxxxxxxxxx",
			"cm_phone_number2":"xxxxxxxxxx",
			"cm_problem_cust":"xxxxxxxxxx",
			"cm_rm_list":"xxxxxxxxxx",
			"cm_rm_rented_list":"xxxxxxxxxx",
			"cm_tax_code":"xxxxxxxxxx",
			"email_address":"xxxxxxxxxx",
			"esp_email_id":"xxxxxxxxxx",
			"esp_sub_date":"xxxxxxxxxx",
			"esp_unsub_date":"xxxxxxxxxx",
			"cm_user_def_1":"xxxxxxxxxx",
			"cm_user_def_7":"xxxxxxxxxx",
			"do_not_phone":"xxxxxxxxxx",
			"company_num":"xxxxxxxxxx",
			"customer_id":"xxxxxxxxxx",
			"load_date":"xxxxxxxxxx",
			"activity_date":"xxxxxxxxxx",
			"email_address_hashed":"xxxxxxxxxx",
			"event_id":"",
			"contains":{
				"uid": 123,
				"flguid": 123,
				"is_validate":"xxxxxxxxxx",
				"createDatetime":"xxxxxxxxxx"
			}
		}
	}]`)

	// we're parsing 125 nquads at a time, so the MB/s == MNquads/s
	b.SetBytes(125)
	for n := 0; n < b.N; n++ {
		_, _ = FastParse(json, SetNquads)
	}
}

func TestNquadsEmptyStringFromJson(t *testing.T) {
	json := `[{"name":""}]`

	nq, err := Parse([]byte(json), SetNquads)
	require.NoError(t, err)

	fastNQ, err := FastParse([]byte(json), SetNquads)
	require.NoError(t, err)

	// The string value should be empty.
	require.Equal(t, nq[0].ObjectValue.GetStrVal(), "")
	require.Equal(t, fastNQ[0].ObjectValue.GetStrVal(), "")
}

func TestNquadsJsonEmptyStringVectorPred(t *testing.T) {
	p := Product{
		Uid:           "1",
		Name:          "",
		Description_v: "",
	}

	b, err := json.Marshal([]Product{p})
	require.NoError(t, err)

	nq, err := Parse(b, SetNquads)
	require.NoError(t, err)
	require.Equal(t, 3, len(nq))

	fastNQ, err := FastParse(b, SetNquads)
	require.NoError(t, err)
	require.Equal(t, 3, len(fastNQ))

	// predicate Name should be empty and edge for Description_v should not be there
	// we do not create edge for "" in float32vector.
	exp := &Experiment{
		t:   t,
		nqs: nq,
		schema: `name: string @index(exact) .
		description_v: float32vector .`,
		query: `{product(func: uid(1)) {
			name
			description_v
		}}`,
		expected: `{"product":[{
			"name":""}]}`,
	}
	exp.verify()

	exp.nqs = fastNQ
	exp.verify()
}

func TestNquadsJsonEmptySquareBracketVectorPred(t *testing.T) {
	p := Product{
		Name:          "ipad",
		Description_v: "[]",
	}

	b, err := json.Marshal(p)
	require.NoError(t, err)

	nq, err := Parse(b, SetNquads)
	require.NoError(t, err)
	require.Equal(t, 3, len(nq))

	fastNQ, err := FastParse(b, SetNquads)
	require.NoError(t, err)
	require.Equal(t, 3, len(fastNQ))

	// predicate Name should have value "ipad" and edge for Description_v should not be there
	// we do not create edge for [] in float32vector.
	exp := &Experiment{
		t:   t,
		nqs: nq,
		schema: `name: string @index(exact) .
		description_v: float32vector .`,
		query: `{product(func: eq(name, "ipad")) {
			name
			description_v
		}}`,
		expected: `{"product":[{
			"name":"ipad"}]}`,
	}
	exp.verify()

	exp.nqs = fastNQ
	exp.verify()
}

func TestNquadsJsonValidVector(t *testing.T) {
	p := Product{
		Name:          "ipad",
		Description_v: "[1.1, 2.2, 3.3]",
	}

	b, err := json.Marshal(p)
	require.NoError(t, err)

	nq, err := Parse(b, SetNquads)
	require.NoError(t, err)
	require.Equal(t, 3, len(nq))

	fastNQ, err := FastParse(b, SetNquads)
	require.NoError(t, err)
	require.Equal(t, 3, len(fastNQ))

	exp := &Experiment{
		t:   t,
		nqs: nq,
		schema: `name: string @index(exact) .
		description_v: float32vector .`,
		query: `{product(func: eq(name, "ipad")) {
			name
			description_v
		}}`,
		expected: `{"product":[{
			"name":"ipad",
			"description_v":[1.1, 2.2, 3.3]}]}`,
	}
	exp.verify()

	exp.nqs = fastNQ
	exp.verify()
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
	code := m.Run()
	if code != 0 {
		c.Cleanup(true)
	} else {
		c.Cleanup(false)
	}
	os.Exit(code)
}
