/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package edgraph

import (
	"context"
	"fmt"
	"strings"

	"github.com/golang/glog"
	"google.golang.org/grpc/status"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/schema"
)

func (s *Server) CreateNamespace(ctx context.Context, in *api.CreateNamespaceRequest) (
	*api.CreateNamespaceResponse, error) {

	if err := AuthSuperAdmin(ctx); err != nil {
		s := status.Convert(err)
		return nil, status.Error(s.Code(),
			"Non superadmin user cannot create namespace. "+s.Message())
	}

	password := "password"
	ns, err := (&Server{}).CreateNamespaceInternal(ctx, password)
	if err != nil {
		return nil, err
	}

	glog.Infof("Created namespace with id [%d]", ns)
	return &api.CreateNamespaceResponse{}, nil
}

func (s *Server) DropNamespace(ctx context.Context, in *api.DropNamespaceRequest) (
	*api.DropNamespaceResponse, error) {

	if err := AuthSuperAdmin(ctx); err != nil {
		s := status.Convert(err)
		return nil, status.Error(s.Code(),
			"Non superadmin user cannot drop namespace. "+s.Message())
	}

	if in.Namespace == 0 {
		glog.Infof("Namespace [%v] cannot be deleted", in.Namespace)
		return nil, fmt.Errorf("namespace [%v] cannot be deleted", in.Namespace)
	}

	if err := (&Server{}).DeleteNamespace(ctx, in.Namespace); err != nil {
		if !strings.Contains(err.Error(), "error deleting non-existing namespace") {
			return nil, err
		} else {
			glog.Infof("Namespace with id [%d] does not exist, cannot be deleted", in.Namespace)
		}
	}

	glog.Infof("Dropped namespace [%v]", in.Namespace)
	return &api.DropNamespaceResponse{}, nil
}

func (s *Server) ListNamespaces(ctx context.Context, in *api.ListNamespacesRequest) (
	*api.ListNamespacesResponse, error) {

	if err := AuthSuperAdmin(ctx); err != nil {
		s := status.Convert(err)
		return nil, status.Error(s.Code(),
			"Non superadmin user cannot list namespaces. "+s.Message())
	}

	dataNsList := make(map[uint64]*api.Namespace)
	for ns := range schema.State().Namespaces() {
		dataNsList[ns] = &api.Namespace{Id: ns}
	}

	return &api.ListNamespacesResponse{Namespaces: dataNsList}, nil
}
