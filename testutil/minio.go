/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package testutil

import (
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// NewMinioClient returns a minio client.
func NewMinioClient() (*minio.Client, error) {
	return minio.New(MinioInstance, &minio.Options{
		Creds:      credentials.NewStaticV4("accesskey", "secretkey", ""),
		Secure:     false,
		MaxRetries: 20,
	})
}
