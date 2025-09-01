/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package edgraph

import (
	"context"
	"fmt"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/worker"

	"github.com/pkg/errors"
	"google.golang.org/grpc/status"
)

func (s *Server) AllocateIDs(ctx context.Context, req *api.AllocateIDsRequest) (
	*api.AllocateIDsResponse, error) {

	// For now, we only allow users in superadmin group to do this operation in v25
	if err := AuthSuperAdmin(ctx); err != nil {
		s := status.Convert(err)
		return nil, status.Error(s.Code(),
			"v25.AllocateIDs can only be called by the superadmin group. "+s.Message())
	}

	num := &pb.Num{Val: req.HowMany}
	switch req.LeaseType {
	case api.LeaseType_NS:
		num.Type = pb.Num_NS_ID
	case api.LeaseType_UID:
		num.Type = pb.Num_UID
	case api.LeaseType_TS:
		num.Type = pb.Num_TXN_TS
	default:
		return nil, fmt.Errorf("invalid lease type: %v", req.LeaseType)
	}

	resp, err := worker.AssignUidsOverNetwork(ctx, num)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to allocate IDs")
	}

	return &api.AllocateIDsResponse{Start: resp.StartId, End: resp.EndId}, nil
}
