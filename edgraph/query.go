/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package edgraph

import (
	"context"
	"fmt"

	"github.com/dgraph-io/dgo/v250/protos/api"
	"github.com/hypermodeinc/dgraph/v25/dql"
)

// Alter handles requests to change the schema or remove parts or all of the data.
func (s *Server) RunDQL(ctx context.Context, req *api.RunDQLRequest) (*api.Response, error) {

	apiReq, err := dql.ParseDQL(req.DqlQuery)
	if err != nil {
		return nil, fmt.Errorf("error parsing DQL query: %w", err)
	}

	apiReq.Vars = req.Vars
	apiReq.ReadOnly = req.ReadOnly
	apiReq.BestEffort = req.BestEffort
	apiReq.RespFormat = req.RespFormat
	if len(apiReq.Mutations) > 0 {
		apiReq.CommitNow = true
	}

	return (&Server{}).doQuery(ctx, &Request{req: apiReq})
}
