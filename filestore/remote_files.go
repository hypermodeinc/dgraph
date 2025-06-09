/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package filestore

import (
	"bufio"
	"context"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"

	"github.com/hypermodeinc/dgraph/v25/chunker"
	"github.com/hypermodeinc/dgraph/v25/x"
)

type remoteFiles struct {
	mc *x.MinioClient
}

func (rf *remoteFiles) Open(path string) (io.ReadCloser, error) {
	url, err := url.Parse(path)
	if err != nil {
		return nil, err
	}

	bucket, prefix := rf.mc.ParseBucketAndPrefix(url.Path)
	obj, err := rf.mc.GetObject(context.Background(), bucket, prefix, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	return obj, nil
}

// Checking if a file exists is a no-op in minio, since s3 cannot confirm if a directory exists
func (rf *remoteFiles) Exists(path string) bool {
	return true
}

func hasAnySuffix(str string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(str, suffix) {
			return true
		}
	}
	return false
}

func (rf *remoteFiles) FindDataFiles(str string, ext []string) (paths []string) {
	for _, dirPath := range strings.Split(str, ",") {
		url, err := url.Parse(dirPath)
		x.Check(err)

		c := make(chan struct{})
		defer close(c)

		bucket, prefix := rf.mc.ParseBucketAndPrefix(url.Path)
		objs := rf.mc.ListObjects(context.Background(), bucket,
			minio.ListObjectsOptions{Prefix: prefix, Recursive: true})
		for obj := range objs {
			if hasAnySuffix(obj.Key, ext) {
				paths = append(paths, bucket+"/"+obj.Key)
			}
		}
	}
	return
}

func (rf *remoteFiles) ChunkReader(file string, key x.Sensitive) (*bufio.Reader, func()) {
	url, err := url.Parse(file)
	x.Check(err)

	bucket, prefix := rf.mc.ParseBucketAndPrefix(url.Path)

	obj, err := rf.mc.GetObject(context.Background(), bucket, prefix, minio.GetObjectOptions{})
	x.Check(err)

	return chunker.StreamReader(url.Path, key, obj)
}

var _ FileStore = (*remoteFiles)(nil)
