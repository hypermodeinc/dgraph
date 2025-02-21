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

type PluginTestSuite struct {
	suite.Suite
	dc dgraphapi.Cluster
}

func (psuite *PluginTestSuite) SetupTest() {
	psuite.dc = dgraphtest.NewComposeCluster()
}

func (psuite *PluginTestSuite) TearDownTest() {
	t := psuite.T()
	gcli, cleanup, err := psuite.dc.Client()
	require.NoError(t, err)
	defer cleanup()
	require.NoError(t, gcli.DropAll(context.Background()))
}

func (psuite *PluginTestSuite) Upgrade() {
	// Not implemented for integration tests
}

func TestPluginTestSuite(t *testing.T) {
	suite.Run(t, new(PluginTestSuite))
}
