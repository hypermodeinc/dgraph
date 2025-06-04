/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package worker

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/golang/glog"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pkg/errors"

	"github.com/hypermodeinc/dgraph/v25/protos/pb"
	"github.com/hypermodeinc/dgraph/v25/x"
)

const (
	// backupPathFmt defines the path to store or index backup objects.
	// The expected parameter is a date in string format.
	backupPathFmt = `dgraph.%s`

	// backupNameFmt defines the name of backups files or objects (remote).
	// The first parameter is the read timestamp at the time of backup. This is used for
	// incremental backups and partial restore.
	// The second parameter is the group ID when backup happened. This is used for partitioning
	// the posting directories 'p' during restore.
	backupNameFmt = `r%d-g%d.backup`

	// backupManifest is the name of backup manifests. This a JSON file that contains the
	// details of the backup. A backup dir without a manifest is ignored.
	//
	// Example manifest:
	// {
	//   "since": 2280,
	//   "groups": [ 1, 2, 3 ],
	// }
	//
	// "since" is the read timestamp used at the backup request. This value is called "since"
	// because it used by subsequent incremental backups.
	// "groups" are the group IDs that participated.
	backupManifest = `manifest.json`

	tmpManifest = `manifest_tmp.json`
)

func createBackupFile(h UriHandler, uri *url.URL, req *pb.BackupRequest) (io.WriteCloser, error) {
	fileName := backupName(req.ReadTs, req.GroupId)
	dir := fmt.Sprintf(backupPathFmt, req.UnixTs)
	if err := h.CreateDir(dir); err != nil {
		return nil, errors.Wrap(err, "while creating backup file")
	}
	backupFile := filepath.Join(dir, fileName)
	w, err := h.CreateFile(backupFile)
	return w, errors.Wrap(err, "while creating backup file")
}

func backupName(since uint64, groupId uint32) string {
	return fmt.Sprintf(backupNameFmt, since, groupId)
}

// UriHandler interface is implemented by URI scheme handlers.
// When adding new scheme handles, for example 'azure://', an object will implement
// this interface to supply Dgraph with a way to create or load backup files into DB.
// For all methods below, the URL object is parsed as described in `newHandler' and
// the Processor object has the DB, estimated tablets size, and backup parameters.
type UriHandler interface {
	// CreateDir creates a directory relative to the root path of the handler.
	CreateDir(path string) error
	// CreateFile creates a file relative to the root path of the handler. It also makes the
	// handler's descriptor to point to this file.
	CreateFile(path string) (io.WriteCloser, error)
	// DirExists returns true if the directory relative to the root path of the handler exists.
	DirExists(path string) bool
	// FileExists returns true if the file relative to the root path of the handler exists.
	FileExists(path string) bool
	// JoinPath appends the given path to the root path of the handler.
	JoinPath(path string) string
	// ListPaths returns a list of all the valid paths from the given root path. The given root path
	// should be relative to the handler's root path.
	ListPaths(path string) []string
	// Read reads the file at given relative path and returns the read bytes.
	Read(path string) ([]byte, error)
	// Rename renames the src file to the destination file.
	Rename(src, dst string) error
	// Stream would stream the path via an instance of io.ReadCloser. Close must be called at the
	// end to release resources appropriately.
	Stream(path string) (io.ReadCloser, error)
}

// NewUriHandler parses the requested URI and finds the corresponding UriHandler.
// If the passed credentials are not nil, they will be used to override the
// default credentials (only for backups to minio or S3).
// Target URI formats:
//
//	[scheme]://[host]/[path]?[args]
//	[scheme]:///[path]?[args]
//	/[path]?[args] (only for local or NFS)
//
// Target URI parts:
//
//	scheme - service handler, one of: "file", "s3", "minio"
//	  host - remote address. ex: "dgraph.s3.amazonaws.com"
//	  path - directory, bucket or container at target. ex: "/dgraph/backups/"
//	  args - specific arguments that are ok to appear in logs.
//
// Global args (if supported by the handler):
//
//	  secure - true|false turn on/off TLS.
//	   trace - true|false turn on/off HTTP tracing.
//	compress - true|false turn on/off data compression.
//	 encrypt - true|false turn on/off data encryption.
//
// Examples:
//
//	s3://dgraph.s3.amazonaws.com/dgraph/backups?secure=true
//	minio://localhost:9000/dgraph?secure=true
//	file:///tmp/dgraph/backups
//	/tmp/dgraph/backups?compress=gzip
func NewUriHandler(uri *url.URL, creds *x.MinioCredentials) (UriHandler, error) {
	switch uri.Scheme {
	case "file", "":
		return NewFileHandler(uri), nil
	case "minio", "s3":
		return NewS3Handler(uri, creds)
	}
	return nil, errors.Errorf("Unable to handle url: %s", uri)
}

// fileHandler is used for 'file:' URI scheme.
type fileHandler struct {
	rootDir string
	prefix  string
}

func NewFileHandler(uri *url.URL) *fileHandler {
	h := &fileHandler{}
	h.rootDir, h.prefix = filepath.Split(uri.Path)
	return h
}

func (h *fileHandler) DirExists(path string) bool       { return pathExist(h.JoinPath(path)) }
func (h *fileHandler) FileExists(path string) bool      { return pathExist(h.JoinPath(path)) }
func (h *fileHandler) Read(path string) ([]byte, error) { return os.ReadFile(h.JoinPath(path)) }

func (h *fileHandler) JoinPath(path string) string {
	return filepath.Join(h.rootDir, h.prefix, path)
}
func (h *fileHandler) Stream(path string) (io.ReadCloser, error) {
	return os.Open(h.JoinPath(path))
}
func (h *fileHandler) ListPaths(path string) []string {
	path = h.JoinPath(path)
	return x.WalkPathFunc(path, func(path string, isDis bool) bool {
		return true
	})
}
func (h *fileHandler) CreateDir(path string) error {
	path = h.JoinPath(path)
	if err := os.MkdirAll(path, 0755); err != nil {
		return errors.Errorf("Create path failed to create path %s, got error: %v", path, err)
	}
	return nil
}

type fileSyncer struct {
	fp *os.File
}

func (fs *fileSyncer) Write(p []byte) (n int, err error) { return fs.fp.Write(p) }
func (fs *fileSyncer) Close() error {
	if err := fs.fp.Sync(); err != nil {
		return errors.Wrapf(err, "while syncing file: %s", fs.fp.Name())
	}
	err := fs.fp.Close()
	return errors.Wrapf(err, "while closing file: %s", fs.fp.Name())
}

func (h *fileHandler) CreateFile(path string) (io.WriteCloser, error) {
	path = h.JoinPath(path)
	fp, err := os.Create(path)
	return &fileSyncer{fp}, errors.Wrapf(err, "File handler failed to create file %s", path)
}

func (h *fileHandler) Rename(src, dst string) error {
	src = h.JoinPath(src)
	dst = h.JoinPath(dst)
	return os.Rename(src, dst)
}

// pathExist checks if a path (file or dir) is found at target.
// Returns true if found, false otherwise.
func pathExist(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	return !os.IsNotExist(err) && !os.IsPermission(err)
}

// S3 Handler.

// FillRestoreCredentials fills the empty values with the default credentials so that
// a restore request is sent to all the groups with the same credentials.
func FillRestoreCredentials(location string, req *pb.RestoreRequest) error {
	uri, err := url.Parse(location)
	if err != nil {
		return err
	}

	defaultCreds := credentials.Value{
		AccessKeyID:     req.AccessKey,
		SecretAccessKey: string(req.SecretKey),
		SessionToken:    string(req.SessionToken),
	}
	provider := x.MinioCredentialsProvider(uri.Scheme, defaultCreds)

	creds, _ := provider.Retrieve() // Error is always nil.

	req.AccessKey = creds.AccessKeyID
	req.SecretKey = pb.Sensitive(creds.SecretAccessKey)
	req.SessionToken = pb.Sensitive(creds.SessionToken)

	return nil
}

// s3Handler is used for 's3:' and 'minio:' URI schemes.
type s3Handler struct {
	bucketName   string
	objectPrefix string
	creds        *x.MinioCredentials
	uri          *url.URL
	mc           *x.MinioClient
}

// NewS3Handler creates a new session, checks valid bucket at uri.Path, and configures a
// minio client. It also fills in values used by the handler in subsequent calls.
// Returns a new S3 minio client, otherwise a nil client with an error.
func NewS3Handler(uri *url.URL, creds *x.MinioCredentials) (*s3Handler, error) {
	h := &s3Handler{
		creds: creds,
		uri:   uri,
	}
	mc, err := x.NewMinioClient(uri, creds)
	if err != nil {
		return nil, err
	}
	h.mc = mc
	h.bucketName, h.objectPrefix = mc.ParseBucketAndPrefix(uri.Path)
	return h, nil
}

func (h *s3Handler) CreateDir(path string) error { return nil }
func (h *s3Handler) DirExists(path string) bool  { return true }

func (h *s3Handler) FileExists(path string) bool {
	objectPath := h.getObjectPath(path)
	_, err := h.mc.StatObject(h.bucketName, objectPath, minio.StatObjectOptions{})
	if err == nil {
		return true
	}
	errResponse := minio.ToErrorResponse(err)
	if errResponse.Code != "NoSuchKey" {
		glog.Errorf("Failed to verify object existence: %v", err)
	}
	return false
}

func (h *s3Handler) JoinPath(path string) string {
	return filepath.Join(h.bucketName, h.objectPrefix, path)
}

func (h *s3Handler) Read(path string) ([]byte, error) {
	objectPath := h.getObjectPath(path)
	var buf bytes.Buffer

	reader, err := h.mc.GetObject(h.bucketName, objectPath, minio.GetObjectOptions{})
	if err != nil {
		return buf.Bytes(), errors.Wrap(err, "Failed to read s3 object")
	}
	defer reader.Close()

	if _, err := buf.ReadFrom(reader); err != nil {
		return buf.Bytes(), errors.Wrap(err, "Failed to read the s3 object")
	}
	return buf.Bytes(), nil
}

func (h *s3Handler) Stream(path string) (io.ReadCloser, error) {
	objectPath := h.getObjectPath(path)
	reader, err := h.mc.GetObject(h.bucketName, objectPath, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	return reader, nil
}

func (h *s3Handler) ListPaths(path string) []string {
	var paths []string
	done := make(chan struct{})
	defer close(done)
	path = h.getObjectPath(path)
	for object := range h.mc.ListObjects(h.bucketName, path, true, done) {
		paths = append(paths, object.Key)
	}
	return paths
}

type s3Writer struct {
	pwriter    *io.PipeWriter
	preader    *io.PipeReader
	bucketName string
	cerr       chan error
}

func (sw *s3Writer) Write(p []byte) (n int, err error) { return sw.pwriter.Write(p) }
func (sw *s3Writer) Close() error {
	if sw.pwriter == nil {
		return nil
	}
	if err := sw.pwriter.CloseWithError(nil); err != nil && err != io.EOF {
		glog.Errorf("Unexpected error when closing pipe: %v", err)
	}
	sw.pwriter = nil
	glog.V(2).Infof("Backup waiting for upload to complete.")
	return <-sw.cerr
}

// upload will block until it's done or an error occurs.
func (sw *s3Writer) upload(mc *x.MinioClient, object string) {
	f := func() error {
		start := time.Now()

		// We don't need to have a progress object, because we're using a Pipe. A write to Pipe
		// would block until it can be fully read. So, the rate of the writes here would be equal to
		// the rate of upload. We're already tracking progress of the writes in stream.Lists, so no
		// need to track the progress of read. By definition, it must be the same.
		//
		// PutObject would block until sw.preader returns EOF.
		n, err := mc.PutObject(sw.bucketName, object, sw.preader, -1, minio.PutObjectOptions{})
		glog.V(2).Infof("Backup sent %d bytes. Time elapsed: %s",
			n, time.Since(start).Round(time.Second))

		if err != nil {
			// This should cause Write to fail as well.
			glog.Errorf("Backup: Closing RW pipe due to error: %v", err)
			if err := sw.pwriter.Close(); err != nil {
				return err
			}
			if err := sw.preader.Close(); err != nil {
				return err
			}
		}
		return err
	}
	sw.cerr <- f()
}

func (h *s3Handler) CreateFile(path string) (io.WriteCloser, error) {
	objectPath := h.getObjectPath(path)
	glog.V(2).Infof("Sending data to %s blob %q ...", h.uri.Scheme, objectPath)

	sw := &s3Writer{
		bucketName: h.bucketName,
		cerr:       make(chan error, 1),
	}
	sw.preader, sw.pwriter = io.Pipe()
	go sw.upload(h.mc, objectPath)
	return sw, nil
}

func (h *s3Handler) Rename(srcPath, dstPath string) error {
	srcPath = h.getObjectPath(srcPath)
	dstPath = h.getObjectPath(dstPath)
	src := minio.NewSourceInfo(h.bucketName, srcPath, nil)
	dst, err := minio.NewDestinationInfo(h.bucketName, dstPath, nil, nil)
	if err != nil {
		return errors.Wrap(err, "Rename failed to create dstInfo")
	}
	// We try copying 100 times, if it still fails, then the user should manually rename.
	err = x.RetryUntilSuccess(100, time.Second, func() error {
		if err := h.mc.CopyObject(dst, src); err != nil {
			return errors.Wrapf(err, "While renaming object in s3, copy failed")
		}
		return nil
	})
	if err != nil {
		return err
	}

	err = h.mc.RemoveObject(h.bucketName, srcPath)
	return errors.Wrap(err, "Rename failed to remove temporary file")
}

func (h *s3Handler) getObjectPath(path string) string {
	return filepath.Join(h.objectPrefix, path)
}
