# Dgraph Testing Framework

Dgraph employs a ~~complex~~ sophisticated testing framework that includes extensive test coverage.
Due to the comprehensive nature of these tests, a complete test run can take several hours,
depending on your hardware. To manage this complex testing process efficiently, we've developed a
custom test framework implemented in Go: [t.go](t.go). This specialized runner provides enhanced
control and flexibility beyond what's available through the standard Go testing framework.

**Note:** This testing framework was built with Linux in mind. Non-Linux testing _can_ be
achieved—see [Running tests on OSX](#running-tests-on-osx) below.

## Requirements

The framework requires several external dependencies. You can check your system for the required
dependencies by running `make check`.

### Go

Version 1.22.12 or higher.

### Docker

The framework uses Docker extensively for integration testing. Tests will often "fail" if Docker is
not given enough resources, specifically memory. If you experience testing that seems to hang
waiting for connections to other cluster members, it's probably a memory issue.

The test runner takes a concurrency flag (./t -j=N) which will attempt to create _N_ number of
Dgraph clusters and run tests concurrently in those clusters. If you are testing on a machine with
limited resources, we advise you to not set this above _1_ (which is the default).

You can preserve the test Docker containers for failure analysis with the `--keep` flag.

### gotestsum

The framework uses [gotestsum](https://github.com/gotestyourself/gotestsum#install) for collating
test output and other advanced functions.

### protoc

On non-Linux systems, protocol buffer tests are skipped. On Linux systems, instructions for
installing and configuring protoc can be found [here](https://github.com/protocolbuffers/protobuf).
Or, `sudo apt update && sudo apt install -y protobuf-compiler`.

## Running Tests

The tests use the Dgraph binary found at $(GOPATH)/bin/dgraph. To check your current $GOPATH:
`go env GOPATH`.

---

Use the `make install` target in the top-level Makefile to build a binary with your changes that
need testing. Note for non-Linux users: because the binary is run in the Docker environment, it
needs to be a valid Linux executable. See the section below on
[Running tests on OSX](#running-tests-on-osx).

First, build the `t` program if you haven't already:

```sh
make check && go build .
```

This will produce a `t` executable, which is the testing framework _cli_.

To see a list of available flags:

```sh
./t --help
```

### Testing packages

One popular use of the framework is to target a specific package for integration testing. For
instance, to test the GraphQL system:

```sh
./t --pkg=graphql/e2e/normal
```

Multiple packages can be specified by separated them with a comma.

### Testing suites

You can test one or more "suites" of functionality using the `--suite` flag. For instance:

```sh
./t --suite=core,vector
```

The `--help` flag lists the available suites.

### Testing single test functions

```sh
./t --test=TestParseCountValError
```

This flag uses `ack` to find all tests matching the specified name(s).

### Other useful flags

The `--dry` (dry-run) flag can be used to list the packages that will be included for testing
without actually invoking the tests.

The `--skip-slow` flag will skip tests known to be slow to complete.

## Docker Compose Conventions

The Docker-based integration tests follow a hierarchical file discovery system. When executing a
test, the framework first searches for a docker-compose.yml file in the test's immediate package
directory. If no file is found, it progressively checks parent directories until it locates one. The
root-level test configuration file is stored at
[../dgraph/docker-compose.yml](../dgraph/docker-compose.yml). When implementing tests that require
unique Docker configurations not covered by existing compose files, you should create a new
directory with your tests also containing a custom docker-compose.yml file tailored to your specific
testing requirements.

## Running tests on OSX

The testing framework works well on Linux systems. Some additional steps need to be taken for the
tests to run on OSX.

### Install location

The Docker environment used to perform integration testing uses the dgraph binary found in
$GOPATH/bin. This binary is required to be a GOOS=linux image. The following commands need to be run
prior to starting tests to ensure the appropriate images are in place. Note, if your GOPATH variable
is not set, run ``export GOPATH=`go env GOPATH` ``

```sh
cd ..
# builds the OSX version
make install
mv $GOPATH/bin/dgraph $GOPATH/bin/dgraph_osx
# builds the linux version, take note of where the target reports it has written the dgraph executable
GOOS=linux make install
mv $GOPATH/bin/linux_arm64/dgraph $GOPATH/bin/dgraph
cd t
make check
```

The following environment variables are needed when tests are executed:

- GOPATH - needed to map the Dgraph image in Docker environments
- DGRAPH_BINARY - the system-native (OSX) dgraph image, used by some tests not in the Docker
  environment
- DOCKER_HOST - on newer Docker Desktop versions, the Docker communications socket was moved to your
  home folder

Example:

```sh
export GOPATH=`go env GOPATH`
export DGRAPH_BINARY=$GOPATH/bin/dgraph_osx
export DOCKER_HOST=unix://${HOME}/.docker/run/docker.sock
```

At this point, the `t` executable can be run as described above.

### Common Pitfalls

If you see `exec format error` output from test runs, it is most likely because some tests attempt
to run the Dgraph image copied from the filesystem in the Docker environment. This is a known issue
with some integration tests.
