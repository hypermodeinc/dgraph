//go:build upgrade

/*
 * Copyright 2025 Hypermode Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package query

import (
	"context"
	"fmt"
	"log"
	"testing"
	"time"

	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/dgraphtest"
	"github.com/hypermodeinc/dgraph/v24/x"
)

func TestMain(m *testing.M) {
	mutate := func(c dgraphapi.Cluster) {
		dg, cleanup, err := c.Client()
		x.Panic(err)
		defer cleanup()
		x.Panic(dg.LoginIntoNamespace(context.Background(), dgraphapi.DefaultUser,
			dgraphapi.DefaultPassword, x.GalaxyNamespace))

		client = dg
		dc = c
		populateCluster(dc)
	}

	query := func(c dgraphapi.Cluster) int {
		dg, cleanup, err := c.Client()
		x.Panic(err)
		defer cleanup()
		x.Panic(dg.LoginIntoNamespace(context.Background(), dgraphapi.DefaultUser,
			dgraphapi.DefaultPassword, x.GalaxyNamespace))

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
		defer func() { c.Cleanup(code != 0) }()
		x.Panic(c.Start())

		hc, err := c.HTTPClient()
		x.Panic(err)
		x.Panic(hc.LoginIntoNamespace(dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

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
