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
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/spf13/viper"

	"github.com/dgraph-io/dgo/v240"
	"github.com/dgraph-io/dgo/v240/protos/api"
	"github.com/hypermodeinc/dgraph/v24/x"
)

func getUserAndGroup(conf *viper.Viper) (userId string, groupId string, err error) {
	userId = conf.GetString("user")
	groupId = conf.GetString("group")
	if (len(userId) == 0 && len(groupId) == 0) ||
		(len(userId) != 0 && len(groupId) != 0) {
		return "", "", errors.New("one of the --user or --group must be specified, but not both")
	}
	return userId, groupId, nil
}

func checkForbiddenOpts(conf *viper.Viper, forbiddenOpts []string) error {
	for _, opt := range forbiddenOpts {
		var isSet bool
		switch conf.Get(opt).(type) {
		case string:
			if opt == "group_list" {
				// handle group_list specially since the default value is not an empty string
				isSet = conf.GetString(opt) != defaultGroupList
			} else {
				isSet = len(conf.GetString(opt)) > 0
			}
		case int:
			isSet = conf.GetInt(opt) > 0
		case bool:
			isSet = conf.GetBool(opt)
		default:
			return fmt.Errorf("unexpected option type for %s", opt)
		}
		if isSet {
			return fmt.Errorf("the option --%s should not be set", opt)
		}
	}

	return nil
}

func add(conf *viper.Viper) error {
	userId, groupId, err := getUserAndGroup(conf)
	if err != nil {
		return err
	}
	password := conf.GetString("password")
	if len(userId) != 0 {
		return userAdd(conf, userId, password)
	}

	// if we are adding a group, then the password should not have been set
	if err := checkForbiddenOpts(conf, []string{"password"}); err != nil {
		return err
	}
	return groupAdd(conf, groupId)
}

func userAdd(conf *viper.Viper, userid string, password string) error {
	dc, cancel, err := getClientWithAdminCtx(conf)
	if err != nil {
		return fmt.Errorf("unable to get admin context: %w", err)
	}
	defer cancel()

	if len(password) == 0 {
		var err error
		password, err = x.AskUserPassword(userid, "New", 2)
		if err != nil {
			return err
		}
	}

	ctx, ctxCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ctxCancel()
	txn := dc.NewTxn()
	defer func() {
		if err := txn.Discard(ctx); err != nil {
			glog.Errorf("Unable to discard transaction:%v", err)
		}
	}()

	user, err := queryUser(ctx, txn, userid)
	if err != nil {
		return fmt.Errorf("while querying user: %w", err)
	}
	if user != nil {
		return fmt.Errorf("unable to create user because of conflict: %v", userid)
	}

	createUserNQuads := CreateUserNQuads(userid, password)

	mu := &api.Mutation{
		CommitNow: true,
		Set:       createUserNQuads,
	}

	if _, err := txn.Mutate(ctx, mu); err != nil {
		return fmt.Errorf("unable to create user: %w", err)
	}

	fmt.Printf("Created new user with id %v\n", userid)
	return nil
}

func groupAdd(conf *viper.Viper, groupId string) error {
	dc, cancel, err := getClientWithAdminCtx(conf)
	if err != nil {
		return fmt.Errorf("unable to get admin context: %w", err)
	}
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ctxCancel()
	txn := dc.NewTxn()
	defer func() {
		if err := txn.Discard(ctx); err != nil {
			fmt.Printf("Unable to discard transaction: %v\n", err)
		}
	}()

	group, err := queryGroup(ctx, txn, groupId)
	if err != nil {
		return fmt.Errorf("while querying group: %w", err)
	}
	if group != nil {
		return fmt.Errorf("group %q already exists", groupId)
	}

	createGroupNQuads := CreateGroupNQuads(groupId)

	mu := &api.Mutation{
		CommitNow: true,
		Set:       createGroupNQuads,
	}
	if _, err = txn.Mutate(ctx, mu); err != nil {
		return fmt.Errorf("unable to create group: %w", err)
	}

	fmt.Printf("Created new group with id %v\n", groupId)
	return nil
}

func del(conf *viper.Viper) error {
	userId, groupId, err := getUserAndGroup(conf)
	if err != nil {
		return err
	}
	if len(userId) != 0 {
		return userOrGroupDel(conf, userId,
			func(ctx context.Context, txn *dgo.Txn, userId string) (AclEntity, error) {
				user, err := queryUser(ctx, txn, userId)
				return user, err
			})
	}
	return userOrGroupDel(conf, groupId,
		func(ctx context.Context, txn *dgo.Txn, groupId string) (AclEntity, error) {
			group, err := queryGroup(ctx, txn, groupId)
			return group, err
		})
}

// AclEntity is an interface that must be met by all the types of entities (i.e users, groups)
// in the ACL system.
type AclEntity interface {
	// GetUid returns the UID of the entity.
	// The implementation of GetUid must check the case that the entity is nil
	// and return an empty string accordingly.
	GetUid() string
}

func userOrGroupDel(conf *viper.Viper, userOrGroupId string,
	queryFn func(context.Context, *dgo.Txn, string) (AclEntity, error)) error {
	dc, cancel, err := getClientWithAdminCtx(conf)
	if err != nil {
		return fmt.Errorf("unable to get admin context: %w", err)
	}
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ctxCancel()
	txn := dc.NewTxn()
	defer func() {
		if err := txn.Discard(ctx); err != nil {
			glog.Errorf("Unable to discard transaction:%v", err)
		}
	}()

	entity, err := queryFn(ctx, txn, userOrGroupId)
	if err != nil {
		return err
	}
	if len(entity.GetUid()) == 0 {
		return fmt.Errorf("unable to delete %q since it does not exist",
			userOrGroupId)
	}

	deleteNQuads := []*api.NQuad{
		{
			Subject:     entity.GetUid(),
			Predicate:   x.Star,
			ObjectValue: &api.Value{Val: &api.Value_DefaultVal{DefaultVal: x.Star}},
		}}

	mu := &api.Mutation{
		CommitNow: true,
		Del:       deleteNQuads,
	}

	if _, err = txn.Mutate(ctx, mu); err != nil {
		return fmt.Errorf("unable to delete %q: %w", userOrGroupId, err)
	}

	fmt.Printf("Successfully deleted %q\n", userOrGroupId)
	return nil
}

func mod(conf *viper.Viper) error {
	userId, _, err := getUserAndGroup(conf)
	if err != nil {
		return err
	}

	if len(userId) != 0 {
		// when modifying the user, some group options are forbidden
		if err := checkForbiddenOpts(conf, []string{"pred", "perm"}); err != nil {
			return err
		}

		newPassword := conf.GetBool("new_password")
		groupList := conf.GetString("group_list")
		if (newPassword && groupList != defaultGroupList) ||
			(!newPassword && groupList == defaultGroupList) {
			return errors.New("one of --new_password or --group_list must be provided, but not both")
		}

		if newPassword {
			return changePassword(conf, userId)
		}

		return userMod(conf, userId, groupList)
	}

	// when modifying the group, some user options are forbidden
	if err := checkForbiddenOpts(conf, []string{"group_list", "new_password"}); err != nil {
		return err
	}
	return chMod(conf)
}

// changePassword changes a user's password
func changePassword(conf *viper.Viper, userId string) error {
	// 1. get the dgo client with appropriate access JWT
	dc, cancel, err := getClientWithAdminCtx(conf)
	if err != nil {
		return fmt.Errorf("unable to get dgo client: %w", err)
	}
	defer cancel()

	// 2. get the new password
	newPassword, err := x.AskUserPassword(userId, "New", 2)
	if err != nil {
		return err
	}

	ctx, ctxCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ctxCancel()
	txn := dc.NewTxn()
	defer func() {
		if err := txn.Discard(ctx); err != nil {
			glog.Errorf("Unable to discard transaction:%v", err)
		}
	}()

	// 3. query the user's current uid
	user, err := queryUser(ctx, txn, userId)
	if err != nil {
		return fmt.Errorf("while querying user: %w", err)
	}
	if user == nil {
		return fmt.Errorf("user %q does not exist", userId)
	}

	// 4. mutate the user's password
	chPdNQuads := []*api.NQuad{
		{
			Subject:     user.Uid,
			Predicate:   "dgraph.password",
			ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: newPassword}},
		}}
	mu := &api.Mutation{
		CommitNow: true,
		Set:       chPdNQuads,
	}
	if _, err := txn.Mutate(ctx, mu); err != nil {
		return fmt.Errorf("unable to change password for user %v: %w", userId, err)
	}
	fmt.Printf("Successfully changed password for %v\n", userId)
	return nil
}

func userMod(conf *viper.Viper, userId string, groups string) error {
	dc, cancel, err := getClientWithAdminCtx(conf)
	if err != nil {
		return fmt.Errorf("unable to get admin context: %w", err)
	}
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ctxCancel()
	txn := dc.NewTxn()
	defer func() {
		if err := txn.Discard(ctx); err != nil {
			fmt.Printf("Unable to discard transaction: %v\n", err)
		}
	}()

	user, err := queryUser(ctx, txn, userId)
	if err != nil {
		return fmt.Errorf("while querying user: %w", err)
	}
	if user == nil {
		return fmt.Errorf("user %q does not exist", userId)
	}

	targetGroupsMap := make(map[string]struct{})
	if len(groups) > 0 {
		for _, g := range strings.Split(groups, ",") {
			targetGroupsMap[g] = struct{}{}
		}
	}

	existingGroupsMap := make(map[string]struct{})
	for _, g := range user.Groups {
		existingGroupsMap[g.GroupID] = struct{}{}
	}
	newGroups, groupsToBeDeleted := x.Diff(targetGroupsMap, existingGroupsMap)

	mu := &api.Mutation{
		CommitNow: true,
		Set:       []*api.NQuad{},
		Del:       []*api.NQuad{},
	}

	for _, g := range newGroups {
		fmt.Printf("Adding user %v to group %v\n", userId, g)
		nquad, err := getUserModNQuad(ctx, txn, user.Uid, g)
		if err != nil {
			return err
		}
		mu.Set = append(mu.Set, nquad)
	}

	for _, g := range groupsToBeDeleted {
		fmt.Printf("Deleting user %v from group %v\n", userId, g)
		nquad, err := getUserModNQuad(ctx, txn, user.Uid, g)
		if err != nil {
			return err
		}
		mu.Del = append(mu.Del, nquad)
	}
	if len(mu.Del) == 0 && len(mu.Set) == 0 {
		fmt.Printf("Nothing needs to be changed for the groups of user: %v\n", userId)
		return nil
	}

	if _, err := txn.Mutate(ctx, mu); err != nil {
		return fmt.Errorf("while mutating the group: %w", err)
	}
	fmt.Printf("Successfully modified groups for user %v.\n", userId)
	fmt.Println("The latest info is:")
	return queryAndPrintUser(ctx, dc.NewReadOnlyTxn(), userId)
}

/*
	chMod adds/updates/deletes rule attached to group.
	1. It will return error if there is no group named <groupName>.
	2. It will add new rule if group doesn't already have a rule for the predicate.
	3. It will update the permission if group already have a rule for the predicate and permission
		is a non-negative integer between 0-7.
	4. It will delete, if group already have a rule for the predicate and the permission is
		a negative integer.
*/

func chMod(conf *viper.Viper) error {
	groupName := conf.GetString("group")
	predicate := conf.GetString("pred")
	perm := conf.GetInt("perm")
	switch {
	case len(groupName) == 0:
		return errors.New("the group must not be empty")
	case len(predicate) == 0:
		return errors.New("no predicates specified")
	case perm > 7:
		return fmt.Errorf("the perm value must be less than or equal to 7, "+
			"the provided value is %d", perm)
	}

	dc, cancel, err := getClientWithAdminCtx(conf)
	if err != nil {
		return fmt.Errorf("unable to get admin context: %w", err)
	}
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ctxCancel()
	txn := dc.NewTxn()
	defer func() {
		if err := txn.Discard(ctx); err != nil {
			fmt.Printf("Unable to discard transaction: %v\n", err)
		}
	}()

	ruleQuery := fmt.Sprintf(`
	{
		var(func: eq(dgraph.xid, "%s")) @filter(type(dgraph.type.Group)) {
			gUID as uid
			rUID as dgraph.acl.rule @filter(eq(dgraph.rule.predicate, "%s"))
		}
		groupUIDCount(func: uid(gUID)) {count(uid)}
	}`, groupName, predicate)

	updateRule := &api.Mutation{
		Set: []*api.NQuad{
			{
				Subject:     "uid(rUID)",
				Predicate:   "dgraph.rule.permission",
				ObjectValue: &api.Value{Val: &api.Value_IntVal{IntVal: int64(perm)}},
			},
		},
		Cond: "@if(eq(len(rUID), 1) AND eq(len(gUID), 1))",
	}

	createRule := &api.Mutation{
		Set: []*api.NQuad{
			{
				Subject:     "_:newrule",
				Predicate:   "dgraph.rule.permission",
				ObjectValue: &api.Value{Val: &api.Value_IntVal{IntVal: int64(perm)}},
			},
			{
				Subject:     "_:newrule",
				Predicate:   "dgraph.rule.predicate",
				ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: predicate}},
			},
			{
				Subject:   "uid(gUID)",
				Predicate: "dgraph.acl.rule",
				ObjectId:  "_:newrule",
			},
		},
		Cond: "@if(eq(len(rUID), 0) AND eq(len(gUID), 1))",
	}

	deleteRule := &api.Mutation{
		Del: []*api.NQuad{
			{
				Subject:   "uid(gUID)",
				Predicate: "dgraph.acl.rule",
				ObjectId:  "uid(rUID)",
			},
		},
		Cond: "@if(eq(len(rUID), 1) AND eq(len(gUID), 1))",
	}

	upsertRequest := &api.Request{
		Query:     ruleQuery,
		Mutations: []*api.Mutation{createRule, updateRule},
		CommitNow: true,
	}
	if perm < 0 {
		upsertRequest.Mutations = []*api.Mutation{deleteRule}
	}
	resp, err := txn.Do(ctx, upsertRequest)
	if err != nil {
		return err
	}
	var jsonResp map[string][]map[string]int
	err = json.Unmarshal(resp.GetJson(), &jsonResp)
	if err != nil {
		return err
	}

	uidCount, ok := jsonResp["groupUIDCount"][0]["count"]
	if !ok {
		return errors.New("malformed output of groupUIDCount")
	} else if uidCount == 0 {
		// We already have a check for multiple groups with same name at dgraph/acl/utils.go:142
		return fmt.Errorf("group <%s> doesn't exist", groupName)
	}
	return nil
}

func queryUser(ctx context.Context, txn *dgo.Txn, userid string) (user *User, err error) {
	query := `
    query search($userid: string){
      user(func: eq(dgraph.xid, $userid)) @filter(type(dgraph.type.User)) {
	    uid
        dgraph.xid
        dgraph.user.group {
          uid
          dgraph.xid
        }
      }
    }`

	queryVars := make(map[string]string)
	queryVars["$userid"] = userid

	queryResp, err := txn.QueryWithVars(ctx, query, queryVars)
	if err != nil {
		return nil, fmt.Errorf("while query user with id %s: %w", userid, err)
	}
	user, err = UnmarshalUser(queryResp, "user")
	if err != nil {
		return nil, err
	}
	return user, nil
}

func getUserModNQuad(ctx context.Context, txn *dgo.Txn, userId string,
	groupId string) (*api.NQuad, error) {
	group, err := queryGroup(ctx, txn, groupId)
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, fmt.Errorf("group %q does not exist", groupId)
	}

	createUserGroupNQuads := &api.NQuad{
		Subject:   userId,
		Predicate: "dgraph.user.group",
		ObjectId:  group.Uid,
	}

	return createUserGroupNQuads, nil
}

func queryGroup(ctx context.Context, txn *dgo.Txn, groupid string,
	fields ...string) (group *Group, err error) {

	// write query header
	query := fmt.Sprintf(`
		query search($groupid: string){
			group(func: eq(dgraph.xid, $groupid)) @filter(type(dgraph.type.Group)) {
				uid
				%s
			}
		}`, strings.Join(fields, ", "))

	queryVars := map[string]string{
		"$groupid": groupid,
	}

	queryResp, err := txn.QueryWithVars(ctx, query, queryVars)
	if err != nil {
		fmt.Printf("Error while querying group with id %s: %v\n", groupid, err)
		return nil, err
	}
	group, err = UnmarshalGroup(queryResp.GetJson(), "group")
	if err != nil {
		return nil, err
	}
	return group, nil
}

func queryAndPrintUser(ctx context.Context, txn *dgo.Txn, userId string) error {
	user, err := queryUser(ctx, txn, userId)
	if err != nil {
		return err
	}
	if user == nil {
		return fmt.Errorf("the user %q does not exist", userId)
	}

	fmt.Printf("User  : %s\n", userId)
	fmt.Printf("UID   : %s\n", user.Uid)
	for _, group := range user.Groups {
		fmt.Printf("Group : %-5s\n", group.GroupID)
	}
	return nil
}

func queryAndPrintGroup(ctx context.Context, txn *dgo.Txn, groupId string) error {
	group, err := queryGroup(ctx, txn, groupId, "dgraph.xid", "~dgraph.user.group{dgraph.xid}",
		"dgraph.acl.rule{dgraph.rule.predicate, dgraph.rule.permission}")
	if err != nil {
		return err
	}
	if group == nil {
		return fmt.Errorf("the group %s doesn't exist", groupId)
	}

	fmt.Printf("Group: %s\n", groupId)
	fmt.Printf("UID  : %s\n", group.Uid)
	fmt.Printf("ID   : %s\n", group.GroupID)

	var userNames []string
	for _, user := range group.Users {
		userNames = append(userNames, user.UserID)
	}
	fmt.Printf("Users: %s\n", strings.Join(userNames, " "))

	for _, acl := range group.Rules {
		fmt.Printf("ACL: %v\n", acl)
	}

	return nil
}

func info(conf *viper.Viper) error {
	userId, groupId, err := getUserAndGroup(conf)
	if err != nil {
		return err
	}

	dc, cancel, err := getClientWithAdminCtx(conf)
	if err != nil {
		return fmt.Errorf("unable to get admin context: %w", err)
	}
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ctxCancel()
	txn := dc.NewTxn()
	defer func() {
		if err := txn.Discard(ctx); err != nil {
			fmt.Printf("Unable to discard transaction: %v\n", err)
		}
	}()

	if len(userId) != 0 {
		return queryAndPrintUser(ctx, txn, userId)
	}

	return queryAndPrintGroup(ctx, txn, groupId)
}
