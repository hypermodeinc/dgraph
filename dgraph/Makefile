#
# Copyright 2025 Hypermode Inc. and Contributors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#

USER_ID         = $(shell id -u)
BIN             = dgraph
BUILD          ?= $(shell git rev-parse --short HEAD)
BUILD_CODENAME ?= dgraph
BUILD_DATE     ?= $(shell git log -1 --format=%ci)
BUILD_BRANCH   ?= $(shell git rev-parse --abbrev-ref HEAD)
BUILD_VERSION  ?= $(shell git describe --always --tags)

# check if DGRAPH_VERSION flag is set (used for release pipeline)
ifneq ($(DGRAPH_VERSION),)
# remove arch suffix from DGRAPH_VERSION for CD steps only
BUILD_VERSION := $(shell echo ${DGRAPH_VERSION} | sed -e 's/-amd64//' -e 's/-arm64//')
endif

GOOS          ?= $(shell go env GOOS)
GOARCH        ?= $(shell go env GOARCH)
# Only build with jemalloc on Linux, mac
ifeq ($(GOOS),$(filter $(GOOS),linux darwin))
	BUILD_TAGS ?= jemalloc
endif
GOPATH        ?= $(shell go env GOPATH)

# Build-time Go variables
dgraphVersion   = github.com/hypermodeinc/dgraph/v24/x.dgraphVersion
dgraphCodename  = github.com/hypermodeinc/dgraph/v24/x.dgraphCodename
gitBranch       = github.com/hypermodeinc/dgraph/v24/x.gitBranch
lastCommitSHA   = github.com/hypermodeinc/dgraph/v24/x.lastCommitSHA
lastCommitTime  = github.com/hypermodeinc/dgraph/v24/x.lastCommitTime

BUILD_FLAGS   ?= -ldflags '-X ${lastCommitSHA}=${BUILD} -X "${lastCommitTime}=${BUILD_DATE}" -X "${dgraphVersion}=${BUILD_VERSION}" -X "${dgraphCodename}=${BUILD_CODENAME}" -X ${gitBranch}=${BUILD_BRANCH}'

# Insert build tags if specified
ifneq ($(strip $(BUILD_TAGS)),)
	BUILD_FLAGS += -tags '$(BUILD_TAGS)'
	ifneq (,$(findstring oss,$(BUILD_TAGS)))
		BUILD_VERSION := $(BUILD_VERSION)-oss
	endif
endif

# Build with compiler optimizations disabled, which will help debugging with dlv.
ifneq ($(strip $(BUILD_DEBUG)),)
	BUILD_FLAGS += -gcflags="all=-N -l"
endif

# Build with race detector enabled.
ifneq ($(strip $(BUILD_RACE)),)
	BUILD_FLAGS += -race
endif

# jemalloc stuff
HAS_JEMALLOC = $(shell test -f /usr/local/lib/libjemalloc.a && echo "jemalloc")
JEMALLOC_URL = "https://github.com/jemalloc/jemalloc/releases/download/5.3.0/jemalloc-5.3.0.tar.bz2"

# go install variables
HAS_SHA256SUM = $(shell which sha256sum)
INSTALL_TARGET = $(GOPATH)/bin/$(BIN)
ifneq ($(strip $(shell go env GOBIN)),)
	INSTALL_TARGET = $(shell go env GOBIN)/$(BIN)
endif
ifneq ($(GOOS)_$(GOARCH),$(shell go env GOHOSTOS)_$(shell go env GOHOSTARCH))
	INSTALL_TARGET = $(GOPATH)/bin/$(GOOS)_$(GOARCH)/$(BIN)
endif

.PHONY: all $(BIN)
all: $(BIN)

$(BIN): clean jemalloc
	@go mod tidy
	@go build $(BUILD_FLAGS) -o $(BIN)

test-coverage-binary: clean jemalloc
	@go test -c -covermode=atomic -coverpkg ../... $(BUILD_FLAGS) -o $(BIN)

clean:
	@rm -f $(BIN)

uninstall:
	@go clean -i -x

install: jemalloc
	@echo "Commit SHA256: `git rev-parse HEAD`"
	@if [ "$(HAS_SHA256SUM)" ] ; then \
		echo "Old SHA256:" `sha256sum $(INSTALL_TARGET) 2>/dev/null | cut -c-64` ; \
	fi
	@go mod tidy
	@go install $(BUILD_FLAGS)
	@echo "Installed $(BIN) to $(INSTALL_TARGET)"
	@if [ "$(HAS_SHA256SUM)" ] ; then \
		echo "New SHA256:" `sha256sum $(INSTALL_TARGET) 2>/dev/null | cut -c-64` ; \
	fi

jemalloc:
	@if [ -z "$(HAS_JEMALLOC)" ] ; then \
		mkdir -p /tmp/jemalloc-temp && cd /tmp/jemalloc-temp ; \
		echo "Downloading jemalloc" ; \
		curl -f -s -L ${JEMALLOC_URL} -o jemalloc.tar.bz2 ; \
		tar xjf ./jemalloc.tar.bz2 ; \
		cd jemalloc-5.3.0 ; \
		./configure --with-jemalloc-prefix='je_' --with-malloc-conf='background_thread:true,metadata_thp:auto'; \
		make ; \
		if [ "$(USER_ID)" = "0" ]; then \
			make install ; \
		else \
			echo "==== Need sudo access to install jemalloc" ; \
			sudo make install ; \
		fi \
	fi
