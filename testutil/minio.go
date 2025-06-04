/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package testutil

import (
	"github.com/minio/minio-go/v7"
)

// NewMinioClient returns a minio client.
func NewMinioClient() (*minio.Client, error) {
	return minio.New(MinioInstance, "accesskey", "secretkey", false)
}
