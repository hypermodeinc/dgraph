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
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pkg/errors"
	"golang.org/x/mod/modfile"
)

var (
	cloneOnce sync.Once
)

func (c *LocalCluster) dgraphImage() string {
	return "dgraph/dgraph:local"
}

func (c *LocalCluster) setupBinary() error {
	if err := ensureDgraphClone(); err != nil {
		panic(err)
	}

	if c.conf.customPlugins {
		race := false // Explicit var declaration to avoid confusion on the next line
		if err := c.GeneratePlugins(race); err != nil {
			return err
		}
	}
	if c.conf.version == localVersion {
		fromDir := filepath.Join(os.Getenv("GOPATH"), "bin")
		return copyBinary(fromDir, c.tempBinDir, c.conf.version)
	}

	isFileThere, err := fileExists(filepath.Join(binariesPath, fmt.Sprintf(binaryNameFmt, c.conf.version)))
	if err != nil {
		return err
	}
	if isFileThere {
		return copyBinary(binariesPath, c.tempBinDir, c.conf.version)
	}

	if err := runGitCheckout(c.conf.version); err != nil {
		return err
	}
	if err := buildDgraphBinary(repoDir, binariesPath, c.conf.version); err != nil {
		return err
	}
	return copyBinary(binariesPath, c.tempBinDir, c.conf.version)
}

func ensureDgraphClone() error {
	f := func() error {
		if _, err := os.Stat(repoDir); err != nil {
			return runGitClone()
		}

		if err := runGitStatus(); err != nil {
			if err := os.RemoveAll(repoDir); err != nil {
				return err
			}
			return runGitClone()
		}
		return nil
	}

	var err error
	cloneOnce.Do(func() {
		err = f()
	})
	return err
}

func runGitClone() error {
	// The dgraph repo is already cloned for running the test. We can just create
	// a copy of this folder by running git clone using this already cloned dgraph
	// repo. After the quick clone, we update the original URL to point to the
	// GitHub dgraph repo and perform a "git fetch".
	log.Printf("[INFO] cloning dgraph repo from [%v]", baseRepoDir)
	cmd := exec.Command("git", "clone", baseRepoDir, repoDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrapf(err, "error cloning dgraph repo\noutput:%v", string(out))
	}

	cmd = exec.Command("git", "remote", "set-url", "origin", dgraphRepoUrl)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrapf(err, "error setting remote URL\noutput:%v", string(out))
	}

	// we do not return error if git fetch fails because maybe there are no changes
	// to pull and it doesn't make sense to fail right now. We can fail later when we
	// do not find the reference that we are looking for.
	if err := runGitFetch(); err != nil {
		log.Printf("[WARNING] error in fetching latest git changes in runGitClone: %v", err)
	}
	return nil
}

func runGitStatus() error {
	cmd := exec.Command("git", "status")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrapf(err, "error running git status\noutput:%v", string(out))
	}
	return nil
}

func runGitFetch() error {
	cmd := exec.Command("git", "fetch", "-p")
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrapf(err, "error fetching latest changes\noutput:%v", string(out))
	}
	return nil
}

func runGitCheckout(gitRef string) error {
	cmd := exec.Command("git", "checkout", "-f", gitRef)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrapf(err, "error checking out gitRef [%v]\noutput:%v", gitRef, string(out))
	}
	return nil
}

func getHash(ref string) (string, error) {
	cmd := exec.Command("git", "rev-parse", ref)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", errors.Wrapf(err, "error while running rev-parse on [%v]\noutput:%v", ref, string(out))
	} else {
		return strings.TrimSpace(string(out)), nil
	}
}

func buildDgraphBinary(dir, binaryDir, version string) error {
	log.Printf("[INFO] building dgraph binary for version [%v]", version)

	if err := fixGoModIfNeeded(); err != nil {
		return err
	}

	cmd := exec.Command("make", "dgraph")
	cmd.Dir = filepath.Join(dir, "dgraph")
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.Wrapf(err, "error while building dgraph binary\noutput:%v", string(out))
	}
	if err := copy(filepath.Join(dir, "dgraph", "dgraph"),
		filepath.Join(binaryDir, fmt.Sprintf(binaryNameFmt, version))); err != nil {
		return errors.Wrap(err, "error while copying binary")
	}
	return nil
}

func copyBinary(fromDir, toDir, version string) error {
	binaryName := "dgraph"
	if version != localVersion {
		binaryName = fmt.Sprintf(binaryNameFmt, version)
	}
	fromPath := filepath.Join(fromDir, binaryName)
	toPath := filepath.Join(toDir, "dgraph")
	if err := copy(fromPath, toPath); err != nil {
		return errors.Wrap(err, "error while copying binary into tempBinDir")
	}
	return nil
}

func copy(src, dst string) error {
	// Validate inputs
	if src == "" || dst == "" {
		return errors.New("source or destination paths cannot be empty")
	}

	// Check source file
	sourceFileStat, err := os.Stat(src)
	if err != nil {
		return errors.Wrapf(err, "failed to stat source file: %s", src)
	}
	if !sourceFileStat.Mode().IsRegular() {
		return errors.Errorf("%s is not a regular file", src)
	}

	// Open source file
	source, err := os.Open(src)
	if err != nil {
		return errors.Wrapf(err, "failed to open source file: %s", src)
	}
	defer source.Close()

	// Create destination directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return errors.Wrapf(err, "failed to create destination directory: %s", filepath.Dir(dst))
	}

	// Create destination file
	destination, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, sourceFileStat.Mode())
	if err != nil {
		return errors.Wrapf(err, "failed to create destination file: %s", dst)
	}

	// Copy the file
	if _, err := io.Copy(destination, source); err != nil {
		return errors.Wrap(err, "failed to copy file contents")
	}

	// Ensure data is flushed to disk
	if err := destination.Sync(); err != nil {
		return errors.Wrap(err, "failed to sync destination file")
	}

	// Close the destination file
	if err := destination.Close(); err != nil {
		return errors.Wrap(err, "failed to close destination file")
	}

	// Stat the new file to check file size match
	destStat, err := os.Stat(dst)
	if err != nil {
		return errors.Wrapf(err, "failed to stat destination file: %s", dst)
	}
	if destStat.Size() != sourceFileStat.Size() {
		log.Printf("[WARNING] size mismatch after copy of %s to %s: source=%d bytes, destination=%d bytes",
			src, dst, sourceFileStat.Size(), destStat.Size())
	}

	return nil
}

// IsHigherVersion checks whether "higher" is the higher version compared to "lower"
func IsHigherVersion(higher, lower string) (bool, error) {
	// the order of if conditions matters here
	if lower == localVersion {
		return false, nil
	}
	if higher == localVersion {
		return true, nil
	}

	// An older commit is usually the ancestor of a newer commit which is a descendant commit
	cmd := exec.Command("git", "merge-base", "--is-ancestor", lower, higher)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok && exitError.ExitCode() == 1 {
			return false, nil
		}

		return false, errors.Wrapf(err, "error checking if [%v] is ancestor of [%v]\noutput:%v",
			higher, lower, string(out))
	}

	return true, nil
}

func fixGoModIfNeeded() error {
	repoModFilePath := filepath.Join(repoDir, "go.mod")
	repoModFile, err := modfile.Parse(repoModFilePath, nil, nil)
	if err != nil {
		return errors.Wrapf(err, "error parsing mod file in repoDir [%v]", repoDir)
	}

	modFile, err := modfile.Parse("go.mod", nil, nil)
	if err != nil {
		return errors.Wrapf(err, "error while parsing go.mod file")
	}

	if len(modFile.Replace) == len(repoModFile.Replace) {
		return nil
	}

	repoModFile.Replace = modFile.Replace
	if data, err := repoModFile.Format(); err != nil {
		return errors.Wrapf(err, "error while formatting mod file")
	} else if err := os.WriteFile(repoModFilePath, data, 0644); err != nil {
		return errors.Wrapf(err, "error while writing to go.mod file")
	}
	return nil
}
