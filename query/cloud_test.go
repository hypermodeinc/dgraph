//go:build cloud

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package query

import (
	"context"
	"testing"
	"time"

	"github.com/hypermodeinc/dgraph/v24/dgraphapi"
	"github.com/hypermodeinc/dgraph/v24/dgraphtest"
	"github.com/hypermodeinc/dgraph/v24/x"
)

func TestMain(m *testing.M) {
	c, err := dgraphtest.NewDCloudCluster()
	x.Panic(err)

	dg, cleanup, err := c.Client()
	x.Panic(err)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	x.Panic(dg.LoginIntoNamespace(ctx, dgraphapi.DefaultUser, dgraphapi.DefaultPassword, x.GalaxyNamespace))

	dc = c
	client.Dgraph = dg
	populateCluster(dc)
	m.Run()
}
