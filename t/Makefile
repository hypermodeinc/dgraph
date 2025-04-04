#
# SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
# SPDX-License-Identifier: Apache-2.0
#

# linux || darwin
GOOS          ?= $(shell go env GOOS)
GOPATH        ?= $(shell go env GOPATH)
MIN_GO_VERSION = "1.24.2"

all: test

.PHONY: check
check:
	@which go > /dev/null 2>&1 || (echo "Error: Go is not installed or not in PATH" && exit 1)
	@go version | awk '{split($$3,v,"go"); if(v[2] < $(MIN_GO_VERSION)) {print "Error: Go version must be $(MIN_GO_VERSION) or higher"; exit 1}}'
	@which docker > /dev/null 2>&1 || (echo "Error: Docker is not installed or not in PATH" && exit 1)
	@which gotestsum > /dev/null 2>&1 || (echo "Error: gotestsum is not installed or not in PATH" && exit 1)
	@if [ "$(GOOS)" = "linux" ]; then \
		which protoc > /dev/null 2>&1 || (echo "Error: protoc is not installed or not in PATH" && exit 1); \
	fi
	@echo "All dependencies are installed"
	@if [ -f "$(GOPATH)/bin/dgraph" ]; then \
		file $(GOPATH)/bin/dgraph | grep -q "ELF.*executable" || (echo "Error: dgraph binary is not a Linux executable" && exit 1); \
	else \
		echo "Error: dgraph binary not found in $(GOPATH)/bin" && exit 1; \
	fi
	@echo "The dgraph binary is a Linux executable (as required)"

.PHONY: test
test: check
# 	build the t.go binary
	@go build .
#	clean go testcache
	@go clean -testcache
#	run t.go with specified arguments; otherwise run standard suite
	@if [ -n "$(args)" ]; then \
		./t $(args); \
	else \
		./t; \
	fi
#	clean up docker containers after test execution
	@./t -r
