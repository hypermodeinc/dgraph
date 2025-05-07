#
# SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
# SPDX-License-Identifier: Apache-2.0
#

# Update BADGER_PB_VERSION when upgrading Badger major versions
BADGER_PB_VERSION = 4
DGO_PB_VERSION = 250

DGO_PATH    := github.com/dgraph-io/dgo/v${DGO_PB_VERSION}
BADGER_PATH := github.com/dgraph-io/badger/v${BADGER_PB_VERSION}
TMPDIR     := $(shell mktemp -d)
PROTO_PATH := ${TMPDIR}/src:.
PROTO_PATH := ${PROTO_PATH}:${TMPDIR}/src/${DGO_PATH}/protos
PROTO_PATH := ${PROTO_PATH}:${TMPDIR}/src/${BADGER_PATH}/pb

.PHONY: help
help:
	@echo Use \"make regenerate\" to proceed.

.PHONY: clean
clean:
	@mkdir -p pb && rm -f pb/pb.pb.go

.PHONY: tidy-deps
tidy-deps:
	@go mod tidy -v

.PHONY: check
check:
	@./depcheck.sh && \
		(echo "Installing proto libraries to versions in go.mod." ; \
		go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.31.0 ; \
		go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.3.0)

.PHONY: copy-protos
copy-protos:
	@mkdir -p ${TMPDIR}/src/${DGO_PATH}/protos
	@mkdir -p ${TMPDIR}/src/${BADGER_PATH}/pb
	@cp $(shell go list -m -f "{{.Dir}}" ${BADGER_PATH})/pb/badgerpb${BADGER_PB_VERSION}.proto \
	 ${TMPDIR}/src/${BADGER_PATH}/pb/pb.proto
	@cp $(shell go list -m -f "{{.Dir}}" ${DGO_PATH})/protos/api.proto \
	 ${TMPDIR}/src/${DGO_PATH}/protos/api.proto
	@cp $(shell go list -m -f "{{.Dir}}" ${DGO_PATH})/protos/api.v25.proto \
	 ${TMPDIR}/src/${DGO_PATH}/protos/api.v25.proto

.PHONY: patch-pb
patch-pb:
	./patch_pb.sh

.PHONY: regenerate
regenerate: tidy-deps copy-protos check clean
	@protoc \
		--proto_path=/usr/local/include \
		--proto_path=/usr/include \
		--proto_path=${PROTO_PATH} \
		--go_out=pb --go-grpc_out=pb \
		--go_opt=paths=source_relative \
		--go-grpc_opt=paths=source_relative \
		--go_opt=M{DGO_PATH}/protos/api.proto={DGO_PATH}/protos/api \
		--go_opt=M{DGO_PATH}/protos/api.v25.proto={DGO_PATH}/protos/api.v25 \
		pb.proto
	@$(MAKE) patch-pb
	@rm -rf ${TMPDIR}
	@echo Done.
