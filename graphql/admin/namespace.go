/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/hypermodeinc/dgraph/v24/edgraph"
	"github.com/hypermodeinc/dgraph/v24/graphql/resolve"
	"github.com/hypermodeinc/dgraph/v24/graphql/schema"
	"github.com/hypermodeinc/dgraph/v24/x"
)

type addNamespaceInput struct {
	Password string
}

type deleteNamespaceInput struct {
	NamespaceId int
}

func resolveAddNamespace(ctx context.Context, m schema.Mutation) (*resolve.Resolved, bool) {
	req, err := getAddNamespaceInput(m)
	if err != nil {
		return resolve.EmptyResult(m, err), false
	}
	if req.Password == "" {
		// Use the default password, if the user does not specify.
		req.Password = "password"
	}
	var ns uint64
	if ns, err = (&edgraph.Server{}).CreateNamespaceInternal(ctx, req.Password); err != nil {
		return resolve.EmptyResult(m, err), false
	}
	return resolve.DataResult(
		m,
		map[string]interface{}{m.Name(): map[string]interface{}{
			"namespaceId": json.Number(strconv.Itoa(int(ns))),
			"message":     "Created namespace successfully",
		}},
		nil,
	), true
}

func resolveDeleteNamespace(ctx context.Context, m schema.Mutation) (*resolve.Resolved, bool) {
	req, err := getDeleteNamespaceInput(m)
	if err != nil {
		return resolve.EmptyResult(m, err), false
	}
	// No one can delete the galaxy(default) namespace.
	if uint64(req.NamespaceId) == x.GalaxyNamespace {
		return resolve.EmptyResult(m, errors.New("Cannot delete default namespace")), false
	}
	if err = (&edgraph.Server{}).DeleteNamespace(ctx, uint64(req.NamespaceId)); err != nil {
		return resolve.EmptyResult(m, err), false
	}
	dropOp := "DROP_NS;" + fmt.Sprintf("%#x", req.NamespaceId)
	if err = edgraph.InsertDropRecord(ctx, dropOp); err != nil {
		return resolve.EmptyResult(m, err), false
	}
	return resolve.DataResult(
		m,
		map[string]interface{}{m.Name(): map[string]interface{}{
			"namespaceId": json.Number(strconv.Itoa(req.NamespaceId)),
			"message":     "Deleted namespace successfully",
		}},
		nil,
	), true
}

func getAddNamespaceInput(m schema.Mutation) (*addNamespaceInput, error) {
	inputArg := m.ArgValue(schema.InputArgName)
	inputByts, err := json.Marshal(inputArg)
	if err != nil {
		return nil, schema.GQLWrapf(err, "couldn't get input argument")
	}

	var input addNamespaceInput
	err = json.Unmarshal(inputByts, &input)
	return &input, schema.GQLWrapf(err, "couldn't get input argument")
}

func getDeleteNamespaceInput(m schema.Mutation) (*deleteNamespaceInput, error) {
	inputArg := m.ArgValue(schema.InputArgName)
	inputByts, err := json.Marshal(inputArg)
	if err != nil {
		return nil, schema.GQLWrapf(err, "couldn't get input argument")
	}

	var input deleteNamespaceInput
	err = json.Unmarshal(inputByts, &input)
	return &input, schema.GQLWrapf(err, "couldn't get input argument")
}
