//go:build upgrade

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package query

import (
	"context"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/hypermodeinc/dgraph/v25/dgraphapi"
	"github.com/hypermodeinc/dgraph/v25/dgraphtest"
	"github.com/hypermodeinc/dgraph/v25/x"
)

func TestMain(m *testing.M) {
	mutate := func(c dgraphapi.Cluster) {
		dg, cleanup, err := c.Client()
		x.Panic(err)
		defer cleanup()
		x.Panic(dg.LoginIntoNamespace(context.Background(), dgraphapi.DefaultUser,
			dgraphapi.DefaultPassword, x.RootNamespace))

		client = dg
		dc = c
		populateCluster(dc)
	}

	query := func(c dgraphapi.Cluster) int {
		dg, cleanup, err := c.Client()
		x.Panic(err)
		defer cleanup()
		x.Panic(dg.LoginIntoNamespace(context.Background(), dgraphapi.DefaultUser,
			dgraphapi.DefaultPassword, x.RootNamespace))

		client = dg
		dc = c
		return m.Run()
	}

	runTest := func(uc dgraphtest.UpgradeCombo) {
		var code int = 2 // it will be set to 0 when tests complete successfully
		conf := dgraphtest.NewClusterConfig().WithNumAlphas(1).WithNumZeros(1).
			WithReplicas(1).WithACL(time.Hour).WithVersion(uc.Before)
		c, err := dgraphtest.NewLocalCluster(conf)
		x.Panic(err)
		defer func() { c.Cleanup(true) }()
		x.Panic(c.Start())

		hc, err := c.HTTPClient()
		x.Panic(err)
		x.Panic(hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.RootNamespace))

		mutate(c)
		x.Panic(c.Upgrade(uc.After, uc.Strategy))
		code = query(c)
		if code != 0 {
			panic(fmt.Sprintf("query upgrade tests failed for [%v -> %v]", uc.Before, uc.After))
		}
	}

	for _, uc := range dgraphtest.AllUpgradeCombos(false) {
		log.Printf("running upgrade tests for confg: %+v", uc)
		runTest(uc)
	}
}
