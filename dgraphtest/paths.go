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
	baseRepoDir  string // baseRepoDir points to the dgraph repo from where tests are running
	repoDir      string // repoDir to store cloned repository of dgraph
	binariesPath string // binariesPath to store multiple binary versions
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

	var err error
	repoDir, err = os.MkdirTemp("", "dgraph-repo")
	if err != nil {
		panic(err)
	}

	log.Printf("[INFO] baseRepoDir: %v", baseRepoDir)
	log.Printf("[INFO] repoDir: %v", repoDir)
	log.Printf("[INFO] binariesPath: %v", binariesPath)
}
