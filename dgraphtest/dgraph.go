/*
 * SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
 * SPDX-License-Identifier: Apache-2.0
 */

package dgraphtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/mount"
	docker "github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/pkg/errors"
)

const (
	binaryNameFmt    = "dgraph_%v"
	zeroNameFmt      = "%v_zero%d"
	zeroAliasNameFmt = "zero%d"
	alphaNameFmt     = "%v_alpha%d"
	alphaLNameFmt    = "alpha%d"
	volNameFmt       = "%v_%v"

	zeroGrpcPort   = "5080"
	zeroHttpPort   = "6080"
	alphaInterPort = "7080"
	alphaHttpPort  = "8080"
	alphaGrpcPort  = "9080"

	alphaWorkingDir  = "/data/alpha"
	zeroWorkingDir   = "/data/zero"
	DefaultAlphaPDir = "/data/alpha/p"
	DefaultBackupDir = "/data/backups"
	DefaultExportDir = "/data/exports"

	secretsMountPath   = "/secrets"
	aclKeyFile         = "secret-key"
	aclSecretMountPath = "/secrets/secret-key"
	encKeyFile         = "enc-key"
	encKeyMountPath    = "/secrets/enc-key"

	goBinMountPath     = "/gobin"
	localVersion       = "local"
	waitDurBeforeRetry = time.Second
	requestTimeout     = 120 * time.Second
)

var (
	errNotImplemented = errors.New("NOT IMPLEMENTED")
)

func fileExists(filename string) (bool, error) {
	info, err := os.Stat(filename)
	if err != nil && !os.IsNotExist(err) {
		return false, errors.Wrap(err, "error while getting file info")
	}
	if err != nil && os.IsNotExist(err) {
		return false, nil
	}
	return !info.IsDir(), nil
}

type dnode interface {
	cname() string
	aname() string
	cid() string
	ports() nat.PortSet
	bindings(int) nat.PortMap
	cmd(*LocalCluster) []string
	workingDir() string
	mounts(*LocalCluster) ([]mount.Mount, error)
	healthURL(*LocalCluster) (string, error)
	assignURL(*LocalCluster) (string, error)
	alphaURL(*LocalCluster) (string, error)
	zeroURL(*LocalCluster) (string, error)
	changeStatus(bool)
}

type zero struct {
	id            int    // 0, 1, 2
	containerID   string // container ID in docker world
	containerName string // something like test-1234_zero2
	aliasName     string // something like alpha0, zero1
	isRunning     bool
}

func (z *zero) cname() string {
	return z.containerName
}

func (z *zero) aname() string {
	return z.aliasName
}

func (z *zero) cid() string {
	return z.containerID
}

func (z *zero) ports() nat.PortSet {
	return nat.PortSet{
		zeroGrpcPort: {},
		zeroHttpPort: {},
	}
}

func (z *zero) bindings(offset int) nat.PortMap {
	if offset < 0 {
		return nil
	}

	grpcPort, _ := strconv.Atoi(zeroGrpcPort)
	httpPort, _ := strconv.Atoi(zeroHttpPort)
	return nat.PortMap(map[nat.Port][]nat.PortBinding{
		zeroGrpcPort: {{HostPort: strconv.Itoa(grpcPort + offset + z.id)}},
		zeroHttpPort: {{HostPort: strconv.Itoa(httpPort + offset + z.id)}},
	})
}

func (z *zero) cmd(c *LocalCluster) []string {
	zcmd := []string{"/gobin/dgraph", "zero", fmt.Sprintf("--my=%s:%v", z.aname(), zeroGrpcPort), "--bindall",
		fmt.Sprintf(`--replicas=%v`, c.conf.replicas), "--logtostderr", fmt.Sprintf("-v=%d", c.conf.verbosity)}

	if c.lowerThanV21 {
		zcmd = append(zcmd, fmt.Sprintf(`--idx=%v`, z.id+1), "--telemetry=false")
	} else {
		zcmd = append(zcmd, fmt.Sprintf(`--raft=idx=%v`, z.id+1), "--telemetry=reports=false;",
			fmt.Sprintf(`--limit=refill-interval=%v;uid-lease=%v`, c.conf.refillInterval, c.conf.uidLease))
	}

	if z.id > 0 {
		zcmd = append(zcmd, "--peer="+c.zeros[0].aname()+":"+zeroGrpcPort)
	}

	return zcmd
}

func (z *zero) workingDir() string {
	return zeroWorkingDir
}

func (z *zero) mounts(c *LocalCluster) ([]mount.Mount, error) {
	var mounts []mount.Mount
	binMount, err := mountBinary(c)
	if err != nil {
		return nil, err
	}
	mounts = append(mounts, binMount)
	return mounts, nil
}

func (z *zero) healthURL(c *LocalCluster) (string, error) {
	publicPort, err := publicPort(c.dcli, z, zeroHttpPort)
	if err != nil {
		return "", err
	}
	return "http://0.0.0.0:" + publicPort + "/health", nil
}

func (z *zero) changeStatus(isRunning bool) {
	z.isRunning = isRunning
}

func (z *zero) assignURL(c *LocalCluster) (string, error) {
	publicPort, err := publicPort(c.dcli, z, zeroHttpPort)
	if err != nil {
		return "", err
	}
	return "http://0.0.0.0:" + publicPort + "/assign", nil
}

func (z *zero) alphaURL(c *LocalCluster) (string, error) {
	return "", errNotImplemented
}

func (z *zero) zeroURL(c *LocalCluster) (string, error) {
	publicPort, err := publicPort(c.dcli, z, zeroGrpcPort)
	if err != nil {
		return "", err
	}
	return "0.0.0.0:" + publicPort + "", nil
}

type alpha struct {
	id            int
	containerID   string
	containerName string
	aliasName     string
	isRunning     bool
}

func (a *alpha) cname() string {
	return a.containerName
}

func (a *alpha) cid() string {
	return a.containerID
}

func (a *alpha) aname() string {
	return a.aliasName
}

func (a *alpha) ports() nat.PortSet {
	return nat.PortSet{
		alphaGrpcPort: {},
		alphaHttpPort: {},
	}
}

func (a *alpha) bindings(offset int) nat.PortMap {
	if offset < 0 {
		return nil
	}

	grpcPort, _ := strconv.Atoi(alphaGrpcPort)
	httpPort, _ := strconv.Atoi(alphaHttpPort)
	return nat.PortMap(map[nat.Port][]nat.PortBinding{
		alphaGrpcPort: {{HostPort: strconv.Itoa(grpcPort + offset + a.id)}},
		alphaHttpPort: {{HostPort: strconv.Itoa(httpPort + offset + a.id)}},
	})
}

func (a *alpha) cmd(c *LocalCluster) []string {
	acmd := []string{"/gobin/dgraph", "alpha", fmt.Sprintf("--my=%s:%v", a.aname(), alphaInterPort),
		"--bindall", "--logtostderr", fmt.Sprintf("-v=%d", c.conf.verbosity)}

	if c.lowerThanV21 {
		acmd = append(acmd, `--whitelist=10.0.0.0/8,172.16.0.0/12,192.168.0.0/16`, "--telemetry=false")
	} else {
		acmd = append(acmd, `--security=whitelist=10.0.0.0/8,172.16.0.0/12,192.168.0.0/16`,
			"--telemetry=reports=false;")
	}

	if c.conf.lambdaURL != "" {
		acmd = append(acmd, fmt.Sprintf(`--graphql=lambda-url=%s`, c.conf.lambdaURL))
	}

	if c.conf.acl {
		if c.lowerThanV21 {
			acmd = append(acmd, fmt.Sprintf(`--acl_secret_file=%s`, aclSecretMountPath),
				fmt.Sprintf(`--acl_access_ttl=%s`, c.conf.aclTTL))
		} else {
			aclPart := "--acl="
			if c.conf.aclTTL > 0 {
				aclPart += fmt.Sprintf(`secret-file=%s;access-ttl=%s;`, aclSecretMountPath, c.conf.aclTTL)
			}
			if c.conf.aclAlg != nil {
				aclPart += fmt.Sprintf(`jwt-alg=%s`, c.conf.aclAlg.Alg())
			}
			acmd = append(acmd, aclPart)
		}
	}
	if c.conf.encryption {
		if c.lowerThanV21 {
			acmd = append(acmd, fmt.Sprintf(`--encryption_key_file=%v`, encKeyMountPath))
		} else {
			acmd = append(acmd, fmt.Sprintf(`--encryption=key-file=%v`, encKeyMountPath))
		}
	}

	zeroAddrsArg, delimiter := "--zero=", ""
	for _, zo := range c.zeros {
		zeroAddrsArg += fmt.Sprintf("%s%v:%v", delimiter, zo.aname(), zeroGrpcPort)
		delimiter = ","
	}
	acmd = append(acmd, zeroAddrsArg)

	if len(c.conf.featureFlags) > 0 {
		acmd = append(acmd, fmt.Sprintf("--feature-flags=%v", strings.Join(c.conf.featureFlags, ";")))
	}

	if c.conf.customPlugins {
		acmd = append(acmd, fmt.Sprintf("--custom_tokenizers=%s", c.customTokenizers))
	}

	if c.conf.snapShotAfterEntries != 0 {
		acmd = append(acmd, fmt.Sprintf("--raft=%s",
			fmt.Sprintf(`snapshot-after-entries=%v;snapshot-after-duration=%v;`,
				c.conf.snapShotAfterEntries, c.conf.snapshotAfterDuration)))
	}

	return acmd
}

func (a *alpha) workingDir() string {
	return alphaWorkingDir
}

func (a *alpha) mounts(c *LocalCluster) ([]mount.Mount, error) {
	var mounts []mount.Mount
	binMount, err := mountBinary(c)
	if err != nil {
		return nil, err
	}
	mounts = append(mounts, binMount)

	if c.conf.acl || c.conf.encryption {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   c.tempSecretsDir,
			Target:   secretsMountPath,
			ReadOnly: true,
		})
	}

	if c.conf.bulkOutDirForMount != "" {
		pDir := filepath.Join(c.conf.bulkOutDirForMount, strconv.Itoa(a.id/c.conf.replicas), "p")
		if err := os.MkdirAll(pDir, os.ModePerm); err != nil {
			return nil, errors.Wrap(err, "erorr creating bulk dir")
		}
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   pDir,
			Target:   DefaultAlphaPDir,
			ReadOnly: false,
		})
	}

	for dir, vol := range c.conf.volumes {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   vol,
			Target:   dir,
			ReadOnly: false,
		})
	}
	return mounts, nil
}

func (a *alpha) healthURL(c *LocalCluster) (string, error) {
	publicPort, err := publicPort(c.dcli, a, alphaHttpPort)
	if err != nil {
		return "", err
	}
	return "http://0.0.0.0:" + publicPort + "/health", nil
}

func (a *alpha) assignURL(c *LocalCluster) (string, error) {
	return "", errors.New("no assign URL for alpha")
}

func (a *alpha) alphaURL(c *LocalCluster) (string, error) {
	publicPort, err := publicPort(c.dcli, a, alphaGrpcPort)
	if err != nil {
		return "", err
	}
	return "0.0.0.0:" + publicPort + "", nil
}

func (a *alpha) changeStatus(isRunning bool) {
	a.isRunning = isRunning
}

func (a *alpha) zeroURL(c *LocalCluster) (string, error) {
	return "", errNotImplemented
}

func publicPort(dcli *docker.Client, dc dnode, privatePort string) (string, error) {
	// TODO(aman): we should cache the port information
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	info, err := dcli.ContainerInspect(ctx, dc.cid())
	if err != nil {
		return "", errors.Wrap(err, "error inspecting container")
	}

	for port, bindings := range info.NetworkSettings.Ports {
		if len(bindings) == 0 {
			continue
		}
		if port.Port() != privatePort {
			continue
		}

		for _, binding := range bindings {
			if binding.HostIP == "0.0.0.0" {
				return binding.HostPort, nil
			}
		}
	}

	return "", fmt.Errorf("no mapping found for private port [%v] for container [%v]", privatePort, dc.cname())
}

func mountBinary(c *LocalCluster) (mount.Mount, error) {
	// We shouldn't need to call setupBinary here, we already call it in LocalCluster.setupBeforeCluster
	// function which is called whenever the dgraph's cluster version is initialized or upgraded. Though,
	// we have observed "exec format error" when we don't do this. Our suspicion is that this is related
	// to the fact that we mount same binary inside multiple docker containers. We noticed a similar
	// issue with this PR when we mount same ACL secret file inside multiple containers.
	if err := c.setupBinary(); err != nil {
		return mount.Mount{}, err
	}

	return mount.Mount{
		Type:     mount.TypeBind,
		Source:   c.tempBinDir,
		Target:   goBinMountPath,
		ReadOnly: true,
	}, nil
}

// ShouldSkipTest skips a given test if clusterVersion < minVersion
func ShouldSkipTest(t *testing.T, minVersion, clusterVersion string) {
	supported, err := IsHigherVersion(clusterVersion, minVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !supported {
		t.Skipf("test is valid for commits greater than [%v]", minVersion)
	}
}
