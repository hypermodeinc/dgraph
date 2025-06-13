/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package edgraph

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/dgraph-io/dgo/v250/protos/api"
	apiv2 "github.com/dgraph-io/dgo/v250/protos/api.v2"
	"github.com/hypermodeinc/dgraph/v25/schema"
	"github.com/hypermodeinc/dgraph/v25/x"
)

const (
	queryAllNamespaces = `{
		namespaces(func: type(dgraph.namespace)) {
			dgraph.namespace.name
			dgraph.namespace.id
		}
	}`
	queryNamespaceByName = `{
		namespaces(func: eq(dgraph.namespace.name, "%v")) {
			dgraph.namespace.id
		}
	}`
	queryNamespaceByID = `{
		namespaces(func: eq(dgraph.namespace.id, "%v")) {
			dgraph.namespace.name
		}
	}`
	queryDeleteNamespaceByName = `{
		namespaces(func: eq(dgraph.namespace.name, "%v")) {
			nsUID as uid
			dgraph.namespace.id
		}
	}`
	queryRenameNamespace = `{
		namespaces as var(func: eq(dgraph.namespace.name, "%v"))
	}`
)

type resultNamespaces struct {
	Namespaces []struct {
		ID   uint64 `json:"dgraph.namespace.id"`
		Name string `json:"dgraph.namespace.name"`
	} `json:"namespaces"`
}

func (s *ServerV25) SignInUser(ctx context.Context,
	request *apiv2.SignInUserRequest) (*apiv2.SignInUserResponse, error) {

	req := &api.LoginRequest{Userid: request.UserId, Password: request.Password, Namespace: 0}
	resp, err := (&Server{}).Login(ctx, req)
	if err != nil {
		return nil, err
	}
	jwt := api.Jwt{}
	if err := proto.Unmarshal(resp.Json, &jwt); err != nil {
		return nil, err
	}

	return &apiv2.SignInUserResponse{AccessJwt: jwt.AccessJwt, RefreshJwt: jwt.RefreshJwt}, nil
}

func (s *ServerV25) CreateNamespace(ctx context.Context, in *apiv2.CreateNamespaceRequest) (
	*apiv2.CreateNamespaceResponse, error) {

	if err := AuthSuperAdmin(ctx); err != nil {
		s := status.Convert(err)
		return nil, status.Error(s.Code(),
			"Non superadmin user cannot create namespace. "+s.Message())
	}

	if err := isValidNamespaceName(in.NsName); err != nil {
		return nil, err
	}

	if _, err := getNamespaceIDFromName(x.AttachJWTNamespace(ctx), in.NsName); err == nil {
		return nil, errors.Errorf("namespace %q already exists", in.NsName)
	} else if !strings.Contains(err.Error(), "not found") {
		return nil, err
	}

	password := "password"
	ns, err := (&Server{}).CreateNamespaceInternal(ctx, password)
	if err != nil {
		return nil, err
	}

	// If we crash at this point, it is possible that namespaces is created
	// but no entry has been added to dgraph.namespace predicate. This is alright
	// because we have not let the user know that namespace has been created.
	// The user would have to try again and another namespace then would be
	// assigned to the provided name here.

	if err := insertNamespace(ctx, in.NsName, ns); err != nil {
		return nil, err
	}

	glog.Infof("Created namespace [%v] with id [%d]", in.NsName, ns)
	return &apiv2.CreateNamespaceResponse{}, nil
}

func (s *ServerV25) DropNamespace(ctx context.Context, in *apiv2.DropNamespaceRequest) (
	*apiv2.DropNamespaceResponse, error) {

	if err := AuthSuperAdmin(ctx); err != nil {
		s := status.Convert(err)
		return nil, status.Error(s.Code(),
			"Non superadmin user cannot drop namespace. "+s.Message())
	}

	if err := isValidNamespaceToDelete(in.NsName); err != nil {
		return nil, err
	}

	var nsID uint64
	if isLgacyNamespace(in.NsName) {
		ns, err := extractNsIDFromLegacyNamespace(in.NsName)
		if err != nil {
			return nil, err
		}

		if _, err := getNamespaceNameFromID(x.AttachJWTNamespace(ctx), ns); err == nil {
			// We found the namespace with a real name
			return nil, fmt.Errorf("namespace [%v] not found", in.NsName)
		} else if !strings.Contains(err.Error(), "not found") {
			return nil, err
		}
		nsID = ns
	} else {
		ns, err := deleteNamespaceByName(x.AttachJWTNamespace(ctx), in.NsName)
		if err != nil {
			return nil, err
		}
		nsID = ns
	}

	if nsID == 0 {
		glog.Infof("Namespace [%v] not found", in.NsName)
		return &apiv2.DropNamespaceResponse{}, nil
	}

	// If we crash at this point, it is possible that namespace is deleted
	// by the name, but the entry has not been removed from dgraph.namespace

	if err := (&Server{}).DeleteNamespace(ctx, nsID); err != nil {
		if !strings.Contains(err.Error(), "error deleting non-existing namespace") {
			return nil, err
		} else {
			glog.Infof("Namespace with id [%d] does not exist, cannot be deleted", nsID)
		}
	}

	glog.Infof("Dropped namespace [%v] with id [%d]", in.NsName, nsID)
	return &apiv2.DropNamespaceResponse{}, nil
}

func (s *ServerV25) UpdateNamespace(ctx context.Context, in *apiv2.UpdateNamespaceRequest) (
	*apiv2.UpdateNamespaceResponse, error) {

	if err := AuthSuperAdmin(ctx); err != nil {
		s := status.Convert(err)
		return nil, status.Error(s.Code(),
			"Non superadmin user cannot rename a namespace. "+s.Message())
	}

	if err := isValidNamespaceToDelete(in.NsName); err != nil {
		return nil, err
	}
	if err := isValidNamespaceName(in.RenameToNs); err != nil {
		return nil, err
	}

	if isLgacyNamespace(in.NsName) {
		err := renameLeagcyNamespace(ctx, in.NsName, in.RenameToNs)
		return &apiv2.UpdateNamespaceResponse{}, err
	}

	if err := renameNamespace(x.AttachJWTNamespace(ctx), in.NsName, in.RenameToNs); err != nil {
		return nil, err
	}

	glog.Infof("Renamed namespace [%v] to [%v]", in.NsName, in.RenameToNs)
	return &apiv2.UpdateNamespaceResponse{}, nil
}

func (s *ServerV25) ListNamespaces(ctx context.Context, in *apiv2.ListNamespacesRequest) (
	*apiv2.ListNamespacesResponse, error) {

	if err := AuthSuperAdmin(ctx); err != nil {
		s := status.Convert(err)
		return nil, status.Error(s.Code(),
			"Non superadmin user cannot list namespaces. "+s.Message())
	}

	resp, err := (&Server{}).doQuery(x.AttachJWTNamespace(ctx),
		&Request{req: &api.Request{Query: queryAllNamespaces}, doAuth: NoAuthorize})
	if err != nil {
		return nil, err
	}

	var data resultNamespaces
	if err := json.Unmarshal(resp.GetJson(), &data); err != nil {
		return nil, err
	}
	dataNsList := make(map[uint64]string)
	for _, e := range data.Namespaces {
		dataNsList[e.ID] = e.Name
	}

	schNsList := schema.State().Namespaces()
	result := &apiv2.ListNamespacesResponse{NsList: make(map[string]*apiv2.Namespace)}
	for id := range schNsList {
		if name, ok := dataNsList[id]; !ok {
			name = fmt.Sprintf("dgraph-%d", id)
			result.NsList[name] = &apiv2.Namespace{Name: name, Id: id}
		} else {
			result.NsList[name] = &apiv2.Namespace{Name: name, Id: id}
		}
	}

	return result, nil
}

func isLgacyNamespace(nsName string) bool {
	return strings.HasPrefix(nsName, "dgraph")
}

func extractNsIDFromLegacyNamespace(nsName string) (uint64, error) {
	if len(nsName) < 8 {
		return 0, errors.Errorf("namespace %q is not a legacy namespace", nsName)
	}
	ns, err := strconv.ParseUint(nsName[7:], 10, 64)
	if err != nil {
		return 0, errors.Errorf("invalid namespace name %q", nsName)
	}
	return ns, nil
}

func isSystemNamespace(nsName string) bool {
	return nsName == "root" || nsName == "galaxy" || nsName == "dgraph-0"
}

func isValidNamespaceName(name string) error {
	if name == "" {
		return errors.Errorf("namespace name cannot be empty")
	}
	hasInvalidChars := strings.ContainsFunc(name, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' && r != '-'
	})
	if hasInvalidChars {
		return fmt.Errorf("namespace name [%v] has invalid characters", name)
	}
	if strings.HasPrefix(name, "_") || strings.HasPrefix(name, "-") {
		return fmt.Errorf("namespace name [%v] cannot start with _ or -", name)
	}
	if isLgacyNamespace(name) {
		return fmt.Errorf("namespace name [%v] cannot start with dgraph", name)
	}
	if isSystemNamespace(name) {
		return fmt.Errorf("namespace name [%v] is reserved", name)
	}
	if _, err := strconv.ParseInt(name, 10, 64); err == nil {
		return fmt.Errorf("namespace name [%v] cannot be a number", name)
	}
	return nil
}

func isValidNamespaceToDelete(name string) error {
	if name == "" {
		return errors.Errorf("namespace name cannot be empty")
	}
	if isSystemNamespace(name) {
		return fmt.Errorf("namespace [%v] cannot be renamed/dropped", name)
	}
	return nil
}

func getNamespaceIDFromName(ctx context.Context, nsName string) (uint64, error) {
	if isSystemNamespace(nsName) {
		return 0, nil
	}
	if isLgacyNamespace(nsName) {
		return extractNsIDFromLegacyNamespace(nsName)
	}

	req := &api.Request{Query: fmt.Sprintf(queryNamespaceByName, nsName)}
	resp, err := (&Server{}).doQuery(ctx, &Request{req: req, doAuth: NoAuthorize})
	if err != nil {
		return 0, err
	}

	var data resultNamespaces
	if err := json.Unmarshal(resp.GetJson(), &data); err != nil {
		return 0, err
	}
	if len(data.Namespaces) == 0 {
		return 0, errors.Wrapf(x.ErrNamespaceNotFound, "namespace %q not found", nsName)
	}

	glog.Infof("Found namespace [%v] with id [%d]", nsName, data.Namespaces[0].ID)
	return data.Namespaces[0].ID, nil
}

func getNamespaceNameFromID(ctx context.Context, nsID uint64) (string, error) {
	req := &api.Request{Query: fmt.Sprintf(queryNamespaceByID, nsID)}
	resp, err := (&Server{}).doQuery(ctx, &Request{req: req, doAuth: NoAuthorize})
	if err != nil {
		return "", err
	}

	var data resultNamespaces
	if err := json.Unmarshal(resp.GetJson(), &data); err != nil {
		return "", err
	}
	if len(data.Namespaces) == 0 {
		return "", errors.Errorf("namespace [%v] not found", nsID)
	}

	glog.Infof("Found namespace with ID [%v] with name [%v]", nsID, data.Namespaces[0].Name)
	return data.Namespaces[0].Name, nil
}

func insertNamespace(ctx context.Context, nsName string, nsID uint64) error {
	if nsID >= math.MaxInt64 {
		return errors.Errorf("namespace ID %d exceeds int64 range", nsID)
	}

	_, err := (&Server{}).QueryNoGrpc(
		context.WithValue(ctx, IsGraphql, true),
		&api.Request{
			Mutations: []*api.Mutation{{
				Set: []*api.NQuad{
					{
						Subject:     "_:ns",
						Predicate:   "dgraph.namespace.name",
						ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: nsName}},
					},
					{
						Subject:     "_:ns",
						Predicate:   "dgraph.namespace.id",
						ObjectValue: &api.Value{Val: &api.Value_IntVal{IntVal: int64(nsID)}},
					},
					{
						Subject:     "_:ns",
						Predicate:   "dgraph.type",
						ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: "dgraph.namespace"}},
					},
				},
			}},
			CommitNow: true,
		})
	return err
}

func deleteNamespaceByName(ctx context.Context, nsName string) (uint64, error) {
	req := &api.Request{
		Query:     fmt.Sprintf(queryDeleteNamespaceByName, nsName),
		Mutations: []*api.Mutation{{DelNquads: []byte(`uid(nsUID) * * .`)}},
		CommitNow: true,
	}
	resp, err := (&Server{}).doQuery(context.WithValue(ctx, IsGraphql, true),
		&Request{req: req, doAuth: NoAuthorize})
	if err != nil {
		return 0, err
	}

	var data resultNamespaces
	if err := json.Unmarshal(resp.GetJson(), &data); err != nil {
		return 0, err
	}
	if len(data.Namespaces) != 1 {
		return 0, nil
	}

	return data.Namespaces[0].ID, nil
}

func renameLeagcyNamespace(ctx context.Context, fromNs, toNs string) error {
	fromID, err := extractNsIDFromLegacyNamespace(fromNs)
	if err != nil {
		return err
	}

	_, err = getNamespaceNameFromID(x.AttachJWTNamespace(ctx), fromID)
	if err == nil {
		return fmt.Errorf("namespace [%v] not found: %v", err, fromNs)
	}
	if !strings.Contains(err.Error(), "not found") {
		return err
	}

	if err := insertNamespace(x.AttachJWTNamespace(ctx), toNs, fromID); err != nil {
		return err
	}

	return nil
}

func renameNamespace(ctx context.Context, fromNs, toNs string) error {
	req := &api.Request{
		Query: fmt.Sprintf(queryRenameNamespace, fromNs),
		Mutations: []*api.Mutation{{
			Cond: `@if(eq(len(namespaces), 1))`,
			Set: []*api.NQuad{
				{
					Subject:     "uid(namespaces)",
					Predicate:   "dgraph.namespace.name",
					ObjectValue: &api.Value{Val: &api.Value_StrVal{StrVal: toNs}},
				},
			},
		}},
		CommitNow: true,
	}
	resp, err := (&Server{}).doQuery(context.WithValue(ctx, IsGraphql, true), &Request{req: req})
	if err != nil {
		return err
	}

	if resp.Metrics.NumUids["namespaces"] != 1 {
		return errors.Errorf("namespace [%v] not found", fromNs)
	}

	return nil
}
