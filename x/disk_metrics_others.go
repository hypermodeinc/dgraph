//go:build !linux
// +build !linux

/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package x

import (
	"github.com/golang/glog"

	"github.com/dgraph-io/ristretto/v2/z"
)

func MonitorDiskMetrics(_ string, _ string, lc *z.Closer) {
	defer lc.Done()
	glog.Infoln("File system metrics are not currently supported on non-Linux platforms")
}
