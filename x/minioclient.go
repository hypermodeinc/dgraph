/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package x

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/golang/glog"
	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pkg/errors"

	"github.com/hypermodeinc/dgraph/v25/protos/pb"
)

const (
	// Shown in transfer logs
	appName = "Dgraph"

	// defaultEndpointS3 is used with s3 scheme when no host is provided
	defaultEndpointS3 = "s3.amazonaws.com"

	// s3AccelerateSubstr S3 acceleration is enabled if the S3 host is contains this substring.
	// See http://docs.aws.amazon.com/AmazonS3/latest/dev/transfer-acceleration.html
	s3AccelerateSubstr = "s3-accelerate"
)

// MinioCredentials holds the credentials needed to perform a backup/export operation.
// If these credentials are missing the default credentials will be used.
type MinioCredentials struct {
	AccessKey    string
	SecretKey    pb.Sensitive
	SessionToken pb.Sensitive
	Anonymous    bool
}

type MinioClient struct {
	*minio.Client
}

func (creds *MinioCredentials) isAnonymous() bool {
	if creds == nil {
		return false
	}
	return creds.Anonymous
}

func MinioCredentialsProviderWithoutEnv(requestCreds credentials.Value) credentials.Provider {
	providers := []credentials.Provider{&credentials.Static{Value: requestCreds}}
	return &credentials.Chain{Providers: providers}
}

func MinioCredentialsProvider(scheme string, requestCreds credentials.Value) credentials.Provider {
	providers := []credentials.Provider{&credentials.Static{Value: requestCreds}}

	switch scheme {
	case "s3":
		providers = append(providers, &credentials.EnvAWS{}, &credentials.IAM{Client: &http.Client{}})
	default:
		providers = append(providers, &credentials.EnvMinio{})
	}

	return &credentials.Chain{Providers: providers}
}

func requestCreds(creds *MinioCredentials) credentials.Value {
	if creds == nil {
		return credentials.Value{}
	}

	return credentials.Value{
		AccessKeyID:     creds.AccessKey,
		SecretAccessKey: string(creds.SecretKey),
		SessionToken:    string(creds.SessionToken),
	}
}

func NewMinioClient(uri *url.URL, creds *MinioCredentials) (*MinioClient, error) {
	if len(uri.Path) < 1 {
		return nil, errors.Errorf("Invalid bucket: %q", uri.Path)
	}

	glog.V(2).Infof("Backup/Export using host: %s, path: %s", uri.Host, uri.Path)

	// Verify URI and set default S3 host if needed.
	switch uri.Scheme {
	case "s3":
		// s3:///bucket/folder
		if !strings.Contains(uri.Host, ".") {
			uri.Host = defaultEndpointS3
		}
	default: // minio
		if uri.Host == "" {
			return nil, errors.Errorf("Minio handler requires a host")
		}
	}

	secure := uri.Query().Get("secure") != "false" // secure by default

	if creds.isAnonymous() {
		mc, err := minio.New(uri.Host, "", "", secure)
		if err != nil {
			return nil, err
		}
		return &MinioClient{mc}, nil
	}

	var credsProvider *credentials.Credentials
	if Config.SharedInstance {
		credsProvider = credentials.New(MinioCredentialsProviderWithoutEnv(requestCreds(creds)))
	} else {
		credsProvider = credentials.New(MinioCredentialsProvider(uri.Scheme, requestCreds(creds)))
	}

	mc, err := minio.NewWithCredentials(uri.Host, credsProvider, secure, "")

	if err != nil {
		return nil, err
	}

	// Set client app name "Dgraph/v1.0.x"
	mc.SetAppInfo(appName, Version())

	// S3 transfer acceleration support.
	if uri.Scheme == "s3" && strings.Contains(uri.Host, s3AccelerateSubstr) {
		mc.SetS3TransferAccelerate(uri.Host)
	}

	// enable HTTP tracing
	if uri.Query().Get("trace") == "true" {
		mc.TraceOn(os.Stderr)
	}

	return &MinioClient{mc}, nil
}

// ParseBucketAndPrefix returns the bucket and prefix given a path string
func (*MinioClient) ParseBucketAndPrefix(path string) (string, string) {
	if path[0] == '/' {
		path = path[1:]
	}
	parts := strings.Split(path, "/")
	bucketName := parts[0] // bucket
	objectPrefix := ""
	if len(parts) > 1 {
		objectPrefix = filepath.Join(parts[1:]...)
	}
	return bucketName, objectPrefix
}

func (mc *MinioClient) ValidateBucket(uri *url.URL) (string, string, error) {
	bucketName, objectPrefix := mc.ParseBucketAndPrefix(uri.Path)

	// verify the requested bucket exists.
	found, err := mc.BucketExists(bucketName)
	if err != nil {
		return "", "", errors.Wrapf(err, "while looking for bucket %s at host %s", bucketName, uri.Host)
	}
	if !found {
		return "", "", errors.Errorf("Bucket was not found: %s", bucketName)
	}

	return bucketName, objectPrefix, nil
}
