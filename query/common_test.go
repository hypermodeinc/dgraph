//go:build integration || cloud || upgrade

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

//nolint:lll
package query

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/dgraphapi"
	"github.com/hypermodeinc/dgraph/v25/dgraphtest"
	"github.com/hypermodeinc/dgraph/v25/x"
)

func setSchema(schema string) {
	var err error
	for retry := 0; retry < 60; retry++ {
		err = client.Alter(context.Background(), &api.Operation{Schema: schema})
		if err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	panic(fmt.Sprintf("Could not alter schema. Got error %v", err.Error()))
}

func dropPredicate(pred string) {
	err := client.Alter(context.Background(), &api.Operation{
		DropAttr: pred,
	})
	if err != nil {
		panic(fmt.Sprintf("Could not drop predicate. Got error %v", err.Error()))
	}
}

func processQuery(ctx context.Context, t *testing.T, query string) (string, error) {
	txn := client.NewTxn()
	defer func() {
		if err := txn.Discard(ctx); err != nil {
			t.Logf("error discarding txn: %v", err)
		}
	}()

	res, err := txn.Query(ctx, query)
	if err != nil {
		return "", err
	}

	response := map[string]interface{}{}
	response["data"] = json.RawMessage(string(res.Json))

	jsonResponse, err := json.Marshal(response)
	require.NoError(t, err)
	return string(jsonResponse), err
}

func processQueryRDF(ctx context.Context, query string) (string, error) {
	txn := client.NewTxn()
	defer func() { _ = txn.Discard(ctx) }()

	res, err := txn.Do(ctx, &api.Request{
		Query:      query,
		RespFormat: api.Request_RDF,
	})
	if err != nil {
		return "", err
	}
	return string(res.Rdf), err
}

func processQueryNoErr(t *testing.T, query string) string {
	res, err := processQuery(context.Background(), t, query)
	require.NoError(t, err)
	return res
}

// processQueryForMetrics works like processQuery but returns metrics instead of response.
func processQueryForMetrics(t *testing.T, query string) *api.Metrics {
	txn := client.NewTxn()
	defer func() { _ = txn.Discard(context.Background()) }()

	res, err := txn.Query(context.Background(), query)
	require.NoError(t, err)
	return res.Metrics
}

func processQueryWithVars(t *testing.T, query string,
	vars map[string]string) (string, error) {
	txn := client.NewTxn()
	defer func() { _ = txn.Discard(context.Background()) }()

	res, err := txn.QueryWithVars(context.Background(), query, vars)
	if err != nil {
		return "", err
	}

	response := map[string]interface{}{}
	response["data"] = json.RawMessage(string(res.Json))

	jsonResponse, err := json.Marshal(response)
	require.NoError(t, err)
	return string(jsonResponse), err
}

func addTriplesToCluster(triples string) error {
	txn := client.NewTxn()
	ctx := context.Background()
	defer func() { _ = txn.Discard(ctx) }()

	_, err := txn.Mutate(ctx, &api.Mutation{
		SetNquads: []byte(triples),
		CommitNow: true,
	})
	return err
}

func deleteTriplesInCluster(triples string) {
	txn := client.NewTxn()
	ctx := context.Background()
	defer func() { _ = txn.Discard(ctx) }()

	_, err := txn.Mutate(ctx, &api.Mutation{
		DelNquads: []byte(triples),
		CommitNow: true,
	})
	if err != nil {
		panic(fmt.Sprintf("Could not delete triples. Got error %v", err.Error()))
	}
}

func addGeoPointToCluster(uid uint64, pred string, point []float64) error {
	triple := fmt.Sprintf(
		`<%d> <%s> "{'type':'Point', 'coordinates':[%v, %v]}"^^<geo:geojson> .`,
		uid, pred, point[0], point[1])
	return addTriplesToCluster(triple)
}

func addGeoPolygonToCluster(uid uint64, pred string, polygon [][][]float64) error {
	coordinates := "["
	for i, ring := range polygon {
		coordinates += "["
		for j, point := range ring {
			coordinates += fmt.Sprintf("[%v, %v]", point[0], point[1])

			if j != len(ring)-1 {
				coordinates += ","
			}
		}

		coordinates += "]"
		if i != len(polygon)-1 {
			coordinates += ","
		}
	}
	coordinates += "]"

	triple := fmt.Sprintf(
		`<%d> <%s> "{'type':'Polygon', 'coordinates': %s}"^^<geo:geojson> .`,
		uid, pred, coordinates)
	return addTriplesToCluster(triple)
}

func addGeoMultiPolygonToCluster(uid uint64, polygons [][][][]float64) error {
	coordinates := "["
	for i, polygon := range polygons {
		coordinates += "["
		for j, ring := range polygon {
			coordinates += "["
			for k, point := range ring {
				coordinates += fmt.Sprintf("[%v, %v]", point[0], point[1])

				if k != len(ring)-1 {
					coordinates += ","
				}
			}

			coordinates += "]"
			if j != len(polygon)-1 {
				coordinates += ","
			}
		}

		coordinates += "]"
		if i != len(polygons)-1 {
			coordinates += ","
		}
	}
	coordinates += "]"

	triple := fmt.Sprintf(
		`<%d> <geometry> "{'type':'MultiPolygon', 'coordinates': %s}"^^<geo:geojson> .`,
		uid, coordinates)
	return addTriplesToCluster(triple)
}

var testSchema = `
type Person {
	name
	pet
	friend
	gender
	alive
}

type Person2 {
	name2
	age2
}

type Animal {
	name
}

type CarModel {
	make
	model
	year
	previous_model
	<~previous_model>
}

type Object {
	name
	owner
}

type SchoolInfo {
	name
	abbr
	school
	district
	state
	county
}

type Node {
	node
	name
}

type Speaker {
	name
	language
}

type User {
	name
	password
	gender
	friend
	alive
}

name                           : string @index(term, exact, trigram) @count @lang .
name_lang                      : string @lang .
lang_type                      : string @index(exact) .
name_lang_index                : string @index(exact) @lang .
alt_name                       : [string] @index(term, exact, trigram) @count .
alias                          : string @index(exact, term, fulltext) .
alias_lang                     : string @index(exact) @lang .
abbr                           : string .
dob                            : dateTime @index(year) .
dob_day                        : dateTime @index(day) .
film.film.initial_release_date : dateTime @index(year) .
loc                            : geo @index(geo) .
genre                          : [uid] @reverse .
survival_rate                  : float .
alive                          : bool @index(bool) .
age                            : int @index(int) .
shadow_deep                    : int .
friend                         : [uid] @reverse @count .
geometry                       : geo @index(geo) .
value                          : string @index(trigram) .
full_name                      : string @index(hash) .
nick_name                      : string @index(term) .
pet_name                       : [string] @index(exact) .
royal_title                    : string @index(hash, term, fulltext) @lang .
school                         : [uid] @count .
lossy                          : string @index(term) @lang .
occupations                    : [string] @index(term) .
graduation                     : [dateTime] @index(year) @count .
salary                         : float @index(float) .
password                       : password .
pass                           : password .
symbol                         : string @index(exact) .
room                           : string @index(term) .
office.room                    : [uid] .
best_friend                    : uid @reverse .
pet                            : [uid] .
node                           : [uid] .
model                          : string @index(term) @lang .
make                           : string @index(term) .
year                           : int .
previous_model                 : uid @reverse .
created_at                     : datetime @index(hour) .
updated_at                     : datetime @index(year) .
number                         : int @index(int) .
district                       : [uid] .
state                          : [uid] .
county                         : [uid] .
firstName                      : string .
lastName                       : string .
newname                        : string @index(exact, term) .
newage                         : int .
boss                           : uid .
newfriend                      : [uid] .
owner                          : [uid] .
noconflict_pred                : string @noconflict .
noindex_name                   : string .
noindex_age                    : int .
noindex_dob                    : datetime .
noindex_alive                  : bool .
noindex_salary                 : float .
language                       : [string] .
score                          : [int] @index(int) .
average                        : [float] @index(float) .
gender                         : string .
indexpred                      : string @index(exact) .
pred                           : string .
pname                          : string .
tweet-a                        : string @index(trigram) .
tweet-b                        : string @index(term) .
tweet-c                        : string @index(fulltext) .
tweet-d                        : string @index(trigram) .
name2                          : string @index(term)  .
age2                           : int @index(int) .

DispatchBoard.column: uid @reverse .
order: int .

type DispatchBoardColumn {
  name
}

type DispatchBoardCard {
  DispatchBoard.column
  order
}

`

const ngramVersionHash = "d7dfe3b4282fa3543e811c5538f86d39268961ba"

func populateCluster(dc dgraphapi.Cluster) {
	x.Panic(client.Alter(context.Background(), &api.Operation{DropAll: true}))

	// In the query package, we test using hard coded UIDs so that we know what results
	// to expect. We need to move the max assigned UID in zero to higher value than
	// all the UIDs we are using during the tests.
	x.Panic(dc.AssignUids(client.Dgraph, 65536))

	// higher, err := dgraphtest.IsHigherVersion(dc.GetVersion(), "160a0faa5fc6233fdc5a4caa4a7a3d1591f460d0")
	// x.Panic(err)
	// var ts string
	// if higher {
	// 	ts = testSchema + `type User {
	// 		name
	// 		password
	// 		gender
	// 		friend
	// 		alive
	// 		user_profile
	// 	}
	// 	user_profile                   : float32vector @index(hnsw(metric:"euclidean")) .`
	// } else {
	// 	ts = testSchema + `type User {
	// 		name
	// 		password
	// 		gender
	// 		friend
	// 		alive
	// 	}`
	// }

	// Ngram indexing handling
	ngramSupport, err := dgraphtest.IsHigherVersion(dc.GetVersion(), ngramVersionHash)
	x.Panic(err)
	if ngramSupport {
		testSchema += "\ndescription: string @index(ngram) ."
	}

	setSchema(testSchema)

	err = addTriplesToCluster(`
		<1> <name> "Michonne" .
		<2> <name> "King Lear" .
		<3> <name> "Margaret" .
		<4> <name> "Leonard" .
		<5> <name> "Garfield" .
		<6> <name> "Bear" .
		<7> <name> "Nemo" .
		<11> <name> "name" .
		<23> <name> "Rick Grimes" .
		<24> <name> "Glenn Rhee" .
		<25> <name> "Daryl Dixon" .
		<31> <name> "Andrea" .
		<33> <name> "San Mateo High School" .
		<34> <name> "San Mateo School District" .
		<35> <name> "San Mateo County" .
		<36> <name> "California" .
		<110> <name> "Alice" .
		<240> <name> "Andrea With no friends" .
		<1000> <name> "Alice" .
		<1001> <name> "Bob" .
		<1002> <name> "Matt" .
		<1003> <name> "John" .
		<2300> <name> "Andre" .
		<2301> <name> "Alice\"" .
		<2333> <name> "Helmut" .
		<3500> <name> "" .
		<3500> <name> "상현"@ko .
		<3501> <name> "Alex" .
		<3501> <name> "Alex"@en .
		<3502> <name> "" .
		<3502> <name> "Amit"@en .
		<3502> <name> "अमित"@hi .
		<3503> <name> "Andrew"@en .
		<3503> <name> ""@hi .
		<4097> <name> "Badger" .
		<4097> <name> "European badger"@en .
		<4097> <name> "European badger barger European"@xx .
		<4097> <name> "Borsuk europejski"@pl .
		<4097> <name> "Europäischer Dachs"@de .
		<4097> <name> "Барсук"@ru .
		<4097> <name> "Blaireau européen"@fr .
		<4098> <name> "Honey badger"@en .
		<4099> <name> "Honey bee"@en .
		<4100> <name> "Artem Tkachenko"@en .
		<4100> <name> "Артём Ткаченко"@ru .
		<5000> <name> "School A" .
		<5001> <name> "School B" .
		<5101> <name> "Googleplex" .
		<5102> <name> "Shoreline Amphitheater" .
		<5103> <name> "San Carlos Airport" .
		<5104> <name> "SF Bay area" .
		<5105> <name> "Mountain View" .
		<5106> <name> "San Carlos" .
		<5107> <name> "New York" .
		<8192> <name> "Regex Master" .
		<10000> <name> "Alice" .
		<10001> <name> "Elizabeth" .
		<10002> <name> "Alice" .
		<10003> <name> "Bob" .
		<10004> <name> "Alice" .
		<10005> <name> "Bob" .
		<10006> <name> "Colin" .
		<10007> <name> "Elizabeth" .
		<10101> <name_lang> "zon"@sv .
		<10101> <name_lang> "öffnen"@de .
		<10101> <name_lang_index> "zon"@sv .
		<10101> <name_lang_index> "öffnen"@de .
		<10101> <lang_type> "Test" .
		<10102> <name_lang> "öppna"@sv .
		<10102> <name_lang> "zumachen"@de .
		<10102> <name_lang_index> "öppna"@sv .
		<10102> <name_lang_index> "zumachen"@de .
		<10102> <lang_type> "Test" .
		<11000> <name> "Baz Luhrmann"@en .
		<11001> <name> "Strictly Ballroom"@en .
		<11002> <name> "Puccini: La boheme (Sydney Opera)"@en .
		<11003> <name> "No. 5 the film"@en .
		<11100> <name> "expand" .

		<51> <name> "A" .
		<52> <name> "B" .
		<53> <name> "C" .
		<54> <name> "D" .
		<55> <name> "E" .
		<56> <name> "F" .
		<57> <name> "G" .
		<58> <name> "H" .
		<59> <name> "I" .
		<60> <name> "J" .

		<1> <full_name> "Michonne's large name for hashing" .

		<1> <noindex_name> "Michonne's name not indexed" .
		<2> <noindex_name> "King Lear's name not indexed" .
		<3> <noindex_name> "Margaret's name not indexed" .
		<4> <noindex_name> "Leonard's name not indexed" .

		<1> <noindex_age> "21" .
		<2> <noindex_age> "22" .
		<3> <noindex_age> "23" .
		<4> <noindex_age> "24" .

		<1> <noindex_dob> "1810-11-01" .
		<2> <noindex_dob> "1710-11-01" .
		<3> <noindex_dob> "1610-11-01" .
		<4> <noindex_dob> "1510-11-01" .

		<1> <noindex_alive> "true" .
		<2> <noindex_alive> "false" .
		<3> <noindex_alive> "false" .
		<4> <noindex_alive> "true" .

		<1> <noindex_salary> "501.23" .
		<2> <noindex_salary> "589.04" .
		<3> <noindex_salary> "459.47" .
		<4> <noindex_salary> "967.68" .

		<1> <friend> <23> .
		<1> <friend> <24> .
		<1> <friend> <25> .
		<1> <friend> <31> .
		<1> <friend> <101> .
		<31> <friend> <24> .
		<23> <friend> <1> .

		<2> <best_friend> <64> (since=2019-03-28T07:41:57+23:00) .
		<3> <best_friend> <64> (since=2018-03-24T14:41:57+05:30) .
		<4> <best_friend> <64> (since=2019-03-27) .

		<1> <age> "38" .
		<23> <age> "15" .
		<24> <age> "15" .
		<25> <age> "17" .
		<31> <age> "19" .
		<10000> <age> "25" .
		<10001> <age> "75" .
		<10002> <age> "75" .
		<10003> <age> "75" .
		<10004> <age> "75" .
		<10005> <age> "25" .
		<10006> <age> "25" .
		<10007> <age> "25" .

		<1> <alive> "true" .
		<23> <alive> "true" .
		<25> <alive> "false" .
		<31> <alive> "false" .

		<1> <gender> "female" .
		<23> <gender> "male" .

		<4001> <office> "office 1" .
		<4002> <room> "room 1" .
		<4003> <room> "room 2" .
		<4004> <room> "" .
		<4001> <office.room> <4002> .
		<4001> <office.room> <4003> .
		<4001> <office.room> <4004> .

		<3001> <symbol> "AAPL" .
		<3002> <symbol> "AMZN" .
		<3003> <symbol> "AMD" .
		<3004> <symbol> "FB" .
		<3005> <symbol> "GOOG" .
		<3006> <symbol> "MSFT" .

		<1> <dob> "1910-01-01" .
		<23> <dob> "1910-01-02" .
		<24> <dob> "1909-05-05" .
		<25> <dob> "1909-01-10" .
		<31> <dob> "1901-01-15" .

		<1> <path> <31> (weight = 0.1, weight1 = 0.2) .
		<1> <path> <24> (weight = 0.2) .
		<31> <path> <1000> (weight = 0.1) .
		<1000> <path> <1001> (weight = 0.1) .
		<1000> <path> <1002> (weight = 0.7) .
		<1001> <path> <1002> (weight = 0.1) .
		<1002> <path> <1003> (weight = 0.6) .
		<1001> <path> <1003> (weight = 1.5) .
		<1003> <path> <1001> .

		<1> <follow> <31> .
		<1> <follow> <24> .
		<31> <follow> <1001> .
		<1001> <follow> <1000> .
		<1002> <follow> <1000> .
		<1001> <follow> <1003> .
		<1003> <follow> <1002> .

		<1> <survival_rate> "98.99" .
		<23> <survival_rate> "1.6" .
		<24> <survival_rate> "1.6" .
		<25> <survival_rate> "1.6" .
		<31> <survival_rate> "1.6" .

		<1> <school> <5000> .
		<23> <school> <5001> .
		<24> <school> <5000> .
		<25> <school> <5000> .
		<31> <school> <5001> .
		<101> <school> <5001> .

		<1> <_xid_> "mich" .
		<24> <_xid_> "g\"lenn" .
		<110> <_xid_> "a.bc" .

		<23> <alias> "Zambo Alice" .
		<24> <alias> "John Alice" .
		<25> <alias> "Bob Joe" .
		<31> <alias> "Allan Matt" .
		<101> <alias> "John Oliver" .

		<23> <alias_lang> "Zambo Alice"@en .
		<24> <alias_lang> "John Alice"@en .
		<25> <alias_lang> "Bob Joe"@en .
		<31> <alias_lang> "Allan Matt"@en .
		<101> <alias_lang> "John Oliver"@en .

		<1> <bin_data> "YmluLWRhdGE=" .

		<1> <graduation> "1932-01-01" .
		<31> <graduation> "1933-01-01" .
		<31> <graduation> "1935-01-01" .

		<10000> <salary> "10000" .
		<10002> <salary> "10002" .

		<1> <address> "31, 32 street, Jupiter" .
		<23> <address> "21, mark street, Mars" .

		<1> <dob_day> "1910-01-01" .
		<23> <dob_day> "1910-01-02" .
		<24> <dob_day> "1909-05-05" .
		<25> <dob_day> "1909-01-10" .
		<31> <dob_day> "1901-01-15" .

		<1> <power> "13.25"^^<xs:float> .

		<1> <sword_present> "true" .

		<1> <son> <2300> .
		<1> <son> <2333> .

		<5010> <nick_name> "Two Terms" .

		<4097> <lossy> "Badger" .
		<4097> <lossy> "European badger"@en .
		<4097> <lossy> "European badger barger European"@xx .
		<4097> <lossy> "Borsuk europejski"@pl .
		<4097> <lossy> "Europäischer Dachs"@de .
		<4097> <lossy> "Барсук"@ru .
		<4097> <lossy> "Blaireau européen"@fr .
		<4098> <lossy> "Honey badger"@en .

		<23> <film.film.initial_release_date> "1900-01-02" .
		<24> <film.film.initial_release_date> "1909-05-05" .
		<25> <film.film.initial_release_date> "1929-01-10" .
		<31> <film.film.initial_release_date> "1801-01-15" .

		<0x10000> <royal_title> "Her Majesty Elizabeth the Second, by the Grace of God of the United Kingdom of Great Britain and Northern Ireland and of Her other Realms and Territories Queen, Head of the Commonwealth, Defender of the Faith"@en .
		<0x10000> <royal_title> "Sa Majesté Elizabeth Deux, par la grâce de Dieu Reine du Royaume-Uni, du Canada et de ses autres royaumes et territoires, Chef du Commonwealth, Défenseur de la Foi"@fr .

		<32> <school> <33> .
		<33> <district> <34> .
		<34> <county> <35> .
		<35> <state> <36> .

		<36> <abbr> "CA" .

		<1> <password> "123456" .
		<32> <password> "123456" .
		<23> <pass> "654321" .

		<23> <shadow_deep> "4" .
		<24> <shadow_deep> "14" .

		<1> <dgraph.type> "User" .
		<2> <dgraph.type> "Person" .
		<3> <dgraph.type> "Person" .
		<4> <dgraph.type> "Person" .
		<5> <dgraph.type> "Animal" .
		<5> <dgraph.type> "Pet" .
		<6> <dgraph.type> "Animal" .
		<6> <dgraph.type> "Pet" .

		<23> <dgraph.type> "Person" .
		<24> <dgraph.type> "Person" .
		<25> <dgraph.type> "Person" .
		<31> <dgraph.type> "Person" .
		<32> <dgraph.type> "SchoolInfo" .
		<33> <dgraph.type> "SchoolInfo" .
		<34> <dgraph.type> "SchoolInfo" .
		<35> <dgraph.type> "SchoolInfo" .
		<36> <dgraph.type> "SchoolInfo" .
		<40> <dgraph.type> "Person2" .
		<41> <dgraph.type> "Person2" .
		<11100> <dgraph.type> "Node" .

		<2> <pet> <5> .
		<3> <pet> <6> .
		<4> <pet> <7> .

		<2> <enemy> <3> .
		<2> <enemy> <4> .

		<11000> <director.film> <11001> .
		<11000> <director.film> <11002> .
		<11000> <director.film> <11003> .

		<11100> <node> <11100> .

		<200> <make> "Ford" .
		<200> <model> "Focus" .
		<200> <year> "2008" .
		<200> <dgraph.type> "CarModel" .

		<201> <make> "Ford" .
		<201> <model> "Focus" .
		<201> <year> "2009" .
		<201> <dgraph.type> "CarModel" .
		<201> <previous_model> <200> .

		<202> <name> "Car" .
		<202> <make> "Toyota" .
		<202> <year> "2009" .
		<202> <model> "Prius" .
		<202> <model> "プリウス"@jp .
		<202> <owner> <203> .
		<202> <dgraph.type> "CarModel" .
		<202> <dgraph.type> "Object" .

		<203> <owner_name> "Owner of Prius" .
		<203> <dgraph.type> "Person" .

		# data for regexp testing
		_:luke <firstName> "Luke" .
		_:luke <lastName> "Skywalker" .
		_:leia <firstName> "Princess" .
		_:leia <lastName> "Leia" .
		_:han <firstName> "Han" .
		_:han <lastName> "Solo" .
		_:har <firstName> "Harrison" .
		_:har <lastName> "Ford" .
		_:ss <firstName> "Steven" .
		_:ss <lastName> "Spielberg" .

		<501> <newname> "P1" .
		<502> <newname> "P2" .
		<503> <newname> "P3" .
		<504> <newname> "P4" .
		<505> <newname> "P5" .
		<506> <newname> "P6" .
		<507> <newname> "P7" .
		<508> <newname> "P8" .
		<509> <newname> "P9" .
		<510> <newname> "P10" .
		<511> <newname> "P11" .
		<512> <newname> "P12" .

		<501> <newage> "21" .
		<502> <newage> "22" .
		<503> <newage> "23" .
		<504> <newage> "24" .
		<505> <newage> "25" .
		<506> <newage> "26" .
		<507> <newage> "27" .
		<508> <newage> "28" .
		<509> <newage> "29" .
		<510> <newage> "30" .
		<511> <newage> "31" .
		<512> <newage> "32" .

		<501> <newfriend> <502> .
		<501> <newfriend> <503> .
		<501> <boss> <504> .
		<502> <newfriend> <505> .
		<502> <newfriend> <506> .
		<503> <newfriend> <507> .
		<503> <newfriend> <508> .
		<504> <newfriend> <509> .
		<504> <newfriend> <510> .
		<502> <boss> <510> .
		<510> <newfriend> <511> .
		<510> <newfriend> <512> .

		<51> <connects> <52>  (weight=11) .
		<51> <connects> <53>  (weight=1) .
		<51> <connects> <54>  (weight=10) .

		<53> <connects> <51>  (weight=10) .
		<53> <connects> <52>  (weight=10) .
		<53> <connects> <54>  (weight=1) .

		<52> <connects> <51>  (weight=10) .
		<52> <connects> <53>  (weight=10) .
		<52> <connects> <54>  (weight=10) .

		<54> <connects> <51>  (weight=10) .
		<54> <connects> <52>  (weight=2) .
		<54> <connects> <53>  (weight=10) .
		<54> <connects> <55>  (weight=1) .


		# tests for testing hop behavior for shortest path queries
		<56> <connects> <57> (weight=1) .
		<56> <connects> <58> (weight=1) .
		<58> <connects> <59> (weight=1) .
		<59> <connects> <60> (weight=1) .

		# data for testing between operator.
		<20000> <score> "90" .
		<20000> <score> "56" .
		<20000> <average> "46.93" .
		<20000> <average> "55.10" .
		<20000> <pet_name> "little master" .
		<20000> <pet_name> "master blaster" .

		<20001> <score> "68" .
		<20001> <score> "85" .
		<20001> <average> "35.20" .
		<20001> <average> "49.33" .
		<20001> <pet_name> "mahi" .
		<20001> <pet_name> "ms" .

		# data for testing consistency of sort
		<61> <pred> "A" .
		<62> <pred> "B" .
		<63> <pred> "C" .
		<64> <pred> "D" .
		<65> <pred> "E" .

		<61> <indexpred> "A" .
		<62> <indexpred> "B" .
		<63> <indexpred> "C" .
		<64> <indexpred> "D" .
		<65> <indexpred> "E" .

		<61> <pname> "nameA" .
		<62> <pname> "nameB" .
		<63> <pname> "nameC" .
		<64> <pname> "nameD" .
		<65> <pname> "nameE" .
		<66> <pname> "nameF" .
		<67> <pname> "nameG" .
		<68> <pname> "nameH" .
		<69> <pname> "nameI" .
		<70> <pname> "nameJ" .

		<61> <pred1> "A" .
		<62> <pred1> "A" .
		<63> <pred1> "A" .
		<64> <pred1> "B" .
		<65> <pred1> "B" .
		<66> <pred1> "B" .
		<67> <pred1> "C" .
		<68> <pred1> "C" .
		<69> <pred1> "C" .
		<70> <pred1> "C" .

		<61> <pred2> "I" .
		<62> <pred2> "J" .

		<64> <pred2> "I" .
		<65> <pred2> "J" .

		<67> <pred2> "I" .
		<68> <pred2> "J" .
		<69> <pred2> "K" .


		<61> <index-pred1> "A" .
		<62> <index-pred1> "A" .
		<63> <index-pred1> "A" .
		<64> <index-pred1> "B" .
		<65> <index-pred1> "B" .
		<66> <index-pred1> "B" .
		<67> <index-pred1> "C" .
		<68> <index-pred1> "C" .
		<69> <index-pred1> "C" .
		<70> <index-pred1> "C" .

		<61> <index-pred2> "I" .
		<62> <index-pred2> "J" .

		<64> <index-pred2> "I" .
		<65> <index-pred2> "J" .

		<67> <index-pred2> "I" .
		<68> <index-pred2> "J" .
		<69> <index-pred2> "K" .

		<61> <tweet-a> "aaa" .
		<62> <tweet-a> "aaaa" .
		<63> <tweet-a> "aaaab" .
		<64> <tweet-a> "aaaabb" .

		<61> <tweet-b> "indiana" .
		<62> <tweet-b> "indiana" .
		<63> <tweet-b> "indiana jones" .
		<64> <tweet-b> "indiana pop" .

		<61> <tweet-c> "I am a citizen" .
		<62> <tweet-c> "I am a citizen" .
		<63> <tweet-c> "I am a citizen" .
		<64> <tweet-c> "I am a citizen of Paradis Island" .

		<61> <tweet-d> "aaabxxx" .
		<62> <tweet-d> "aaacdxx" .
		<63> <tweet-d> "aaabcd" .

		<40> <name2> "Alice" .
		<41> <age2> "20" .

		<1023> <dgraph.type> "DispatchBoardColumn" .
                <1024> <dgraph.type> "DispatchBoardColumn" .
                <1025> <dgraph.type> "DispatchBoardCard" .
                <1026> <dgraph.type> "DispatchBoardCard" .

                <1025> <DispatchBoard.column> <1023> .
                <1025> <order> "0" .
                <1026> <DispatchBoard.column> <1023> .
                <1026> <order> "1" .
	`)
	x.Panic(err)

	x.Panic(addGeoPointToCluster(1, "loc", []float64{1.1, 2.0}))
	x.Panic(addGeoPointToCluster(24, "loc", []float64{1.10001, 2.000001}))
	x.Panic(addGeoPointToCluster(25, "loc", []float64{1.1, 2.0}))
	x.Panic(addGeoPointToCluster(5101, "geometry", []float64{-122.082506, 37.4249518}))
	x.Panic(addGeoPointToCluster(5102, "geometry", []float64{-122.080668, 37.426753}))
	x.Panic(addGeoPointToCluster(5103, "geometry", []float64{-122.2527428, 37.513653}))

	x.Panic(addGeoPolygonToCluster(23, "loc", [][][]float64{
		{{0.0, 0.0}, {2.0, 0.0}, {2.0, 2.0}, {0.0, 2.0}, {0.0, 0.0}},
	}))
	x.Panic(addGeoPolygonToCluster(5104, "geometry", [][][]float64{
		{{-121.6, 37.1}, {-122.4, 37.3}, {-122.6, 37.8}, {-122.5, 38.3}, {-121.9, 38},
			{-121.6, 37.1}},
	}))
	x.Panic(addGeoPolygonToCluster(5105, "geometry", [][][]float64{
		{{-122.06, 37.37}, {-122.1, 37.36}, {-122.12, 37.4}, {-122.11, 37.43},
			{-122.04, 37.43}, {-122.06, 37.37}},
	}))
	x.Panic(addGeoPolygonToCluster(5106, "geometry", [][][]float64{
		{{-122.25, 37.49}, {-122.28, 37.49}, {-122.27, 37.51}, {-122.25, 37.52},
			{-122.25, 37.49}},
	}))

	x.Panic(addGeoMultiPolygonToCluster(5107, [][][][]float64{
		{{{-74.29504394531249, 40.19146303804063}, {-74.59716796875, 40.39258071969131},
			{-74.6466064453125, 40.20824570152502}, {-74.454345703125, 40.06125658140474},
			{-74.28955078125, 40.17467622056341}, {-74.29504394531249, 40.19146303804063}}},
		{{{-74.102783203125, 40.8595252289932}, {-74.2730712890625, 40.718119379753446},
			{-74.0478515625, 40.66813955408042}, {-73.98193359375, 40.772221877329024},
			{-74.102783203125, 40.8595252289932}}},
	}))

	// Add data for regex tests.
	nextId := uint64(0x2000)
	patterns := []string{"mississippi", "missouri", "mission", "missionary",
		"whissle", "transmission", "zipped", "monosiphonic", "vasopressin", "vapoured",
		"virtuously", "zurich", "synopsis", "subsensuously",
		"admission", "commission", "submission", "subcommission", "retransmission", "omission",
		"permission", "intermission", "dimission", "discommission",
	}
	for _, p := range patterns {
		triples := fmt.Sprintf(`
			<%d> <value> "%s" .
			<0x1234> <pattern> <%d> .
		`, nextId, p, nextId)
		x.Panic(addTriplesToCluster(triples))
		nextId++
	}

	// Add data for datetime tests
	err = addTriplesToCluster(`
		<301> <created_at> "2019-03-28T07:41:57+23:00" (modified_at=2019-05-28T07:41:57+23:00) .
		<302> <created_at> "2019-03-28T07:41:57+23:00" (modified_at=2019-03-28T07:41:57+23:00) .
		<303> <created_at> "2019-03-27T14:41:57+06:00" (modified_at=2019-03-29) .
		<304> <created_at> "2019-03-28T08:41:57+23:00" (modified_at=2019-03-27T14:41:57+06:00) .
		<305> <created_at> "2019-03-28T06:41:57+23:00" (modified_at=2019-03-28) .
		<306> <created_at> "2019-03-24T14:41:57+05:30" (modified_at=2019-03-28T06:41:57+23:00) .
		<307> <created_at> "2019-05-28T07:41:57+23:00" .

		<301> <updated_at> "2019-03-28T07:41:57+23:00" (modified_at=2019-05-28) .
		<302> <updated_at> "2019-03-28T06:41:57+22:00" (modified_at=2019-03-28T07:41:57+23:00) .
		<303> <updated_at> "2019-03-27T14:41:57+06:00" (modified_at=2019-03-28T05:41:57+21:00) .
		<304> <updated_at> "2019-03-27T09:41:57" .
		<305> <updated_at> "2019-03-28T06:41:57+23:00" (modified_at=2019-03-28T08:41:57+23:00) .
		<306> <updated_at> "2019-03-24T14:41:57+05:30" (modified_at=2019-03-28T06:41:57+23:00) .
		<307> <updated_at> "2019-05-28" (modified_at=2019-03-24T14:41:57+05:30) .
	`)
	x.Panic(err)

	// Add data for ngram tests
	err = addTriplesToCluster(`
		<401> <description> "The quick brown fox jumps over the lazy dog" .
		<402> <description> "A quick brown fox leaps over a sleeping dog" .
		<403> <description> "The lazy dog sleeps under the warm sun" .
		<404> <description> "Brown foxes are quick and agile animals" .
		<405> <description> "Dogs are loyal companions to humans" .
		<406> <description> "The sun shines brightly in the clear sky" .
		<407> <description> "Quick movements help foxes catch their prey" .
		<408> <description> "Lazy afternoons are perfect for sleeping dogs" .
		<409> <description> "Jumping over obstacles requires agility and speed" .
		<410> <description> "The brown animal moved quickly through the forest" .
		<411> <description> "Machine learning algorithms process natural language text" .
		<412> <description> "Natural language processing uses advanced algorithms" .
		<413> <description> "Text processing algorithms analyze linguistic patterns" .
		<414> <description> "Advanced machine learning techniques improve accuracy" .
		<415> <description> "Linguistic analysis helps understand text meaning" .
	`)
	x.Panic(err)
}
