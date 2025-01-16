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

//nolint:lll
package schema

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/dgraph-io/gqlparser/v2/ast"
	"github.com/hypermodeinc/dgraph/v24/x"
)

func TestDgraphMapping_WithoutDirectives(t *testing.T) {
	schemaStr := `
type Author {
        id: ID!

        name: String! @search(by: ["hash", "trigram"])
        dob: DateTime @search
        reputation: Float @search
        posts: [Post!] @hasInverse(field: author)
}

type Post {
        postID: ID!
        postType: PostType @search
        author: Author! @hasInverse(field: posts)
}

enum PostType {
        Fact
        Question
        Opinion
}

interface Employee {
        ename: String!
}

interface Character {
        id: ID!
        name: String! @search(by: [exact])
        appearsIn: [Episode!] @search
}

type Human implements Character & Employee {
        starships: [Starship]
        totalCredits: Float
}

type Droid implements Character {
        primaryFunction: String
}

enum Episode {
        NEWHOPE
        EMPIRE
        JEDI
}

type Starship {
        id: ID!
        name: String! @search(by: [term])
        length: Float
}`

	schHandler, errs := NewHandler(schemaStr, false)
	require.NoError(t, errs)
	sch, err := FromString(schHandler.GQLSchema(), x.GalaxyNamespace)
	require.NoError(t, err)

	s, ok := sch.(*schema)
	require.True(t, ok, "expected to be able to convert sch to internal schema type")

	author := map[string]string{
		"name":           "Author.name",
		"dob":            "Author.dob",
		"reputation":     "Author.reputation",
		"posts":          "Author.posts",
		"postsAggregate": "Author.postsAggregate",
	}
	authorAggregateResult := map[string]string{
		"count":         "AuthorAggregateResult.count",
		"dobMax":        "AuthorAggregateResult.dobMax",
		"dobMin":        "AuthorAggregateResult.dobMin",
		"nameMax":       "AuthorAggregateResult.nameMax",
		"nameMin":       "AuthorAggregateResult.nameMin",
		"reputationAvg": "AuthorAggregateResult.reputationAvg",
		"reputationMax": "AuthorAggregateResult.reputationMax",
		"reputationMin": "AuthorAggregateResult.reputationMin",
		"reputationSum": "AuthorAggregateResult.reputationSum",
	}
	post := map[string]string{
		"postType": "Post.postType",
		"author":   "Post.author",
	}
	postAggregateResult := map[string]string{
		"count": "PostAggregateResult.count",
	}
	character := map[string]string{
		"name":      "Character.name",
		"appearsIn": "Character.appearsIn",
	}
	characterAggregateResult := map[string]string{
		"count":   "CharacterAggregateResult.count",
		"nameMax": "CharacterAggregateResult.nameMax",
		"nameMin": "CharacterAggregateResult.nameMin",
	}
	employee := map[string]string{
		"ename": "Employee.ename",
	}
	employeeAggregateResult := map[string]string{
		"count":    "EmployeeAggregateResult.count",
		"enameMax": "EmployeeAggregateResult.enameMax",
		"enameMin": "EmployeeAggregateResult.enameMin",
	}
	human := map[string]string{
		"ename":              "Employee.ename",
		"name":               "Character.name",
		"appearsIn":          "Character.appearsIn",
		"starships":          "Human.starships",
		"totalCredits":       "Human.totalCredits",
		"starshipsAggregate": "Human.starshipsAggregate",
	}
	humanAggregateResult := map[string]string{
		"count":           "HumanAggregateResult.count",
		"enameMax":        "HumanAggregateResult.enameMax",
		"enameMin":        "HumanAggregateResult.enameMin",
		"nameMax":         "HumanAggregateResult.nameMax",
		"nameMin":         "HumanAggregateResult.nameMin",
		"totalCreditsAvg": "HumanAggregateResult.totalCreditsAvg",
		"totalCreditsMax": "HumanAggregateResult.totalCreditsMax",
		"totalCreditsMin": "HumanAggregateResult.totalCreditsMin",
		"totalCreditsSum": "HumanAggregateResult.totalCreditsSum",
	}
	droid := map[string]string{
		"name":            "Character.name",
		"appearsIn":       "Character.appearsIn",
		"primaryFunction": "Droid.primaryFunction",
	}
	droidAggregateResult := map[string]string{
		"count":              "DroidAggregateResult.count",
		"nameMax":            "DroidAggregateResult.nameMax",
		"nameMin":            "DroidAggregateResult.nameMin",
		"primaryFunctionMax": "DroidAggregateResult.primaryFunctionMax",
		"primaryFunctionMin": "DroidAggregateResult.primaryFunctionMin",
	}
	starship := map[string]string{
		"name":   "Starship.name",
		"length": "Starship.length",
	}
	starshipAggregateResult := map[string]string{
		"count":     "StarshipAggregateResult.count",
		"lengthAvg": "StarshipAggregateResult.lengthAvg",
		"lengthMax": "StarshipAggregateResult.lengthMax",
		"lengthMin": "StarshipAggregateResult.lengthMin",
		"lengthSum": "StarshipAggregateResult.lengthSum",
		"nameMax":   "StarshipAggregateResult.nameMax",
		"nameMin":   "StarshipAggregateResult.nameMin",
	}

	expected := map[string]map[string]string{
		"Author":                   author,
		"UpdateAuthorPayload":      author,
		"DeleteAuthorPayload":      author,
		"Post":                     post,
		"UpdatePostPayload":        post,
		"DeletePostPayload":        post,
		"Employee":                 employee,
		"Character":                character,
		"UpdateCharacterPayload":   character,
		"DeleteCharacterPayload":   character,
		"Human":                    human,
		"UpdateHumanPayload":       human,
		"DeleteHumanPayload":       human,
		"Droid":                    droid,
		"UpdateDroidPayload":       droid,
		"DeleteDroidPayload":       droid,
		"UpdateEmployeePayload":    employee,
		"DeleteEmployeePayload":    employee,
		"Starship":                 starship,
		"UpdateStarshipPayload":    starship,
		"DeleteStarshipPayload":    starship,
		"AuthorAggregateResult":    authorAggregateResult,
		"CharacterAggregateResult": characterAggregateResult,
		"DroidAggregateResult":     droidAggregateResult,
		"EmployeeAggregateResult":  employeeAggregateResult,
		"HumanAggregateResult":     humanAggregateResult,
		"PostAggregateResult":      postAggregateResult,
		"StarshipAggregateResult":  starshipAggregateResult,
	}

	if diff := cmp.Diff(expected, s.dgraphPredicate); diff != "" {
		t.Errorf("dgraph predicate map mismatch (-want +got):\n%s", diff)
	}
}

func TestDgraphMapping_WithDirectives(t *testing.T) {
	schemaStr := `
	type Author @dgraph(type: "dgraph.author") {
			id: ID!

			name: String! @search(by: ["hash", "trigram"])
			dob: DateTime @search
			reputation: Float @search
			posts: [Post!] @hasInverse(field: author)
	}

	type Post @dgraph(type: "dgraph.Post") {
			postID: ID!
			postType: PostType @search @dgraph(pred: "dgraph.post_type")
			author: Author! @hasInverse(field: posts) @dgraph(pred: "dgraph.post_author")
	}

	enum PostType {
			Fact
			Question
			Opinion
	}

	interface Employee @dgraph(type: "dgraph.employee.en") {
			ename: String!
	}

	interface Character @dgraph(type: "performance.character") {
			id: ID!
			name: String! @search(by: [exact])
			appearsIn: [Episode!] @search @dgraph(pred: "appears_in")
	}

	type Human implements Character & Employee {
			starships: [Starship]
			totalCredits: Float @dgraph(pred: "credits")
	}

	type Droid implements Character @dgraph(type: "roboDroid") {
			primaryFunction: String
	}

	enum Episode {
			NEWHOPE
			EMPIRE
			JEDI
	}

	type Starship @dgraph(type: "star.ship") {
			id: ID!
			name: String! @search(by: [term]) @dgraph(pred: "star.ship.name")
			length: Float
	}`

	schHandler, errs := NewHandler(schemaStr, false)
	require.NoError(t, errs)
	sch, err := FromString(schHandler.GQLSchema(), x.GalaxyNamespace)
	require.NoError(t, err)

	s, ok := sch.(*schema)
	require.True(t, ok, "expected to be able to convert sch to internal schema type")

	author := map[string]string{
		"name":           "dgraph.author.name",
		"dob":            "dgraph.author.dob",
		"reputation":     "dgraph.author.reputation",
		"posts":          "dgraph.author.posts",
		"postsAggregate": "dgraph.author.postsAggregate",
	}
	authorAggregateResult := map[string]string{
		"count":         "AuthorAggregateResult.count",
		"dobMax":        "AuthorAggregateResult.dobMax",
		"dobMin":        "AuthorAggregateResult.dobMin",
		"nameMax":       "AuthorAggregateResult.nameMax",
		"nameMin":       "AuthorAggregateResult.nameMin",
		"reputationAvg": "AuthorAggregateResult.reputationAvg",
		"reputationMax": "AuthorAggregateResult.reputationMax",
		"reputationMin": "AuthorAggregateResult.reputationMin",
		"reputationSum": "AuthorAggregateResult.reputationSum",
	}
	post := map[string]string{
		"postType": "dgraph.post_type",
		"author":   "dgraph.post_author",
	}
	postAggregateResult := map[string]string{
		"count": "PostAggregateResult.count",
	}
	character := map[string]string{
		"name":      "performance.character.name",
		"appearsIn": "appears_in",
	}
	characterAggregateResult := map[string]string{
		"count":   "CharacterAggregateResult.count",
		"nameMax": "CharacterAggregateResult.nameMax",
		"nameMin": "CharacterAggregateResult.nameMin",
	}
	human := map[string]string{
		"ename":              "dgraph.employee.en.ename",
		"name":               "performance.character.name",
		"appearsIn":          "appears_in",
		"starships":          "Human.starships",
		"totalCredits":       "credits",
		"starshipsAggregate": "Human.starshipsAggregate",
	}
	humanAggregateResult := map[string]string{
		"count":           "HumanAggregateResult.count",
		"enameMax":        "HumanAggregateResult.enameMax",
		"enameMin":        "HumanAggregateResult.enameMin",
		"nameMax":         "HumanAggregateResult.nameMax",
		"nameMin":         "HumanAggregateResult.nameMin",
		"totalCreditsAvg": "HumanAggregateResult.totalCreditsAvg",
		"totalCreditsMax": "HumanAggregateResult.totalCreditsMax",
		"totalCreditsMin": "HumanAggregateResult.totalCreditsMin",
		"totalCreditsSum": "HumanAggregateResult.totalCreditsSum",
	}
	droid := map[string]string{
		"name":            "performance.character.name",
		"appearsIn":       "appears_in",
		"primaryFunction": "roboDroid.primaryFunction",
	}
	droidAggregateResult := map[string]string{
		"count":              "DroidAggregateResult.count",
		"nameMax":            "DroidAggregateResult.nameMax",
		"nameMin":            "DroidAggregateResult.nameMin",
		"primaryFunctionMax": "DroidAggregateResult.primaryFunctionMax",
		"primaryFunctionMin": "DroidAggregateResult.primaryFunctionMin",
	}
	employee := map[string]string{
		"ename": "dgraph.employee.en.ename",
	}
	employeeAggregateResult := map[string]string{
		"count":    "EmployeeAggregateResult.count",
		"enameMax": "EmployeeAggregateResult.enameMax",
		"enameMin": "EmployeeAggregateResult.enameMin",
	}
	starship := map[string]string{
		"name":   "star.ship.name",
		"length": "star.ship.length",
	}
	starshipAggregateResult := map[string]string{
		"count":     "StarshipAggregateResult.count",
		"lengthAvg": "StarshipAggregateResult.lengthAvg",
		"lengthMax": "StarshipAggregateResult.lengthMax",
		"lengthMin": "StarshipAggregateResult.lengthMin",
		"lengthSum": "StarshipAggregateResult.lengthSum",
		"nameMax":   "StarshipAggregateResult.nameMax",
		"nameMin":   "StarshipAggregateResult.nameMin",
	}

	expected := map[string]map[string]string{
		"Author":                   author,
		"UpdateAuthorPayload":      author,
		"DeleteAuthorPayload":      author,
		"Post":                     post,
		"UpdatePostPayload":        post,
		"DeletePostPayload":        post,
		"Employee":                 employee,
		"DeleteEmployeePayload":    employee,
		"UpdateEmployeePayload":    employee,
		"Character":                character,
		"UpdateCharacterPayload":   character,
		"DeleteCharacterPayload":   character,
		"Human":                    human,
		"UpdateHumanPayload":       human,
		"DeleteHumanPayload":       human,
		"Droid":                    droid,
		"UpdateDroidPayload":       droid,
		"DeleteDroidPayload":       droid,
		"Starship":                 starship,
		"UpdateStarshipPayload":    starship,
		"DeleteStarshipPayload":    starship,
		"AuthorAggregateResult":    authorAggregateResult,
		"CharacterAggregateResult": characterAggregateResult,
		"DroidAggregateResult":     droidAggregateResult,
		"EmployeeAggregateResult":  employeeAggregateResult,
		"HumanAggregateResult":     humanAggregateResult,
		"PostAggregateResult":      postAggregateResult,
		"StarshipAggregateResult":  starshipAggregateResult,
	}

	if diff := cmp.Diff(expected, s.dgraphPredicate); diff != "" {
		t.Errorf("dgraph predicate map mismatch (-want +got):\n%s", diff)
	}
}

func TestCheckNonNulls(t *testing.T) {

	gqlSchema, err := FromString(`
	type T {
		req: String!
		notReq: String
		alsoReq: String!
	}`, x.GalaxyNamespace)
	require.NoError(t, err)

	tcases := map[string]struct {
		obj map[string]interface{}
		exc string
		err error
	}{
		"all present": {
			obj: map[string]interface{}{"req": "here", "notReq": "here", "alsoReq": "here"},
			err: nil,
		},
		"only non-null": {
			obj: map[string]interface{}{"req": "here", "alsoReq": "here"},
			err: nil,
		},
		"missing non-null": {
			obj: map[string]interface{}{"req": "here", "notReq": "here"},
			err: errors.Errorf("type T requires a value for field alsoReq, but no value present"),
		},
		"missing all non-null": {
			obj: map[string]interface{}{"notReq": "here"},
			err: errors.Errorf("type T requires a value for field req, but no value present"),
		},
		"with exclusion": {
			obj: map[string]interface{}{"req": "here", "notReq": "here"},
			exc: "alsoReq",
			err: nil,
		},
	}

	typ := &astType{
		typ:      &ast.Type{NamedType: "T"},
		inSchema: (gqlSchema.(*schema)),
	}

	for name, test := range tcases {
		t.Run(name, func(t *testing.T) {
			err := typ.EnsureNonNulls(test.obj, test.exc)
			if test.err == nil {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, test.err.Error())
			}
		})
	}
}

func TestSubstituteVarsInBody(t *testing.T) {
	tcases := []struct {
		name      string
		variables map[string]interface{}
		template  interface{}
		expected  interface{}
	}{
		{
			"handle nil template correctly",
			map[string]interface{}{"id": "0x3", "postID": "0x9"},
			nil,
			nil,
		},
		{
			"handle empty object template correctly",
			map[string]interface{}{"id": "0x3", "postID": "0x9"},
			map[string]interface{}{},
			map[string]interface{}{},
		},
		{
			"substitutes variables correctly",
			map[string]interface{}{"id": "0x3", "postID": "0x9"},
			map[string]interface{}{"author": "$id", "post": map[string]interface{}{"id": "$postID"}},
			map[string]interface{}{"author": "0x3", "post": map[string]interface{}{"id": "0x9"}},
		},
		{
			"substitutes nil variables correctly",
			map[string]interface{}{"id": nil},
			map[string]interface{}{"author": "$id"},
			map[string]interface{}{"author": nil},
		},
		{
			"substitutes variables with an array in template correctly",
			map[string]interface{}{"id": "0x3", "admin": false, "postID": "0x9",
				"text": "Random comment", "age": 28},
			map[string]interface{}{"author": "$id", "admin": "$admin",
				"post": map[string]interface{}{"id": "$postID",
					"comments": []interface{}{"$text", "$age"}}, "age": "$age"},
			map[string]interface{}{"author": "0x3", "admin": false,
				"post": map[string]interface{}{"id": "0x9",
					"comments": []interface{}{"Random comment", 28}}, "age": 28},
		},
		{
			"substitutes variables with an array of object in template correctly",
			map[string]interface{}{"id": "0x3", "admin": false, "postID": "0x9",
				"text": "Random comment", "age": 28},
			map[string]interface{}{"author": "$id", "admin": "$admin",
				"post": map[string]interface{}{"id": "$postID",
					"comments": []interface{}{map[string]interface{}{"text": "$text"}}}, "age": "$age"},
			map[string]interface{}{"author": "0x3", "admin": false,
				"post": map[string]interface{}{"id": "0x9",
					"comments": []interface{}{map[string]interface{}{"text": "Random comment"}}}, "age": 28},
		},
		{
			"substitutes array variables correctly",
			map[string]interface{}{"ids": []int{1, 2, 3}, "names": []string{"M1", "M2"},
				"check": []interface{}{1, 3.14, "test"}},
			map[string]interface{}{"ids": "$ids", "names": "$names", "check": "$check"},
			map[string]interface{}{"ids": []int{1, 2, 3}, "names": []string{"M1", "M2"},
				"check": []interface{}{1, 3.14, "test"}},
		},
		{
			"substitutes object variables correctly",
			map[string]interface{}{"author": map[string]interface{}{"id": 1, "name": "George"}},
			map[string]interface{}{"author": "$author"},
			map[string]interface{}{"author": map[string]interface{}{"id": 1, "name": "George"}},
		},
		{
			"substitutes array of object variables correctly",
			map[string]interface{}{"authors": []interface{}{map[string]interface{}{"id": 1,
				"name": "George"}, map[string]interface{}{"id": 2, "name": "Jerry"}}},
			map[string]interface{}{"authors": "$authors"},
			map[string]interface{}{"authors": []interface{}{map[string]interface{}{"id": 1,
				"name": "George"}, map[string]interface{}{"id": 2, "name": "Jerry"}}},
		},
		{
			"substitutes direct body variable correctly",
			map[string]interface{}{"authors": []interface{}{map[string]interface{}{"id": 1,
				"name": "George"}, map[string]interface{}{"id": 2, "name": "Jerry"}}},
			"$authors",
			[]interface{}{map[string]interface{}{"id": 1, "name": "George"},
				map[string]interface{}{"id": 2, "name": "Jerry"}},
		},
		{
			"keep direct hardcoded string as is",
			map[string]interface{}{"authors": []interface{}{map[string]interface{}{"id": 1,
				"name": "George"}, map[string]interface{}{"id": 2, "name": "Jerry"}}},
			"authors",
			"authors",
		},
		{
			"keep direct hardcoded int as is",
			map[string]interface{}{"authors": []interface{}{map[string]interface{}{"id": 1,
				"name": "George"}, map[string]interface{}{"id": 2, "name": "Jerry"}}},
			3,
			3,
		},
		{
			"substitute only variables and keep deep hardcoded values as is",
			map[string]interface{}{"id": "0x3", "admin": false, "postID": "0x9",
				"text": "Random comment", "age": 28},
			map[string]interface{}{"author": "$id", "admin": true,
				"post": map[string]interface{}{"id": "$postID", "rating": 4.5,
					"comments": []interface{}{map[string]interface{}{"text": "$text",
						"type": "hidden"}}},
				"age": int64(23), "meta": nil},
			map[string]interface{}{"author": "0x3", "admin": true,
				"post": map[string]interface{}{"id": "0x9", "rating": 4.5,
					"comments": []interface{}{map[string]interface{}{"text": "Random comment",
						"type": "hidden"}}},
				"age": int64(23), "meta": nil},
		},
		{
			"Skip one missing variable in the HTTP body",
			map[string]interface{}{"postID": "0x9"},
			map[string]interface{}{"author": "$id", "post": map[string]interface{}{"id": "$postID"}},
			map[string]interface{}{"post": map[string]interface{}{"id": "0x9"}},
		},
		{
			"Skip all missing variables in the HTTP body",
			map[string]interface{}{},
			map[string]interface{}{"author": "$id", "post": map[string]interface{}{"id": "$postID"}},
			map[string]interface{}{"post": map[string]interface{}{}},
		},
	}

	for _, test := range tcases {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.expected, SubstituteVarsInBody(test.template, test.variables))
		})
	}
}

func TestParseBodyTemplate(t *testing.T) {
	tcases := []struct {
		name           string
		template       string
		expected       interface{}
		requiredFields map[string]bool
		expectedErr    error
	}{
		{
			"parses empty body template correctly",
			``,
			nil,
			nil,
			nil,
		},
		{
			"parses whitespace body template correctly",
			` `,
			nil,
			nil,
			nil,
		},
		{
			"parses body template with direct variable correctly",
			`$authors`,
			"$authors",
			map[string]bool{"authors": true},
			nil,
		},
		{
			"parses body template with direct hardcoded int correctly",
			`67`,
			int64(67),
			map[string]bool{},
			nil,
		},
		{
			"parses body template with direct hardcoded float correctly",
			`67.23`,
			67.23,
			map[string]bool{},
			nil,
		},
		{
			"parses body template with direct hardcoded boolean correctly",
			`true`,
			true,
			map[string]bool{},
			nil,
		},
		{
			"parses body template with direct hardcoded string correctly",
			`"alice"`,
			"alice",
			map[string]bool{},
			nil,
		},
		{
			"parses body template with direct hardcoded null correctly",
			`null`,
			nil,
			map[string]bool{},
			nil,
		},
		{
			"parses empty object body template correctly",
			`{}`,
			map[string]interface{}{},
			map[string]bool{},
			nil,
		},
		{
			"parses body template correctly",
			`{ author: $id, post: { id: $postID }}`,
			map[string]interface{}{"author": "$id",
				"post": map[string]interface{}{"id": "$postID"}},
			map[string]bool{"id": true, "postID": true},
			nil,
		},
		{
			"parses body template with underscores correctly",
			`{ author_name: $author_name, post: { id: $postID }}`,
			map[string]interface{}{"author_name": "$author_name",
				"post": map[string]interface{}{"id": "$postID"}},
			map[string]bool{"author_name": true, "postID": true},
			nil,
		},
		{
			"parses body template with an array correctly",
			`{ author: $id, admin: $admin, post: { id: $postID, comments: [$text] },
			   age: $age}`,
			map[string]interface{}{"author": "$id", "admin": "$admin",
				"post": map[string]interface{}{"id": "$postID",
					"comments": []interface{}{"$text"}}, "age": "$age"},
			map[string]bool{"id": true, "admin": true, "postID": true, "text": true, "age": true},
			nil,
		},
		{
			"parses body template with an array of object correctly",
			`{ author: $id, admin: $admin, post: { id: $postID, comments: [{ text: $text }] },
			   age: $age}`,
			map[string]interface{}{"author": "$id", "admin": "$admin",
				"post": map[string]interface{}{"id": "$postID",
					"comments": []interface{}{map[string]interface{}{"text": "$text"}}}, "age": "$age"},
			map[string]bool{"id": true, "admin": true, "postID": true, "text": true, "age": true},
			nil,
		},
		{
			"parses body template with an array of object and hardcoded scalars correctly",
			`{ author: $id, admin: false, post: { id: $postID, rating: 4.5,
				comments: [{ text: $text, type: "hidden" }] }, age: 23, meta: null}`,
			map[string]interface{}{"author": "$id", "admin": false,
				"post": map[string]interface{}{"id": "$postID", "rating": 4.5,
					"comments": []interface{}{map[string]interface{}{"text": "$text",
						"type": "hidden"}}},
				"age": int64(23), "meta": nil},
			map[string]bool{"id": true, "postID": true, "text": true},
			nil,
		},
		{
			"bad template error",
			`{ author: $id, post: { id $postID }}`,
			nil,
			nil,
			errors.New("input:1: Expected :, found $"),
		},
		{
			"unmatched brackets error",
			`{{ author: $id, post: { id: $postID }}`,
			nil,
			nil,
			errors.New("input:1: Expected Name, found {"),
		},
		{
			"invalid character error",
			`(author: $id, post: { id: $postID }}`,
			nil,
			nil,
			errors.New("input:1: Unexpected ("),
		},
	}

	for _, test := range tcases {
		t.Run(test.name, func(t *testing.T) {
			b, requiredFields, err := parseBodyTemplate(test.template, true)
			if test.expectedErr == nil {
				require.NoError(t, err)
				require.Equal(t, test.requiredFields, requiredFields)
				if b == nil {
					require.Nil(t, test.expected)
				} else {
					require.Equal(t, test.expected, b)
				}
			} else {
				require.EqualError(t, err, test.expectedErr.Error())
			}
		})
	}
}

func TestSubstituteVarsInURL(t *testing.T) {
	tcases := []struct {
		name        string
		variables   map[string]interface{}
		url         string
		expected    string
		expectedErr error
	}{
		{
			"Return url as is when no params",
			nil,
			"http://myapi.com/favMovies/1?num=10",
			"http://myapi.com/favMovies/1?num=10",
			nil,
		},
		{
			"Substitute query params with space properly",
			map[string]interface{}{"id": "0x9", "name": "Michael Compton",
				"num": 10},
			"http://myapi.com/favMovies/$id?name=$name&num=$num",
			"http://myapi.com/favMovies/0x9?name=Michael+Compton&num=10",
			nil,
		},
		{
			"Substitute query params for variables with array value",
			map[string]interface{}{"ids": []interface{}{1, 2}, "names": []interface{}{"M1", "M2"},
				"check": []interface{}{1, 3.14, "test"}},
			"http://myapi.com/favMovies?id=$ids&name=$names&check=$check",
			"http://myapi.com/favMovies?check=1&check=3.14&check=test&id=1&id=2&name=M1&name=M2",
			nil,
		},
		{
			"Substitute query params for variables with object value",
			map[string]interface{}{"data": map[string]interface{}{"id": 1, "name": "George"}},
			"http://myapi.com/favMovies?author=$data",
			"http://myapi.com/favMovies?author%5Bid%5D=1&author%5Bname%5D=George",
			nil,
		},
		{
			"Substitute query params for variables with array of object value",
			map[string]interface{}{"data": []interface{}{map[string]interface{}{"id": 1,
				"name": "George"}, map[string]interface{}{"id": 2, "name": "Jerry"}}},
			"http://myapi.com/favMovies?author=$data",
			"http://myapi.com/favMovies?author%5Bid%5D=1&author%5Bid%5D=2&author%5Bname%5D=George" +
				"&author%5Bname%5D=Jerry",
			nil,
		},
		{
			"Substitute query params for a variable value that is null as empty",
			map[string]interface{}{"id": "0x9", "name": nil, "num": 10},
			"http://myapi.com/favMovies/$id?name=$name&num=$num",
			"http://myapi.com/favMovies/0x9?name=&num=10",
			nil,
		},
		{
			"Remove query params corresponding to variables that are empty.",
			map[string]interface{}{"id": "0x9", "num": 10},
			"http://myapi.com/favMovies/$id?name=$name&num=$num",
			"http://myapi.com/favMovies/0x9?num=10",
			nil,
		},
		{
			"Substitute multiple path params properly",
			map[string]interface{}{"id": "0x9", "num": 10},
			"http://myapi.com/favMovies/$id/$num",
			"http://myapi.com/favMovies/0x9/10",
			nil,
		},
		{
			"Substitute path params for variables with array value",
			map[string]interface{}{"ids": []interface{}{1, 2}, "names": []interface{}{"M1", "M2"},
				"check": []interface{}{1, 3.14, "test"}},
			"http://myapi.com/favMovies/$ids/$names/$check",
			"http://myapi.com/favMovies/1%2C2/M1%2CM2/1%2C3.14%2Ctest",
			nil,
		},
		{
			"Substitute path params for variables with object value",
			map[string]interface{}{"author": map[string]interface{}{"id": 1, "name": "George"}},
			"http://myapi.com/favMovies/$author",
			"http://myapi.com/favMovies/id%2C1%2Cname%2CGeorge",
			nil,
		},
		{
			"Substitute path params for variables with array of object value",
			map[string]interface{}{"authors": []interface{}{map[string]interface{}{"id": 1,
				"name": "George/"}, map[string]interface{}{"id": 2, "name": "Jerry"}}},
			"http://myapi.com/favMovies/$authors",
			"http://myapi.com/favMovies/id%2C1%2Cname%2CGeorge%2F%2Cid%2C2%2Cname%2CJerry",
			nil,
		},
	}

	for _, test := range tcases {
		t.Run(test.name, func(t *testing.T) {
			b, err := SubstituteVarsInURL(test.url, test.variables)
			if test.expectedErr == nil {
				require.NoError(t, err)
				require.Equal(t, test.expected, b)
			} else {
				require.EqualError(t, err, test.expectedErr.Error())
			}
		})
	}
}

func TestParseRequiredArgsFromGQLRequest(t *testing.T) {
	tcases := []struct {
		name         string
		req          string
		body         string
		requiredArgs map[string]bool
	}{
		{
			"parse required args for single request",
			"query($id: ID!, $age: String!) { userNames(id: $id, age: $age) }",
			"",
			map[string]bool{"id": true, "age": true},
		},
		{
			"parse required nested args for single request",
			"query($id: ID!, $age: String!) { userNames(id: $id, car: {age: $age}) }",
			"",
			map[string]bool{"id": true, "age": true},
		},
	}

	for _, test := range tcases {
		t.Run(test.name, func(t *testing.T) {
			args, err := parseRequiredArgsFromGQLRequest(test.req)
			require.NoError(t, err)
			require.Equal(t, test.requiredArgs, args)
		})
	}
}

// Tests showing that the correct query and variables are sent to the remote server.
type CustomHTTPConfigCase struct {
	Name string
	Type string

	// the query and variables given as input by the user.
	GQLQuery     string
	GQLVariables string
	// our schema against which the above query and variables are resolved.
	GQLSchema string

	// for resolving fields variables are populated from the result of resolving a Dgraph query
	// so RemoteVariables won't have anything.
	InputVariables string
	// remote query and variables which are built as part of the HTTP config and checked.
	RemoteQuery     string
	RemoteVariables string
	// remote schema against which the RemoteQuery and RemoteVariables are validated.
	RemoteSchema string
}

func TestGraphQLQueryInCustomHTTPConfig(t *testing.T) {
	b, err := os.ReadFile("custom_http_config_test.yaml")
	require.NoError(t, err, "Unable to read test file")

	var tests []CustomHTTPConfigCase
	err = yaml.Unmarshal(b, &tests)
	require.NoError(t, err, "Unable to unmarshal tests to yaml.")

	for _, tcase := range tests {
		t.Run(tcase.Name, func(t *testing.T) {
			schHandler, errs := NewHandler(tcase.GQLSchema, false)
			require.NoError(t, errs)
			sch, err := FromString(schHandler.GQLSchema(), x.GalaxyNamespace)
			require.NoError(t, err)

			var vars map[string]interface{}
			if tcase.GQLVariables != "" {
				require.NoError(t, json.Unmarshal([]byte(tcase.GQLVariables), &vars))
			}

			op, err := sch.Operation(
				&Request{
					Query:     tcase.GQLQuery,
					Variables: vars,
				})
			require.NoError(t, err)
			require.NotNil(t, op)

			var field Field
			if tcase.Type == "query" {
				queries := op.Queries()
				require.Len(t, queries, 1)
				field = queries[0]
			} else if tcase.Type == "mutation" {
				mutations := op.Mutations()
				require.Len(t, mutations, 1)
				field = mutations[0]
			} else if tcase.Type == "field" {
				queries := op.Queries()
				require.Len(t, queries, 1)
				q := queries[0]
				require.Len(t, q.SelectionSet(), 1)
				// We are allow checking the custom http config on the first field of the query.
				field = q.SelectionSet()[0]
			}

			c, err := field.CustomHTTPConfig()
			require.NoError(t, err)

			remoteSchemaHandler, errs := NewHandler(tcase.RemoteSchema, false)
			require.NoError(t, errs)
			remoteSchema, err := FromString(remoteSchemaHandler.GQLSchema(), x.GalaxyNamespace)
			require.NoError(t, err)

			// Validate the generated query against the remote schema.
			tmpl, ok := (c.Template).(map[string]interface{})
			require.True(t, ok)

			require.Equal(t, tcase.RemoteQuery, c.RemoteGqlQuery)

			v, _ := tmpl["variables"].(map[string]interface{})
			var rv map[string]interface{}
			if tcase.RemoteVariables != "" {
				require.NoError(t, json.Unmarshal([]byte(tcase.RemoteVariables), &rv))
			}
			require.Equal(t, rv, v)

			if tcase.InputVariables != "" {
				require.NoError(t, json.Unmarshal([]byte(tcase.InputVariables), &v))
			}
			op, err = remoteSchema.Operation(
				&Request{
					Query:     c.RemoteGqlQuery,
					Variables: v,
				})
			require.NoError(t, err)
			require.NotNil(t, op)
		})
	}
}

func TestAllowedHeadersList(t *testing.T) {
	tcases := []struct {
		name      string
		schemaStr string
		expected  string
	}{
		{
			"auth header present in extraCorsHeaders headers list",
			`
	 type X @auth(
        query: {rule: """
          query {
            queryX(filter: { userRole: { eq: "ADMIN" } }) {
              __typename
            }
          }"""
        }
      ) {
        username: String! @id
        userRole: String @search(by: [hash])
	  }
	  # Dgraph.Authorization  {"VerificationKey":"-----BEGIN PUBLIC KEY-----\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAsppQMzPRyYP9KcIAg4CG\nUV3NGCIRdi2PqkFAWzlyo0mpZlHf5Hxzqb7KMaXBt8Yh+1fbi9jcBbB4CYgbvgV0\n7pAZY/HE4ET9LqnjeF2sjmYiGVxLARv8MHXpNLcw7NGcL0FgSX7+B2PB2WjBPnJY\ndvaJ5tsT+AuZbySaJNS1Ha77lW6gy/dmBDybZ1UU+ixRjDWEqPmtD71g2Fpk8fgr\nReNm2h/ZQsJ19onFaGPQN6L6uJR+hfYN0xmOdTC21rXRMUJT8Pw9Xsi6wSt+tI4T\nKxDfMTxKksfjv93dnnof5zJtIcMFQlSKLOrgDC0WP07gVTR2b85tFod80ykevvgu\nAQIDAQAB\n-----END PUBLIC KEY-----","Header":"X-Test-Dgraph","Namespace":"https://dgraph.io/jwt/claims","Algo":"RS256"}
	`,
			"X-Test-Dgraph",
		},
	}
	for _, test := range tcases {
		t.Run(test.name, func(t *testing.T) {
			schHandler, errs := NewHandler(test.schemaStr, false)
			require.NoError(t, errs)
			_, err := FromString(schHandler.GQLSchema(), x.GalaxyNamespace)
			require.NoError(t, err)
			require.Equal(t, strings.Join([]string{x.AccessControlAllowedHeaders, test.expected},
				","), schHandler.MetaInfo().AllowedCorsHeaders())
		})
	}
}

func TestCustomLogicHeaders(t *testing.T) {
	tcases := []struct {
		name      string
		schemaStr string
		err       error
	}{
		{
			"check for introspection header to always use value from secrets",
			`
			type User @remote {
 				description: String
			}

			type Query {
			user(name: String!): User
				@custom(
				http: {
					url: "http://api:8888/graphql"
					method: "POST"
					introspectionHeaders: ["Authorization:Api-Token"]
					graphql: "query($name: String!) { getUser(name: $name) }"
				}
   	 			)
				}
			`,
			errors.New("input:13: Type Query; Field user; introspectionHeaders in @custom directive should use " +
				"secrets to store the header value. " + "To do that specify `Api-Token` in this format " +
				"'#Dgraph.Secret name value' at the bottom of your schema file." + "\n"),
		},
		{
			"check for secret and forward headers overlapping",
			`
			type User @remote {
 				description: String
			}

			type Query {
			user(name: String!): User
				@custom(
				http: {
					url: "http://api:8888/graphql"
					method: "POST"
					forwardHeaders: ["API-Token", "Authorization"]
					secretHeaders: ["Authorization"]
					graphql: "query($name: String!) { getUser(name: $name) }"
				}
   	 			)
				}
			`,
			errors.New("input:14: Type Query; Field user; secretHeaders and forwardHeaders in @custom directive " +
				"cannot have overlapping headers, found: `Authorization`." + "\n"),
		},
		{
			"check for header structure",
			`
			type User @remote {
 				description: String
			}

			type Query {
			user(name: String!): User
				@custom(
				http: {
					url: "http://api:8888/graphql"
					method: "POST"
					forwardHeaders: ["API-Token",  "Content-Type"]
					secretHeaders: ["Authorization:Auth:random"]
					graphql: "query($name: String!) { getUser(name: $name) }"
				}
   	 			)
				}
			`,
			errors.New("input:14: Type Query; Field user; secretHeaders in @custom directive should be of the form " +
				"'remote_headername:local_headername' or just 'headername', found: `Authorization:Auth:random`.\n"),
		},
	}
	for _, test := range tcases {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewHandler(test.schemaStr, false)
			require.EqualError(t, err, test.err.Error())
		})
	}
}

func TestParseSecrets(t *testing.T) {
	tcases := []struct {
		name                   string
		schemaStr              string
		expectedSecrets        map[string]string
		expectedAuthHeader     string
		expectedAllowedOrigins []string
		err                    error
	}{
		{"should be able to parse secrets",
			`
			type User {
				id: ID!
				name: String!
			}

			# Dgraph.Secret  GITHUB_API_TOKEN   "some-super-secret-token"
			# Dgraph.Secret STRIPE_API_KEY "stripe-api-key-value"
		`,
			map[string]string{"GITHUB_API_TOKEN": "some-super-secret-token",
				"STRIPE_API_KEY": "stripe-api-key-value"},
			"",
			nil,
			nil,
		},
		{"should be able to parse secret where schema also has other comments.",
			`
		# Dgraph.Secret  GITHUB_API_TOKEN   "some-super-secret-token"

		type User {
			id: ID!
			name: String!
		}

		# Dgraph.Secret STRIPE_API_KEY "stripe-api-key-value"
		# random comment
	`,
			map[string]string{"GITHUB_API_TOKEN": "some-super-secret-token",
				"STRIPE_API_KEY": "stripe-api-key-value"},
			"",
			nil,
			nil,
		},
		{
			"should throw an error if the secret is not in the correct format",
			`
		type User {
			id: ID!
			name: String!
		}

		# Dgraph.Secret RANDOM_TOKEN
		`,
			nil,
			"",
			nil,
			errors.New("incorrect format for specifying Dgraph secret found for " +
				"comment: `# Dgraph.Secret RANDOM_TOKEN`, it should " +
				"be `# Dgraph.Secret key value`"),
		},
		{
			"Dgraph.Authorization old format",
			`
			type User {
				id: ID!
				name: String!
			}

			# Dgraph.Secret  "GITHUB_API_TOKEN"   "some-super-secret-token"
			# Dgraph.Authorization X-Test-Dgraph https://dgraph.io/jwt/claims HS256 "key"
			# Dgraph.Secret STRIPE_API_KEY "stripe-api-key-value"
			`,
			map[string]string{"GITHUB_API_TOKEN": "some-super-secret-token",
				"STRIPE_API_KEY": "stripe-api-key-value"},
			"X-Test-Dgraph",
			nil,
			nil,
		},
		{
			"Dgraph.Authorization old format error",
			`
			type User {
				id: ID!
				name: String!
			}

			# Dgraph.Secret  "GITHUB_API_TOKEN"   "some-super-secret-token"
			# Dgraph.Authorization X-Test-Dgraph https://dgraph.io/jwt/claims "key"
			# Dgraph.Secret STRIPE_API_KEY "stripe-api-key-value"
			`,
			nil,
			"",
			nil,
			errors.New("input: Invalid `Dgraph.Authorization` format: # Dgraph.Authorization X-Test-Dgraph " +
				"https://dgraph.io/jwt/claims \"key\""),
		},
		{
			"should throw an error if multiple authorization values are specified",
			`
			type User {
				id: ID!
				name: String!
			}

			# Dgraph.Authorization {"VerificationKey":"secretkey","Header":"X-Test-Auth","Namespace":"https://xyz.io/jwt/claims","Algo":"HS256"}
			# Dgraph.Authorization {"VerificationKey":"secretkey","Header":"X-Test-Auth","Namespace":"https://xyz.io/jwt/claims","Algo":"HS256"}
			`,
			nil,
			"",
			nil,
			errors.New(`Dgraph.Authorization should be only be specified once in a schema` +
				`, found second mention: # Dgraph.Authorization {"VerificationKey":"secretkey","Header":"X-Test-Auth","Namespace":"https://xyz.io/jwt/claims","Algo":"HS256"}`),
		},
		{
			"Should throw an error if required fields are missing in Authorizaiton Information",
			`
			type User {
				id: ID!
				name: String!
			}

			# Dgraph.Authorization {}
			`,
			nil,
			"",
			nil,
			errors.New("required field missing in Dgraph.Authorization: " +
				"`Verification key`/`JWKUrl`/`JWKUrls` `Algo` `Header` `Namespace`"),
		},
		{
			"Should be able to parse  Dgraph.Authorization irrespective of spacing between # and Dgraph.Authorization",
			`
			type User {
				id: ID!
				name: String!
			}

			#Dgraph.Authorization {"VerificationKey":"secretkey","Header":"X-Test-Auth","Namespace":"https://xyz.io/jwt/claims","Algo":"HS256","Audience":["aud1","63do0q16n6ebjgkumu05kkeian","aud5"]}
			`,
			map[string]string{},
			"X-Test-Auth",
			nil,
			nil,
		},
		{
			"Valid Dgraph.Authorization with audience field",
			`
			type User {
				id: ID!
				name: String!
			}

			# Dgraph.Authorization {"VerificationKey":"secretkey","Header":"X-Test-Auth","Namespace":"https://xyz.io/jwt/claims","Algo":"HS256","Audience":["aud1","63do0q16n6ebjgkumu05kkeian","aud5"]}
			`,
			map[string]string{},
			"X-Test-Auth",
			nil,
			nil,
		},
		{
			"Valid Dgraph.Authorization without audience field",
			`
			type User {
				id: ID!
				name: String!
			}

			# Dgraph.Authorization {"VerificationKey":"secretkey","Header":"X-Test-Auth","Namespace":"https://xyz.io/jwt/claims","Algo":"HS256"}
			`,
			map[string]string{},
			"X-Test-Auth",
			nil,
			nil,
		},
		{
			"should parse Dgraph.Allow-Origin correctly",
			`
			type User {
				id: ID!
				name: String!
			}

			# Dgraph.Authorization {"VerificationKey":"secretkey","Header":"X-Test-Auth","Namespace":"https://xyz.io/jwt/claims","Algo":"HS256"}
			# Dgraph.Allow-Origin   "https://dgraph.io"
			# Dgraph.Secret  GITHUB_API_TOKEN   "some-super-secret-token"
			`,
			map[string]string{"GITHUB_API_TOKEN": "some-super-secret-token"},
			"X-Test-Auth",
			[]string{"https://dgraph.io"},
			nil,
		},
		{
			"should parse multiple Dgraph.Allow-Origin correctly",
			`
			type User {
				id: ID!
				name: String!
			}

			# Dgraph.Allow-Origin   "https://dgraph.io"
			# Dgraph.Allow-Origin "https://developer.mozilla.org"
			`,
			map[string]string{},
			"",
			[]string{"https://dgraph.io", "https://developer.mozilla.org"},
			nil,
		},
		{
			"should throw error if Dgraph.Allow-Origin has incorrect format",
			`
			type User {
				id: ID!
				name: String!
			}

			# Dgraph.Allow-Origin 1"https://dgraph.io"
			`,
			map[string]string{},
			"",
			nil,
			errors.New("incorrect format for specifying Dgraph.Allow-Origin found for " +
				"comment: `# Dgraph.Allow-Origin 1\"https://dgraph.io\"`, it should " +
				"be `# Dgraph.Allow-Origin \"http://example.com\"`"),
		},
	}
	for _, test := range tcases {
		t.Run(test.name, func(t *testing.T) {
			meta, err := parseMetaInfo(test.schemaStr)
			if test.err != nil || err != nil {
				require.EqualError(t, err, test.err.Error())
				return
			}
			require.NotNil(t, meta)
			require.Len(t, meta.secrets, len(test.expectedSecrets))
			for k, v := range test.expectedSecrets {
				require.Equal(t, v, string(meta.secrets[k]))
			}
			require.Len(t, meta.allowedCorsOrigins, len(test.expectedAllowedOrigins))
			for _, k := range test.expectedAllowedOrigins {
				require.True(t, meta.allowedCorsOrigins[k])
			}
			if test.expectedAuthHeader != "" {
				require.NotNil(t, meta.authMeta)
				require.Equal(t, test.expectedAuthHeader, meta.authMeta.Header)
			}
		})
	}
}
