//go:build integration

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/dgraphtest"
)

type MultitenancyTestSuite struct {
	suite.Suite
	dc dgraphapi.Cluster
}

func (msuite *MultitenancyTestSuite) SetupTest() {
	msuite.dc = dgraphtest.NewComposeCluster()
}

func (msuite *MultitenancyTestSuite) TearDownTest() {
	t := msuite.T()
	gcli, cleanup, err := msuite.dc.Client()
	defer cleanup()
	require.NoError(t, err)
	require.NoError(t, gcli.LoginUser(context.Background(),
		dgraphapi.DefaultUser, dgraphapi.DefaultPassword))
	require.NoError(t, gcli.DropAll(context.Background()))
}

func (msuite *MultitenancyTestSuite) Upgrade() {
	// Not implemented for integration tests
}

func TestACLSuite(t *testing.T) {
	suite.Run(t, new(MultitenancyTestSuite))
}
