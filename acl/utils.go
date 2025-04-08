/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package acl

import (
	"encoding/json"
	"fmt"

	"github.com/golang/glog"
	"github.com/spf13/viper"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/dgraph-io/ristretto/v2/z"
	"github.com/hypermodeinc/dgraph/v24/x"
)

// GetGroupIDs returns a slice containing the group ids of all the given groups.
func GetGroupIDs(groups []Group) []string {
	if len(groups) == 0 {
		// the user does not have any groups
		return nil
	}

	jwtGroups := make([]string, 0, len(groups))
	for _, g := range groups {
		jwtGroups = append(jwtGroups, g.GroupID)
	}
	return jwtGroups
}

var (
	OpRead   = "Read"
	OpWrite  = "Write"
	OpModify = "Modify"
)

// Operation represents a Dgraph data operation (e.g write or read).
type Operation struct {
	Code int32
	Name string
}

var (
	// Read is used when doing a query.
	Read = &Operation{
		Code: 4,
		Name: OpRead,
	}
	// Write is used when mutating data.
	Write = &Operation{
		Code: 2,
		Name: OpWrite,
	}
	// Modify is used when altering the schema or dropping data.
	Modify = &Operation{
		Code: 1,
		Name: OpModify,
	}
)

// User represents a user in the ACL system.
type User struct {
	Uid           string  `json:"uid"`
	UserID        string  `json:"dgraph.xid"`
	Password      string  `json:"dgraph.password"`
	Namespace     uint64  `json:"namespace"`
	PasswordMatch bool    `json:"password_match"`
	Groups        []Group `json:"dgraph.user.group"`
}

// GetUid returns the UID of the user.
func (u *User) GetUid() string {
	if u == nil {
		return ""
	}
	return u.Uid
}

// UnmarshalUser extracts the first User pointed by the userKey in the query response.
func UnmarshalUser(resp *api.Response, userKey string) (user *User, err error) {
	m := make(map[string][]User)

	err = json.Unmarshal(resp.GetJson(), &m)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal the query user response: %w", err)
	}
	users := m[userKey]
	if len(users) == 0 {
		// the user does not exist
		return nil, nil
	}
	if len(users) > 1 {
		return nil, fmt.Errorf("found multiple users: %s", resp.GetJson())
	}
	return &users[0], nil
}

// Acl represents the permissions in the ACL system.
// An Acl can have a predicate and permission for that predicate.
type Acl struct {
	Predicate string `json:"dgraph.rule.predicate"`
	Perm      int32  `json:"dgraph.rule.permission"`
}

// Group represents a group in the ACL system.
type Group struct {
	Uid     string `json:"uid"`
	GroupID string `json:"dgraph.xid"`
	Users   []User `json:"~dgraph.user.group"`
	Rules   []Acl  `json:"dgraph.acl.rule"`
}

// GetUid returns the UID of the group.
func (g *Group) GetUid() string {
	if g == nil {
		return ""
	}
	return g.Uid
}

// UnmarshalGroup extracts the first Group pointed by the groupKey in the query response.
func UnmarshalGroup(input []byte, groupKey string) (group *Group, err error) {
	m := make(map[string][]Group)

	if err = json.Unmarshal(input, &m); err != nil {
		glog.Errorf("Unable to unmarshal the query group response:%v", err)
		return nil, err
	}
	groups := m[groupKey]
	if len(groups) == 0 {
		// The group does not exist.
		return nil, nil
	}
	if len(groups) > 1 {
		return nil, fmt.Errorf("found multiple groups: %s", input)
	}

	return &groups[0], nil
}

// UnmarshalGroups extracts a sequence of groups from the input.
func UnmarshalGroups(input []byte, groupKey string) (group []Group, err error) {
	m := make(map[string][]Group)

	if err = json.Unmarshal(input, &m); err != nil {
		glog.Errorf("Unable to unmarshal the query group response:%v", err)
		return nil, err
	}
	groups := m[groupKey]
	return groups, nil
}

// getClientWithAdminCtx creates a client by checking the --alpha, various --tls*, and --retries
// options, and then login using groot id and password
func getClientWithAdminCtx(conf *viper.Viper) (*dgo.Dgraph, x.CloseFunc, error) {
	dg, closeClient := x.GetDgraphClient(conf, false)
	creds := z.NewSuperFlag(conf.GetString("guardian-creds"))
	err := x.GetPassAndLogin(dg, &x.CredOpt{
		UserID:    creds.GetString("user"),
		Password:  creds.GetString("password"),
		Namespace: creds.GetUint64("namespace"),
	})
	if err != nil {
		return nil, nil, err
	}
	return dg, closeClient, nil
}

// CreateUserNQuads creates the NQuads needed to store a user with the given ID and
// password in the ACL system.
func CreateUserNQuads(userId, password string) []*api.NQuad {
	return []*api.NQuad{
		{
			Subject:     "_:newuser",
			Predicate:   "dgraph.xid",
			ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: userId}},
		},
		{
			Subject:     "_:newuser",
			Predicate:   "dgraph.password",
			ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: password}},
		},
		{
			Subject:     "_:newuser",
			Predicate:   "dgraph.type",
			ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: "dgraph.type.User"}},
		},
	}
}

// CreateGroupNQuads cretes NQuads needed to store a group with the give ID.
func CreateGroupNQuads(groupId string) []*api.NQuad {
	return []*api.NQuad{
		{
			Subject:     "_:newgroup",
			Predicate:   "dgraph.xid",
			ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: groupId}},
		},
		{
			Subject:     "_:newgroup",
			Predicate:   "dgraph.type",
			ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: "dgraph.type.Group"}},
		},
	}
}
