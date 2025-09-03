#!/usr/bin/env bash

#
# SPDX-FileCopyrightText: © Hypermode Inc. <hello@hypermode.com>
# SPDX-License-Identifier: Apache-2.0
#

set -e

readonly PROTOCMINVER="3.6.1"

which protoc &>/dev/null || (
	echo "Error: protoc not found" >&2
	exit 1
)

PROTOCVER=$(protoc --version | awk '{printf $2}')

# CompareSemVer compares the minimum version minver against another version curver.
# If the version is below our min it will exit with non-zero to trigger error in make.
function CompareSemVer() {
	local minver=(${1//./ })
	local curver=(${2//./ })

	echo -n "Checking protoc for semantic version $1 or newer... "

	for i in 0 1 2; do
		if [[ ${minver[${i}]} -gt ${curver[${i}]} ]]; then
			echo "FAIL" >&2
			echo "Error: version $2 is lower than the required version $1" >&2
			exit 1
		elif [[ ${curver[${i}]} -gt ${minver[${i}]} ]]; then
			break
		fi
	done
}

function CheckProtobufIncludes() {
	echo -n "Checking for directory /usr/include/google/protobuf or /usr/local/include/google/protobuf... "
	if ! { [[ -d /usr/include/google/protobuf ]] || [[ -d /usr/local/include/google/protobuf ]] || [[ -d /opt/homebrew/include/google/protobuf ]]; }; then
		echo "FAIL" >&2
		echo "Missing protobuf headers in /usr/include/google/protobuf or /usr/local/include/google/protobuf:" \
			"directory not found." >&2
		echo "Download and install protoc and the protobuf headers by installing protoc via a package manager" \
			"or downloading it from the protobuf releases page:" >&2
		echo "https://github.com/protocolbuffers/protobuf/releases/" >&2
		exit 1
	fi
}

CompareSemVer "${PROTOCMINVER}" "${PROTOCVER}"
echo "OK"

CheckProtobufIncludes
echo "OK"

exit 0
