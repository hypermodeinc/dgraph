/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package dgraphtest

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var (
	baseRepoDir      string // baseRepoDir points to the dgraph repo from where tests are running
	repoDir          string // repoDir to store cloned repository of dgraph
	binariesPath     string // binariesPath to store multiple binary versions
	datasetFilesPath string
)

const (
	dgraphRepoUrl = "https://github.com/hypermodeinc/dgraph.git"
	cloneTimeout  = 10 * time.Minute
)

func init() {
	// init logging
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// setup paths
	_, thisFilePath, _, _ := runtime.Caller(0)
	basePath := strings.ReplaceAll(thisFilePath, "/paths.go", "")
	baseRepoDir = strings.ReplaceAll(basePath, "/dgraphtest", "")
	binariesPath = filepath.Join(basePath, "binaries")
	datasetFilesPath = filepath.Join(basePath, "datafiles")

	var err error
	repoDir, err = os.MkdirTemp("", "dgraph-repo")
	if err != nil {
		panic(err)
	}

	log.Printf("[INFO] baseRepoDir: %v", baseRepoDir)
	log.Printf("[INFO] repoDir: %v", repoDir)
	log.Printf("[INFO] binariesPath: %v", binariesPath)
}
