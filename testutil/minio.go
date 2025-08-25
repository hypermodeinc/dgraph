/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package testutil

import (
	"context"
	"fmt"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// NewMinioClient returns a minio client.
func NewMinioClient() (*minio.Client, error) {
	ensureAddressesInitialized()
	if MinioInstance == "" {
		return nil, fmt.Errorf("testutil.MinioInstance is not set")
	}

	mc, err := minio.New(MinioInstance, &minio.Options{
		Creds:  credentials.NewStaticV4("accesskey", "secretkey", ""),
		Secure: false,
	})
	if err != nil {
		return nil, err
	}

	var errHealthCheck error
	for i := 0; i < 5; i++ {
		// Use a short timeout for the health check to avoid long waits.
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)

		// BucketExists is a lightweight call to check for connectivity.
		// We don't care about the result, just that it doesn't error.
		_, errHealthCheck = mc.BucketExists(ctx, "healthcheck")
		if errHealthCheck == nil {
			cancel()
			return mc, nil
		}
		cancel()
		time.Sleep(time.Second)
	}

	return nil, fmt.Errorf("minio client not ready: %w", errHealthCheck)
}
