/*
 * Copyright 2025 Dgraph Labs, Inc. and Contributors
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

package dgraphapi

import "fmt"

const (
	aclPreds = `
{"predicate":"dgraph.xid","type":"string", "index":true, "tokenizer":["exact"], "unique": true, "upsert":true},
{"predicate":"dgraph.password","type":"password"},
{"predicate":"dgraph.user.group","list":true, "reverse":true, "type":"uid"},
{"predicate":"dgraph.acl.rule","type":"uid","list":true},
{"predicate":"dgraph.rule.predicate","type":"string","index":true,"tokenizer":["exact"],"upsert":true},
{"predicate":"dgraph.rule.permission","type":"int"}
`
	otherInternalPreds = `
{"predicate":"dgraph.type","type":"string","index":true,"tokenizer":["exact"],"list":true},
{"predicate":"dgraph.drop.op", "type": "string"},
{"predicate":"dgraph.graphql.p_query","type":"string","index":true,"tokenizer":["sha256"]},
{"predicate":"dgraph.graphql.schema", "type": "string"},
{"predicate":"dgraph.graphql.xid","type":"string","index":true,"tokenizer":["exact"],"upsert":true}
`
	aclTypes = `
{
	"fields": [{"name": "dgraph.password"},{"name": "dgraph.xid"},{"name": "dgraph.user.group"}],
	"name": "dgraph.type.User"
},{
	"fields": [{"name": "dgraph.acl.rule"},{"name": "dgraph.xid"}],
	"name": "dgraph.type.Group"
},{
	"fields": [{"name": "dgraph.rule.predicate"},{"name": "dgraph.rule.permission"}],
	"name": "dgraph.type.Rule"
}
`
	otherInternalTypes = `
{
	"fields": [{"name": "dgraph.graphql.schema"},{"name": "dgraph.graphql.xid"}],
	"name": "dgraph.graphql"
},{
	"fields": [{"name": "dgraph.graphql.p_query"}],
	"name": "dgraph.graphql.persisted_query"
}
`
)

type SchemaOptions struct {
	UserPreds        string
	UserTypes        string
	ExcludeAclSchema bool
}

// GetFullSchemaHTTPResponse returns a string representation of the HTTP response returned by the
// full schema{} query. It uses the user provided predicates and types along with the initial
// internal schema to generate the string. Example response looks like:
//
//	{
//		"data": {
//			"schema": [ ... ],
//			"types": [ ... ]
//		}
//	}
func GetFullSchemaHTTPResponse(opts SchemaOptions) string {
	return `{"data":` + GetFullSchemaJSON(opts) + `}`
}

// GetFullSchemaJSON returns a string representation of the JSON object returned by the full
// schema{} query. It uses the user provided predicates and types along with the initial internal
// schema to generate the string. Example response looks like:
//
//	{
//		"schema": [ ... ],
//		"types": [ ... ]
//	}
func GetFullSchemaJSON(opts SchemaOptions) string {
	expectedPreds := GetInternalPreds(opts.ExcludeAclSchema)
	if len(opts.UserPreds) > 0 {
		expectedPreds += "," + opts.UserPreds
	}

	expectedTypes := GetInternalTypes(opts.ExcludeAclSchema)
	if len(opts.UserTypes) > 0 {
		expectedTypes += "," + opts.UserTypes
	}

	return fmt.Sprintf(`
	{
		"schema": [%s],
		"types": [%s]
	}`, expectedPreds, expectedTypes)
}

func GetInternalPreds(excludeAclPreds bool) string {
	if excludeAclPreds {
		return otherInternalPreds
	}
	return aclPreds + "," + otherInternalPreds
}

func GetInternalTypes(excludeAclTypes bool) string {
	if excludeAclTypes {
		return otherInternalTypes
	}
	return aclTypes + "," + otherInternalTypes
}
