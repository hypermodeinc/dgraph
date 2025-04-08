//go:build integration || upgrade
// +build integration upgrade

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package acl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/dgraphtest"
	"github.com/hypermodeinc/dgraph/v24/x"
)

var (
	userid       = "alice"
	userpassword = "simplepassword"
)

const (

	// This is the groot schema before adding @unique directive to the dgraph.xid predicate
	oldGrootSchema = `{
	"schema": [
	  {
		"predicate": "dgraph.acl.rule",
		"type": "uid",
		"list": true
	  },
	  {
		  "predicate":"dgraph.drop.op",
		  "type":"string"
	  },
	  {
		  "predicate":"dgraph.graphql.p_query",
		  "type":"string",
		  "index":true,
		  "tokenizer":["sha256"]
	  },
	  {
		"predicate": "dgraph.graphql.schema",
		"type": "string"
	  },
	  {
		"predicate": "dgraph.graphql.xid",
		"type": "string",
		"index": true,
		"tokenizer": [
		  "exact"
		],
		"upsert": true
	  },
	  {
		"predicate": "dgraph.password",
		"type": "password"
	  },
	  {
		"predicate": "dgraph.rule.permission",
		"type": "int"
	  },
	  {
		"predicate": "dgraph.rule.predicate",
		"type": "string",
		"index": true,
		"tokenizer": [
		  "exact"
		],
		"upsert": true
	  },
	  {
		"predicate": "dgraph.type",
		"type": "string",
		"index": true,
		"tokenizer": [
		  "exact"
		],
		"list": true
	  },
	  {
		"predicate": "dgraph.user.group",
		"type": "uid",
		"reverse": true,
		"list": true
	  },
	  {
		"predicate": "dgraph.xid",
		"type": "string",
		"index": true,
		"tokenizer": [
		  "exact"
		],
		"upsert": true
	  }
	],
	"types": [
	  {
		"fields": [
		  {
			"name": "dgraph.graphql.schema"
		  },
		  {
			"name": "dgraph.graphql.xid"
		  }
		],
		"name": "dgraph.graphql"
	  },
	  {
		  "fields": [
			  {
				  "name": "dgraph.graphql.p_query"
			  }
		  ],
		  "name": "dgraph.graphql.persisted_query"
	  },
	  {
		"fields": [
		  {
			"name": "dgraph.xid"
		  },
		  {
			"name": "dgraph.acl.rule"
		  }
		],
		"name": "dgraph.type.Group"
	  },
	  {
		"fields": [
		  {
			"name": "dgraph.rule.predicate"
		  },
		  {
			"name": "dgraph.rule.permission"
		  }
		],
		"name": "dgraph.type.Rule"
	  },
	  {
		"fields": [
		  {
			"name": "dgraph.xid"
		  },
		  {
			"name": "dgraph.password"
		  },
		  {
			"name": "dgraph.user.group"
		  }
		],
		"name": "dgraph.type.User"
	  }
	]
  }`

	// This is the groot schema after adding @unique directive to the dgraph.xid predicate
	uniqueXidGrootSchema = `{
	"schema": [
	  {
		"predicate": "dgraph.acl.rule",
		"type": "uid",
		"list": true
	  },
	  {
		  "predicate":"dgraph.drop.op",
		  "type":"string"
	  },
	  {
		  "predicate":"dgraph.graphql.p_query",
		  "type":"string",
		  "index":true,
		  "tokenizer":["sha256"]
	  },
	  {
		"predicate": "dgraph.graphql.schema",
		"type": "string"
	  },
	  {
		"predicate": "dgraph.graphql.xid",
		"type": "string",
		"index": true,
		"tokenizer": [
		  "exact"
		],
		"upsert": true
		},
	  {
		"predicate": "dgraph.password",
		"type": "password"
	  },
	  {
		"predicate": "dgraph.rule.permission",
		"type": "int"
	  },
	  {
		"predicate": "dgraph.rule.predicate",
		"type": "string",
		"index": true,
		"tokenizer": [
		  "exact"
		],
		"upsert": true
	  },
	  {
		"predicate": "dgraph.type",
		"type": "string",
		"index": true,
		"tokenizer": [
		  "exact"
		],
		"list": true
	  },
	  {
		"predicate": "dgraph.user.group",
		"type": "uid",
		"reverse": true,
		"list": true
	  },
	  {
		"predicate": "dgraph.xid",
		"type": "string",
		"index": true,
		"tokenizer": [
		  "exact"
		],
		"upsert": true,
		"unique": true
	  }
	],
	"types": [
	  {
		"fields": [
		  {
			"name": "dgraph.graphql.schema"
		  },
		  {
			"name": "dgraph.graphql.xid"
		  }
		],
		"name": "dgraph.graphql"
	  },
	  {
		  "fields": [
			  {
				  "name": "dgraph.graphql.p_query"
			  }
		  ],
		  "name": "dgraph.graphql.persisted_query"
	  },
	  {
		"fields": [
		  {
			"name": "dgraph.xid"
		  },
		  {
			"name": "dgraph.acl.rule"
		  }
		],
		"name": "dgraph.type.Group"
	  },
	  {
		"fields": [
		  {
			"name": "dgraph.rule.predicate"
		  },
		  {
			"name": "dgraph.rule.permission"
		  }
		],
		"name": "dgraph.type.Rule"
	  },
	  {
		"fields": [
		  {
			"name": "dgraph.xid"
		  },
		  {
			"name": "dgraph.password"
		  },
		  {
			"name": "dgraph.user.group"
		  }
		],
		"name": "dgraph.type.User"
	  }
	]
  }`

	grootSchemaWithNamespaceSchema = `{
	"schema": [
	  {
		"predicate": "dgraph.acl.rule",
		"type": "uid",
		"list": true
	  },
	  {
		  "predicate":"dgraph.drop.op",
		  "type":"string"
	  },
	  {
		  "predicate":"dgraph.graphql.p_query",
		  "type":"string",
		  "index":true,
		  "tokenizer":["sha256"]
	  },
	  {
		"predicate": "dgraph.graphql.schema",
		"type": "string"
	  },
	  {
		"predicate": "dgraph.graphql.xid",
		"type": "string",
		"index": true,
		"tokenizer": [
		  "exact"
		],
		"upsert": true
	  },
	  {
		"predicate": "dgraph.namespace.id",
		"type": "int",
		"index": true,
		"tokenizer": [
		  "int"
		],
		"upsert": true,
		"unique": true
	  },
	  {
		"predicate": "dgraph.namespace.name",
		"type": "string",
		"index": true,
		"tokenizer": [
		  "exact"
		],
		"upsert": true,
		"unique": true
	  },
	  {
		"predicate": "dgraph.password",
		"type": "password"
	  },
	  {
		"predicate": "dgraph.rule.permission",
		"type": "int"
	  },
	  {
		"predicate": "dgraph.rule.predicate",
		"type": "string",
		"index": true,
		"tokenizer": [
		  "exact"
		],
		"upsert": true
	  },
	  {
		"predicate": "dgraph.type",
		"type": "string",
		"index": true,
		"tokenizer": [
		  "exact"
		],
		"list": true
	  },
	  {
		"predicate": "dgraph.user.group",
		"type": "uid",
		"reverse": true,
		"list": true
	  },
	  {
		"predicate": "dgraph.xid",
		"type": "string",
		"index": true,
		"tokenizer": [
		  "exact"
		],
		"upsert": true,
		"unique": true
	  }
	],
	"types": [
	  {
		"fields": [
		  {
			"name": "dgraph.graphql.schema"
		  },
		  {
			"name": "dgraph.graphql.xid"
		  }
		],
		"name": "dgraph.graphql"
	  },
	  {
		  "fields": [
			  {
				  "name": "dgraph.graphql.p_query"
			  }
		  ],
		  "name": "dgraph.graphql.persisted_query"
	  },
	  {
	    "fields": [
		  {
			"name": "dgraph.namespace.name"
		  },
		  {
			"name": "dgraph.namespace.id"
		  }
		],
		"name": "dgraph.namespace"
	  },
	  {
		"fields": [
		  {
			"name": "dgraph.xid"
		  },
		  {
			"name": "dgraph.acl.rule"
		  }
		],
		"name": "dgraph.type.Group"
	  },
	  {
		"fields": [
		  {
			"name": "dgraph.rule.predicate"
		  },
		  {
			"name": "dgraph.rule.permission"
		  }
		],
		"name": "dgraph.type.Rule"
	  },
	  {
		"fields": [
		  {
			"name": "dgraph.xid"
		  },
		  {
			"name": "dgraph.password"
		  },
		  {
			"name": "dgraph.user.group"
		  }
		],
		"name": "dgraph.type.User"
	  }
	]
  }`
)

func (asuite *AclTestSuite) TestGetCurrentUser() {
	t := asuite.T()

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace), "login failed")
	currentUser, err := hc.GetCurrentUser()
	require.NoError(t, err)
	require.Equal(t, currentUser, "groot")

	// clean up the user to allow repeated running of this test
	userid := "hamilton"
	require.NoError(t, hc.DeleteUser(userid), "error while deleteing user")

	user, err := hc.CreateUser(userid, userpassword)
	require.NoError(t, err)
	require.Equal(t, userid, user)

	asuite.Upgrade()

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(userid, userpassword, x.GalaxyNamespace), "login failed")
	currentUser, err = hc.GetCurrentUser()
	require.NoError(t, err)
	require.Equal(t, currentUser, userid)
}

func (asuite *AclTestSuite) TestCreateAndDeleteUsers() {
	t := asuite.T()

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	resetUser(t, hc)

	// adding the user again should fail
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))
	user, err := hc.CreateUser(userid, userpassword)
	require.Error(t, err)
	require.Contains(t, err.Error(), "because id alice already exists")
	require.Equal(t, "", user)

	asuite.Upgrade()

	// delete the user
	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))
	require.NoError(t, hc.DeleteUser(userid), "error while deleteing user")
	user, err = hc.CreateUser(userid, userpassword)
	require.NoError(t, err)
	require.Equal(t, userid, user)
}

func resetUser(t *testing.T, hc *dgraphapi.HTTPClient) {
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	// clean up the user to allow repeated running of this test
	require.NoError(t, hc.DeleteUser(userid), "error while deleteing user")

	user, err := hc.CreateUser(userid, userpassword)
	require.NoError(t, err)
	require.Equal(t, userid, user)
}

func (asuite *AclTestSuite) TestPreDefinedPredicates() {
	t := asuite.T()

	// This test uses the groot account to ensure that pre-defined predicates
	// cannot be altered even if the permissions allow it.
	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	ctx := context.Background()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))
	alterPreDefinedPredicates(t, gc.Dgraph, asuite.dc.GetVersion())
}

func (asuite *AclTestSuite) TestPreDefinedTypes() {
	t := asuite.T()

	// This test uses the groot account to ensure that pre-defined types
	// cannot be altered even if the permissions allow it.
	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	ctx := context.Background()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	alterPreDefinedTypes(t, gc.Dgraph)
}

func (asuite *AclTestSuite) TestAuthorization() {
	t := asuite.T()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)

	testAuthorization(t, gc, hc, asuite)
}

func getGrootAndGuardiansUid(t *testing.T, gc *dgraphapi.GrpcClient) (string, string) {
	grootUserQuery := `
	{
		grootUser(func:eq(dgraph.xid, "groot")){
			uid
		}
	}`

	// Structs to parse groot user query response
	type userQryResp struct {
		GrootUser []struct {
			Uid string `json:"uid"`
		} `json:"grootUser"`
	}

	resp, err := gc.Query(grootUserQuery)
	require.NoError(t, err, "groot user query failed")

	var userResp userQryResp
	if err := json.Unmarshal(resp.GetJson(), &userResp); err != nil {
		t.Fatal("Couldn't unmarshal response from groot user query")
	}
	grootUserUid := userResp.GrootUser[0].Uid

	guardiansGroupQuery := `
	{
		guardiansGroup(func:eq(dgraph.xid, "guardians")){
			uid
		}
	}`

	// Structs to parse guardians group query response
	type groupQryResp struct {
		GuardiansGroup []struct {
			Uid string `json:"uid"`
		} `json:"guardiansGroup"`
	}

	resp, err = gc.Query(guardiansGroupQuery)
	require.NoError(t, err, "guardians group query failed")

	var groupResp groupQryResp
	if err := json.Unmarshal(resp.GetJson(), &groupResp); err != nil {
		t.Fatal("Couldn't unmarshal response from guardians group query")
	}
	guardiansGroupUid := groupResp.GuardiansGroup[0].Uid

	return grootUserUid, guardiansGroupUid
}

const (
	defaultTimeToSleep = 500 * time.Millisecond
	timeout            = 5 * time.Second
	expireJwtSleep     = 21 * time.Second
)

func testAuthorization(t *testing.T, gc *dgraphapi.GrpcClient, hc *dgraphapi.HTTPClient, asuite *AclTestSuite) {
	createAccountAndData(t, gc, hc)
	asuite.Upgrade()

	gc, cleanup, err := asuite.dc.Client()
	defer cleanup()
	require.NoError(t, err)
	require.NoError(t, gc.LoginIntoNamespace(context.Background(), userid, userpassword, x.GalaxyNamespace))

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	// initially the query should return empty result, mutate and alter
	// operations should all fail when there are no rules defined on the predicates
	queryWithShouldFail(t, gc, false, query)
	mutatePredicateWithUserAccount(t, gc, true)
	alterPredicateWithUserAccount(t, gc, true)
	createGroupAndAcls(t, unusedGroup, false, hc)

	// wait for 5 seconds to ensure the new acl have reached all acl caches
	time.Sleep(defaultTimeToSleep)

	// now all these operations except query should fail since
	// there are rules defined on the unusedGroup
	queryWithShouldFail(t, gc, false, query)
	mutatePredicateWithUserAccount(t, gc, true)
	alterPredicateWithUserAccount(t, gc, true)

	// create the dev group and add the user to it
	createGroupAndAcls(t, devGroup, true, hc)

	// wait for 5 seconds to ensure the new acl have reached all acl caches
	time.Sleep(defaultTimeToSleep)

	// now the operations should succeed again through the devGroup
	queryWithShouldFail(t, gc, false, query)

	// sleep long enough (10s per the docker-compose.yml)
	// for the accessJwt to expire in order to test auto login through refresh jwt
	time.Sleep(expireJwtSleep)
	mutatePredicateWithUserAccount(t, gc, false)

	time.Sleep(expireJwtSleep)
	alterPredicateWithUserAccount(t, gc, false)
}

const (
	predicateToRead  = "predicate_to_read"
	queryAttr        = "name"
	predicateToWrite = "predicate_to_write"
	predicateToAlter = "predicate_to_alter"
	devGroup         = "dev"
	sreGroup         = "sre"
	unusedGroup      = "unusedGroup"
	schemaQuery      = "schema {}"
)

var (
	query = fmt.Sprintf(`
	{
		q(func: eq(%s, "SF")) {
			%s
		}
	}`, predicateToRead, queryAttr)
)

func alterPreDefinedPredicates(t *testing.T, dg *dgo.Dgraph, clusterVersion string) {
	ctx := context.Background()

	// Commit 532df27a09ba25f88687bab344e3add2b81b5c23 represents the latest update to the main branch.
	// In this commit, the @unique directive is not applied to ACL predicates.
	// Therefore, we are now deciding which schema to test.
	// 'newGrootSchema' refers to the default schema with the @unique directive defined on ACL predicates.
	// 'oldGrootSchema' refers to the default schema without the @unique directive on ACL predicates.
	supported, err := dgraphtest.IsHigherVersion(clusterVersion, "532df27a09ba25f88687bab344e3add2b81b5c23")
	require.NoError(t, err)
	if supported {
		require.NoError(t, dg.Alter(ctx, &api.Operation{
			Schema: "dgraph.xid: string @index(exact) @upsert @unique .",
		}))
	} else {
		require.NoError(t, dg.Alter(ctx, &api.Operation{
			Schema: "dgraph.xid: string @index(exact) @upsert .",
		}))
	}

	err = dg.Alter(ctx, &api.Operation{Schema: "dgraph.xid: int ."})
	require.Error(t, err)
	require.Contains(t, err.Error(), "predicate dgraph.xid is pre-defined and is not allowed to be modified")

	err = dg.Alter(ctx, &api.Operation{DropAttr: "dgraph.xid"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "predicate dgraph.xid is pre-defined and is not allowed to be dropped")

	// Test that pre-defined predicates act as case-insensitive.
	err = dg.Alter(ctx, &api.Operation{Schema: "dgraph.XID: int ."})
	require.Error(t, err)
	require.Contains(t, err.Error(), "predicate dgraph.XID is pre-defined and is not allowed to be modified")
}

func alterPreDefinedTypes(t *testing.T, dg *dgo.Dgraph) {
	ctx := context.Background()

	// Test that alter requests are allowed if the new update is the same as
	// the initial update for a pre-defined type.
	err := dg.Alter(ctx, &api.Operation{
		Schema: `
			type dgraph.type.Group {
				dgraph.xid
				dgraph.acl.rule
			}
		`,
	})
	require.NoError(t, err)

	err = dg.Alter(ctx, &api.Operation{
		Schema: `
			type dgraph.type.Group {
				dgraph.xid
			}
		`,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "type dgraph.type.Group is pre-defined and is not allowed to be modified")

	err = dg.Alter(ctx, &api.Operation{
		DropOp:    api.Operation_TYPE,
		DropValue: "dgraph.type.Group",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "type dgraph.type.Group is pre-defined and is not allowed to be dropped")
}

func queryWithShouldFail(t *testing.T, gc *dgraphapi.GrpcClient, shouldFail bool, query string) {
	_, err := gc.Query(query)
	if shouldFail {
		require.Error(t, err, "the query should have failed")
	} else {
		require.NoError(t, err, "the query should have succeeded")
	}
}

func mutatePredicateWithUserAccount(t *testing.T, gc *dgraphapi.GrpcClient, shouldFail bool) {
	mu := &api.Mutation{SetNquads: []byte(fmt.Sprintf(`_:a <%s>  "string" .`, predicateToWrite)), CommitNow: true}
	_, err := gc.Mutate(mu)
	if shouldFail {
		require.Error(t, err, "the mutation should have failed")
	} else {
		require.NoError(t, err, "the mutation should have succeeded")
	}
}

func alterPredicateWithUserAccount(t *testing.T, gc *dgraphapi.GrpcClient, shouldFail bool) {
	err := gc.Alter(context.Background(), &api.Operation{Schema: fmt.Sprintf(`%s: int .`, predicateToAlter)})
	if shouldFail {
		require.Error(t, err, "the alter should have failed")
	} else {
		require.NoError(t, err, "the alter should have succeeded")
	}
}

func createAccountAndData(t *testing.T, gc *dgraphapi.GrpcClient, hc *dgraphapi.HTTPClient) {
	// use the groot account to clean the database
	require.NoError(t, gc.LoginIntoNamespace(context.Background(), dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.DropAll(), "Unable to cleanup db")
	require.NoError(t, gc.Alter(context.Background(), &api.Operation{
		Schema: fmt.Sprintf(`%s: string @index(exact) .`, predicateToRead),
	}))
	// wait for 5 seconds to ensure the new acl have reached all acl caches
	time.Sleep(defaultTimeToSleep)

	// create some data, e.g. user with name alice
	resetUser(t, hc)

	mu := &api.Mutation{SetNquads: []byte(fmt.Sprintf("_:a <%s> \"SF\" .", predicateToRead)), CommitNow: true}
	_, err := gc.Mutate(mu)
	require.NoError(t, err)
}

func createGroupAndAcls(t *testing.T, group string, addUserToGroup bool, hc *dgraphapi.HTTPClient) {
	// create a new group
	createdGroup, err := hc.CreateGroup(group)
	require.NoError(t, err)
	require.Equal(t, group, createdGroup)

	// add the user to the group
	if addUserToGroup {
		require.NoError(t, hc.AddUserToGroup(userid, group))
	}

	rules := []dgraphapi.AclRule{
		{
			Predicate: predicateToRead, Permission: Read.Code,
		},
		{
			Predicate: queryAttr, Permission: Read.Code,
		},
		{
			Predicate: predicateToWrite, Permission: Write.Code,
		},
		{
			Predicate: predicateToAlter, Permission: Modify.Code,
		},
	}

	// add READ permission on the predicateToRead to the group
	// also add read permission to the attribute queryAttr, which is used inside the query block
	// add WRITE permission on the predicateToWrite
	// add MODIFY permission on the predicateToAlter
	require.NoError(t, hc.AddRulesToGroup(group, rules, true))
}

func (asuite *AclTestSuite) TestPredicatePermission() {
	t := asuite.T()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	createAccountAndData(t, gc, hc)

	asuite.Upgrade()

	gc, cleanup, err = asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()

	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))
	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.LoginIntoNamespace(ctx, userid, userpassword, x.GalaxyNamespace),
		"Logging in with the current password should have succeeded")

	// Schema query is allowed to all logged in users.
	queryWithShouldFail(t, gc, false, schemaQuery)
	// The query should return emptry response, alter and mutation
	// should be blocked when no rule is defined.
	queryWithShouldFail(t, gc, false, query)
	mutatePredicateWithUserAccount(t, gc, true)
	alterPredicateWithUserAccount(t, gc, true)
	createGroupAndAcls(t, unusedGroup, false, hc)

	// Wait for 5 seconds to ensure the new acl have reached all acl caches.
	time.Sleep(defaultTimeToSleep)

	// The operations except query should fail when there is a rule defined, but the
	// current user is not allowed.
	queryWithShouldFail(t, gc, false, query)
	mutatePredicateWithUserAccount(t, gc, true)
	alterPredicateWithUserAccount(t, gc, true)
	// Schema queries should still succeed since they are not tied to specific predicates.
	queryWithShouldFail(t, gc, false, schemaQuery)
}

func (asuite *AclTestSuite) TestAccessWithoutLoggingIn() {
	t := asuite.T()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	createAccountAndData(t, gc, hc)

	asuite.Upgrade()

	gc, cleanup, err = asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()

	// Without logging in, the anonymous user should be evaluated as if the user does not
	// belong to any group, and access should not be granted if there is no ACL rule defined
	// for a predicate.
	queryWithShouldFail(t, gc, true, query)
	mutatePredicateWithUserAccount(t, gc, true)
	alterPredicateWithUserAccount(t, gc, true)

	// Schema queries should fail if the user has not logged in.
	queryWithShouldFail(t, gc, true, schemaQuery)
}

func (asuite *AclTestSuite) TestUnauthorizedDeletion() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.DropAll())

	unAuthPred := "unauthorizedPredicate"
	op := api.Operation{Schema: fmt.Sprintf("%s: string @index(exact) .", unAuthPred)}
	require.NoError(t, gc.Alter(ctx, &op))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	resetUser(t, hc)

	createdGroup, err := hc.CreateGroup(devGroup)
	require.NoError(t, err)
	require.Equal(t, devGroup, createdGroup)
	require.NoError(t, hc.AddUserToGroup(userid, devGroup))

	mu := &api.Mutation{SetNquads: []byte(fmt.Sprintf("_:a <%s> \"testdata\" .", unAuthPred)), CommitNow: true}
	resp, err := gc.Mutate(mu)
	require.NoError(t, err)
	nodeUID, ok := resp.Uids["a"]
	require.True(t, ok)

	require.NoError(t, hc.AddRulesToGroup(devGroup, []dgraphapi.AclRule{{Predicate: unAuthPred, Permission: 0}}, true))

	asuite.Upgrade()

	userClient, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()

	time.Sleep(defaultTimeToSleep)

	require.NoError(t, userClient.LoginIntoNamespace(ctx, userid, userpassword, x.GalaxyNamespace))
	mu = &api.Mutation{
		DelNquads: []byte(fmt.Sprintf("%s %s %s .", "<"+nodeUID+">", "<"+unAuthPred+">", "*")),
		CommitNow: true,
	}
	_, err = userClient.Mutate(mu)
	require.Error(t, err)
	require.Contains(t, err.Error(), "PermissionDenied")
}

func (asuite *AclTestSuite) TestGuardianAccess() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.DropAll())
	op := api.Operation{Schema: "unauthpred: string @index(exact) ."}
	require.NoError(t, gc.Dgraph.Alter(ctx, &op))

	user, err := hc.CreateUser("guardian", "guardianpass")
	require.NoError(t, err)
	require.Equal(t, "guardian", user)
	require.NoError(t, hc.AddUserToGroup("guardian", "guardians"))

	mu := &api.Mutation{
		SetNquads: []byte("_:a <unauthpred> \"testdata\" ."),
		CommitNow: true,
	}
	resp, err := gc.Mutate(mu)
	require.NoError(t, err)

	nodeUID, ok := resp.Uids["a"]
	require.True(t, ok)

	asuite.Upgrade()

	time.Sleep(defaultTimeToSleep)
	gClient, cleanup, err := asuite.dc.Client()
	require.NoError(t, err, "Error while creating client")
	defer cleanup()

	require.NoError(t, gClient.LoginIntoNamespace(ctx, "guardian", "guardianpass", x.GalaxyNamespace))
	mu = &api.Mutation{
		SetNquads: []byte(fmt.Sprintf("<%s> <unauthpred> \"testdata\" .", nodeUID)),
		CommitNow: true,
	}
	_, err = gClient.Mutate(mu)
	require.NoError(t, err, "Error while mutating unauthorized predicate")

	query := `
	{
		me(func: eq(unauthpred, "testdata")) {
			uid
		}
	}`
	resp, err = gClient.Query(query)
	require.NoError(t, err, "Error while querying unauthorized predicate")
	require.Contains(t, string(resp.GetJson()), "uid")

	op = api.Operation{Schema: "unauthpred: int ."}
	require.NoError(t, gClient.Alter(ctx, &op), "Error while altering unauthorized predicate")

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, hc.RemoveUserFromGroup("guardian", "guardians"))

	_, err = gClient.NewTxn().Query(ctx, query)
	require.Error(t, err, "Query succeeded. It should have failed.")
}

func (asuite *AclTestSuite) TestQueryRemoveUnauthorizedPred() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.DropAll())

	op := api.Operation{Schema: `
		name	 : string @index(exact) .
		nickname : string @index(exact) .
		age 	 : int .
		type TypeName {
			name: string
			age: int
		}
	`}
	require.NoError(t, gc.Alter(ctx, &op))

	resetUser(t, hc)
	createdGroup, err := hc.CreateGroup(devGroup)
	require.NoError(t, err)
	require.Equal(t, devGroup, createdGroup)
	require.NoError(t, hc.AddUserToGroup(userid, devGroup))

	mu := &api.Mutation{
		SetNquads: []byte(`
			_:a <name> "RandomGuy" .
			_:a <age> "23" .
			_:a <nickname> "RG" .
			_:a <dgraph.type> "TypeName" .
			_:b <name> "RandomGuy2" .
			_:b <age> "25" .
			_:b <nickname> "RG2" .
			_:b <dgraph.type> "TypeName" .`),
		CommitNow: true,
	}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	// give read access of <name> to alice
	require.NoError(t, hc.AddRulesToGroup(devGroup,
		[]dgraphapi.AclRule{{Predicate: "name", Permission: Read.Code}}, true))
	asuite.Upgrade()
	userClient, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()

	time.Sleep(defaultTimeToSleep)
	require.NoError(t, userClient.LoginIntoNamespace(ctx, userid, userpassword, x.GalaxyNamespace))

	tests := []struct {
		input       string
		output      string
		description string
	}{
		{
			`
			{
				me(func: has(name), orderasc: name) {
					name
					age
				}
			}
			`,
			`{"me":[{"name":"RandomGuy"},{"name":"RandomGuy2"}]}`,
			"alice doesn't have access to <age>",
		},
		{
			`
			{
				me(func: has(age), orderasc: name) {
					name
					age
				}
			}
			`,
			`{}`,
			`alice doesn't have access to <age> so "has(age)" is unauthorized`,
		},
		{
			`
			{
				me1(func: has(name), orderdesc: age) {
					age
				}
				me2(func: has(name), orderasc: age) {
					age
				}
			}
			`,
			`{"me1":[],"me2":[]}`,
			`me1, me2 will have same results, can't order by <age> since it is unauthorized`,
		},
		{
			`
			{
				me(func: has(name), orderasc: name) @groupby(age) {
					count(name)
				}
			}
			`,
			`{}`,
			`can't groupby <age> since <age> is unauthorized`,
		},
		{
			`
			{
				me(func: has(name), orderasc: name) @filter(eq(nickname, "RG")) {
					name
					age
				}
			}
			`,
			`{"me":[{"name":"RandomGuy"},{"name":"RandomGuy2"}]}`,
			`filter won't work because <nickname> is unauthorized`,
		},
		{
			`
			{
				me(func: has(name)) {
					expand(_all_)
				}
			}
			`,
			`{"me":[{"name":"RandomGuy"},{"name":"RandomGuy2"}]}`,
			`expand(_all_) expands to only <name> because other predicates are unauthorized`,
		},
	}

	for _, tc := range tests {
		tc := tc // capture range variable
		t.Run(tc.description, func(t *testing.T) {
			// testify does not support subtests running in parallel with suite package
			// t.Parallel()
			require.NoError(t, dgraphapi.PollTillPassOrTimeout(userClient, tc.input, tc.output, timeout))
		})
	}
}

func (asuite *AclTestSuite) TestExpandQueryWithACLPermissions() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.DropAll())

	op := api.Operation{Schema: `
		name	 : string @index(exact) .
		nickname : string @index(exact) .
		age 	 : int .
		type TypeName {
			name: string
			nickname: string
			age: int
		}
	`}
	require.NoError(t, gc.Alter(ctx, &op))

	resetUser(t, hc)

	createdGroup, err := hc.CreateGroup(devGroup)
	require.NoError(t, err)
	require.Equal(t, devGroup, createdGroup)
	createdGroup, err = hc.CreateGroup(sreGroup)
	require.NoError(t, err)
	require.Equal(t, sreGroup, createdGroup)
	require.NoError(t, hc.AddRulesToGroup(sreGroup, []dgraphapi.AclRule{{Predicate: "age", Permission: Read.Code},
		{Predicate: "name", Permission: Write.Code}}, true))

	require.NoError(t, hc.AddUserToGroup(userid, devGroup))

	mu := &api.Mutation{
		SetNquads: []byte(`
			_:a <name> "RandomGuy" .
			_:a <age> "23" .
			_:a <nickname> "RG" .
			_:a <dgraph.type> "TypeName" .
			_:b <name> "RandomGuy2" .
			_:b <age> "25" .
			_:b <nickname> "RG2" .
			_:b <dgraph.type> "TypeName" .`),
		CommitNow: true,
	}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	// Test that groot has access to all the predicates
	query := "{me(func: has(name)){expand(_all_)}}"
	resp, err := gc.Query(query)
	require.NoError(t, err, "Error while querying data")
	require.NoError(t, dgraphapi.CompareJSON(
		`{"me":[{"name":"RandomGuy","age":23, "nickname":"RG"},{"name":"RandomGuy2","age":25, "nickname":"RG2"}]}`,
		string(resp.GetJson())))

	asuite.Upgrade()

	userClient, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	time.Sleep(defaultTimeToSleep)

	require.NoError(t, userClient.LoginIntoNamespace(ctx, userid, userpassword, x.GalaxyNamespace))

	// Query via user when user has no permissions
	require.NoError(t, dgraphapi.PollTillPassOrTimeout(userClient, query, `{}`, timeout))

	// Give read access of <name>, write access of <age> to dev
	require.NoError(t, hc.AddRulesToGroup(devGroup, []dgraphapi.AclRule{{Predicate: "age", Permission: Write.Code},
		{Predicate: "name", Permission: Read.Code}}, true))

	require.NoError(t, dgraphapi.PollTillPassOrTimeout(userClient, query,
		`{"me":[{"name":"RandomGuy"},{"name":"RandomGuy2"}]}`, timeout))

	// Login to groot to modify accesses (2)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))
	// Add alice to sre group which has read access to <age> and write access to <name>
	require.NoError(t, hc.AddUserToGroup(userid, sreGroup))

	require.NoError(t, dgraphapi.PollTillPassOrTimeout(userClient, query,
		`{"me":[{"name":"RandomGuy","age":23},{"name":"RandomGuy2","age":25}]}`, timeout))

	// Login to groot to modify accesses (3)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	// Give read access of <name> and <nickname>, write access of <age> to dev
	require.NoError(t, hc.AddRulesToGroup(devGroup, []dgraphapi.AclRule{{Predicate: "age", Permission: Write.Code},
		{Predicate: "name", Permission: Read.Code}, {Predicate: "nickname", Permission: Read.Code}}, true))

	require.NoError(t, dgraphapi.PollTillPassOrTimeout(userClient, query,
		`{"me":[{"name":"RandomGuy","age":23, "nickname":"RG"},{"name":"RandomGuy2","age":25, "nickname":"RG2"}]}`,
		timeout))
}

func (asuite *AclTestSuite) TestDeleteQueryWithACLPermissions() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.DropAll())

	op := api.Operation{Schema: `
		name	 : string @index(exact) .
		nickname : string @index(exact) .
		age 	 : int .
		type Person {
			name: string
			nickname: string
			age: int
		}
	`}
	require.NoError(t, gc.Alter(ctx, &op))

	resetUser(t, hc)
	createdGroup, err := hc.CreateGroup(devGroup)
	require.NoError(t, err)
	require.Equal(t, devGroup, createdGroup)
	require.NoError(t, hc.AddUserToGroup(userid, devGroup))

	mu := &api.Mutation{
		SetNquads: []byte(`
			_:a <name> "RandomGuy" .
			_:a <age> "23" .
			_:a <nickname> "RG" .
			_:a <dgraph.type> "Person" .
			_:b <name> "RandomGuy2" .
			_:b <age> "25" .
			_:b <nickname> "RG2" .
			_:b <dgraph.type> "Person" .`),
		CommitNow: true,
	}
	resp, err := gc.Mutate(mu)
	require.NoError(t, err)

	nodeUID := resp.Uids["a"]
	query := `{q1(func: type(Person)){
		expand(_all_)
    }}`

	// Test that groot has access to all the predicates
	resp, err = gc.Query(query)
	require.NoError(t, err, "Error while querying data")
	require.NoError(t, dgraphapi.CompareJSON(
		`{"q1":[{"name":"RandomGuy","age":23, "nickname": "RG"},{"name":"RandomGuy2","age":25,  "nickname": "RG2"}]}`,
		string(resp.GetJson())))

	// Give Write Access to alice for name and age predicate
	require.NoError(t, hc.AddRulesToGroup(devGroup, []dgraphapi.AclRule{{Predicate: "name", Permission: Write.Code},
		{Predicate: "age", Permission: Write.Code}}, true))

	asuite.Upgrade()

	gc, _, err = asuite.dc.Client()
	require.NoError(t, err)
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	userClient, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	time.Sleep(defaultTimeToSleep)
	require.NoError(t, userClient.LoginIntoNamespace(ctx, userid, userpassword, x.GalaxyNamespace))

	mu = &api.Mutation{DelNquads: []byte(fmt.Sprintf("%s %s %s .", "<"+nodeUID+">", "*", "*")), CommitNow: true}
	// delete S * * (user now has permission to name and age)
	_, err = userClient.Mutate(mu)
	require.NoError(t, err)

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	resp, err = gc.Query(query)
	require.NoError(t, err, "Error while querying data")
	// Only name and age predicates got deleted via user - alice
	require.NoError(t, dgraphapi.CompareJSON(
		`{"q1":[{"nickname": "RG"},{"name":"RandomGuy2","age":25,  "nickname": "RG2"}]}`,
		string(resp.GetJson())))

	// Give write access of <name> <dgraph.type> to dev
	require.NoError(t, hc.AddRulesToGroup(devGroup, []dgraphapi.AclRule{{Predicate: "name", Permission: Write.Code},
		{Predicate: "age", Permission: Write.Code}, {Predicate: "dgraph.type", Permission: Write.Code}}, true))
	time.Sleep(defaultTimeToSleep)

	// delete S * * (user now has permission to name, age and dgraph.type)
	mu = &api.Mutation{DelNquads: []byte(fmt.Sprintf("%s %s %s .", "<"+nodeUID+">", "*", "*")), CommitNow: true}
	_, err = userClient.Mutate(mu)
	require.NoError(t, err)

	resp, err = gc.Query(query)
	require.NoError(t, err, "Error while querying data")
	// Because alise had permission to dgraph.type the node reference has been deleted
	require.NoError(t, dgraphapi.CompareJSON(`{"q1":[{"name":"RandomGuy2","age":25,  "nickname": "RG2"}]}`,
		string(resp.GetJson())))
}

func (asuite *AclTestSuite) TestValQueryWithACLPermissions() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.DropAll())

	op := api.Operation{Schema: `
		name	 : string @index(exact) .
		nickname : string @index(exact) .
		age 	 : int .
		type TypeName {
			name: string
			nickname: string
			age: int
		}
	`}
	require.NoError(t, gc.Alter(ctx, &op))

	resetUser(t, hc)
	createdGroup, err := hc.CreateGroup(devGroup)
	require.NoError(t, err)
	require.Equal(t, devGroup, createdGroup)
	require.NoError(t, hc.AddUserToGroup(userid, devGroup))

	mu := &api.Mutation{SetNquads: []byte(`
		_:a <name> "RandomGuy" .
		_:a <age> "23" .
		_:a <nickname> "RG" .
		_:a <dgraph.type> "TypeName" .
		_:b <name> "RandomGuy2" .
		_:b <age> "25" .
		_:b <nickname> "RG2" .
		_:b <dgraph.type> "TypeName" .
	`), CommitNow: true}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	query := `{q1(func: has(name)){
		v as name
		a as age
    }
    q2(func: eq(val(v), "RandomGuy")) {
		val(v)
		val(a)
	}}`

	// Test that groot has access to all the predicates
	resp, err := gc.Query(query)
	require.NoError(t, err, "Error while querying data")
	require.NoError(t, dgraphapi.CompareJSON(
		`{"q1":[{"name":"RandomGuy","age":23},{"name":"RandomGuy2","age":25}],`+
			`"q2":[{"val(v)":"RandomGuy","val(a)":23}]}`, string(resp.GetJson())))

	// All test cases
	tests := []struct {
		input                  string
		descriptionNoPerm      string
		outputNoPerm           string
		descriptionNamePerm    string
		outputNamePerm         string
		descriptionNameAgePerm string
		outputNameAgePerm      string
	}{
		{
			`
			{
				q1(func: has(name), orderasc: name) {
					n as name
					a as age
				}
				q2(func: eq(val(n), "RandomGuy"), orderasc: val(n)) {
					val(n)
					val(a)
				}
			}
			`,
			"alice doesn't have access to name or age",
			`{}`,

			`alice has access to name`,
			`{"q1":[{"name":"RandomGuy"},{"name":"RandomGuy2"}],"q2":[{"val(n)":"RandomGuy"}]}`,

			"alice has access to name and age",
			`{"q1":[{"name":"RandomGuy","age":23},{"name":"RandomGuy2","age":25}],"q2":[{"val(n)":"RandomGuy","val(a)":23}]}`,
		},
		{
			`{
				q1(func: has(name), orderasc: age) {
					a as age
				}
				q2(func: has(name)) {
					val(a)
				}
			}`,
			"alice doesn't have access to name or age",
			`{}`,

			`alice has access to name`,
			`{"q1":[],"q2":[]}`,

			"alice has access to name and age",
			`{"q1":[{"age":23},{"age":25}],"q2":[{"val(a)":23},{"val(a)":25}]}`,
		},
		{
			`{
				f as q1(func: has(name), orderasc: name) {
					n as name
					a as age
				}
				q2(func: uid(f), orderasc: val(n)) {
					name
					val(n)
					val(a)
				}
			}`,
			"alice doesn't have access to name or age",
			`{"q2":[]}`,

			`alice has access to name`,
			`{"q1":[{"name":"RandomGuy"},{"name":"RandomGuy2"}],
			"q2":[{"name":"RandomGuy","val(n)":"RandomGuy"},{"name":"RandomGuy2","val(n)":"RandomGuy2"}]}`,

			"alice has access to name and age",
			`{"q1":[{"name":"RandomGuy","age":23},{"name":"RandomGuy2","age":25}],
			"q2":[{"name":"RandomGuy2","val(n)":"RandomGuy2","val(a)":25},{"name":"RandomGuy","val(n)":"RandomGuy","val(a)":23}]}`, //nolint:lll
		},
		{
			`{
				f as q1(func: has(name), orderasc: name) {
					name
					age
				}
				q2(func: uid(f), orderasc: name) {
					name
					age
				}
			}`,
			"alice doesn't have access to name or age",
			`{"q2":[]}`,

			`alice has access to name`,
			`{"q1":[{"name":"RandomGuy"},{"name":"RandomGuy2"}],
			"q2":[{"name":"RandomGuy"},{"name":"RandomGuy2"}]}`,

			"alice has access to name and age",
			`{"q1":[{"name":"RandomGuy","age":23},{"name":"RandomGuy2","age":25}],
			"q2":[{"name":"RandomGuy2","age":25},{"name":"RandomGuy","age":23}]}`,
		},
	}

	asuite.Upgrade()

	userClient, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, userClient.LoginIntoNamespace(ctx, userid, userpassword, x.GalaxyNamespace))

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	time.Sleep(defaultTimeToSleep)

	// Query via user when user has no permissions
	for _, tc := range tests {
		desc := tc.descriptionNoPerm
		t.Run(desc, func(t *testing.T) {
			resp, err := userClient.Query(tc.input)
			require.NoError(t, err)
			require.NoError(t, dgraphapi.CompareJSON(tc.outputNoPerm, string(resp.Json)))
		})
	}

	// Give read access of <name> to dev
	require.NoError(t, hc.AddRulesToGroup(devGroup,
		[]dgraphapi.AclRule{{Predicate: "name", Permission: Read.Code}}, true))
	time.Sleep(defaultTimeToSleep)

	for _, tc := range tests {
		desc := tc.descriptionNamePerm
		t.Run(desc, func(t *testing.T) {
			resp, err := userClient.Query(tc.input)
			require.NoError(t, err)
			require.NoError(t, dgraphapi.CompareJSON(tc.outputNamePerm, string(resp.Json)))
		})
	}

	// Login to groot to modify accesses (1)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	// Give read access of <name> and <age> to dev
	require.NoError(t, hc.AddRulesToGroup(devGroup, []dgraphapi.AclRule{{Predicate: "name", Permission: Read.Code},
		{Predicate: "age", Permission: Read.Code}}, true))

	time.Sleep(defaultTimeToSleep)

	for _, tc := range tests {
		desc := tc.descriptionNameAgePerm
		t.Run(desc, func(t *testing.T) {
			resp, err := userClient.Query(tc.input)
			require.NoError(t, err)
			require.NoError(t, dgraphapi.CompareJSON(tc.outputNameAgePerm, string(resp.Json)))
		})
	}

}

func (asuite *AclTestSuite) TestAllPredsPermission() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.DropAll())

	op := api.Operation{Schema: `
		name	 : string @index(exact) .
		nickname : string @index(exact) .
		age 	 : int .
		type TypeName {
			name: string
			nickname: string
			age: int
		}
	`}
	require.NoError(t, gc.Alter(ctx, &op))

	resetUser(t, hc)

	createdGroup, err := hc.CreateGroup(devGroup)
	require.NoError(t, err)
	require.Equal(t, devGroup, createdGroup)
	require.NoError(t, hc.AddUserToGroup(userid, devGroup))

	mu := &api.Mutation{SetNquads: []byte(`
		_:a <name> "RandomGuy" .
		_:a <age> "23" .
		_:a <nickname> "RG" .
		_:a <dgraph.type> "TypeName" .
		_:b <name> "RandomGuy2" .
		_:b <age> "25" .
		_:b <nickname> "RG2" .
		_:b <dgraph.type> "TypeName" .
	`), CommitNow: true}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	query := `{
		q1(func: has(name)){
			v as name
			a as age
		}
		q2(func: eq(val(v), "RandomGuy")) {
			val(v)
			val(a)
		}
	}`

	// Test that groot has access to all the predicates
	resp, err := gc.Query(query)
	require.NoError(t, err, "Error while querying data")
	require.NoError(t, dgraphapi.CompareJSON(
		`{"q1":[{"name":"RandomGuy","age":23},{"name":"RandomGuy2","age":25}],`+
			`"q2":[{"val(v)":"RandomGuy","val(a)":23}]}`, string(resp.GetJson())))

	// All test cases
	tests := []struct {
		input                  string
		descriptionNoPerm      string
		outputNoPerm           string
		descriptionNamePerm    string
		outputNamePerm         string
		descriptionNameAgePerm string
		outputNameAgePerm      string
	}{
		{
			`
			{
				q1(func: has(name), orderasc: name) {
					n as name
					a as age
				}
				q2(func: eq(val(n), "RandomGuy")) {
					val(n)
					val(a)
				}
			}
			`,
			"alice doesn't have access to name or age",
			`{}`,

			`alice has access to name`,
			`{"q1":[{"name":"RandomGuy"},{"name":"RandomGuy2"}],"q2":[{"val(n)":"RandomGuy"}]}`,

			"alice has access to name and age",
			`{"q1":[{"name":"RandomGuy","age":23},{"name":"RandomGuy2","age":25}],"q2":[{"val(n)":"RandomGuy","val(a)":23}]}`, //nolint:lll
		},
	}

	asuite.Upgrade()

	userClient, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	time.Sleep(defaultTimeToSleep)

	require.NoError(t, userClient.LoginIntoNamespace(ctx, userid, userpassword, x.GalaxyNamespace))

	// Query via user when user has no permissions
	for _, tc := range tests {
		desc := tc.descriptionNoPerm
		t.Run(desc, func(t *testing.T) {
			resp, err := userClient.Query(tc.input)
			require.NoError(t, err)
			require.NoError(t, dgraphapi.CompareJSON(tc.outputNoPerm, string(resp.Json)))
		})
	}

	// Login to groot to modify accesses (1)
	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	// Give read access of all predicates to dev
	require.NoError(t, hc.AddRulesToGroup(devGroup,
		[]dgraphapi.AclRule{{Predicate: "dgraph.all", Permission: Read.Code}}, true))
	time.Sleep(defaultTimeToSleep)

	for _, tc := range tests {
		desc := tc.descriptionNameAgePerm
		t.Run(desc, func(t *testing.T) {
			resp, err := userClient.Query(tc.input)
			require.NoError(t, err)
			require.NoError(t, dgraphapi.CompareJSON(tc.outputNameAgePerm, string(resp.Json)))
		})
	}

	// Mutation shall fail.

	mu = &api.Mutation{SetNquads: []byte(`
		_:a <name> "RandomGuy" .
		_:a <age> "23" .
		_:a <dgraph.type> "TypeName" .
	`), CommitNow: true}

	_, err = userClient.Mutate(mu)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unauthorized to mutate")

	// Give write access of all predicates to dev. Now mutation should succeed.
	require.NoError(t, hc.AddRulesToGroup(devGroup,
		[]dgraphapi.AclRule{{Predicate: "dgraph.all", Permission: Write.Code | Read.Code}}, true))
	time.Sleep(defaultTimeToSleep)

	_, err = userClient.Mutate(mu)
	require.NoError(t, err)
}

func (asuite *AclTestSuite) TestNewACLPredicates() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	addDataAndRules(ctx, t, gc, hc)

	asuite.Upgrade()

	userClient, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, userClient.LoginIntoNamespace(ctx, userid, userpassword, x.GalaxyNamespace))

	time.Sleep(defaultTimeToSleep)

	queryTests := []struct {
		input       string
		output      string
		description string
	}{
		{
			`
			{
				me(func: has(name)) {
					name
					nickname
				}
			}
			`,
			`{"me":[{"name":"RandomGuy"},{"name":"RandomGuy2"}]}`,
			"alice doesn't have read access to <nickname>",
		},
		{
			`
			{
				me(func: has(nickname)) {
					name
					nickname
				}
			}
			`,
			`{}`,
			`alice doesn't have access to <nickname> so "has(nickname)" is unauthorized`,
		},
	}

	for _, tc := range queryTests {
		tc := tc // capture range variable
		t.Run(tc.description, func(t *testing.T) {
			// testify does not support subtests running in parallel with suite package
			// t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
			defer cancel()

			resp, err := userClient.NewTxn().Query(ctx, tc.input)
			require.NoError(t, err)
			require.NoError(t, dgraphapi.CompareJSON(tc.output, string(resp.Json)))
		})
	}

	mutationTests := []struct {
		input       string
		output      string
		err         error
		description string
	}{
		{
			"_:a <name> \"Animesh\" .",
			"",
			errors.New(""),
			"alice doesn't have write access on <name>.",
		},
		{
			"_:a <nickname> \"Pathak\" .",
			"",
			nil,
			"alice can mutate <nickname> predicate.",
		},
	}
	for _, tc := range mutationTests {
		t.Run(tc.description, func(t *testing.T) {
			_, err := userClient.Mutate(&api.Mutation{
				SetNquads: []byte(tc.input),
				CommitNow: true,
			})
			require.True(t, (err == nil) == (tc.err == nil))
		})
	}
}

func (asuite *AclTestSuite) TestDeleteRule() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	addDataAndRules(ctx, t, gc, hc)

	asuite.Upgrade()

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	userClient, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	time.Sleep(defaultTimeToSleep)

	require.NoError(t, userClient.LoginIntoNamespace(ctx, userid, userpassword, x.GalaxyNamespace))

	queryName := "{me(func: has(name)) {name}}"
	resp, err := userClient.Query(queryName)
	require.NoError(t, err, "Error while querying data")

	require.NoError(t, dgraphapi.CompareJSON(`{"me":[{"name":"RandomGuy"},{"name":"RandomGuy2"}]}`,
		string(resp.GetJson())))

	require.NoError(t, hc.RemovePredicateFromGroup(devGroup, "name"))
	time.Sleep(defaultTimeToSleep)

	resp, err = userClient.Query(queryName)
	require.NoError(t, err, "Error while querying data")
	require.NoError(t, dgraphapi.CompareJSON(string(resp.GetJson()), `{}`))
}

func addDataAndRules(ctx context.Context, t *testing.T, gc *dgraphapi.GrpcClient, hc *dgraphapi.HTTPClient) {
	require.NoError(t, gc.DropAll())
	op := api.Operation{Schema: `
		name	 : string @index(exact) .
		nickname : string @index(exact) .
	`}
	require.NoError(t, gc.Alter(ctx, &op))

	resetUser(t, hc)

	// TODO - We should be adding this data using the GraphQL API.
	// We create three groups here, dev, dev-a and dev-b and add alice to two of them.
	devGroupMut := `
		_:g  <dgraph.xid>        "dev" .
		_:g  <dgraph.type>       "dgraph.type.Group" .
		_:g1  <dgraph.xid>       "dev-a" .
		_:g1  <dgraph.type>      "dgraph.type.Group" .
		_:g2  <dgraph.xid>       "dev-b" .
		_:g2  <dgraph.type>      "dgraph.type.Group" .
		_:g  <dgraph.acl.rule>   _:r1 .
		_:r1 <dgraph.type> "dgraph.type.Rule" .
		_:r1 <dgraph.rule.predicate>  "name" .
		_:r1 <dgraph.rule.permission> "4" .
		_:g  <dgraph.acl.rule>   _:r2 .
		_:r2 <dgraph.type> "dgraph.type.Rule" .
		_:r2 <dgraph.rule.predicate>  "nickname" .
		_:r2 <dgraph.rule.permission> "2" .
	`
	mu := &api.Mutation{SetNquads: []byte(devGroupMut), CommitNow: true}
	_, err := gc.Mutate(mu)
	require.NoError(t, err, "Error adding group and permissions")

	idQuery := fmt.Sprintf(`
	{
		userid as var(func: eq(dgraph.xid, "%s"))
		gid as var(func: eq(dgraph.type, "dgraph.type.Group")) @filter(eq(dgraph.xid, "dev") OR
			eq(dgraph.xid, "dev-a"))
	}`, userid)
	addAliceToGroups := &api.NQuad{
		Subject:   "uid(userid)",
		Predicate: "dgraph.user.group",
		ObjectId:  "uid(gid)",
	}
	_, err = gc.NewTxn().Do(ctx, &api.Request{
		CommitNow: true,
		Query:     idQuery,
		Mutations: []*api.Mutation{
			{
				Set: []*api.NQuad{addAliceToGroups},
			},
		},
	})
	require.NoError(t, err, "Error adding user to dev group")

	mu = &api.Mutation{SetNquads: []byte(`
		_:a <name> "RandomGuy" .
		_:a <nickname> "RG" .
		_:b <name> "RandomGuy2" .
		_:b <age> "25" .
		_:b <nickname> "RG2" .
	`), CommitNow: true}

	_, err = gc.Mutate(mu)
	require.NoError(t, err)
}

func (asuite *AclTestSuite) TestQueryUserInfo() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	addDataAndRules(ctx, t, gc, hc)
	require.NoError(t, hc.LoginIntoNamespace(userid, userpassword, x.GalaxyNamespace))

	gqlQuery := `
	query {
		queryUser {
			name
			groups {
				name
				rules {
					predicate
					permission
				}
				users {
					name
				}
			}
		}
	}
	`
	params := dgraphapi.GraphQLParams{
		Query: gqlQuery,
	}
	gqlResp, err := hc.RunGraphqlQuery(params, true)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`
	{
	  "queryUser": [
		{
		  "name": "alice",
		  "groups": [
			{
			  "name": "dev",
			  "rules": [
				{
				  "predicate": "name",
				  "permission": 4
				},
				{
				  "predicate": "nickname",
				  "permission": 2
				}
			  ],
			  "users": [
				  {
					  "name": "alice"
				  }
			  ]
			},
			{
				"name": "dev-a",
				"rules": [],
				"users": [
					{
						"name": "alice"
					}
				]
			  }
		  ]
		}
	  ]
	}`, string(gqlResp)))

	query := `
	{
		me(func: type(dgraph.type.User)) {
			dgraph.xid
			dgraph.user.group {
				dgraph.xid
				dgraph.acl.rule {
					dgraph.rule.predicate
					dgraph.rule.permission
				}
			}
		}
	}
	`

	asuite.Upgrade()

	userClient, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, userClient.LoginIntoNamespace(ctx, userid, userpassword, x.GalaxyNamespace))

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(userid, userpassword, x.GalaxyNamespace))

	resp, err := userClient.Query(query)
	require.NoError(t, err, "Error while querying ACL")
	require.NoError(t, dgraphapi.CompareJSON(`{"me":[]}`, string(resp.GetJson())))

	gqlQuery = `
	query {
		queryGroup {
			name
			users {
				name
			}
			rules {
				predicate
				permission
			}
		}
	}`
	params = dgraphapi.GraphQLParams{Query: gqlQuery}
	gqlResp, err = hc.RunGraphqlQuery(params, true)
	require.NoError(t, err)

	// The user should only be able to see their group dev and themselves as the user.
	require.NoError(t, dgraphapi.CompareJSON(`{
		  "queryGroup": [
			{
			  "name": "dev",
			  "users": [
				{
				  "name": "alice"
				}
			  ],
			  "rules": [
				{
				  "predicate": "name",
				  "permission": 4
				},
				{
				  "predicate": "nickname",
				  "permission": 2
				}
			  ]
			},
			{
				"name": "dev-a",
				"users": [
				  {
					"name": "alice"
				  }
				],
				"rules": []
			  }

		  ]
	  }`, string(gqlResp)))
	gqlQuery = `
		query {
			getGroup(name: "guardians") {
				name
				rules {
					predicate
					permission
				}
				users {
					name
				}
			}
		}`
	params = dgraphapi.GraphQLParams{Query: gqlQuery}
	gqlResp, err = hc.RunGraphqlQuery(params, true)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{"getGroup": null}`, string(gqlResp)))
}

func (asuite *AclTestSuite) TestQueriesWithUserAndGroupOfSameName() {
	t := asuite.T()
	dgraphtest.ShouldSkipTest(t, asuite.dc.GetVersion(), "532df27a09ba25f88687bab344e3add2b81b5c23")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.DropAll())
	// Creates a user -- alice
	resetUser(t, hc)

	mu := &api.Mutation{
		SetNquads: []byte(`
			_:a <name> "RandomGuy" .
			_:a <age> "23" .
			_:a <nickname> "RG" .
			_:a <dgraph.type> "TypeName" .
			_:b <name> "RandomGuy2" .
			_:b <age> "25" .
			_:b <nickname> "RG2" .
			_:b <dgraph.type> "TypeName" .`),
		CommitNow: true,
	}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	createdGroup, err := hc.CreateGroup("alice")
	require.NoError(t, err)
	require.Equal(t, "alice", createdGroup)
	require.NoError(t, hc.AddUserToGroup(userid, "alice"))

	// add rules to groups
	require.NoError(t, hc.AddRulesToGroup("alice",
		[]dgraphapi.AclRule{{Predicate: "name", Permission: Read.Code}}, true))

	asuite.Upgrade()

	dc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, dc.LoginIntoNamespace(ctx, userid, userpassword, x.GalaxyNamespace))

	query := `
	{
		q(func: has(name)) {
			name
			age
		}
	}`
	require.NoError(t, dgraphapi.PollTillPassOrTimeout(dc, query,
		`{"q":[{"name":"RandomGuy"},{"name":"RandomGuy2"}]}`, timeout))
}

func (asuite *AclTestSuite) TestQueriesForNonGuardianUserWithoutGroup() {
	t := asuite.T()

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	// Create a new user without any groups, queryGroup should return an empty result.
	resetUser(t, hc)

	asuite.Upgrade()

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(userid, userpassword, x.GalaxyNamespace))

	gqlQuery := `
	query {
		queryGroup {
			name
			users {
				name
			}
		}
	}
	`

	params := dgraphapi.GraphQLParams{Query: gqlQuery}
	gqlResp, err := hc.RunGraphqlQuery(params, true)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{"queryGroup": []}`, string(gqlResp)))

	gqlQuery = `
	query {
		queryUser {
			name
			groups {
				name
			}
		}
	}`
	params = dgraphapi.GraphQLParams{Query: gqlQuery}
	gqlResp, err = hc.RunGraphqlQuery(params, true)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{"queryUser": [{ "groups": [], "name": "alice"}]}`, string(gqlResp)))
}

func (asuite *AclTestSuite) TestSchemaQueryWithACL() {
	t := asuite.T()
	dgraphtest.ShouldSkipTest(t, "9a964dd9c794a9731fa6ec35aded6693acc7a1fd", asuite.dc.GetVersion())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	schemaQuery := "schema{}"

	aliceSchema := `{
  "schema": [
    {
      "predicate": "name",
      "type": "string",
      "index": true,
      "tokenizer": [
        "exact"
      ]
    }
  ],
  "types": [
    {
      "fields": [],
      "name": "dgraph.graphql"
	},
	{
		"fields":[],
		"name":"dgraph.graphql.persisted_query"
	},
	{
      "fields": [],
      "name": "dgraph.namespace"
	},
    {
      "fields": [],
      "name": "dgraph.type.Group"
    },
    {
      "fields": [],
      "name": "dgraph.type.Rule"
    },
    {
      "fields": [],
      "name": "dgraph.type.User"
    }
  ]
}`

	// guardian user should be able to view full schema
	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.DropAll())
	resp, err := gc.Query(schemaQuery)
	require.NoError(t, err)
	uniqueSchemaSupported, err := dgraphtest.IsHigherVersion(asuite.dc.GetVersion(), "532df27a09ba25f88687bab344e3add2b81b5c23")
	require.NoError(t, err)
	nsSchemaSupported, err := dgraphtest.IsHigherVersion(asuite.dc.GetVersion(), "053a44054dc665f573ee7b92136b551a7b70c37c")
	require.NoError(t, err)
	if nsSchemaSupported {
		require.JSONEq(t, grootSchemaWithNamespaceSchema, string(resp.GetJson()))
	} else if uniqueSchemaSupported {
		require.JSONEq(t, uniqueXidGrootSchema, string(resp.GetJson()))
	} else {
		require.JSONEq(t, oldGrootSchema, string(resp.GetJson()))
	}

	// add another user and some data for that user with permissions on predicates
	resetUser(t, hc)

	addDataAndRules(ctx, t, gc, hc)
	time.Sleep(defaultTimeToSleep) // wait for ACL cache to refresh, otherwise it will be flaky test
	asuite.Upgrade()
	// the other user should be able to view only the part of schema for which it has read access
	gc, cleanup, err = asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(context.Background(), userid, userpassword, x.GalaxyNamespace))

	resp, err = gc.Query(schemaQuery)
	require.NoError(t, err)
	require.JSONEq(t, aliceSchema, string(resp.GetJson()))
}

func (asuite *AclTestSuite) TestDeleteUserShouldDeleteUserFromGroup() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	resetUser(t, hc)
	addDataAndRules(ctx, t, gc, hc)

	asuite.Upgrade()

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))
	require.NoError(t, hc.DeleteUser(userid))

	gqlQuery := `
	query {
		queryUser {
			name
		}
	}`
	params := dgraphapi.GraphQLParams{Query: gqlQuery}
	gqlResp, err := hc.RunGraphqlQuery(params, true)
	require.NoError(t, err)
	require.JSONEq(t, `{"queryUser":[{"name":"groot"}]}`, string(gqlResp))

	// The user should also be deleted from the dev group.
	gqlQuery = `
	query {
		queryGroup {
			name
			users {
				name
			}
		}
	}`
	params = dgraphapi.GraphQLParams{Query: gqlQuery}
	gqlResp, err = hc.RunGraphqlQuery(params, true)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{
		  "queryGroup": [
			{
			  "name": "guardians",
			  "users": [
				{
				  "name": "groot"
				}
			  ]
			},
			{
			  "name": "dev",
			  "users": []
			},
			{
				"name": "dev-a",
				"users": []
			},
			{
				"name": "dev-b",
				"users": []
			}
		  ]
	  }`, string(gqlResp)))
}

func (asuite *AclTestSuite) TestGroupDeleteShouldDeleteGroupFromUser() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))
	resetUser(t, hc)

	addDataAndRules(ctx, t, gc, hc)

	asuite.Upgrade()

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))
	require.NoError(t, hc.DeleteGroup("dev-a"))

	gqlQuery := `
	query {
		queryGroup {
			name
		}
	}`
	params := dgraphapi.GraphQLParams{Query: gqlQuery}
	gqlResp, err := hc.RunGraphqlQuery(params, true)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{
		  "queryGroup": [
			{
			  "name": "guardians"
			},
			{
			  "name": "dev"
			},
			{
			  "name": "dev-b"
			}
		  ]
	  }`, string(gqlResp)))

	gqlQuery = `
	query {
		getUser(name: "alice") {
			name
			groups {
				name
			}
		}
	}`
	params = dgraphapi.GraphQLParams{Query: gqlQuery}
	gqlResp, err = hc.RunGraphqlQuery(params, true)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{
		"getUser": {
			"name": "alice",
			"groups": [
				{
					"name": "dev"
				}
			]
		}
	}`, string(gqlResp)))
}

func assertNonGuardianFailure(t *testing.T, queryName string, respIsNull bool, gqlResp []byte, err error) {
	require.Contains(t, err.Error(), "rpc error: code = PermissionDenied")
	require.Contains(t, err.Error(), fmt.Sprintf(
		"Only guardians are allowed access. User '%s' is not a member of guardians group.",
		userid))
	if len(gqlResp) != 0 {
		queryVal := "null"
		if !respIsNull {
			queryVal = "[]"
		}
		require.JSONEq(t, fmt.Sprintf(`{"%s": %s}`, queryName, queryVal), string(gqlResp))
	}
}

type graphQLAdminEndpointTestCase struct {
	name               string
	query              string
	queryName          string
	respIsArray        bool
	testGuardianAccess bool
	guardianErr        string
	// specifying this as empty string means it won't be compared with response data
	guardianData string
}

func (asuite *AclTestSuite) TestAddUpdateGroupWithDuplicateRules() {
	t := asuite.T()

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	groupName := "testGroup"
	addedRules := []dgraphapi.AclRule{
		{
			Predicate:  "test",
			Permission: 1,
		},
		{
			Predicate:  "test",
			Permission: 2,
		},
		{
			Predicate:  "test1",
			Permission: 3,
		},
	}

	addedGroup, err := hc.CreateGroupWithRules(groupName, addedRules)
	require.NoError(t, err)

	require.Equal(t, groupName, addedGroup.Name)
	require.Len(t, addedGroup.Rules, 2)
	require.ElementsMatch(t, addedRules[1:], addedGroup.Rules)

	asuite.Upgrade()

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))
	updatedRules := []dgraphapi.AclRule{
		{
			Predicate:  "test",
			Permission: 3,
		},
		{
			Predicate:  "test2",
			Permission: 1,
		},
		{
			Predicate:  "test2",
			Permission: 2,
		},
	}
	updatedGroup, err := hc.UpdateGroup(groupName, updatedRules, nil)
	require.NoError(t, err)
	require.Equal(t, groupName, updatedGroup.Name)
	require.Len(t, updatedGroup.Rules, 3)
	require.ElementsMatch(t, []dgraphapi.AclRule{updatedRules[0], addedRules[2], updatedRules[2]},
		updatedGroup.Rules)

	updatedGroup1, err := hc.UpdateGroup(groupName, nil,
		[]string{"test1", "test1", "test3"})
	require.NoError(t, err)

	require.Equal(t, groupName, updatedGroup1.Name)
	require.Len(t, updatedGroup1.Rules, 2)
	require.ElementsMatch(t, []dgraphapi.AclRule{updatedRules[0], updatedRules[2]}, updatedGroup1.Rules)

	// cleanup
	require.NoError(t, hc.DeleteGroup(groupName))
}

func (asuite *AclTestSuite) TestAllowUIDAccess() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.DropAll())
	op := api.Operation{Schema: `
		name	 : string @index(exact) .
	`}
	require.NoError(t, gc.Alter(ctx, &op))

	resetUser(t, hc)

	createdGroup, err := hc.CreateGroup(devGroup)
	require.NoError(t, err)
	require.Equal(t, devGroup, createdGroup)
	require.NoError(t, hc.AddUserToGroup(userid, devGroup))

	require.NoError(t, asuite.dc.AssignUids(gc.Dgraph, 101))
	mu := &api.Mutation{SetNquads: []byte(`<100> <name> "100th User" .`), CommitNow: true}
	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	// give read access of <name> to alice
	require.NoError(t, hc.AddRulesToGroup(devGroup,
		[]dgraphapi.AclRule{{Predicate: "name", Permission: Read.Code}}, true))

	asuite.Upgrade()

	userClient, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	time.Sleep(defaultTimeToSleep)

	require.NoError(t, userClient.LoginIntoNamespace(ctx, userid, userpassword, x.GalaxyNamespace))

	uidQuery := `
	{
		me(func: uid(100)) {
			uid
			name
		}
	}`
	resp, err := userClient.Query(uidQuery)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{"me":[{"name":"100th User", "uid": "0x64"}]}`, string(resp.GetJson())))
}

func (asuite *AclTestSuite) TestAddNewPredicate() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.DropAll())
	resetUser(t, hc)

	asuite.Upgrade()

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))
	userClient, cancel, err := asuite.dc.Client()
	defer cleanup()
	require.NoError(t, err)
	require.NoError(t, userClient.LoginIntoNamespace(ctx, userid, userpassword, x.GalaxyNamespace))

	// Alice doesn't have access to create new predicate.
	err = userClient.Alter(ctx, &api.Operation{Schema: `newpred: string .`})
	require.Error(t, err, "User can't create new predicate. Alter should have returned error.")

	require.NoError(t, hc.AddUserToGroup(userid, "guardians"))
	time.Sleep(expireJwtSleep)

	// Alice is a guardian now, it can create new predicate.
	err = userClient.Alter(ctx, &api.Operation{
		Schema: `newpred: string .`,
	})
	require.NoError(t, err, "User is a guardian. Alter should have succeeded.")
}

func (asuite *AclTestSuite) TestCrossGroupPermission() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.DropAll())

	err = gc.Alter(ctx, &api.Operation{Schema: `newpred: string .`})
	require.NoError(t, err)

	// create groups
	createdGroup, err := hc.CreateGroup("reader")
	require.NoError(t, err)
	require.Equal(t, "reader", createdGroup)
	createdGroup, err = hc.CreateGroup("writer")
	require.NoError(t, err)
	require.Equal(t, "writer", createdGroup)
	createdGroup, err = hc.CreateGroup("alterer")
	require.NoError(t, err)
	require.Equal(t, "alterer", createdGroup)
	// add rules to groups
	require.NoError(t, hc.AddRulesToGroup("reader", []dgraphapi.AclRule{{Predicate: "newpred", Permission: 4}}, true))
	require.NoError(t, hc.AddRulesToGroup("writer", []dgraphapi.AclRule{{Predicate: "newpred", Permission: 2}}, true))
	require.NoError(t, hc.AddRulesToGroup("alterer", []dgraphapi.AclRule{{Predicate: "newpred", Permission: 1}}, true))
	// Wait for acl cache to be refreshed
	time.Sleep(defaultTimeToSleep)

	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))
	// create 8 users.
	for i := 0; i < 8; i++ {
		userIdx := strconv.Itoa(i)
		_, err := hc.CreateUser("user"+userIdx, "password"+userIdx)
		require.NoError(t, err)
	}

	// add users to groups. we create all possible combination
	// of groups and assign a user for that combination.
	for i := 0; i < 8; i++ {
		userIdx := strconv.Itoa(i)
		if i&1 > 0 {
			require.NoError(t, hc.AddUserToGroup("user"+userIdx, "alterer"))
		}
		if i&2 > 0 {
			require.NoError(t, hc.AddUserToGroup("user"+userIdx, "writer"))
		}
		if i&4 > 0 {
			require.NoError(t, hc.AddUserToGroup("user"+userIdx, "reader"))
		}
	}
	time.Sleep(defaultTimeToSleep)

	// operations
	dgQuery := func(client *dgraphapi.GrpcClient, shouldFail bool, user string) {
		_, err := client.Query(`
		{
			me(func: has(newpred)) {
				newpred
			}
		}
		`)
		require.True(t, (err != nil) == shouldFail,
			"Query test Failed for: "+user+", shouldFail: "+strconv.FormatBool(shouldFail))
	}
	dgMutation := func(client *dgraphapi.GrpcClient, shouldFail bool, user string) {
		_, err := client.Mutate(&api.Mutation{
			Set: []*api.NQuad{
				{
					Subject:     "_:a",
					Predicate:   "newpred",
					ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: "testval"}},
				},
			},
			CommitNow: true,
		})
		require.True(t, (err != nil) == shouldFail,
			"Mutation test failed for: "+user+", shouldFail: "+strconv.FormatBool(shouldFail))
	}
	dgAlter := func(client *dgraphapi.GrpcClient, shouldFail bool, user string) {
		err := client.Alter(ctx, &api.Operation{Schema: `newpred: string @index(exact) .`})
		require.True(t, (err != nil) == shouldFail,
			"Alter test failed for: "+user+", shouldFail: "+strconv.FormatBool(shouldFail))

		// set back the schema to initial value
		err = client.Alter(ctx, &api.Operation{Schema: `newpred: string .`})
		require.True(t, (err != nil) == shouldFail,
			"Alter test failed for: "+user+", shouldFail: "+strconv.FormatBool(shouldFail))
	}

	asuite.Upgrade()

	// test user access.
	for i := 0; i < 8; i++ {
		userIdx := strconv.Itoa(i)
		userClient, cleanup, err := asuite.dc.Client()
		require.NoError(t, err, "Client creation error")
		defer cleanup()

		require.NoError(t, userClient.LoginIntoNamespace(ctx, "user"+userIdx,
			"password"+userIdx, x.GalaxyNamespace), "Login error")

		dgQuery(userClient, false, "user"+userIdx) // Query won't fail, will return empty result instead.
		dgMutation(userClient, i&2 == 0, "user"+userIdx)
		dgAlter(userClient, i&1 == 0, "user"+userIdx)
	}
}

func (asuite *AclTestSuite) TestMutationWithValueVar() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	require.NoError(t, gc.DropAll())
	err = gc.Alter(ctx, &api.Operation{
		Schema: `
			name	: string @index(exact) .
			nickname: string .
			age     : int .
		`,
	})
	require.NoError(t, err)

	mu := &api.Mutation{SetNquads: []byte(`
	    _:u1 <name> "RandomGuy" .
	    _:u1 <nickname> "r1" .
	`), CommitNow: true}

	_, err = gc.Mutate(mu)
	require.NoError(t, err)

	resetUser(t, hc)
	// require.NoError(t, hc.CreateUser(userid, userpassword))
	createdGroup, err := hc.CreateGroup(devGroup)
	require.NoError(t, err)
	require.Equal(t, devGroup, createdGroup)
	require.NoError(t, hc.AddUserToGroup(userid, devGroup))
	require.NoError(t, hc.AddRulesToGroup(devGroup, []dgraphapi.AclRule{
		{
			Predicate:  "name",
			Permission: Read.Code | Write.Code,
		},
		{
			Predicate:  "nickname",
			Permission: Read.Code,
		},
		{
			Predicate:  "age",
			Permission: Write.Code,
		},
	}, true))

	time.Sleep(defaultTimeToSleep)

	query := `
		{
			u1 as var(func: has(name)) {
				nick1 as nickname
				age1 as age
			}
		}
	`

	mutation1 := &api.Mutation{
		SetNquads: []byte(`
			uid(u1) <name> val(nick1) .
			uid(u1) <name> val(age1) .
		`),
		CommitNow: true,
	}

	asuite.Upgrade()

	userClient, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, userClient.LoginIntoNamespace(ctx, userid, userpassword, x.GalaxyNamespace))

	_, err = userClient.NewTxn().Do(ctx, &api.Request{
		Query:     query,
		Mutations: []*api.Mutation{mutation1},
		CommitNow: true,
	})
	require.NoError(t, err)

	query = `
		{
			me(func: has(name)) {
				nickname
				name
				age
			}
		}
	`

	resp, err := userClient.Query(query)
	require.NoError(t, err)
	require.NoError(t, dgraphapi.CompareJSON(`{"me": [{"name":"r1","nickname":"r1"}]}`, string(resp.GetJson())))
}

func (asuite *AclTestSuite) TestDeleteGuardiansGroupShouldFail() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	addDataAndRules(ctx, t, gc, hc)

	asuite.Upgrade()

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	err = hc.DeleteGroup("guardians")
	require.Error(t, err)
	require.Contains(t, err.Error(), "guardians group and groot user cannot be deleted.")
}

func (asuite *AclTestSuite) TestDeleteGrootUserShouldFail() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	addDataAndRules(ctx, t, gc, hc)

	asuite.Upgrade()

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	err = hc.DeleteUser("groot")
	require.Error(t, err)
	require.Contains(t, err.Error(),
		"guardians group and groot user cannot be deleted.")
}

func (asuite *AclTestSuite) TestDeleteGrootUserFromGuardiansGroupShouldFail() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	addDataAndRules(ctx, t, gc, hc)

	asuite.Upgrade()

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	err = hc.RemoveUserFromGroup("groot", "guardians")
	require.Error(t, err)
	require.Contains(t, err.Error(), "guardians group and groot user cannot be deleted.")
}

func (asuite *AclTestSuite) TestDeleteGrootAndGuardiansUsingDelNQuadShouldFail() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	hc, err := asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	addDataAndRules(ctx, t, gc, hc)

	asuite.Upgrade()

	gc, cleanup, err = asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	grootUid, guardiansUid := getGrootAndGuardiansUid(t, gc)

	mu := &api.Mutation{DelNquads: []byte(fmt.Sprintf("%s %s %s .", "<"+grootUid+">", "*", "*")), CommitNow: true}
	// Try deleting groot user
	_, err = gc.Mutate(mu)
	require.Error(t, err, "Deleting groot user should have returned an error")
	require.Contains(t, err.Error(), "Properties of guardians group and groot user cannot be deleted")

	mu = &api.Mutation{DelNquads: []byte(fmt.Sprintf("%s %s %s .", "<"+guardiansUid+">", "*", "*")), CommitNow: true}
	// Try deleting guardians group
	_, err = gc.Mutate(mu)
	require.Error(t, err, "Deleting guardians group should have returned an error")
	require.Contains(t, err.Error(), "Properties of guardians group and groot user cannot be deleted")
}

func deleteGuardiansGroupAndGrootUserShouldFail(t *testing.T, hc *dgraphapi.HTTPClient) {
	// Try deleting guardians group should fail
	err := hc.DeleteGroup("guardians")
	require.Error(t, err)
	require.Contains(t, err.Error(), "guardians group and groot user cannot be deleted.")
	// Try deleting groot user should fail
	err = hc.DeleteUser("groot")
	require.Error(t, err)
	require.Contains(t, err.Error(), "guardians group and groot user cannot be deleted.")
}

func (asuite *AclTestSuite) TestDropAllShouldResetGuardiansAndGroot() {
	t := asuite.T()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	gc, cleanup, err := asuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))
	hc, err := asuite.dc.HTTPClient()

	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	addDataAndRules(ctx, t, gc, hc)

	asuite.Upgrade()

	gc, cleanup, err = asuite.dc.Client()
	defer cleanup()
	require.NoError(t, err)
	require.NoError(t, gc.LoginIntoNamespace(ctx, dgraphapi.DefaultUser,
		dgraphapi.DefaultPassword, x.GalaxyNamespace))

	// Try Drop All
	op := api.Operation{
		DropAll: true,
		DropOp:  api.Operation_ALL,
	}
	if err := gc.Alter(ctx, &op); err != nil {
		t.Fatalf("Unable to drop all. Error:%v", err)
	}

	time.Sleep(defaultTimeToSleep)

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))
	deleteGuardiansGroupAndGrootUserShouldFail(t, hc)

	// Try Drop Data
	op = api.Operation{
		DropOp: api.Operation_DATA,
	}
	if err := gc.Alter(ctx, &op); err != nil {
		t.Fatalf("Unable to drop data. Error:%v", err)
	}
	time.Sleep(defaultTimeToSleep)

	hc, err = asuite.dc.HTTPClient()
	require.NoError(t, err)
	require.NoError(t, hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))
	deleteGuardiansGroupAndGrootUserShouldFail(t, hc)
}
