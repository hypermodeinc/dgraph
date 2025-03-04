# Contributing to Dgraph

- [Getting Started](#getting-started)
- [Setting Up the Development Environment](#setting-up-the-development-environment)
  - [Prerequisites](#prerequisites)
  - [Setup Dgraph from source repo](#setup-dgraph-from-source-repo)
  - [Setup Badger from source repo](#setup-badger-from-source-repo)
  - [Protocol buffers](#protocol-buffers)
  - [Build Dgraph](#build-dgraph)
  - [Build Docker Image](#build-docker-image)
  - [Testing](#testing)
- [Contributing](#contributing)
  - [Guidelines](#guidelines)
  - [Code style](#code-style)
  - [License Header](#license-header)
  - [Signed Commits](#signed-commits)

## Getting Started

- Read the [Getting Started Guide](https://dgraph.io/docs/get-started/)
- [Take the Dgraph tour](https://dgraph.io/tour/)

## Setting Up the Development Environment

### Prerequisites

- Install [Git](https://git-scm.com/) (may be already installed on your system, or available through
  your OS package manager)
- Install [Make](https://www.gnu.org/software/make/) (may be already installed on your system, or
  available through your OS package manager)
- Install [Docker](https://docs.docker.com/install/) and
  [Docker Compose](https://docs.docker.com/compose/install/).
- [Install Go 1.22.12 or above](https://golang.org/doc/install).

### Setup Dgraph from source repo

It's best to put the Dgraph repo somewhere in `$GOPATH`.

```bash
mkdir -p "$(go env GOPATH)/src/github.com/hypermodeinc"
cd "$(go env GOPATH)/src/github.com/hypermodeinc"
git clone https://github.com/hypermodeinc/dgraph.git
cd ./dgraph
make install
```

This will put the source code in a Git repo under `$GOPATH/src/github.com/hypermodeinc/dgraph` and
compile the binaries to `$GOPATH/bin`.

### Setup Badger from source repo

Dgraph source repo vendors its own version of Badger. If you are just working on Dgraph, you do not
necessarily need to check out Badger from its own repo. However, if you want to contribute to Badger
as well, you will need to check it out from its own repo.

```bash
go get -t -v github.com/dgraph-io/badger
```

This will put the source code in a Git repo under `$GOPATH/src/github.com/dgraph-io/badger`.

### Protocol buffers

We use [protocol buffers](https://developers.google.com/protocol-buffers/) to serialize data between
our server and the Go client and also for inter-worker communication. If you make any changes to the
`.proto` files, you would have to recompile them.

Install the `protoc` compiler which is required for compiling proto files used for gRPC
communication. Get `protoc` version 3.0.0 or above from
[GitHub releases page](https://github.com/google/protobuf/releases/latest) (look for the binary
releases at the bottom, or compile from sources
[following the instructions](https://github.com/google/protobuf/tree/main/src)).

We use [gogo protobuf](https://github.com/gogo/protobuf) in Dgraph. To get the protocol buffer
compiler plugin from gogo run

```bash
go get -u github.com/gogo/protobuf/protoc-gen-gofast
```

To compile the proto file using the `protoc` plugin and the gogo compiler plugin run the command
`make regenerate` from within the directory containing the `.proto` files.

```bash
cd protos
make regenerate
```

This should generate the required `.pb.go` file.

### Build Dgraph

You can build Dgraph using `make dgraph` or `make install` which add the version information to the
binary.

- `make dgraph`: Creates a `dgraph` binary at `./dgraph/dgraph`
- `make install`: Creates a `dgraph` binary at `$GOPATH/bin/dgraph`. You should add `$GOPATH/bin` to
  your `$PATH` if it isn't there already.

```sh
$ make install
Installing Dgraph...
Commit SHA256: 15839b156e9920ca2c4ab718e1e73b6637b8ecec
Old SHA256: 596e362ede7466a2569d19ded91241e457e665ada785d05a902af2c6f2cea508
Installed dgraph to /Users/<homedir>/go/bin/dgraph

$ dgraph version
Dgraph version   : v24.0.2-103-g15839b156
Dgraph codename  : dgraph
Dgraph SHA-256   : 9ce738cd055dfebdef5d68b2a49ea4e062e597799498607dbd1bb618d48861a6
Commit SHA-1     : 15839b156
Commit timestamp : 2025-01-10 17:56:49 -0500
Branch           : username/some-branch-that-im-on
Go version       : go1.22.12
jemalloc enabled : true

For Dgraph official documentation, visit https://dgraph.io/docs.
For discussions about Dgraph     , visit https://discuss.dgraph.io.
For fully-managed Dgraph Cloud   , visit https://dgraph.io/cloud.

Licensed variously under the Apache Public License 2.0 and Dgraph Community License.
Copyright 2015-2025 Hypermode Inc.
```

### Build Docker Image

```sh
make image-local
```

To build a test Docker image from source, use `make image-local`. This builds a linux-compatible
Dgraph binary using `make dgraph` and creates a Docker image tagged `dgraph/dgraph:local`. You can
then use this local image to test Dgraph in your local Docker setup.

### Testing

Dgraph employs a ~~complex~~ sophisticated testing framework that includes extensive test coverage.
Due to the comprehensive nature of these tests, a complete test run can take several hours,
depending on your hardware. To manage this complex testing process efficiently, we've developed a
custom test framework implemented in Go, which resides in the [./t](/t) directory. This specialized
framework provides enhanced control and flexibility beyond what's available through standard Go
testing framework.

For dependencies, runner flags and instructions for running tests on non-Linux machines, see the
[README](t/README.md) in the [_t_](t) folder.

Other integration tests do not use the testing framework located in the `t` folder. Consult the
[github actions definitions](.github) folder to discover the tests we run as part of our continuous
delivery process.

Non-integration unit tests exist for many core packages that can be exercised without invoking the
testing framework. For instance, to unit test the core DQL parsing package:
`go test github.com/hypermodeinc/dgraph/v24/dql`.

## Contributing

### Guidelines

Over years of writing big scalable systems, we are convinced that striving for simplicity wherever
possible is the only way to build robust systems. This simplicity could be in design, could be in
coding, or could be achieved by rewriting an entire module, that you may have painstakingly finished
yesterday.

- **Pull requests are welcome**, as long as you're willing to put in the effort to meet the
  guidelines. After you fork dgraph, create your pull request against our `main` branch
- Aim for clear, well written, maintainable code
- Simple and minimal approach to features, like Go
- New features must include passing unit tests, and integration tests when appropriate
- Refactoring existing code now for better performance, better readability or better testability
  wins over adding a new feature
- Don't add a function to a module that you don't use right now, or doesn't clearly enable a planned
  functionality
- Don't ship a half done feature, which would require significant alterations to work fully
- Avoid [Technical debt](https://en.wikipedia.org/wiki/Technical_debt) like cancer
- Leave the code cleaner than when you began

### Code style

- We're following [Go Code Review](https://github.com/golang/go/wiki/CodeReviewComments)
- Use `go fmt` to format your code before committing
- If you see _any code_ which clearly violates the style guide, please fix it and send a pull
  request. No need to ask for permission
- Avoid unnecessary vertical spaces. Use your judgment or follow the code review comments
- Wrap your code and comments to 120 characters, unless doing so makes the code less legible

### License Header

Every new source file must begin with a license header.

Most of Dgraph, Badger, and the Dgraph clients (dgo, dgraph-js, pydgraph and dgraph4j) are licensed
under the Apache 2.0 license:

```sh
/*
  * Copyright 2016-2025 Hypermode Inc. and Contributors
  *
  * Licensed under the Apache License, Version 2.0 (the "License");
  * you may not use this file except in compliance with the License.
  * You may obtain a copy of the License at
  *
  *    http://www.apache.org/licenses/LICENSE-2.0
  *
  * Unless required by applicable law or agreed to in writing, software
  * distributed under the License is distributed on an "AS IS" BASIS,
  * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
  * See the License for the specific language governing permissions and
  * limitations under the License.
  */
```

### Signed Commits

Signed commits help in verifying the authenticity of the contributor. We use signed commits in
Dgraph, and we prefer it, though it's not compulsory to have signed commits. This is a recommended
step for people who intend to contribute to Dgraph on a regular basis.

Follow instructions to generate and setup GPG keys for signing code commits on this
[Github Help page](https://help.github.com/articles/signing-commits-with-gpg/).
