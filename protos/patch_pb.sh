#!/bin/bash
# This script modifies the pb.pb.go file to replace certain fields and methods
# to use the custom data type `Sensitive`.
#
# We are doing this because google.golang.org/protobuf does not support custom data types,
# and we have sensitive data that we do not want to expose in logs.
# To address this, we defined a `Sensitive` data type to handle such cases.
#
# This patch script applies the necessary changes to pb.pb.go.
PB_GEN_FILE="./pb/pb.pb.go"

# Function to perform in-place sed that works on both macOS and Linux
sed_inplace() {
	local tmpfile
	tmpfile=$(mktemp)
	sed "$1" "$2" >"${tmpfile}" && mv "${tmpfile}" "$2"
	rm -f "${tmpfile}"
}

# Apply the replacements using the cross-platform function
sed_inplace 's/SessionToken string/SessionToken Sensitive/' "${PB_GEN_FILE}"
sed_inplace 's/SecretKey    string/SecretKey    Sensitive/' "${PB_GEN_FILE}"
sed_inplace 's/GetSessionToken() string {/GetSessionToken() Sensitive {/' "${PB_GEN_FILE}"
sed_inplace 's/GetSecretKey() string/GetSecretKey() Sensitive/' "${PB_GEN_FILE}"

echo "Patches applied successfully."
